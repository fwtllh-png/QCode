package protocol

import (
	"errors"
	"strings"
	"testing"
)

func TestBudgetExhaustionIsStructuredAndResumable(t *testing.T) {
	sentinel := errors.New("ledger exhausted")
	testCases := []struct {
		name     string
		resource BudgetResource
		used     uint64
		limit    uint64
		reason   string
	}{
		{
			name: "tokens", resource: BudgetResourceTokens,
			used: 101, limit: 100,
			reason: ProblemReasonTokenBudgetExhausted,
		},
		{
			name: "cost", resource: BudgetResourceCostMicrounits,
			used: 11, limit: 10,
			reason: ProblemReasonCostBudgetExhausted,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			problem := NewBudgetExhausted(BudgetExhaustion{
				Resource: testCase.resource,
				Scope:    "turn:fixture",
				Used:     testCase.used,
				Limit:    testCase.limit,
			}, sentinel)
			if problem.Code != CodeResourceExhausted ||
				problem.Retryable ||
				problem.Fault == nil ||
				problem.Fault.Disposition != FaultResumeTurn ||
				problem.Fault.Reason != testCase.reason ||
				problem.Fault.SideEffects != SideEffectDraft ||
				problem.Fault.RecoveryAction == "" ||
				problem.Details == nil ||
				problem.Details.Reason != testCase.reason ||
				problem.Details.ResourceID != "turn:fixture" ||
				!errors.Is(problem, sentinel) {
				t.Fatalf("budget problem = %+v", problem)
			}
		})
	}
}

func TestProjectedBudgetExhaustionIsNotReportedAsSpent(t *testing.T) {
	problem := NewBudgetExhausted(BudgetExhaustion{
		Resource:  BudgetResourceTokens,
		Scope:     "agent:fixture",
		Used:      244_985,
		Limit:     200_000,
		Projected: true,
	}, nil)
	if !strings.Contains(problem.Message, "projected 244985") ||
		strings.Contains(problem.Message, "used 244985") {
		t.Fatalf("projected budget message = %q", problem.Message)
	}
}

func TestBudgetExhaustionRejectsInvalidMetadata(t *testing.T) {
	for _, exhaustion := range []BudgetExhaustion{
		{Resource: BudgetResourceTokens, Limit: 1},
		{Resource: "unknown", Scope: "turn", Limit: 1},
		{Resource: BudgetResourceTokens, Scope: "turn"},
	} {
		problem := NewBudgetExhausted(exhaustion, nil)
		if problem.Code != CodeInternal ||
			problem.Fault == nil ||
			problem.Fault.Disposition != FaultFailTurn {
			t.Fatalf("invalid budget problem = %+v", problem)
		}
	}
}
