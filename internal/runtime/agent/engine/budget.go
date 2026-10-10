package engine

import (
	"fmt"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	contextview "github.com/fwtllh-png/QCode/internal/runtime/agent/contextview"
)

func (e *Engine) checkBudget(
	estimatedInput uint64,
	turnUsage provider.Usage,
	stepUsage provider.Usage,
	outputReserve uint64,
) (uint64, error) {
	route := e.activeRoute()
	current := turnUsage
	current.Add(stepUsage)
	e.syncSessionTitleState(current)
	sessionUsage, cost := e.accountedUsage()
	turnID := ""
	if scope := e.runningScope(); scope != nil {
		turnID = scope.spec.Identity.TurnID
	}
	pending, auxiliaryTurn, pendingCost := e.auxiliaryBudgetUsage(turnID)
	cost += e.guardianTurnCostAdjustment(turnID, route.Model().Pricing)
	sessionUsage.Add(pending)
	cost += pendingCost
	request := agentcontext.BudgetRequest{
		ContextTokens:  route.Model().Limits.ContextTokens,
		EstimatedInput: estimatedInput, OutputReserve: outputReserve,
		SessionUsage: sessionUsage, TurnUsage: turnUsage, StepUsage: stepUsage,
		MaxTokens: e.options.Budget.MaxTokens, SpentCostUSD: cost,
		MaxCostUSD: e.options.Budget.MaxCostUSD, Pricing: route.Model().Pricing,
		Scope: e.turnBudgetScope(),
	}
	if e.options.Budget.MaxTurnTokens > 0 {
		turnRequest := request
		turnRequest.SessionUsage = provider.Usage{}
		turnRequest.TurnUsage.Add(auxiliaryTurn)
		turnRequest.MaxTokens = e.options.Budget.MaxTurnTokens
		turnRequest.MaxCostUSD = 0
		var err error
		request.OutputReserve, err = agentcontext.CheckBudget(turnRequest)
		if err != nil {
			return 0, err
		}
	}
	return agentcontext.CheckBudget(request)
}

func (e *Engine) turnBudgetScope() string {
	if scope := e.runningScope(); scope != nil &&
		scope.spec.Identity.TurnID != "" {
		return "turn:" + scope.spec.Identity.TurnID
	}
	return "turn"
}

func (e *Engine) economicAdmission(turnUsage, stepUsage provider.Usage,
	currentOutput, finalizationOutput, remainingCalls uint64,
) contextview.EconomicAdmission {
	capacity := e.contextCapacity()
	sessionUsage, _ := e.accountedUsage()
	return contextview.ResolveEconomicAdmission(contextview.EconomicAdmissionRequest{
		HardInput: capacity.HardInputTokens, OperatorInput: e.autoCompactLimit(),
		SessionUsage: sessionUsage, TurnUsage: turnUsage, StepUsage: stepUsage,
		MaxSessionTokens: e.options.Budget.MaxTokens,
		MaxTurnTokens:    e.options.Budget.MaxTurnTokens,
		CurrentOutput:    currentOutput, FinalizationOutput: finalizationOutput,
		RemainingCalls: remainingCalls, TurnScope: e.turnBudgetScope(),
		OperatorConfigured: e.options.Context.Window.AutoTokens != 0,
	})
}

func (e *Engine) budgetConvergence(turnUsed uint64) (provider.Message, bool) {
	outputReserve := e.maxOutputFor(e.activeRoute())
	used, limit := turnUsed, e.options.Budget.MaxTurnTokens
	stage, finishOnly := contextview.BudgetStage(used, limit, outputReserve)
	sessionUsed := e.BudgetSnapshot().TokensUsed + turnUsed
	sessionStage, sessionFinishOnly := contextview.BudgetStage(
		sessionUsed,
		e.options.Budget.MaxTokens,
		outputReserve,
	)
	if sessionStage > stage {
		used, limit = sessionUsed, e.options.Budget.MaxTokens
		stage, finishOnly = sessionStage, sessionFinishOnly
	}
	if stage == 0 {
		return provider.Message{}, false
	}
	scope := e.runningScope()
	if scope == nil {
		return provider.Message{}, finishOnly
	}
	scope.mu.Lock()
	if stage <= scope.state.budgetStage {
		scope.mu.Unlock()
		return provider.Message{}, finishOnly
	}
	scope.state.budgetStage = stage
	scope.mu.Unlock()
	message := provider.TextMessage(provider.RoleUser, fmt.Sprintf(
		"[token_budget]\nused_tokens=%d\nmax_tokens=%d\nstage=%s\n"+
			"Stop broad exploration. When stage=finish_only, do not request exploratory tools: produce the concise user-facing final answer now using current evidence. Preserve required completion/verification tools and report blockers.",
		used, limit, []string{"", "converge", "finish_only"}[stage],
	))
	message.Turn = e.turn
	return message, finishOnly
}

func (e *Engine) Usage() (provider.Usage, float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.accountedUsage()
}
