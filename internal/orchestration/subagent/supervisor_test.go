package subagent

import (
	"context"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type estimatingRuntime struct {
	recordingRuntime
	projected uint64
	limit     uint64
}

func (r *estimatingRuntime) EstimateTurn(
	context.Context, string, string,
) (TurnEstimate, error) {
	return TurnEstimate{
		ProjectedTokens: r.projected, LimitTokens: r.limit,
	}, nil
}

func TestDelegateBindsSessionParentAndRejectsOverBudget(t *testing.T) {
	runtime := &estimatingRuntime{projected: 17698, limit: 15000}
	control, err := OpenControl(Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
		Budget:    Budget{MaxDepth: 2, MaxParallel: 4},
		SessionID: "session-admit",
	}, DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = control.Delegate(t.Context(), DelegationRequest{
		Intent: DelegationIntent{
			SessionID: "session-admit", TaskName: "audit-tests",
			Role: RoleReview, Objective: "audit tests",
			ExpectedOutput: "findings", Trigger: TriggerUser,
			Budget: AgentBudget{MaxTokens: 15000},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "token budget exhausted") {
		t.Fatalf("admit error = %v", err)
	}
	listed := control.List(ListFilter{
		SessionID: "session-admit", IncludeClosed: true,
	})
	for _, agent := range listed {
		if agent.Status == StatusRunning {
			t.Fatalf("rejected spawn left a running agent: %+v", agent)
		}
	}
}

func TestDelegateBindsParentMailbox(t *testing.T) {
	runtime := &estimatingRuntime{projected: 100, limit: 20000}
	control, err := OpenControl(Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
		Budget:    Budget{MaxDepth: 2, MaxParallel: 4},
		SessionID: "session-parent",
	}, DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	result, err := control.Delegate(t.Context(), DelegationRequest{
		Intent: DelegationIntent{
			SessionID: "session-parent", TaskName: "review-core",
			Role: RoleReview, Objective: "review core",
			ExpectedOutput: "findings", Trigger: TriggerUser,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Agent.Parent != SessionParentID {
		t.Fatalf("parent = %q", result.Agent.Parent)
	}
	if err := control.Settle(Result{
		AgentID: result.Agent.ID, Status: StatusCompleted,
		Summary: "done",
	}); err != nil {
		t.Fatal(err)
	}
	messages := control.Mailbox().Receive(SessionParentID)
	if len(messages) != 1 || messages[0].Kind != MessageCompletion {
		t.Fatalf("completion mailbox = %+v", messages)
	}
}

func TestDelegateSerializesWhenProviderIsHot(t *testing.T) {
	runtime := &estimatingRuntime{projected: 100, limit: 20000}
	control, err := OpenControl(Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
		Budget:    Budget{MaxDepth: 2, MaxParallel: 4},
		SessionID: "session-hot",
	}, DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	control.BindProviderGate(func() bool { return true })
	first, err := control.Delegate(t.Context(), DelegationRequest{
		Intent: DelegationIntent{
			SessionID: "session-hot", TaskName: "audit-one",
			Role: RoleReview, Objective: "review one",
			ExpectedOutput: "findings", Trigger: TriggerUser,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = control.Delegate(t.Context(), DelegationRequest{
		Intent: DelegationIntent{
			SessionID: "session-hot", TaskName: "audit-two",
			Role: RoleReview, Objective: "review two",
			ExpectedOutput: "findings", Trigger: TriggerUser,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "provider cooldown") {
		t.Fatalf("second spawn = %v", err)
	}
	listed := control.List(ListFilter{
		SessionID: "session-hot", IncludeClosed: true,
	})
	running := 0
	for _, agent := range listed {
		if agent.Status == StatusRunning {
			running++
		}
	}
	if running != 1 || first.Agent == nil {
		t.Fatalf("hot spawn should keep one running child: %+v", listed)
	}
}

func TestClassifySettlementReasonCodes(t *testing.T) {
	token := protocol.NewBudgetExhausted(protocol.BudgetExhaustion{
		Resource: protocol.BudgetResourceTokens, Scope: "agent:test",
		Used: 17698, Limit: 15000, Projected: true,
	}, nil)
	cost := protocol.NewBudgetExhausted(protocol.BudgetExhaustion{
		Resource: protocol.BudgetResourceCostMicrounits, Scope: "agent:test",
		Used: 2, Limit: 1,
	}, nil)
	rate := protocol.NewFault(
		protocol.CodeUnavailable, "请求暂时过于频繁", true,
		protocol.FaultMetadata{
			Origin: protocol.FaultOriginProvider, Disposition: protocol.FaultRetryTurn,
			Reason: protocol.ProblemReasonProviderRateLimited,
		}, nil,
	)
	quota := protocol.NewFault(
		protocol.CodeResourceExhausted, "provider quota exhausted", false,
		protocol.FaultMetadata{
			Origin: protocol.FaultOriginProvider, Disposition: protocol.FaultResumeTurn,
		}, nil,
	)
	overflow := protocol.NewProblem(
		protocol.CodeResourceExhausted, "context window exceeded", false, nil,
	)
	wrongOrigin := protocol.ProblemOf(token)
	wrongOrigin.Fault.Origin = protocol.FaultOriginTool
	wrongCode := protocol.ProblemOf(rate)
	wrongCode.Code = protocol.CodeResourceExhausted
	permanent := protocol.ProblemOf(rate)
	permanent.Fault.Disposition = protocol.FaultFailTurn
	// The machine-readable facts must work independently of message language.
	token.Message, cost.Message = "令牌预算不足", "费用预算不足"
	testCases := []struct {
		name      string
		status    Status
		problem   *protocol.Problem
		reason    string
		retryable bool
	}{
		{"completed", StatusCompleted, nil, "", false},
		{"completed ignores failure", StatusCompleted, rate, "", false},
		{"interrupted", StatusInterrupted, token, ReasonInterrupted, false},
		{"notes only", StatusFailed, nil, ReasonTaskFailed, false},
		{"token budget", StatusFailed, token, ReasonBudgetExhausted, true},
		{"cost budget", StatusFailed, cost, ReasonBudgetExhausted, true},
		{"provider limit", StatusErrored, rate, ReasonProviderRateLimited, true},
		{"provider quota", StatusFailed, quota, ReasonTaskFailed, false},
		{"context overflow", StatusFailed, overflow, ReasonTaskFailed, false},
		{"wrong origin", StatusFailed, wrongOrigin, ReasonTaskFailed, false},
		{"wrong code", StatusFailed, wrongCode, ReasonTaskFailed, false},
		{"no recovery", StatusFailed, permanent, ReasonProviderRateLimited, false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			failure := SettlementFailure{}
			if problem := testCase.problem; problem != nil {
				failure = SettlementFailure{
					Code: problem.Code, Message: problem.Message, Fault: problem.Fault,
				}
			}
			summary := "reviewed rate limit and resource_exhausted handling"
			notes := []string{"token budget exhausted; cost budget exhausted"}
			reason, message, retryable := ClassifySettlement(
				testCase.status, failure, notes, summary,
			)
			if reason != testCase.reason || retryable != testCase.retryable {
				t.Fatalf("classification = %q %v, want %q %v",
					reason, retryable, testCase.reason, testCase.retryable)
			}
			wantMessage := summary
			if testCase.status == StatusFailed {
				wantMessage = notes[0]
				if failure.Message != "" {
					wantMessage = failure.Message
				}
			}
			if message != wantMessage {
				t.Fatalf("message = %q, want %q", message, wantMessage)
			}
		})
	}
}
