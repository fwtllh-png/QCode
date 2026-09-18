package wire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

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
			children := newChildRuntime(config.Subagent{}, t.TempDir(), nil, nil)
			t.Cleanup(children.close)
			children.manager = control
			children.turns[threadID] = turn
			children.observe(protocol.Event{
				ThreadID: threadID, TurnID: turn.turnID, OperationID: "op-unrelated",
				Data: &protocol.OperationRejectedData{
					Code: budget.Code, Message: "token budget exhausted", Fault: budget.Fault,
				},
			})
			children.observe(protocol.Event{
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
			children.observe(replay)
			select {
			case <-turn.terminalSignal:
			case <-time.After(5 * time.Second):
				t.Fatal("child did not settle")
			}
			children.observe(replay)
			result, ok := control.Result(child.ID)
			if !ok || result.Status != testCase.status ||
				result.ReasonCode != testCase.reason || result.Retryable != testCase.retryable ||
				result.Summary != testCase.summary || result.SuggestedAction != testCase.action {
				t.Fatalf("settlement = %+v, want %+v", result, testCase)
			}
			if result.Usage.Tokens() != 17 || children.governor.Snapshot().SpentTokens != 17 {
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
