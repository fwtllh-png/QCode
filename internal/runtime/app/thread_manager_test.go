package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/common/contextsnapshot"
	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestThreadManagerIsolatesHistory(t *testing.T) {
	seed, err := newTestAgentEngine(agentengine.Options{ProviderConfig: agentengine.ProviderConfig{Provider: &threadEchoProvider{}, Route: runtimeTestRoute(t),

		MaxOutputTokens: 128}, ToolConfig: agentengine.ToolConfig{Tools: tool.NewRegistry(nil, nil)}, TelemetryConfig: agentengine.TelemetryConfig{Metrics: telemetry.NewMetrics()},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := NewThreadManager(func() (*EngineAdapter, error) {
		clone, err := seed.CloneEmpty()
		if err != nil {
			return nil, err
		}
		return AdaptEngine(clone), nil
	})
	runtime := NewRuntime(Options{Engine: manager})
	defer runtime.Close(context.Background())

	startA, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: "thread-a", TurnID: "turn-a", ItemID: "item-a", Prompt: "alpha-unique",
	})
	if err != nil {
		t.Fatal(err)
	}
	startB, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: "thread-b", TurnID: "turn-b", ItemID: "item-b", Prompt: "beta-unique",
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	go func() {
		defer wg.Done()
		errs <- runtime.Submit(context.Background(), startA)
	}()
	go func() {
		defer wg.Done()
		errs <- runtime.Submit(context.Background(), startB)
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		histA, err := manager.History("thread-a")
		if err != nil {
			t.Fatal(err)
		}
		histB, err := manager.History("thread-b")
		if err != nil {
			t.Fatal(err)
		}
		if historyContains(histA, "alpha-unique") && historyContains(histB, "beta-unique") &&
			!historyContains(histA, "beta-unique") && !historyContains(histB, "alpha-unique") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	histA, _ := manager.History("thread-a")
	histB, _ := manager.History("thread-b")
	t.Fatalf("histories not isolated:\nA=%+v\nB=%+v", histA, histB)
}

func TestRuntimeCloseReleasesSharedThreadToolRegistryOnce(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	instance := &closeTrackingTool{}
	if err := registry.Register(instance); err != nil {
		t.Fatal(err)
	}
	engine, err := newTestAgentEngine(agentengine.Options{
		ProviderConfig: agentengine.ProviderConfig{
			Provider: &threadEchoProvider{}, Route: runtimeTestRoute(t),
		},
		ToolConfig: agentengine.ToolConfig{Tools: registry},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := NewThreadManager(func() (*EngineAdapter, error) {
		return AdaptEngine(engine), nil
	})
	if _, err := manager.forThread("thread-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.forThread("thread-b"); err != nil {
		t.Fatal(err)
	}
	manager.Release("thread-a")
	manager.Release("thread-b")
	runtime := NewRuntime(Options{Engine: manager})
	if err := runtime.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if instance.closed != 1 {
		t.Fatalf("tool close calls = %d, want 1", instance.closed)
	}
}

func TestThreadManagerBindsToolIdentityAndContextLookup(t *testing.T) {
	seen := make(chan tool.InvocationIdentity, 1)
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&identityCaptureTool{seen: seen}); err != nil {
		t.Fatal(err)
	}
	engine, err := newTestAgentEngine(agentengine.Options{ProviderConfig: agentengine.ProviderConfig{Provider: &identityToolProvider{}, Route: runtimeTestRoute(t),

		MaxOutputTokens: 128}, ToolConfig: agentengine.ToolConfig{Tools: registry}, TelemetryConfig: agentengine.TelemetryConfig{Metrics: telemetry.NewMetrics()}, LifecycleConfig: agentengine.LifecycleConfig{SessionID: "process-session"},
	})
	if err != nil {
		t.Fatal(err)
	}
	created := 0
	manager := NewThreadManager(func() (*EngineAdapter, error) {
		created++
		return AdaptEngine(engine), nil
	})
	runtime := NewRuntime(Options{
		Engine: manager,
		SessionLifecycle: &memorySessionLifecycleStore{
			summary: protocol.SessionSummary{
				Version:   protocol.SessionLifecycleVersion,
				SessionID: "session-real", ThreadID: "thread-parent",
			},
		},
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	operation, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: "thread-parent", TurnID: "turn-parent",
		ItemID: "item-parent", Prompt: "capture identity",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	select {
	case identity := <-seen:
		if identity.SessionID != "session-real" ||
			identity.ThreadID != "thread-parent" ||
			identity.TurnID != "turn-parent" ||
			identity.CallID != "call-identity" {
			t.Fatalf("tool identity = %+v", identity)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("identity tool was not called")
	}
	if resolved, err := manager.ContextEngine("thread-parent"); err != nil ||
		resolved != engine {
		t.Fatalf("context engine = %p, err = %v", resolved, err)
	}
	snapshot, err := manager.Snapshot(t.Context(), contextsnapshot.SourceRef{
		ThreadID: "thread-parent", TurnID: "turn-parent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SourceThread != "thread-parent" || snapshot.SourceTurn != "turn-parent" ||
		snapshot.UserRequest != "capture identity" || snapshot.ParentGoal != "capture identity" ||
		len(snapshot.Messages) == 0 {
		t.Fatalf("parent snapshot = %+v", snapshot)
	}
	for _, test := range []struct {
		ref       contextsnapshot.SourceRef
		wantError string
	}{
		{contextsnapshot.SourceRef{ThreadID: "thread-missing", TurnID: "turn-parent"}, "has no context engine"},
		{contextsnapshot.SourceRef{TurnID: "turn-parent"}, "thread id is required"},
		{contextsnapshot.SourceRef{ThreadID: "thread-parent"}, "parent turn id is required"},
		{contextsnapshot.SourceRef{ThreadID: "thread-parent", TurnID: "turn-previous"}, "parent turn changed"},
	} {
		if _, err := manager.Snapshot(t.Context(), test.ref); err == nil ||
			!strings.Contains(err.Error(), test.wantError) {
			t.Fatalf("snapshot(%+v) error = %v, want %q", test.ref, err, test.wantError)
		}
	}
	if created != 1 {
		t.Fatalf("engine factory calls = %d, want 1", created)
	}
}

func TestThreadManagerRestoresPendingApprovalOnChildThread(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(restoredApprovalTool{}); err != nil {
		t.Fatal(err)
	}
	worker, err := newTestAgentEngine(agentengine.Options{ProviderConfig: agentengine.ProviderConfig{Provider: &restoredApprovalProvider{}, Route: runtimeTestRoute(t)}, ToolConfig: agentengine.ToolConfig{Tools: registry}, SecurityConfig: agentengine.SecurityConfig{Security: policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)}, TelemetryConfig: agentengine.TelemetryConfig{Metrics: telemetry.NewMetrics()}})
	if err != nil {
		t.Fatal(err)
	}
	manager := NewThreadManager(func() (*EngineAdapter, error) {
		return nil, errors.New("host factory must not restore child approval")
	})
	childFactoryCalls := 0
	manager.SetChildFactory(func(ChildSpec) (*EngineAdapter, error) {
		childFactoryCalls++
		return AdaptEngine(worker), nil
	})
	if err := manager.RegisterChild("thread-child", ChildSpec{
		AgentID: "agent-1", AgentPath: "/root/write",
	}); err != nil {
		t.Fatal(err)
	}
	pending := PendingApproval{
		RequestID: "approval-stable", ThreadID: "thread-child",
		TurnID: "turn-child", ItemID: "item-child",
		Data: protocol.ApprovalRequiredData{
			RequestID: "approval-stable", CallID: "call-write",
			Tool: "write_file", Arguments: json.RawMessage(`{"path":"note.txt"}`),
			AllowedScopes: []protocol.ApprovalScope{protocol.ApprovalScopeOnce},
			ExpiresAt:     time.Now().Add(time.Minute),
			Effect:        "external.mutation",
			Risk:          "high",
			ReasonCode:    "approval_required",
		},
	}
	runtime, err := newRuntimeWithRecovery(t.Context(), Options{
		Engine:        manager,
		EventStore:    NewMemoryEventStore(32),
		ContentStore:  NewMemoryContentStore(),
		TerminalStore: turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil),
		Lifecycle: &approvalRecoveryLifecycle{recovery: RecoveryState{
			PendingApprovals: map[string]PendingApproval{
				pending.RequestID: pending,
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	if childFactoryCalls != 1 {
		t.Fatalf("child factory calls = %d, want 1", childFactoryCalls)
	}

	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: "thread-child", TurnID: "turn-child",
		ItemID: "item-child", Prompt: "resume recovered write",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	var observed []protocol.EventKind
	decisionSubmitted := false
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-events:
			observed = append(observed, event.Kind)
			if rejected, ok := event.Data.(*protocol.OperationRejectedData); ok {
				t.Fatalf("approval decision rejected: %+v; events=%v", rejected, observed)
			}
			if event.Kind == protocol.EventToolStart && !decisionSubmitted {
				itemID, itemErr := protocol.NewItemID()
				if itemErr != nil {
					t.Fatal(itemErr)
				}
				decision, decisionErr := protocol.NewOperation(
					&protocol.ApprovalDecisionPayload{
						ThreadID: "thread-parent", TurnID: "turn-parent",
						ItemID: itemID, RequestID: pending.RequestID,
						Decision: protocol.ApprovalApprove,
						Scope:    protocol.ApprovalScopeOnce,
					},
				)
				if decisionErr != nil {
					t.Fatal(decisionErr)
				}
				if submitErr := runtime.Submit(t.Context(), decision); submitErr != nil {
					t.Fatal(submitErr)
				}
				decisionSubmitted = true
			}
			resolved, ok := event.Data.(*protocol.ApprovalResolvedData)
			if !ok {
				continue
			}
			if resolved.RequestID != pending.RequestID ||
				event.ThreadID != pending.ThreadID ||
				event.TurnID != pending.TurnID {
				t.Fatalf(
					"recovered approval = %+v meta=(%s,%s), want request=%q meta=(%s,%s)",
					resolved, event.ThreadID, event.TurnID,
					pending.RequestID, pending.ThreadID, pending.TurnID,
				)
			}
			return
		case <-deadline:
			t.Fatalf(
				"recovered child tool call did not preserve approval request ID; events=%v",
				observed,
			)
		}
	}
}

func historyContains(messages []provider.Message, needle string) bool {
	for _, message := range messages {
		if strings.Contains(message.Text(), needle) {
			return true
		}
		for _, block := range message.Blocks {
			if strings.Contains(block.Text, needle) {
				return true
			}
		}
	}
	return false
}

type identityCaptureTool struct {
	seen chan<- tool.InvocationIdentity
}

type closeTrackingTool struct {
	closed int
}

func (*closeTrackingTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "close_tracking", Description: "track registry closure",
		Visibility: tool.VisibleModel, Capability: tool.CapabilityRead,
		AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelConcurrent,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
		},
	}
}

func (*closeTrackingTool) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Content: "ok"}, nil
}

func (t *closeTrackingTool) Close() error {
	t.closed++
	return nil
}

type restoredApprovalTool struct{}

func (restoredApprovalTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "write_file", Description: "write a recovered file",
		Visibility: tool.VisibleModel, Capability: tool.CapabilityWrite,
		AccessMode: tool.AccessWrite, ParallelPolicy: tool.ParallelSerial,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		},
	}
}

func (restoredApprovalTool) Execute(
	context.Context,
	json.RawMessage,
) (tool.Result, error) {
	return tool.Result{Content: "written"}, nil
}

type restoredApprovalProvider struct{}

func (*restoredApprovalProvider) Stream(
	context.Context,
	provider.ModelRequest,
) (provider.Stream, error) {
	return &providerfixture.SliceStream{Events: []provider.StreamEvent{
		{
			Type: provider.EventToolCallDelta,
			ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call-write", Name: "write_file",
				Arguments: `{"path":"note.txt"}`,
			},
		},
		{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
	}}, nil
}

type approvalRecoveryLifecycle struct {
	recovery RecoveryState
}

func (l *approvalRecoveryLifecycle) Recover(
	context.Context,
) (RecoveryState, error) {
	return l.recovery, nil
}

func (*approvalRecoveryLifecycle) Accept(
	_ context.Context,
	operation protocol.Operation,
	_ string,
	_ json.RawMessage,
) (Acceptance, error) {
	return Acceptance{OperationID: operation.ID}, nil
}

func (*approvalRecoveryLifecycle) Project(
	context.Context,
	protocol.Event,
) error {
	return nil
}

func (*approvalRecoveryLifecycle) Commit(
	context.Context,
	CommitReceipt,
) error {
	return nil
}

func (*identityCaptureTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "capture_identity", Description: "capture the invocation identity",
		Visibility: tool.VisibleModel, Capability: tool.CapabilityRead,
		AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelSerial,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{},
			"additionalProperties": false,
		},
	}
}

