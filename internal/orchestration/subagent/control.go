package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/fwtllh-png/QCode/internal/observability/tracecontext"
)

// Status is the stable agent lifecycle state.
type Status string

const (
	StatusRequested         Status = "requested"
	StatusStarting          Status = "starting"
	StatusRunning           Status = "running"
	StatusWaiting           Status = "waiting"
	StatusCompleted         Status = "completed"
	StatusFailed            Status = "failed"
	StatusInterrupted       Status = "interrupted"
	StatusIntegrating       Status = "integrating"
	StatusIntegrated        Status = "integrated"
	StatusIntegrationFailed Status = "integration_failed"
	StatusClosed            Status = "closed"

	StatusPendingInit = StatusRequested
	StatusErrored     = StatusFailed
	StatusShutdown    = StatusClosed
)

// ListFilter selects agents for List.
type ListFilter struct {
	SessionID     string
	ParentID      string
	IncludeClosed bool
}

// WaitResult is returned by Wait when agents reach a terminal status or time out.
type WaitResult struct {
	TimedOut bool
	Agents   []Agent
}

func isTerminal(status Status) bool {
	switch status {
	case StatusCompleted, StatusFailed, StatusInterrupted, StatusIntegrated,
		StatusIntegrationFailed, StatusClosed:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether status is a settled child state.
func IsTerminal(status Status) bool { return isTerminal(status) }

func sameParent(filter, parent string) bool {
	if filter == parent {
		return true
	}
	return IsSessionParent(filter) && IsSessionParent(parent)
}

func CanTransition(from, to Status) bool {
	switch from {
	case "":
		return to == StatusRequested
	case StatusRequested:
		return to == StatusStarting || to == StatusCompleted ||
			to == StatusFailed || to == StatusInterrupted || to == StatusClosed
	case StatusStarting:
		return to == StatusRunning || to == StatusCompleted ||
			to == StatusFailed || to == StatusInterrupted
	case StatusRunning:
		return to == StatusWaiting || to == StatusCompleted ||
			to == StatusFailed || to == StatusInterrupted
	case StatusWaiting:
		return to == StatusRunning || to == StatusCompleted ||
			to == StatusFailed || to == StatusInterrupted
	case StatusCompleted:
		return to == StatusStarting || to == StatusIntegrating || to == StatusClosed
	case StatusFailed, StatusInterrupted:
		return to == StatusStarting || to == StatusIntegrating || to == StatusClosed
	case StatusIntegrating:
		return to == StatusIntegrated || to == StatusIntegrationFailed
	case StatusIntegrationFailed:
		return to == StatusIntegrating || to == StatusClosed
	case StatusIntegrated:
		return to == StatusClosed
	default:
		return false
	}
}

// List returns agent snapshots matching filter, sorted by ID.
// When a durable Graph is attached, missing children are merged from projection
// so restart List does not depend on the in-memory mailbox.
func (m *Manager) List(filter ListFilter) []Agent {
	m.mu.Lock()
	graph := m.graph
	out := make([]Agent, 0, len(m.agents))
	seen := make(map[string]struct{}, len(m.agents))
	for _, agent := range m.agents {
		if filter.SessionID != "" && agent.SessionID != filter.SessionID {
			continue
		}
		if !filter.IncludeClosed && (agent.Closed || agent.Status == StatusShutdown) {
			continue
		}
		if filter.ParentID != "" &&
			!sameParent(filter.ParentID, agent.Parent) {
			continue
		}
		out = append(out, cloneAgent(agent))
		seen[agent.ID] = struct{}{}
	}
	m.mu.Unlock()

	if graph != nil {
		edges, err := graph.ListChildren(filter.SessionID, filter.ParentID)
		if err == nil {
			for _, edge := range edges {
				if _, ok := seen[edge.ChildID]; ok {
					continue
				}
				if !filter.IncludeClosed && edge.Status == StatusShutdown {
					continue
				}
				if filter.ParentID != "" &&
					!sameParent(filter.ParentID, edge.ParentID) {
					continue
				}
				out = append(out, *agentFromEdge(edge))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Wait blocks until every listed agent is terminal, or any agent is terminal when
// agentIDs is empty. A non-positive timeout means wait until ctx is done.
func (m *Manager) Wait(ctx context.Context, agentIDs []string, timeout time.Duration) (WaitResult, error) {
	return m.WaitSession(ctx, "", agentIDs, timeout)
}

func (m *Manager) WaitSession(
	ctx context.Context,
	sessionID string,
	agentIDs []string,
	timeout time.Duration,
) (WaitResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	wake := context.AfterFunc(ctx, func() {
		m.mu.Lock()
		m.wait.Broadcast()
		m.mu.Unlock()
	})
	defer wake()

	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		done, ready, err := m.waitProgressLocked(sessionID, agentIDs)
		if err != nil {
			return WaitResult{}, err
		}
		if ready {
			return WaitResult{Agents: done}, nil
		}
		if err := ctx.Err(); err != nil {
			return WaitResult{}, err
		}
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return WaitResult{TimedOut: true, Agents: done}, nil
			}
			timer := time.AfterFunc(remaining, func() {
				m.mu.Lock()
				m.wait.Broadcast()
				m.mu.Unlock()
			})
			m.wait.Wait()
			timer.Stop()
			continue
		}
		m.wait.Wait()
	}
}

func (m *Manager) waitProgressLocked(
	sessionID string,
	agentIDs []string,
) ([]Agent, bool, error) {
	if len(agentIDs) == 0 {
		var done []Agent
		for _, agent := range m.agents {
			if sessionID != "" && agent.SessionID != sessionID {
				continue
			}
			if isTerminal(agent.Status) {
				done = append(done, cloneAgent(agent))
			}
		}
		sort.Slice(done, func(i, j int) bool { return done[i].ID < done[j].ID })
		return done, len(done) > 0, nil
	}
	done := make([]Agent, 0, len(agentIDs))
	for _, id := range agentIDs {
		agent, ok := m.agents[id]
		if !ok || sessionID != "" && agent.SessionID != sessionID {
			return nil, false, fmt.Errorf("agent %q not found", id)
		}
		if !isTerminal(agent.Status) {
			return done, false, nil
		}
		done = append(done, cloneAgent(agent))
	}
	return done, true, nil
}

// FollowUp starts another turn on a resident agent. Rejects closed/shutdown and
// busy (running) agents — no silent steer queue in this slice.
func (m *Manager) FollowUp(ctx context.Context, agentID, prompt string) (string, error) {
	if len(prompt) == 0 || len(prompt) > 16<<10 {
		return "", errors.New("follow-up prompt is empty or exceeds 16 KiB")
	}
	m.mu.Lock()
	agent, ok := m.agents[agentID]
	if !ok || agent.Closed || agent.Status == StatusClosed {
		m.mu.Unlock()
		return "", errors.New("agent not found")
	}
	if occupiesSlot(agent.Status) {
		m.mu.Unlock()
		return "", errors.New("agent is busy")
	}
	m.mu.Unlock()
	body, err := json.Marshal(map[string]string{"prompt": prompt})
	if err != nil {
		return "", err
	}
	if _, err := m.mailbox.Enqueue(Message{
		SessionID: agent.SessionID,
		From:      "parent", To: agentID, Kind: MessageTask,
		Body: body, TriggerTurn: true,
	}); err != nil {
		return "", err
	}
	return m.startTurn(ctx, agentID, "", m.runtime)
}

// Interrupt requests cancellation; the runtime's terminal result owns settlement.
// The active slot is retained until Settle, and the worktree remains for FollowUp.
func (m *Manager) Interrupt(ctx context.Context, agentID string) (Status, error) {
	m.mu.Lock()
	agent, ok := m.agents[agentID]
	if !ok || agent.Closed || agent.Status == StatusShutdown {
		m.mu.Unlock()
		return "", errors.New("agent not found")
	}
	prev := agent.Status
	turnID := agent.TurnID
	runtime := m.runtime
	if isTerminal(prev) {
		m.mu.Unlock()
		return prev, nil
	}
	if runtime != nil {
		m.mu.Unlock()
		if turnID == "" || prev == StatusStarting {
			return prev, errors.New("agent turn has not started")
		}
		// CancelTurn only submits an operation. Settle may run before or after
		// it returns, and a FollowUp may already own a different turn by then.
		return prev, runtime.CancelTurn(ctx, agentID, turnID)
	}
	// Without a runtime there is no asynchronous result producer. Publish a
	// synthetic result through the same atomic result/usage/mailbox transition.
	defer m.mu.Unlock()
	result := Result{
		AgentID: agentID, ThreadID: agent.ThreadID, TurnID: turnID,
		Status: StatusInterrupted, Summary: "interrupt requested",
	}
	return prev, m.transitionLocked(
		agent, result.Status, turnID, result.Digest(),
		"parent", "interrupt requested", &result,
	)
}

func (m *Manager) AwaitApproval(agentID, requestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[agentID]
	if !ok || agent.Closed {
		return errors.New("agent not found")
	}
	if agent.Status == StatusWaiting {
		return nil
	}
	if agent.Status != StatusRunning {
		return fmt.Errorf("agent %s cannot wait for approval from %s", agentID, agent.Status)
	}
	return m.transitionLocked(
		agent, StatusWaiting, agent.TurnID,
		"waiting for approval "+requestID,
		"runtime", "child approval requested", nil,
	)
}

func (m *Manager) ResumeApproval(agentID, requestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[agentID]
	if !ok || agent.Closed {
		return errors.New("agent not found")
	}
	if agent.Status == StatusRunning {
		return nil
	}
	if agent.Status != StatusWaiting {
		return fmt.Errorf("agent %s cannot resume approval from %s", agentID, agent.Status)
	}
	return m.transitionLocked(
		agent, StatusRunning, agent.TurnID,
		"approval resolved "+requestID,
		"runtime", "child approval resolved", nil,
	)
}

// Complete marks an agent completed (runtime/test hook for Wait).
func (m *Manager) Complete(agentID, message string) error {
	return m.settleSynthetic(agentID, StatusCompleted, message)
}

// Fail marks an agent errored (runtime/test hook for Wait).
func (m *Manager) Fail(agentID, message string) error {
	return m.settleSynthetic(agentID, StatusFailed, message)
}

func (m *Manager) settleSynthetic(agentID string, status Status, message string) error {
	m.mu.Lock()
	agent, ok := m.agents[agentID]
	if !ok || agent.Closed || agent.Status == StatusClosed {
		m.mu.Unlock()
		return errors.New("agent not found")
	}
	result := Result{
		AgentID: agentID, ThreadID: agent.ThreadID, TurnID: agent.TurnID,
		Status: status, Summary: message,
	}
	m.mu.Unlock()
	return m.Settle(result)
}

func (m *Manager) startTurn(
	ctx context.Context, agentID, prompt string, runtime RuntimeHost,
) (string, error) {
	m.mu.Lock()
	agent, ok := m.agents[agentID]
	if !ok || agent.Closed || agent.Status == StatusClosed {
		m.mu.Unlock()
		return "", errors.New("agent not found")
	}
	if err := m.transitionLocked(
		agent, StatusStarting, "", "", "runtime", "turn requested", nil,
	); err != nil {
		m.mu.Unlock()
		return "", err
	}
	pending := m.mailbox.PendingSession(agent.SessionID, agentID)
	traceParent, traceState := agent.TraceParent, agent.TraceState
	m.mu.Unlock()
	prompt = promptWithMessages(prompt, pending)
	if traceParent != "" {
		carrier := map[string]string{
			tracecontext.HeaderTraceParent: traceParent,
			tracecontext.HeaderTraceState:  traceState,
		}
		if traced, traceErr := tracecontext.ExtractMap(ctx, carrier); traceErr == nil {
			ctx = traced
		}
	}
	var (
		out string
		err error
	)
	if runtime == nil {
		out = "takeover:" + agentID + ":" + prompt
	} else {
		out, err = runtime.StartTurn(ctx, agentID, prompt)
	}
	m.mu.Lock()
	agent, ok = m.agents[agentID]
	if !ok || agent.Closed {
		m.mu.Unlock()
		if err != nil {
			return "", err
		}
		return "", errors.New("agent not found")
	}
	if err != nil {
		result := Result{
			AgentID: agentID, ThreadID: agent.ThreadID, Status: StatusFailed,
			Summary: err.Error(),
		}
		if transitionErr := m.transitionLocked(
			agent, StatusFailed, "", err.Error(),
			"runtime", "start turn failed", &result,
		); transitionErr != nil {
			m.mu.Unlock()
			return "", errors.Join(err, transitionErr)
		}
		m.mu.Unlock()
		return "", err
	}
	// A real child turn runs asynchronously, so Settle may already have observed
	// its terminal event before this returns. Terminal wins: overwriting it with
	// running would leave the agent unreachable for Wait forever.
	if isTerminal(agent.Status) && agent.TurnID == out {
		m.mu.Unlock()
		return out, nil
	}
	if err := m.transitionLocked(
		agent, StatusRunning, out, "", "runtime", "turn accepted", nil,
	); err != nil {
		m.mu.Unlock()
		return "", err
	}
	m.mu.Unlock()
	_ = m.mailbox.Ack(pending)
	return out, nil
}
