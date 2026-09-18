package subagent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type estimatingRuntime struct {
	recordingRuntime
	projected uint64
	limit     uint64
}

func (r *estimatingRuntime) EstimateTurn(
	context.Context, string, string,
) (subagent.TurnEstimate, error) {
	return subagent.TurnEstimate{
		ProjectedTokens: r.projected, LimitTokens: r.limit,
	}, nil
}

func TestDelegateBindsSessionParentAndRejectsOverBudget(t *testing.T) {
	runtime := &estimatingRuntime{projected: 17698, limit: 15000}
	control, err := subagent.OpenControl(subagent.Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
		Budget:    subagent.Budget{MaxDepth: 2, MaxParallel: 4},
		SessionID: "session-admit",
	}, subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = control.Delegate(t.Context(), subagent.DelegationRequest{
		Intent: subagent.DelegationIntent{
			SessionID: "session-admit", TaskName: "audit-tests",
			Role: subagent.RoleReview, Objective: "audit tests",
			ExpectedOutput: "findings", Trigger: subagent.TriggerUser,
			Budget: subagent.AgentBudget{MaxTokens: 15000},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "token budget exhausted") {
		t.Fatalf("admit error = %v", err)
	}
	listed := control.List(subagent.ListFilter{
		SessionID: "session-admit", IncludeClosed: true,
	})
	for _, agent := range listed {
		if agent.Status == subagent.StatusRunning {
			t.Fatalf("rejected spawn left a running agent: %+v", agent)
		}
	}
}

func TestDelegateBindsParentMailbox(t *testing.T) {
	runtime := &estimatingRuntime{projected: 100, limit: 20000}
	control, err := subagent.OpenControl(subagent.Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
		Budget:    subagent.Budget{MaxDepth: 2, MaxParallel: 4},
		SessionID: "session-parent",
	}, subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	result, err := control.Delegate(t.Context(), subagent.DelegationRequest{
		Intent: subagent.DelegationIntent{
			SessionID: "session-parent", TaskName: "review-core",
			Role: subagent.RoleReview, Objective: "review core",
			ExpectedOutput: "findings", Trigger: subagent.TriggerUser,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Agent.Parent != subagent.SessionParentID {
		t.Fatalf("parent = %q", result.Agent.Parent)
	}
	if err := control.Settle(subagent.Result{
		AgentID: result.Agent.ID, Status: subagent.StatusCompleted,
		Summary: "done",
	}); err != nil {
		t.Fatal(err)
	}
	messages := control.Mailbox().Receive(subagent.SessionParentID)
	if len(messages) != 1 || messages[0].Kind != subagent.MessageCompletion {
		t.Fatalf("completion mailbox = %+v", messages)
	}
}

func TestReadOnlySpawnSkipsWorktreeProvision(t *testing.T) {
	manager, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: &fakeGate{},
		Budget:    subagent.Budget{MaxDepth: 2, MaxParallel: 2},
		SessionID: "session-readonly",
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Spawn("", subagent.RoleReview, "review only")
	if err != nil {
		t.Fatal(err)
	}
	if child.Isolated || child.Worktree != child.ExecutionRoot {
		t.Fatalf("read-only child provisioned a worktree: %+v", child)
	}
}

func TestDelegateSerializesWhenProviderIsHot(t *testing.T) {
	runtime := &estimatingRuntime{projected: 100, limit: 20000}
	control, err := subagent.OpenControl(subagent.Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
		Budget:    subagent.Budget{MaxDepth: 2, MaxParallel: 4},
		SessionID: "session-hot",
	}, subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	control.BindProviderGate(func() bool { return true })
	first, err := control.Delegate(t.Context(), subagent.DelegationRequest{
		Intent: subagent.DelegationIntent{
			SessionID: "session-hot", TaskName: "audit-one",
			Role: subagent.RoleReview, Objective: "review one",
			ExpectedOutput: "findings", Trigger: subagent.TriggerUser,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = control.Delegate(t.Context(), subagent.DelegationRequest{
		Intent: subagent.DelegationIntent{
			SessionID: "session-hot", TaskName: "audit-two",
			Role: subagent.RoleReview, Objective: "review two",
			ExpectedOutput: "findings", Trigger: subagent.TriggerUser,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "provider cooldown") {
		t.Fatalf("second spawn = %v", err)
	}
	listed := control.List(subagent.ListFilter{
		SessionID: "session-hot", IncludeClosed: true,
	})
	running := 0
	for _, agent := range listed {
		if agent.Status == subagent.StatusRunning {
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
		status    subagent.Status
		problem   *protocol.Problem
		reason    string
		retryable bool
	}{
		{"completed", subagent.StatusCompleted, nil, "", false},
		{"completed ignores failure", subagent.StatusCompleted, rate, "", false},
		{"interrupted", subagent.StatusInterrupted, token, subagent.ReasonInterrupted, false},
		{"notes only", subagent.StatusFailed, nil, subagent.ReasonTaskFailed, false},
		{"token budget", subagent.StatusFailed, token, subagent.ReasonBudgetExhausted, true},
		{"cost budget", subagent.StatusFailed, cost, subagent.ReasonBudgetExhausted, true},
		{"provider limit", subagent.StatusErrored, rate, subagent.ReasonProviderRateLimited, true},
		{"provider quota", subagent.StatusFailed, quota, subagent.ReasonTaskFailed, false},
		{"context overflow", subagent.StatusFailed, overflow, subagent.ReasonTaskFailed, false},
		{"wrong origin", subagent.StatusFailed, wrongOrigin, subagent.ReasonTaskFailed, false},
		{"wrong code", subagent.StatusFailed, wrongCode, subagent.ReasonTaskFailed, false},
		{"no recovery", subagent.StatusFailed, permanent, subagent.ReasonProviderRateLimited, false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			failure := subagent.SettlementFailure{}
			if problem := testCase.problem; problem != nil {
				failure = subagent.SettlementFailure{
					Code: problem.Code, Message: problem.Message, Fault: problem.Fault,
				}
			}
			summary := "reviewed rate limit and resource_exhausted handling"
			notes := []string{"token budget exhausted; cost budget exhausted"}
			reason, message, retryable := subagent.ClassifySettlement(
				testCase.status, failure, notes, summary,
			)
			if reason != testCase.reason || retryable != testCase.retryable {
				t.Fatalf("classification = %q %v, want %q %v",
					reason, retryable, testCase.reason, testCase.retryable)
			}
			wantMessage := summary
			if testCase.status == subagent.StatusFailed {
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
