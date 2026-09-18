package subagent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// SessionParentID is the durable mailbox actor for top-level children. It is not
// a real Agent Node: it names the parent session turn that spawned them.
const SessionParentID = "parent"

const (
	ReasonBudgetExhausted     = "budget_exhausted"
	ReasonProviderRateLimited = "provider_rate_limited"
	ReasonInterrupted         = "interrupted"
	ReasonTaskFailed          = "task_failed"
)

// TurnEstimate is the first-packet window a child would consume before any tool
// work. LimitTokens is the child's explicit lifecycle budget, not the model
// context window.
type TurnEstimate struct {
	ProjectedTokens uint64
	LimitTokens     uint64
}

// TurnEstimator projects a child's first window without starting a turn.
type TurnEstimator interface {
	EstimateTurn(ctx context.Context, agentID, prompt string) (TurnEstimate, error)
}

// DelegationRequest is the only production spawn contract: tools submit intent
// and context mode, then Supervisor owns bind, admit, start, and cleanup.
type DelegationRequest struct {
	Intent      DelegationIntent
	ContextMode ContextMode
	LastTurns   int
	Source      ContextSourceRef
}

// DelegationResult is the accepted child after admission and turn start.
type DelegationResult struct {
	Agent *Agent
	Turn  string
	Fork  ContextFork
}

// BindSessionParent maps an empty parent to the session actor so top-level
// children share one mailbox recipient with completions and context receipts.
func BindSessionParent(parentID string) string {
	if strings.TrimSpace(parentID) == "" {
		return SessionParentID
	}
	return strings.TrimSpace(parentID)
}

// IsSessionParent reports whether id is the virtual session parent actor.
func IsSessionParent(parentID string) bool {
	return strings.TrimSpace(parentID) == "" ||
		strings.TrimSpace(parentID) == SessionParentID
}

// Delegate binds identity, admits policy and first-packet budget, then starts
// the child turn. A failed admit closes the agent so it never becomes running.
func (c *AgentControl) Delegate(
	ctx context.Context,
	request DelegationRequest,
) (DelegationResult, error) {
	if c == nil {
		return DelegationResult{}, errors.New("agent control is unavailable")
	}
	request.Intent.ParentID = BindSessionParent(request.Intent.ParentID)
	if err := c.admitProviderCapacity(request.Intent.ParentID); err != nil {
		return DelegationResult{}, err
	}
	child, err := c.SpawnIntentContext(ctx, request.Intent)
	if err != nil {
		return DelegationResult{}, err
	}
	cleanup := func(cause error) error {
		return errors.Join(cause, c.Close(child.ID))
	}
	role, err := c.RoleSpec(request.Intent.Role)
	if err != nil {
		return DelegationResult{}, cleanup(err)
	}
	fork, err := c.ForkContext(ctx, ContextRequest{
		Mode:      request.ContextMode,
		LastTurns: request.LastTurns,
		Source:    request.Source,
		Agent:     *child,
		Role:      role,
		Objective: request.Intent.Objective,
		Trigger:   child.DelegationTrigger,
	})
	if err != nil {
		return DelegationResult{}, cleanup(err)
	}
	if err := c.admitFirstWindow(ctx, *child, fork.Prompt); err != nil {
		return DelegationResult{}, cleanup(err)
	}
	turn, err := c.Takeover(ctx, child.ID, fork.Prompt)
	if err != nil {
		return DelegationResult{}, cleanup(err)
	}
	accepted, ok := c.Agent(child.ID)
	if ok {
		child = &accepted
	}
	return DelegationResult{Agent: child, Turn: turn, Fork: fork}, nil
}

