package subagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (m *Manager) transitionLocked(
	agent *Agent,
	status Status,
	turnID, message, actor, reason string,
	result *Result,
) error {
	if agent == nil {
		return errors.New("agent is required")
	}
	if !CanTransition(agent.Status, status) {
		return fmt.Errorf(
			"agent %s cannot transition from %s to %s",
			agent.ID, agent.Status, status,
		)
	}
	reserve := !occupiesSlot(agent.Status) && occupiesSlot(status)
	var reserveTokens, reserveMicros uint64
	if reserve {
		var err error
		reserveTokens, reserveMicros, err = m.reserveTurnLocked(agent)
		if err != nil {
			return err
		}
	}
	release := occupiesSlot(agent.Status) && !occupiesSlot(status)
	transition := GraphTransition{
		SessionID: agent.SessionID,
		AgentID:   agent.ID, Path: agent.Path,
		ExpectedRevision: agent.Revision, Status: status,
		TurnID: turnID, Message: message,
		OperationID: fmt.Sprintf("agent:%s:%d", agent.ID, agent.Revision+1),
		Actor:       actor, Reason: reason, Result: result,
		ReserveTokens: reserveTokens, ReserveMicros: reserveMicros,
		ReleaseBudget: release,
		CreatedAt:     time.Now().UTC(),
	}
	if result != nil {
		stored := *result
		transition.Result = &stored
		transition.ReasonCode = stored.ReasonCode
		if stored.ReasonCode != "" && strings.TrimSpace(message) == "" {
			transition.Message = stored.ReasonCode
		}
		envelope := completionEnvelope(agent, stored)
		body, err := json.Marshal(envelope)
		if err != nil {
			return err
		}
		target := BindSessionParent(agent.Parent)
		completion, err := m.mailbox.Prepare(Message{
			SessionID: agent.SessionID,
			From:      agent.ID, To: target, Kind: MessageCompletion,
			PayloadRef: envelope.ResultRef, Body: body,
		})
		if err != nil {
			return err
		}
		transition.Completion = &envelope
		transition.CompletionMessage = &completion
	}
	if err := m.recordTransitionLocked(transition); err != nil {
		return err
	}
	agent.Status = status
	agent.Revision++
	agent.LastMessage = message
	if turnID != "" {
		agent.TurnID = turnID
	}
	if result != nil {
		stored := *transition.Result
		agent.Result = &stored
		if integrationBaseline(agent, stored) {
			agent.IntegrationResult = &stored
		}
		agent.SpentTokens += stored.Usage.Tokens()
		agent.SpentMicros += stored.Usage.CostMicrounits
		m.mailbox.Accept(*transition.CompletionMessage)
	}
	switch {
	case reserve:
		agent.ReservedTokens = reserveTokens
		agent.ReservedMicros = reserveMicros
	case release:
		agent.ReservedTokens = 0
		agent.ReservedMicros = 0
	}
	m.wait.Broadcast()
	return nil
}

func occupiesSlot(status Status) bool {
	switch status {
	case StatusStarting, StatusRunning, StatusWaiting:
		return true
	default:
		return false
	}
}

func OccupiesSlot(status Status) bool { return occupiesSlot(status) }

func completionEnvelope(agent *Agent, result Result) CompletionEnvelope {
	paths := result.WritePaths()
	if len(paths) > 64 {
		paths = paths[:64]
	}
	return CompletionEnvelope{
		AgentPath: agent.Path, Status: result.Status,
		Summary: firstLine(strings.TrimSpace(result.Summary), 400),
		ResultRef: fmt.Sprintf(
			"agent-result://%s/%s/%d",
			agent.SessionID, agent.ID, agent.Revision+1,
		),
		ReceiptRef:   result.Context.Digest,
		ChangedPaths: append([]string(nil), paths...),
		Verification: result.Verification, Usage: result.Usage,
		IntegrationReady: integrationReady(agent, result),
	}
}

func integrationReady(agent *Agent, result Result) bool {
	return integrationBaseline(agent, result) &&
		len(result.Unresolved) == 0
}

func integrationBaseline(agent *Agent, result Result) bool {
	return agent != nil &&
		agent.Isolated &&
		result.Status == StatusCompleted &&
		len(result.WritePaths()) > 0
}

func promptWithMessages(prompt string, messages []Message) string {
	if len(messages) == 0 {
		return prompt
	}
	var out strings.Builder
	if strings.TrimSpace(prompt) != "" {
		out.WriteString(prompt)
		out.WriteString("\n\n")
	}
	out.WriteString("<agent_mailbox>\n")
	for _, message := range messages {
		encoded, _ := json.Marshal(message)
		out.Write(encoded)
		out.WriteByte('\n')
	}
	out.WriteString("</agent_mailbox>")
	return out.String()
}