func (t *identityCaptureTool) Execute(
	ctx context.Context,
	_ json.RawMessage,
) (tool.Result, error) {
	t.seen <- tool.InvocationIdentityFrom(ctx)
	return tool.Result{Content: "captured"}, nil
}

type identityToolProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *identityToolProvider) Stream(
	context.Context,
	provider.ModelRequest,
) (provider.Stream, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		return &providerfixture.SliceStream{Events: []provider.StreamEvent{
			{
				Type: provider.EventToolCallDelta,
				ToolCall: &provider.ToolCallFragment{
					Index: 0, ID: "call-identity",
					Name: "capture_identity", Arguments: `{}`,
				},
			},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}}, nil
	}
	return &providerfixture.SliceStream{Events: []provider.StreamEvent{
		{Type: provider.EventTextDelta, Text: "done"},
		{Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn},
	}}, nil
}

type threadEchoProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *threadEchoProvider) Stream(
	_ context.Context, request provider.ModelRequest,
) (provider.Stream, error) {
	prompt := ""
	for i := len(request.Messages) - 1; i >= 0; i-- {
		if request.Messages[i].Role == provider.RoleUser {
			prompt = request.Messages[i].Text()
			break
		}
	}
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return &providerfixture.SliceStream{Events: []provider.StreamEvent{
		{Type: provider.EventTextDelta, Text: "echo:" + prompt},
		{Type: provider.EventMessageStop},
	}}, nil
}
