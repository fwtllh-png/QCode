package engine

import (
	"encoding/json"
	"fmt"
	"strings"

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
		_, descriptor, _, err := e.options.Tools.ResolveBound(
			call.Name,
			tool.BindingForCall(call),
		)
		if err != nil || descriptor.AccessMode != tool.AccessWrite {
			continue
		}
		count := writeReservationCount(descriptor, call.Arguments)
		for index := range count {
			key := fmt.Sprintf("reservation:%s:%d", call.ID, index)
			entity := agentcontext.NewTruthEntity(
				agentcontext.EntityChange,
				key,
				"reserved workspace change for "+call.Name,
				"runtime.evidence",
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
			". Split the pending operations into smaller batches or verify existing changes first. Do not resubmit the unchanged batch. Earlier completed tools remain completed. If required facts cannot be reduced, explain the remaining constraint.",
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

func writeReservationCount(descriptor tool.Descriptor, arguments string) int {
	resolver := descriptor.ResourceResolver
	count := len(resolver.Templates)
	var values map[string]any
	if json.Unmarshal([]byte(arguments), &values) == nil {
		paths := make(map[string]struct{})
		collect := func(value any) {
			if path, ok := value.(string); ok && strings.TrimSpace(path) != "" {
				paths[path] = struct{}{}
			}
		}
		if items, ok := values[resolver.PathsField].([]any); resolver.PathsField != "" && ok {
			for _, item := range items {
				collect(item)
			}
		}
		if items, ok := values[resolver.ChangesField].([]any); resolver.ChangesField != "" && ok {
			for _, item := range items {
				change, _ := item.(map[string]any)
				collect(change["path"])
				collect(change["to"])
			}
		}
		if patch, ok := values[resolver.PatchField].(string); resolver.PatchField != "" && ok {
			for line := range strings.SplitSeq(patch, "\n") {
				if after, found := strings.CutPrefix(line, "--- "); found {
					collectPatchPath(paths, after)
				} else if after, found := strings.CutPrefix(line, "+++ "); found {
					collectPatchPath(paths, after)
				}
			}
		}
		count += len(paths)
	}
	if count > 0 {
		return count
	}
	// A write-capable tool without resource templates still represents one
	// consequential call, but must not reserve an arbitrary batch of changes.
	return 1
}

func collectPatchPath(paths map[string]struct{}, value string) {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 || fields[0] == "/dev/null" {
		return
	}
	path := strings.TrimPrefix(strings.TrimPrefix(fields[0], "a/"), "b/")
	if path != "" {
		paths[path] = struct{}{}
	}
}
