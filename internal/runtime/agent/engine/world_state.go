package engine

import (
	"context"
	"errors"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
)

func (e *Engine) projectWorldState(
	ctx context.Context,
	history []provider.Message,
	catalog tool.CatalogSnapshot,
	advertised map[string]bool,
) ([]provider.Message, []provider.Message, []promptcontext.Receipt,
	agentcontext.WorldProjection, error) {
	scope := e.executionScope()
	if scope == nil {
		return nil, nil, nil, agentcontext.WorldProjection{}, errors.New("turn scope is not active")
	}
	e.ensureClosedTurnCheckpoints()
	scope.mu.Lock()
	baseline := scope.state.context.World()
	if scope.state.contextLedger != nil &&
		agentcontext.WorldBaselineValid(history, baseline) {
		receipts := append([]promptcontext.Receipt(nil), scope.state.contextSeen...)
		scope.mu.Unlock()
		return promptcontext.FrozenWorld(e.promptMessages(), receipts, baseline)
	}
	scope.mu.Unlock()
	evidence := e.evidenceSet().Snapshot(e.options.EvidenceLimit)
	if e.options.RepoContext != nil {
		e.options.Metrics.Evidence(
			len(evidence.Risks),
			len(evidence.Reminders),
		)
	}
	e.planMu.Lock()
	plan := e.planText
	var planReceipt *promptcontext.Receipt
	if e.planReceipt != nil {
		copy := *e.planReceipt
		planReceipt = &copy
	}
	e.planMu.Unlock()
	sessionState, err := e.sessionStatePartition(history)
	if err != nil {
		return nil, nil, nil, agentcontext.WorldProjection{}, err
	}
	projected, err := promptcontext.ProjectWorldState(
		promptcontext.WorldProjectionInput{
			Context: ctx, History: history, Stable: e.promptMessages(),
			Catalog: catalog, Advertised: advertised, Baseline: baseline,
			Turn: e.turn, ImageInput: scope.spec.Route.Model().Capabilities.ImageInput,
			Policy: scope.spec.Policy, CodingPolicy: e.options.CodingPolicy,
			Memory: scope.spec.Memory, Skills: scope.spec.Skills,
			Budgets: e.options.ContextBudgets, Repository: e.options.RepoContext,
			WorkingSet: e.workingLedger().Select(e.turn, e.options.WorkingSetLimit),
			Evidence:   evidence, PlanText: plan, PlanReceipt: planReceipt,
			SessionState: sessionState, Narrative: e.narrativePartition(),
		},
	)
	if err != nil {
		return nil, nil, nil, agentcontext.WorldProjection{}, err
	}
	scope.mu.Lock()
	scope.state.selections = cloneSelections(projected.Selections)
	scope.state.contextSeen = append(
		[]promptcontext.Receipt(nil),
		projected.Receipts...,
	)
	scope.state.context.SetWorld(projected.Projection.Baseline)
	scope.mu.Unlock()
	return projected.Stable, projected.Delta, projected.Receipts, projected.Projection, nil
}