func (c *AgentControl) admitFirstWindow(
	ctx context.Context,
	agent Agent,
	prompt string,
) error {
	limit := agent.Budget.MaxTokens
	if limit == 0 {
		return nil
	}
	estimator, ok := c.manager.runtime.(TurnEstimator)
	if !ok || estimator == nil {
		return nil
	}
	estimate, err := estimator.EstimateTurn(ctx, agent.ID, prompt)
	if err != nil {
		return err
	}
	if estimate.LimitTokens == 0 {
		estimate.LimitTokens = limit
	}
	if estimate.ProjectedTokens == 0 ||
		estimate.ProjectedTokens <= estimate.LimitTokens {
		return nil
	}
	return protocol.NewBudgetExhausted(protocol.BudgetExhaustion{
		Resource:  protocol.BudgetResourceTokens,
		Scope:     "agent:" + agent.ID,
		Used:      estimate.ProjectedTokens,
		Limit:     estimate.LimitTokens,
		Projected: true,
	}, fmt.Errorf(
		"raise max_tokens to at least %d before spawning this child",
		estimate.ProjectedTokens,
	))
}

func (c *AgentControl) admitProviderCapacity(parentID string) error {
	if c == nil || c.providerHot == nil || !c.providerHot() {
		return nil
	}
	if c.activeDelegates(parentID) == 0 {
		return nil
	}
	return protocol.NewProblem(
		protocol.CodeUnavailable,
		"provider cooldown is active; wait for the running child to settle before spawning another",
		true,
		nil,
	)
}

func (c *AgentControl) activeDelegates(parentID string) int {
	if c == nil {
		return 0
	}
	count := 0
	for _, agent := range c.List(ListFilter{ParentID: parentID}) {
		switch agent.Status {
		case StatusStarting, StatusRunning, StatusWaiting:
			count++
		}
	}
	return count
}

// SettlementFailure contains only the primary terminal failure. Receipt notes
// and model output are display text, not evidence of failure or retryability.
type SettlementFailure struct {
	Code    protocol.ErrorCode
	Message string
	Fault   *protocol.FaultMetadata
}

// ClassifySettlement derives a stable reason from the terminal status and fault.
func ClassifySettlement(
	status Status, failure SettlementFailure, notes []string, summary string,
) (
	reasonCode, message string, retryable bool,
) {
	switch status {
	case StatusInterrupted:
		return ReasonInterrupted, firstNonEmpty(summary, "interrupted"), false
	case StatusFailed:
	default:
		return "", strings.TrimSpace(summary), false
	}
	message = strings.TrimSpace(failure.Message)
	if message == "" {
		message = firstSettlementNote(notes, summary, "task failed")
	}
	if fault := failure.Fault; fault != nil {
		switch fault.Reason {
		case protocol.ProblemReasonTokenBudgetExhausted,
			protocol.ProblemReasonCostBudgetExhausted:
			if failure.Code == protocol.CodeResourceExhausted &&
				fault.Origin == protocol.FaultOriginRuntime {
				return ReasonBudgetExhausted, message,
					protocol.FaultAllowsTurnRecovery(fault)
			}
		case protocol.ProblemReasonProviderRateLimited:
			if failure.Code == protocol.CodeUnavailable &&
				fault.Origin == protocol.FaultOriginProvider {
				return ReasonProviderRateLimited, message,
					protocol.FaultAllowsTurnRecovery(fault)
			}
		}
	}
	return ReasonTaskFailed, message, false
}

func SuggestedAction(reasonCode string) string {
	switch reasonCode {
	case ReasonBudgetExhausted:
		return "increase or replenish the exhausted explicit token or cost budget, then followup_task"
	case ReasonProviderRateLimited:
		return "wait for the shared provider cooldown, then followup_task"
	case ReasonInterrupted:
		return "followup_task to resume the resident agent"
	default:
		return ""
	}
}

func firstSettlementNote(notes []string, summary, fallback string) string {
	for _, note := range notes {
		if text := strings.TrimSpace(note); text != "" {
			return text
		}
	}
	if text := strings.TrimSpace(summary); text != "" {
		return text
	}
	return fallback
}
