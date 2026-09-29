package childrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type recoveredChildRuntimeHost struct{}

func (recoveredChildRuntimeHost) StartTurn(
	context.Context, string, string,
) (string, error) {
	return "turn-recovered", nil
}

func (recoveredChildRuntimeHost) CancelTurn(
	context.Context, string, string,
) error {
	return nil
}

type recoveryToolGate struct{}

func (recoveryToolGate) Execute(
	context.Context, string, string, json.RawMessage,
) (tool.Result, error) {
	return tool.Result{Content: "ok"}, nil
}

func TestRunnerUsesAgentGraphAndDirectEventObservation(t *testing.T) {
	source, err := os.ReadFile("runner.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, forbidden := range []string{
		".Events(",
		"time.Sleep(",
		"waitForStart",
		"waitForTerminal",
		"ensurePump",
		"func (c *Runner) pump",
		"orchestration/kernel",
		"orchestration/model",
		"internal/runtime/app/wire",
		"orchestration/admission",
		"orchestration/budget",
		"governor",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("child Runner retained polling or wiring authority: %s", forbidden)
		}
	}
	for _, required := range []string{
		"host.ObserveEvents(c.Observe)",
		"manager.ActivateResident(agentID)",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("child Runner is missing %s", required)
		}
	}
}

func TestTurnIntentUsesEffectiveWorkspaceAuthority(t *testing.T) {
	testCases := []struct {
		role     subagent.Role
		readOnly bool
		want     protocol.TurnIntent
	}{
		{subagent.RoleImplementer, false, protocol.TurnIntentWorkspaceChange},
		{subagent.RoleGeneral, false, protocol.TurnIntentWorkspaceChange},
		{subagent.RoleImplementer, true, protocol.TurnIntentAnswer},
		{subagent.RoleExplore, true, protocol.TurnIntentAnswer},
		{subagent.RolePlan, true, protocol.TurnIntentPlan},
	}
	for _, testCase := range testCases {
		if got := turnIntent(testCase.role, testCase.readOnly); got != testCase.want {
			t.Fatalf(
				"turnIntent(%q, %v) = %q, want %q",
				testCase.role, testCase.readOnly, got, testCase.want,
			)
		}
	}
}

func TestBindRejectsIncompleteDependencies(t *testing.T) {
	children := New(Options{Workspace: t.TempDir()})
	t.Cleanup(children.Close)
	if err := children.Bind(nil, nil, nil); err == nil {
		t.Fatal("Bind accepted missing dependencies")
	}
	if _, err := children.StartTurn(t.Context(), "agent", "prompt"); err == nil {
		t.Fatal("unbound Runner started a turn")
	}
}

