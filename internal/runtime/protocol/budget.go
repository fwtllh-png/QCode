package protocol

import (
	"fmt"
	"strings"
)

type BudgetResource string

const (
	BudgetResourceTokens         BudgetResource = "tokens"
	BudgetResourceCostMicrounits BudgetResource = "cost_microunits"

	ProblemReasonTokenBudgetExhausted = "token_budget_exhausted"
	ProblemReasonCostBudgetExhausted  = "cost_budget_exhausted"
)

// BudgetExhaustion is the shared resumable contract for explicit Token and
// Cost limits. Used and Limit use Tokens or USD microunits according to
// Resource, avoiding floating-point drift across Runtime and Orchestration.
// Projected distinguishes admission demand from already committed spend.
type BudgetExhaustion struct {
	Resource    BudgetResource
	Scope       string
	Used        uint64
	Limit       uint64
	Projected   bool
	SideEffects SideEffectState
}

func NewBudgetExhausted(
	exhaustion BudgetExhaustion,
	cause error,
) *Problem {
	scope := strings.TrimSpace(exhaustion.Scope)
	if scope == "" || exhaustion.Limit == 0 {
		return NewFault(
			CodeInternal,
			"budget exhaustion metadata is invalid",
			false,
			FaultMetadata{
				Origin:      FaultOriginRuntime,
				Disposition: FaultFailTurn,
				SideEffects: SideEffectUnknown,
			},
			cause,
		)
	}
	sideEffects := exhaustion.SideEffects
	if sideEffects == "" {
		sideEffects = SideEffectDraft
	}
	reason := ProblemReasonTokenBudgetExhausted
	usageLabel := "used"
	if exhaustion.Projected {
		usageLabel = "projected"
	}
	message := fmt.Sprintf(
		"token budget exhausted: %s %d, limit %d",
		usageLabel,
		exhaustion.Used,
		exhaustion.Limit,
	)
	recoveryAction := "increase or replenish the explicit token budget, then continue from durable state"
	switch exhaustion.Resource {
	case BudgetResourceTokens:
	case BudgetResourceCostMicrounits:
		reason = ProblemReasonCostBudgetExhausted
		message = fmt.Sprintf(
			"cost budget exhausted: %s $%.6f, limit $%.6f",
			usageLabel,
			float64(exhaustion.Used)/1e6,
			float64(exhaustion.Limit)/1e6,
		)
		recoveryAction = "increase or replenish the explicit cost budget, then continue from durable state"
	default:
		return NewFault(
			CodeInternal,
			"budget exhaustion resource is invalid",
			false,
			FaultMetadata{
				Origin:      FaultOriginRuntime,
				Disposition: FaultFailTurn,
				SideEffects: SideEffectUnknown,
			},
			cause,
		)
	}
	problem := NewFault(
		CodeResourceExhausted,
		message,
		false,
		FaultMetadata{
			Origin:         FaultOriginRuntime,
			Disposition:    FaultResumeTurn,
			Reason:         reason,
			SideEffects:    sideEffects,
			RecoveryAction: recoveryAction,
		},
		cause,
	)
	problem.Details = &ProblemDetails{
		Reason:     reason,
		ResourceID: scope,
	}
	return problem
}
