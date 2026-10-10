package engine

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestScopeCloseReleasesTurnResourcesOnce(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	var released []TurnIdentity
	engine.options.ReleaseTurnResources = func(identity TurnIdentity) {
		released = append(released, identity)
	}
	scope := &Scope{
		engine: engine,
		spec: TurnSpec{Identity: TurnIdentity{
			SessionID: "session-1",
			TurnID:    "turn-1",
		}},
		state: newScopeState(engine),
	}
	engine.publishScope(scope)
	scope.Close()
	scope.Close()
	if len(released) != 1 || released[0].TurnID != "turn-1" {
		t.Fatalf("released identities = %+v", released)
	}
}

func TestRejectedRecoveredApprovalClosesOnlyItsWait(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(map[bool]string{false: "unanswered", true: "early_approve"}[early], func(t *testing.T) {
			engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
			scope := attachTestScope(t, engine)
			kernel := newEngineTurnKernel(protocol.TurnIntentAnswer, "act", nil, 0, nil, nil)
			scope.state.kernel = kernel
			var resolutions []Event
			scope.state.approvalEmit = func(event Event) error { resolutions = append(resolutions, event); return nil }
			calls := []provider.ToolCall{{ID: "stale-call", Name: "write"}, {ID: "other-call", Name: "write"}}
			if err := kernel.StartTools(calls); err != nil {
				t.Fatal(err)
			}
			for _, call := range calls {
				if err := kernel.StartTool(call.ID); err != nil {
					t.Fatal(err)
				}
				if err := kernel.RequireApproval(call.ID, call.ID); err != nil {
					t.Fatal(err)
				}
				if err := engine.RestoreApprovalRequest(toolguard.ApprovalRequest{RequestID: call.ID, CallID: call.ID}); err != nil {
					t.Fatal(err)
				}
			}
			if early && !engine.queueRecoveredApproval(toolguard.ApprovalDecision{RequestID: calls[0].ID, Approved: true}) {
				t.Fatal("early decision was not queued")
			}
			if err := engine.rejectUnresumedApproval(calls[0].ID); err != nil {
				t.Fatal(err)
			}
			pending := kernel.Snapshot().PendingApprovals
			if len(pending) != 1 || pending[calls[1].ID].CallID != calls[1].ID {
				t.Fatal("unrelated approval was retired")
			}
			if early {
				if len(resolutions) != 0 {
					t.Fatal("early user resolution was duplicated")
				}
			} else if len(resolutions) != 1 || resolutions[0].ApprovalResolution.Reason != "approval_recovery_invalidated" {
				t.Fatalf("invalidated wait resolution = %+v", resolutions)
			}
			if engine.queueRecoveredApproval(toolguard.ApprovalDecision{RequestID: calls[0].ID, Approved: true}) {
				t.Fatal("retired approval accepted another decision")
			}
			if err := kernel.CloseTool(calls[0], tool.Result{IsError: true}, nil); err != nil {
				t.Fatalf("rejected result could not close: %v", err)
			}
		})
	}
}

func TestApprovalExpiryResolvesKernelBeforeToolResult(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	scope := attachTestScope(t, engine)
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	scope.mu.Lock()
	scope.state.kernel = kernel
	var emitted Event
	scope.state.approvalEmit = func(event Event) error {
		emitted = event
		return nil
	}
	scope.mu.Unlock()

	call := provider.ToolCall{ID: "approval-call", Name: "write"}
	if err := kernel.StartTools([]provider.ToolCall{call}); err != nil {
		t.Fatal(err)
	}
	if err := kernel.StartTool(call.ID); err != nil {
		t.Fatal(err)
	}
	if err := kernel.RequireApproval("approval-1", call.ID); err != nil {
		t.Fatal(err)
	}
	if err := scope.state.requests.Register(
		turnkernel.RequestApproval,
		"approval-1",
	); err != nil {
		t.Fatal(err)
	}

	if err := engine.expireApprovalWait(toolguard.ApprovalWait{
		RequestID: "approval-1",
		CallID:    call.ID,
		Tool:      call.Name,
		Outcome:   toolguard.ApprovalWaitExpired,
	}); err != nil {
		t.Fatal(err)
	}
	if emitted.ApprovalResolution == nil ||
		emitted.ApprovalResolution.RequestID != "approval-1" ||
		emitted.ApprovalResolution.Decision != "deny" ||
		emitted.ApprovalResolution.Reason != "approval_expired" {
		t.Fatalf("approval resolution event = %+v", emitted)
	}
	if len(kernel.Snapshot().PendingApprovals) != 0 {
		t.Fatalf("pending approvals = %+v", kernel.Snapshot().PendingApprovals)
	}
	if err := kernel.CloseTool(call, tool.Result{IsError: true}, nil); err != nil {
		t.Fatalf("expired tool result was rejected: %v", err)
	}
}
