package engine

import (
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (e *Engine) ApplyPlan(input interact.Plan) error {
	plan := planFromTool(input)
	current := e.buildTruthCapsule(e.buildCompactSummary(nil), nil)
	var resolved []string
	for _, entity := range current.Entities {
		if entity.Kind == agentcontext.EntityGoal || entity.Kind == agentcontext.EntityTodo {
			resolved = append(resolved, entity.ID)
		}
	}
	added := agentcontext.PlanTruthEntities(plan, e.turn)
	decision := (agentcontext.ContextAdmissionController{
		Policy: e.options.Context.TruthRetention,
	}).Decide(current, agentcontext.AdmissionRequest{
		BaseContextRevision:  e.sessionRevision,
		RouteCompatibility:   current.CompatibilityHash,
		AddedMandatory:       added,
		ResolvedMandatoryIDs: resolved,
	})
	if !decision.Allowed {
		return protocol.NewProblem(
			protocol.CodeResourceExhausted,
			"context admission rejected plan update: "+decision.Reason,
			false,
			nil,
		)
	}
	e.setPlan(plan)
	for _, path := range plan.CriticalFiles {
		e.contextAuthority().ObservePath(
			e.options.Workspace,
			agentcontext.SourcePlan,
			e.turn,
			path,
		)
	}
	return nil
}

func (e *Engine) setPlan(plan agentcontext.Plan) {
	text := promptcontext.FormatPlan(plan)
	receipt := promptcontext.PlanReceipt(plan)
	e.planMu.Lock()
	e.planText = text
	e.plan = plan
	e.planReceipt = &receipt
	e.planMu.Unlock()
}

// planFromTool takes ownership of the tool payload before context admission.
func planFromTool(input interact.Plan) agentcontext.Plan {
	var steps []agentcontext.PlanStep
	if input.Steps != nil {
		steps = make([]agentcontext.PlanStep, len(input.Steps))
		for index, step := range input.Steps {
			steps[index] = agentcontext.PlanStep{Title: step.Title, Status: step.Status}
		}
	}
	return agentcontext.Plan{
		Title: input.Title, Steps: steps, Notes: input.Notes,
		Objective: input.Objective, ContextSummary: input.ContextSummary,
		SourcesUsed:         append([]string(nil), input.SourcesUsed...),
		CriticalFiles:       append([]string(nil), input.CriticalFiles...),
		Constraints:         append([]string(nil), input.Constraints...),
		RecommendedApproach: input.RecommendedApproach,
		VerificationPlan:    input.VerificationPlan,
		RisksAndUnknowns:    input.RisksAndUnknowns,
		HandoffPacket:       input.HandoffPacket,
	}
}
