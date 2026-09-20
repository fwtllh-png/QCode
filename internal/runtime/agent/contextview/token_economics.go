package contextview

import (
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/platform/tokenestimate"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// EconomicAdmission is the model-input working set allowed after reserving
// output for the current call and the required terminal path.
type EconomicAdmission struct {
	AllowedInput, HardInput, OperatorInput uint64
	Used, Limit, Remaining                 uint64
	CurrentOutput, FinalizationOutput      uint64
	RemainingCalls                         uint64
	Scope, Reason, Source, Provenance      string
	Budgeted                               bool
}

type EconomicAdmissionRequest struct {
	HardInput, OperatorInput           uint64
	SessionUsage, TurnUsage, StepUsage provider.Usage
	MaxSessionTokens, MaxTurnTokens    uint64
	CurrentOutput, FinalizationOutput  uint64
	RemainingCalls                     uint64
	TurnScope                          string
	OperatorConfigured                 bool
}

// ResolveEconomicAdmission uses only authoritative capacity, explicit
// budgets, and committed usage. It contains no model-name tiers or ratios.
func ResolveEconomicAdmission(request EconomicAdmissionRequest) EconomicAdmission {
	operator := request.OperatorInput
	if operator == 0 {
		operator = request.HardInput
	}
	result := EconomicAdmission{
		AllowedInput: min(request.HardInput, operator), HardInput: request.HardInput,
		OperatorInput: operator, CurrentOutput: request.CurrentOutput,
		FinalizationOutput: request.FinalizationOutput,
		RemainingCalls:     maxUint64(1, request.RemainingCalls),
		Reason:             "hard_capacity",
		Source:             "model_capability",
		Provenance:         "model_catalog",
	}
	if request.OperatorConfigured {
		result.Reason = "operator_context_ceiling"
		result.Source = "context.compact.auto_compact_tokens"
		result.Provenance = "operator_config"
	}
	turn := request.TurnUsage
	turn.Add(request.StepUsage)
	session := request.SessionUsage
	session.Add(turn)
	for _, budget := range []struct {
		scope       string
		used, limit uint64
	}{
		{request.TurnScope, turn.Total(), request.MaxTurnTokens},
		{"session", session.Total(), request.MaxSessionTokens},
	} {
		if budget.limit == 0 {
			continue
		}
		remaining := budget.limit - min(budget.limit, budget.used)
		reserved := saturatingAdd(
			saturatingMultiply(result.RemainingCalls, request.CurrentOutput),
			request.FinalizationOutput,
		)
		candidate := (remaining - min(remaining, reserved)) / result.RemainingCalls
		if !result.Budgeted || candidate < result.AllowedInput {
			result.Budgeted, result.AllowedInput = true, candidate
			result.Used, result.Limit = budget.used, budget.limit
			result.Remaining, result.Scope = remaining, budget.scope
			result.Reason = "explicit_token_budget"
			result.Source = budget.scope
			result.Provenance = "operator_config"
		}
	}
	return result
}

func EconomicBudgetError(admission EconomicAdmission, projectedInput uint64) error {
	used := admission.Used
	for _, value := range []uint64{
		projectedInput, admission.CurrentOutput, admission.FinalizationOutput,
	} {
		used = saturatingAdd(used, value)
	}
	return protocol.NewBudgetExhausted(protocol.BudgetExhaustion{
		Resource: protocol.BudgetResourceTokens, Scope: admission.Scope,
		Used: used, Limit: admission.Limit, Projected: true,
	}, nil)
}

func ToolSurfaceBudget(
	context protocol.SampleContextData,
	admission EconomicAdmission,
	resultCapacity uint64,
) (int, int) {
	nonTool := context.EstimatedTokens - min(context.EstimatedTokens, context.HistoryToolTokens)
	toolTokens := admission.AllowedInput - min(admission.AllowedInput, nonTool)
	return tokenByteLimit(toolTokens), tokenByteLimit(min(toolTokens, resultCapacity))
}

func tokenByteLimit(tokens uint64) int {
	return int(tokenestimate.BytesForTokens(tokens))
}

func BudgetStage(used, limit, outputReserve uint64) (uint8, bool) {
	if limit == 0 || outputReserve == 0 {
		return 0, false
	}
	remaining := limit - min(limit, used)
	if remaining <= saturatingMultiply(2, outputReserve) {
		return 2, true
	}
	if remaining <= saturatingMultiply(3, outputReserve) {
		return 1, false
	}
	return 0, false
}

func saturatingAdd(left, right uint64) uint64 {
	if ^uint64(0)-left < right {
		return ^uint64(0)
	}
	return left + right
}

func saturatingMultiply(left, right uint64) uint64 {
	if left != 0 && right > ^uint64(0)/left {
		return ^uint64(0)
	}
	return left * right
}

func maxUint64(left, right uint64) uint64 {
	if left > right {
		return left
	}
	return right
}
