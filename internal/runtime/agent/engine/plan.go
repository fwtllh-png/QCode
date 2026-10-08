package engine

import (
	"errors"

	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/google/uuid"
)

func (e *Engine) ApplyPlan(input interact.Plan) error {
	plan := planFromTool(input)
	conversation := e.contextAuthority().Conversation()
	if input.ContextSelection != nil {
		if e.referenceRecoveryOnly() && len(input.ContextSelection.GroupIDs) == 0 && len(input.ContextSelection.ItemIDs) == 0 {
			return errors.New("source recovery requires a nonempty context_selection; recover and bind the required definition, or ask for clarification")
		}
		scope := e.runningScope()
		if scope == nil {
			return errors.New("conversation selection requires an active user turn")
		}
		if conversation == nil {
			conversation = &agentcontext.ConversationState{}
		}
		selection := agentcontext.NewConversationSelection(input.ContextSelection.GroupIDs, input.ContextSelection.ItemIDs, e.currentTurn(), scope.spec.Identity.TurnID, scope.spec.Request.Prompt)
		var replacements []agentcontext.ConversationReplacement
		for _, replacement := range input.ContextSelection.Replacements {
			replacements = append(replacements, agentcontext.ConversationReplacement{OldGroupID: replacement.OldGroupID, NewGroupID: replacement.NewGroupID, SourceTurn: selection.SourceTurn, SourceTurnID: selection.SourceTurnID, UserRequestDigest: selection.UserRequestDigest})
		}
		if err := conversation.Select(selection, replacements); err != nil {
			return err
		}
		if input.Steps == nil {
			current := e.currentPlan()
			if err := conversation.ValidatePlan(&current); err != nil {
				return err
			}
			e.contextAuthority().SetConversation(conversation)
			return nil
		}
	}
	for i := range plan.Steps {
		if plan.Steps[i].ID == "" {
			plan.Steps[i].ID = "step:" + uuid.NewString()
		}
	}
	if err := conversation.ValidatePlan(&plan); err != nil {
		return err
	}

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
	e.contextAuthority().SetConversation(conversation)
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
			steps[index] = agentcontext.PlanStep{ID: step.ID, ReferenceItemIDs: append([]string(nil), step.ReferenceItemIDs...), Title: step.Title, Status: step.Status}
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
