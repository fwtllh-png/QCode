package subagent

import (
	"fmt"
	"time"
)

// RecoveryNode contains the durable node and latest child Turn facts needed at startup.
type RecoveryNode struct {
	AgentID, Path, ThreadID, TurnID string
	Status                          Status
	Revision                        uint64
	TurnStatus                      string
}

// PlanReconciliation reconstructs child status transitions from durable facts.
func PlanReconciliation(sessionID string, nodes []RecoveryNode, now time.Time) []GraphTransition {
	var transitions []GraphTransition
	for _, value := range nodes {
		turnID, turnStatus := value.TurnID, value.TurnStatus
		revision := value.Revision
		appendTransition := func(status Status, reason string, result *Result) {
			transitions = append(transitions, GraphTransition{
				SessionID: sessionID,
				AgentID:   value.AgentID, Path: value.Path,
				ExpectedRevision: revision, Status: status, TurnID: turnID,
				Message: reason, OperationID: fmt.Sprintf(
					"reconcile:%s:%d", value.AgentID, revision+1,
				),
				Actor: "startup_reconciler", Reason: reason,
				Result: result, CreatedAt: now,
			})
			revision++
		}
		current := value.Status
		if turnStatus == "active" {
			if current == StatusRequested {
				appendTransition(StatusStarting, "rebound accepted durable turn", nil)
				current = StatusStarting
			}
			if current == StatusStarting {
				appendTransition(StatusRunning, "rebound active durable turn", nil)
			}
			continue
		}
		reason := "no durable child turn survived restart"
		if turnID != "" {
			reason = fmt.Sprintf(
				"durable turn %s reached %s before agent result commit",
				turnID, turnStatus,
			)
		}
		result := &Result{
			AgentID: value.AgentID, ThreadID: value.ThreadID, TurnID: turnID,
			Status: StatusFailed, Summary: reason,
		}
		appendTransition(StatusFailed, reason, result)
	}
	return transitions
}

// IntegrationRecovery records an integration candidate and its owning agent revision.
type IntegrationRecovery struct {
	Candidate     IntegrationCandidate
	AgentStatus   Status
	AgentRevision uint64
}

type IntegrationRecoveryPlan struct {
	Candidates []IntegrationCandidate
	Transition *GraphTransition
}

// PlanIntegrationRecovery preserves candidate transitions before the owning
// agent transition, so each step can be durably published and retried.
func PlanIntegrationRecovery(recovery IntegrationRecovery, now time.Time) IntegrationRecoveryPlan {
	var plan IntegrationRecoveryPlan
	candidate := recovery.Candidate
	if candidate.Status == IntegrationPreviewed {
		candidate.Status = IntegrationApplying
		candidate.Revision++
		candidate.UpdatedAt = now
		candidate.Message = "integration interrupted before apply began"
		plan.Candidates = append(plan.Candidates, candidate)
	}
	target := StatusIntegrated
	message := "recovered applied integration"
	if candidate.Status != IntegrationApplied {
		candidate.Status = IntegrationFailed
		candidate.Revision++
		candidate.UpdatedAt = now
		candidate.Message = "integration apply interrupted and workspace journal recovered"
		plan.Candidates = append(plan.Candidates, candidate)
		target, message = StatusIntegrationFailed, candidate.Message
	}
	if recovery.AgentStatus == StatusIntegrating {
		plan.Transition = &GraphTransition{
			SessionID: candidate.SessionID,
			AgentID:   candidate.AgentID, Path: candidate.AgentPath,
			ExpectedRevision: recovery.AgentRevision, Status: target,
			OperationID: "reconcile:integration:" + candidate.AgentID,
			Actor:       "startup_reconciler", Reason: message, Message: message,
			CreatedAt: now,
		}
	}
	return plan
}