func TestBindRestoresActiveChildObservation(t *testing.T) {
	control, err := subagent.OpenControl(subagent.Options{
		Root: t.TempDir(), Gate: recoveryToolGate{},
		Runtime: recoveredChildRuntimeHost{}, Workspace: t.TempDir(), SessionID: "session-recovered",
	}, subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	child, err := control.SpawnSystem(
		"recover child", "", subagent.RoleExplore, "inspect", "report",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Takeover(
		t.Context(), child.ID, "resume after restart",
	); err != nil {
		t.Fatal(err)
	}
	if err := control.AwaitApproval(child.ID, "approval-stable"); err != nil {
		t.Fatal(err)
	}
	current, _ := control.Agent(child.ID)
	child = &current

	threads := app.NewThreadManager(nil)
	threads.SetChildFactory(func(app.ChildSpec) (*app.EngineAdapter, error) {
		return nil, errors.New("recovery test must not instantiate an engine")
	})
	runtime := app.NewRuntime(app.Options{Engine: threads})
	children := New(Options{
		Limits: config.Subagent{
			Workspace: config.SubagentWorkspaceReadOnly,
			WallTime:  time.Minute,
		},
		Workspace: t.TempDir(),
	})
	t.Cleanup(func() {
		children.Close()
		_ = runtime.Close(context.Background())
	})
	if err := children.Bind(runtime, threads, control); err != nil {
		t.Fatal(err)
	}
	threadID := protocol.ThreadID(child.ThreadID)
	recovered, tracked := children.Tracked(threadID)
	children.mu.Lock()
	observerBound := children.removeObserver != nil
	children.mu.Unlock()
	if !tracked || recovered.TurnID != protocol.TurnID(child.TurnID) || !observerBound {
		t.Fatalf(
			"recovered turn = %+v, observer_bound=%v, child=%+v",
			recovered, observerBound, child,
		)
	}
	if _, registered := threads.ChildSpecFor(threadID); !registered {
		t.Fatal("recovered child thread was not registered")
	}

	children.Observe(protocol.Event{
		ThreadID: threadID, TurnID: protocol.TurnID(child.TurnID),
		Data: &protocol.ApprovalResolvedData{
			RequestID: "approval-stable", Decision: protocol.ApprovalApprove,
		},
	})
	resumed, _ := control.Agent(child.ID)
	if resumed.Status != subagent.StatusRunning {
		t.Fatalf("resumed child status = %q, want running", resumed.Status)
	}
	children.Observe(protocol.Event{
		ThreadID: threadID, TurnID: protocol.TurnID(child.TurnID),
		Data: &protocol.TurnCompletedData{Text: "recovered completion"},
	})
	select {
	case <-recovered.Terminal:
	case <-time.After(time.Second):
		t.Fatal("recovered child settlement did not finish")
	}
	result, ok := control.Result(child.ID)
	if !ok || result.Status != subagent.StatusCompleted ||
		result.Summary != "recovered completion" {
		t.Fatalf("recovered result = %+v, ok=%v", result, ok)
	}
}

func TestChildSettlementUsesPrimaryTerminalFacts(t *testing.T) {
	budget := protocol.NewBudgetExhausted(protocol.BudgetExhaustion{
		Resource: protocol.BudgetResourceCostMicrounits, Scope: "agent:test",
		Used: 2, Limit: 1,
	}, nil)
	testCases := []struct {
		name      string
		data      protocol.EventData
		status    subagent.Status
		reason    string
		retryable bool
		summary   string
		action    string
	}{
		{
			name: "success mentioning failures",
			data: &protocol.TurnCompletedData{
				Text: "fixed rate limit and resource_exhausted handling",
			},
			status:  subagent.StatusCompleted,
			summary: "fixed rate limit and resource_exhausted handling",
		},
		{
			name: "cost budget",
			data: &protocol.TurnFailedData{
				Code: budget.Code, Message: "费用预算不足", Fault: budget.Fault,
			},
			status: subagent.StatusFailed, reason: subagent.ReasonBudgetExhausted,
			retryable: true, summary: "费用预算不足",
			action: budget.Fault.RecoveryAction,
		},
		{
			name: "start rejected by budget",
			data: &protocol.OperationRejectedData{
				Code: budget.Code, Message: "费用预算不足", Fault: budget.Fault,
			},
			status: subagent.StatusFailed, reason: subagent.ReasonBudgetExhausted,
			retryable: true, summary: "费用预算不足",
			action: budget.Fault.RecoveryAction,
		},
		{
			name: "provider throttled",
			data: &protocol.TurnFailedData{
				Code: protocol.CodeUnavailable, Message: "请稍后再试",
				Fault: &protocol.FaultMetadata{
					Origin: protocol.FaultOriginProvider, Disposition: protocol.FaultRetryTurn,
					Reason:         protocol.ProblemReasonProviderRateLimited,
					RecoveryAction: "wait for the shared provider cooldown",
				},
			},
			status: subagent.StatusFailed, reason: subagent.ReasonProviderRateLimited,
			retryable: true, summary: "请稍后再试",
			action: "wait for the shared provider cooldown",
		},
		{
			name: "provider quota",
			data: &protocol.TurnFailedData{
				Code: protocol.CodeResourceExhausted, Message: "quota exhausted",
				Fault: &protocol.FaultMetadata{
					Origin: protocol.FaultOriginProvider, Disposition: protocol.FaultResumeTurn,
					RecoveryAction: "restore the provider balance",
				},
			},
			status: subagent.StatusFailed, reason: subagent.ReasonTaskFailed,
			summary: "quota exhausted", action: "restore the provider balance",
		},
		{
			name: "failure without typed reason",
			data: &protocol.TurnFailedData{
				Code: protocol.CodeResourceExhausted, Message: "token budget exhausted",
			},
			status: subagent.StatusFailed, reason: subagent.ReasonTaskFailed,
			summary: "token budget exhausted",
		},
		{
			name:   "interrupted",
			data:   &protocol.TurnCanceledData{Reason: protocol.CancelReasonUserInterrupted},
			status: subagent.StatusInterrupted, reason: subagent.ReasonInterrupted,
			summary: "interrupted", action: subagent.SuggestedAction(subagent.ReasonInterrupted),
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			control, err := subagent.OpenControl(subagent.Options{
				Root: t.TempDir(), Workspace: t.TempDir(),
				Gate: recoveryToolGate{}, Runtime: recoveredChildRuntimeHost{},
			}, subagent.DelegationExplicit)
			if err != nil {
				t.Fatal(err)
			}
			child, err := control.SpawnSystem(
				"settlement fixture", subagent.SessionParentID, subagent.RoleExplore,
				"inspect", "report",
			)
			if err != nil {
				t.Fatal(err)
			}
			turnID, err := control.Takeover(t.Context(), child.ID, "inspect")
			if err != nil {
				t.Fatal(err)
			}
			threadID := protocol.ThreadID(subagent.ThreadIDFor(child.ID))
			turn := &childTurn{
				agentID: child.ID, turnID: protocol.TurnID(turnID),
				startOperation: "op-start", terminalSignal: make(chan struct{}),
			}
			children := New(Options{Workspace: t.TempDir()})
			t.Cleanup(children.Close)
			children.manager = control
			children.turns[threadID] = turn
			children.Observe(protocol.Event{
				ThreadID: threadID, TurnID: turn.turnID, OperationID: "op-unrelated",
				Data: &protocol.OperationRejectedData{
					Code: budget.Code, Message: "token budget exhausted", Fault: budget.Fault,
				},
			})
			children.Observe(protocol.Event{
				ThreadID: threadID, TurnID: turn.turnID,
				Data: &protocol.ExecutionReceiptData{
					InputTokens: 11, OutputTokens: 6,
					UnresolvedIssues: []string{"rate limit; resource_exhausted"},
				},
			})
			event, err := protocol.NewEvent(protocol.EventMeta{
				Sequence: 1, ThreadID: threadID, TurnID: turn.turnID,
				OperationID: turn.startOperation, ItemID: "item-start",
			}, testCase.data)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			var replay protocol.Event
			if err := json.Unmarshal(raw, &replay); err != nil {
				t.Fatal(err)
			}
			children.Observe(replay)
			select {
			case <-turn.terminalSignal:
			case <-time.After(5 * time.Second):
				t.Fatal("child did not settle")
			}
			children.Observe(replay)
			result, ok := control.Result(child.ID)
			if !ok || result.Status != testCase.status ||
				result.ReasonCode != testCase.reason || result.Retryable != testCase.retryable ||
				result.Summary != testCase.summary || result.SuggestedAction != testCase.action {
				t.Fatalf("settlement = %+v, want %+v", result, testCase)
			}
			if result.Usage.Tokens() != 17 ||
				control.SessionBudget(child.SessionID).SpentTokens != 17 {
				t.Fatalf("settlement usage = %+v", result.Usage)
			}
			messages := control.Mailbox().Receive(subagent.SessionParentID)
			if len(messages) != 1 || messages[0].Kind != subagent.MessageCompletion {
				t.Fatalf("completion messages = %+v", messages)
			}
			if !strings.Contains(strings.Join(result.Unresolved, "\n"), "resource_exhausted") {
				t.Fatalf("receipt issues lost: %+v", result)
			}
		})
	}
}

func TestReceiptBeyondTheReservationKeepsTheChildCompleted(t *testing.T) {
	control, err := subagent.OpenControl(subagent.Options{
		Root: t.TempDir(), Workspace: t.TempDir(),
		Gate: recoveryToolGate{}, Runtime: recoveredChildRuntimeHost{},
	}, subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	child, err := control.SpawnIntent(subagent.DelegationIntent{
		SessionID: "session-overdraw", TaskName: "overdraw",
		Role: subagent.RoleExplore, Objective: "inspect", ExpectedOutput: "report",
		Trigger: subagent.TriggerUser, Budget: subagent.AgentBudget{MaxTokens: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	turnID, err := control.Takeover(t.Context(), child.ID, "inspect")
	if err != nil {
		t.Fatal(err)
	}
	if reserved := control.SessionBudget(child.SessionID).ReservedTokens; reserved != 10 {
		t.Fatalf("reserved tokens = %d, want the Agent's lifetime budget", reserved)
	}
	threadID := protocol.ThreadID(child.ThreadID)
	turn := &childTurn{
		agentID: child.ID, turnID: protocol.TurnID(turnID),
		startOperation: "op-start", terminalSignal: make(chan struct{}),
	}
	children := New(Options{Workspace: t.TempDir()})
	t.Cleanup(children.Close)
	children.manager = control
	children.turns[threadID] = turn
	children.Observe(protocol.Event{
		ThreadID: threadID, TurnID: turn.turnID,
		Data: &protocol.ExecutionReceiptData{InputTokens: 11, OutputTokens: 6},
	})
	children.Observe(protocol.Event{
		ThreadID: threadID, TurnID: turn.turnID,
		Data: &protocol.TurnCompletedData{Text: "done"},
	})
	select {
	case <-turn.terminalSignal:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not settle")
	}
	result, ok := control.Result(child.ID)
	if !ok || result.Status != subagent.StatusCompleted || len(result.Unresolved) != 0 {
		t.Fatalf("an overdrawn but finished child must stay completed: %+v", result)
	}
	ledger := control.SessionBudget(child.SessionID)
	if ledger.SpentTokens != 17 || ledger.ReservedTokens != 0 || ledger.ReservedSlots != 0 {
		t.Fatalf("settled ledger = %+v", ledger)
	}
	_, err = control.FollowUp(t.Context(), child.ID, "again")
	var problem *protocol.Problem
	if !errors.As(err, &problem) || problem.Code != protocol.CodeResourceExhausted ||
		problem.Details == nil ||
		problem.Details.Reason != protocol.ProblemReasonTokenBudgetExhausted ||
		problem.Details.ResourceID != "agent:"+child.ID {
		t.Fatalf("follow-up after an overdraw = %v", err)
	}
}
