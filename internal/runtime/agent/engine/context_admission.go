package engine

import (
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func (e *Engine) ContextAdmission(
	additions []agentcontext.TruthEntity,
	resolvedIDs []string,
) agentcontext.AdmissionDecision {
	current := e.buildTruthCapsule(e.buildCompactSummary(nil), nil)
	return (agentcontext.ContextAdmissionController{
		Policy: e.options.Context.TruthRetention,
	}).Decide(current, agentcontext.AdmissionRequest{
		BaseContextRevision:  e.sessionRevision,
		RouteCompatibility:   current.CompatibilityHash,
		AddedMandatory:       additions,
		ResolvedMandatoryIDs: resolvedIDs,
	})
}

func (e *Engine) admitToolBatch(calls []provider.ToolCall) (*tool.Result, error) {
	var reservations []agentcontext.TruthEntity
	for _, call := range calls {
		if call.Name == "request_user_input" {
			entity := agentcontext.NewTruthEntity(
				agentcontext.EntityPendingInput,
				call.ID,
				"pending user input",
				"runtime.input",
			)
			entity.Turn = e.turn
			reservations = append(reservations, entity)
		}

	}
	if len(reservations) == 0 {
		return nil, nil
	}
	decision := e.ContextAdmission(reservations, nil)
	if decision.Allowed {
		return nil, nil
	}
	return &tool.Result{
		Content: "The entire pending tool batch was rejected before execution because its context reservation exceeds capacity: " + decision.Reason +
			". Split the pending operations into smaller batches or resolve pending user input first. Do not resubmit the unchanged batch. Earlier completed tools remain completed. If required facts cannot be reduced, explain the remaining constraint.",
		IsError: true,
		Outcome: &tool.Outcome{Status: tool.OutcomeRejected, Facts: &tool.OutcomeFacts{
			Failure: &tool.FailureFact{Category: "context_reservation_exceeded"},
		}},
		Metadata: map[string]any{
			"error_category":  "context_reservation_exceeded",
			"required_action": "split_batch_or_resolve_obligations",
			"retry_original":  false, "executed": false,
			"projected_truth_bytes": decision.ProjectedTruthBytes,
			"projected_entities":    decision.ProjectedEntities,
		},
	}, nil
}
