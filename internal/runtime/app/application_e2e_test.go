package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	interacttool "github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestValidToolArgumentsOmitsMalformedProviderPayload(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "valid", value: `{"path":"a.go"}`, want: `{"path":"a.go"}`},
		{name: "empty", value: ""},
		{name: "whitespace", value: "  \n"},
		{name: "truncated", value: `{"path":`},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got := ValidToolArguments(testCase.value)
			if string(got) != testCase.want {
				t.Fatalf(
					"ValidToolArguments(%q) = %q, want %q",
					testCase.value, got, testCase.want,
				)
			}
			data, err := json.Marshal(&protocol.ToolStartData{
				Tool: "read", CallID: "call-1", Arguments: got,
			})
			if err != nil {
				t.Fatalf("marshal safe ToolStartData: %v", err)
			}
			if !json.Valid(data) {
				t.Fatalf("encoded ToolStartData is invalid: %s", data)
			}
		})
	}
}

func TestStartTurnSeparatesModelAndDisplayPrompts(t *testing.T) {
	worker, err := newTestAgentEngine(agentengine.Options{ProviderConfig: agentengine.ProviderConfig{Provider: &singleAnswerProvider{},
		Route: runtimeTestRoute(t),

		MaxOutputTokens: 128}, ToolConfig: agentengine.ToolConfig{Tools: tool.NewRegistry(nil, nil)}, SecurityConfig: agentengine.SecurityConfig{Workspace: t.TempDir()}, TelemetryConfig: agentengine.TelemetryConfig{Metrics: telemetry.NewMetrics()},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntime(Options{Engine: AdaptEngine(worker)})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: "thread-prompt", TurnID: "turn-prompt", ItemID: "item-prompt",
		Prompt:        "internal recovery context\n<recovery_evidence>{}</recovery_evidence>",
		DisplayPrompt: "Continue: fix the parser",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	for {
		event := receiveEvent(t, events)
		if event.Kind != protocol.EventTurnStarted {
			continue
		}
		started, ok := event.Data.(*protocol.TurnStartedData)
		if !ok {
			t.Fatalf("turn.started data = %T", event.Data)
		}
		if !strings.Contains(started.Prompt, "<recovery_evidence>") {
			t.Fatalf("model prompt lost recovery evidence: %q", started.Prompt)
		}
		if started.DisplayPrompt != "Continue: fix the parser" ||
			strings.Contains(started.DisplayPrompt, "<recovery_evidence>") {
			t.Fatalf("display prompt leaked recovery evidence: %q", started.DisplayPrompt)
		}
		return
	}
}

func TestWorkspaceChangeReceiptMatchesTerminalOutcome(t *testing.T) {
	t.Run("failed_without_changes", func(t *testing.T) {
		worker, err := newTestAgentEngine(agentengine.Options{ProviderConfig: agentengine.ProviderConfig{Provider: &singleAnswerProvider{}, Route: runtimeTestRoute(t),

			MaxOutputTokens: 128}, ToolConfig: agentengine.ToolConfig{Tools: tool.NewRegistry(nil, nil)}, SecurityConfig: agentengine.SecurityConfig{Security: policy.DefaultRuntime(
			policy.ModeAct,
			policy.PermissionSuggest,
		),
			Workspace: t.TempDir()}, TelemetryConfig: agentengine.TelemetryConfig{Metrics: telemetry.NewMetrics()},
		})
		if err != nil {
			t.Fatal(err)
		}
		receipt, terminal := runWorkspaceChangeTurn(t, worker)
		if terminal.Kind != protocol.EventTurnFailed {
			t.Fatalf("terminal = %s, want turn.failed", terminal.Kind)
		}
		if receipt.Outcome != "" || len(receipt.Changes) != 0 ||
			receipt.WorkspaceOutcome == nil ||
			receipt.WorkspaceOutcome.Status != "unchanged" ||
			receipt.Convergence == nil ||
			receipt.Convergence.Cause != string(turnkernel.ConvergenceRepairBudget) {
			t.Fatalf("failed receipt = %+v", receipt)
		}
		failed, _ := terminal.Data.(*protocol.TurnFailedData)
		if failed == nil || failed.Convergence == nil ||
			failed.Convergence.Cause != receipt.Convergence.Cause {
			t.Fatalf("failed terminal = %+v", terminal.Data)
		}
	})

	t.Run("completed_with_changes", func(t *testing.T) {
		root := t.TempDir()
		registry := tool.NewRegistry(nil, nil)
		if err := registry.Register(&runtimeWriteTool{root: root}); err != nil {
			t.Fatal(err)
		}
		worker, err := newTestAgentEngine(agentengine.Options{ProviderConfig: agentengine.ProviderConfig{Provider: &runtimeApprovalProvider{}, Route: runtimeTestRoute(t),

			MaxOutputTokens: 128}, ToolConfig: agentengine.ToolConfig{Tools: registry}, SecurityConfig: agentengine.SecurityConfig{Security: policy.DefaultRuntime(
			policy.ModeAct,
			policy.PermissionBypass,
		),
			Workspace: root, Journal: newTestWorkspaceJournal(t, root)}, TelemetryConfig: agentengine.TelemetryConfig{Metrics: telemetry.NewMetrics()},
		})
		if err != nil {
			t.Fatal(err)
		}
		receipt, terminal := runWorkspaceChangeTurn(t, worker)
		if terminal.Kind != protocol.EventTurnCompleted {
			t.Fatalf(
				"terminal = %s data=%+v, want turn.completed",
				terminal.Kind,
				terminal.Data,
			)
		}
		if receipt.Outcome != protocol.TurnOutcomeChanged ||
			len(receipt.Changes) != 1 ||
			receipt.WorkspaceOutcome == nil ||
			receipt.WorkspaceOutcome.Status != "changed" {
			t.Fatalf("completed receipt = %+v", receipt)
		}
	})
}

func runWorkspaceChangeTurn(
	t *testing.T,
	worker *agentengine.Engine,
) (*protocol.ExecutionReceiptData, protocol.Event) {
	t.Helper()
	runtime := NewRuntime(Options{Engine: AdaptEngine(worker)})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	start, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: "thread", TurnID: "turn", ItemID: "prompt",
		Prompt: "change the workspace", Intent: protocol.TurnIntentWorkspaceChange,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	var receipt *protocol.ExecutionReceiptData
	var observed []protocol.Event
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-events:
			observed = append(observed, event)
			if data, ok := event.Data.(*protocol.OperationRejectedData); ok {
				t.Fatalf("turn operation rejected: %+v", data)
			}
			if data, ok := event.Data.(*protocol.ExecutionReceiptData); ok {
				receipt = data
			}
			if protocol.IsTerminalEvent(event.Kind) {
				if receipt == nil {
					t.Fatal("terminal event arrived without an execution receipt")
				}
				return receipt, event
			}
		case <-deadline:
			t.Fatalf("turn did not reach a terminal event: %+v", observed)
		}
	}
}

type singleAnswerProvider struct{}

func (*singleAnswerProvider) Stream(
	context.Context,
	provider.ModelRequest,
) (provider.Stream, error) {
	return &providerfixture.SliceStream{Events: []provider.StreamEvent{
		{Type: provider.EventTextDelta, Text: "no changes needed"},
		{Type: provider.EventMessageStop},
	}}, nil
}

func TestTerminalEnvelopeFailurePublishesNoReceiptOrTerminal(t *testing.T) {
	worker, err := newTestAgentEngine(agentengine.Options{ProviderConfig: agentengine.ProviderConfig{Provider: &singleAnswerProvider{},
		Route: runtimeTestRoute(t),

		MaxOutputTokens: 128}, ToolConfig: agentengine.ToolConfig{Tools: tool.NewRegistry(nil, nil)}, SecurityConfig: agentengine.SecurityConfig{Security: policy.DefaultRuntime(
		policy.ModeAct,
		policy.PermissionBypass,
	),
		Workspace: t.TempDir()}, TelemetryConfig: agentengine.TelemetryConfig{Metrics: telemetry.NewMetrics()},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := turnkernel.NewMemoryTerminalEnvelopeStore(
		nil,
		func(stage turnkernel.TerminalEnvelopeStage) error {
			if stage == turnkernel.StageCommitMarker {
				return errors.New("injected terminal commit failure")
			}
			return nil
		},
	)
	runtime := NewRuntime(Options{
		Engine: AdaptEngine(worker), TerminalStore: store,
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: "thread-envelope-failure",
		TurnID:   "turn-envelope-failure",
		ItemID:   "item-envelope-failure",
		Prompt:   "answer",
		Intent:   protocol.TurnIntentAnswer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Kind == protocol.EventExecutionReceipt ||
				protocol.IsTerminalEvent(event.Kind) {
				t.Fatalf("terminal envelope leaked event: %+v", event)
			}
			if event.Kind == protocol.EventOperationRejected {
				if _, _, loadErr := store.LoadTerminal(
					t.Context(),
					"turn-envelope-failure",
				); !errors.Is(loadErr, turnkernel.ErrTerminalEnvelopeMissing) {
					t.Fatalf("terminal store error = %v", loadErr)
				}
				usage, cost := worker.Usage()
				if len(worker.History()) != 0 ||
					usage.Total() != 0 ||
					cost != 0 ||
					worker.SessionRevision() != 0 {
					t.Fatalf(
						"failed terminal applied session state: history=%d usage=%+v cost=%f revision=%d",
						len(worker.History()),
						usage,
						cost,
						worker.SessionRevision(),
					)
				}
				return
			}
		case <-deadline:
			t.Fatal("terminal commit failure did not reject operation")
		}
	}
}

func TestRuntimeApprovalPauseResumeE2E(t *testing.T) {
	for _, decision := range []protocol.ApprovalDecision{
		protocol.ApprovalApprove, protocol.ApprovalDeny, protocol.ApprovalCancel,
	} {
		t.Run(string(decision), func(t *testing.T) {
			registry := tool.NewRegistry(nil, nil)
			executor := &runtimeWriteTool{}
			if err := registry.Register(executor); err != nil {
				t.Fatal(err)
			}
			security := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
			root := t.TempDir()
			worker, err := newTestAgentEngine(agentengine.Options{ProviderConfig: agentengine.ProviderConfig{Provider: &runtimeApprovalProvider{}, Route: runtimeTestRoute(t),

				MaxOutputTokens: 128}, ToolConfig: agentengine.ToolConfig{Tools: registry}, SecurityConfig: agentengine.SecurityConfig{Security: security, Workspace: root,
				Journal: newTestWorkspaceJournal(t, root)}, TelemetryConfig: agentengine.TelemetryConfig{Metrics: telemetry.NewMetrics()},
			})
			if err != nil {
				t.Fatal(err)
			}
			runtime := NewRuntime(Options{Engine: AdaptEngine(worker)})
			defer runtime.Close(context.Background())
			events, err := runtime.Events(t.Context(), 0)
			if err != nil {
				t.Fatal(err)
			}
			start, err := protocol.NewOperation(&protocol.StartTurnPayload{
				ThreadID: "thread", TurnID: "turn", ItemID: "prompt", Prompt: "write",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.Submit(t.Context(), start); err != nil {
				t.Fatal(err)
			}

			var required *protocol.ApprovalRequiredData
			deadline := time.After(3 * time.Second)
			for required == nil {
				select {
				case event := <-events:
					if data, ok := event.Data.(*protocol.ApprovalRequiredData); ok {
						required = data
					}
				case <-deadline:
					t.Fatal("approval.required was not emitted")
				}
			}
			if executor.calls.Load() != 0 {
				t.Fatal("tool executed before approval decision")
			}
			approval, err := protocol.NewOperation(&protocol.ApprovalDecisionPayload{
				ThreadID: "thread", TurnID: "turn", ItemID: "approval",
				RequestID: required.RequestID, Decision: decision,
				Scope: protocol.ApprovalScopeOnce, ExpiresAt: required.ExpiresAt,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.Submit(t.Context(), approval); err != nil {
				t.Fatal(err)
			}

			wantTerminal := protocol.EventTurnCompleted
			wantCalls := int32(1)
			switch decision {
			case protocol.ApprovalDeny:
				// Decline feeds a tool error; model continues and completes.
				wantTerminal, wantCalls = protocol.EventTurnCompleted, 0
			case protocol.ApprovalCancel:
				wantTerminal, wantCalls = protocol.EventTurnCanceled, 0
			}
			for {
				select {
				case event := <-events:
					if protocol.IsTerminalEvent(event.Kind) {
						if event.Kind != wantTerminal {
							t.Fatalf(
								"terminal = %s data=%+v, want %s",
								event.Kind,
								event.Data,
								wantTerminal,
							)
						}
						if executor.calls.Load() != wantCalls {
							t.Fatalf("tool calls = %d, want %d", executor.calls.Load(), wantCalls)
						}
						return
					}
				case <-deadline:
					t.Fatal("turn did not resume to terminal event")
				}
			}
		})
	}
}

func TestRuntimeInputPauseResumeE2E(t *testing.T) {
	runtime, events, terminalStore := newRuntimeInputFixture(t)
	startRuntimeInputTurn(t, runtime)
	required := waitRuntimeInputRequired(t, events)
	facts, err := terminalStore.LoadDomainFacts(t.Context(), "turn-input")
	if err != nil {
		t.Fatal(err)
	}
	var running bool
	for _, effect := range facts[len(facts)-1].State.PendingEffects {
		running = running ||
			(effect.Kind == turnkernel.EffectAwaitInput &&
				effect.Status == turnkernel.EffectRunning &&
				effect.Attempt == 1)
	}
	if !running {
		t.Fatalf("input wait was not durably running: %+v", facts[len(facts)-1])
	}
	reply, err := protocol.NewOperation(&protocol.InputReplyPayload{
		ThreadID:  "thread-input",
		TurnID:    "turn-input",
		ItemID:    "item-input-reply",
		RequestID: required.RequestID,
		Answer:    "yes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), reply); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	var resolved, completed int
	for completed == 0 {
		select {
		case event := <-events:
			if event.Kind == protocol.EventInputResolved {
				resolved++
			}
			if event.Kind == protocol.EventTurnCompleted {
				completed++
			}
		case <-deadline:
			t.Fatal("input turn did not resume to completion")
		}
	}
	if resolved != 1 || completed != 1 {
		t.Fatalf("input projections resolved=%d completed=%d", resolved, completed)
	}
	facts, err = terminalStore.LoadDomainFacts(t.Context(), "turn-input")
	if err != nil {
		t.Fatal(err)
	}
	state := facts[len(facts)-1].State
	var closed int
	for _, effect := range state.CompletedEffects {
		if effect.Kind == turnkernel.EffectAwaitInput &&
			effect.Status == turnkernel.EffectSucceeded {
			closed++
		}
	}
	if closed != 1 || state.Phase != turnkernel.PhaseCompleted {
		t.Fatalf("input terminal state = %+v", state)
	}
}

func TestRuntimeCancelPendingInputHasOneCanceledTerminal(t *testing.T) {
	runtime, events, terminalStore := newRuntimeInputFixture(t)
	startRuntimeInputTurn(t, runtime)
	_ = waitRuntimeInputRequired(t, events)
	cancel, err := protocol.NewOperation(&protocol.CancelTurnPayload{
		ThreadID: "thread-input", TurnID: "turn-input",
		ItemID: "item-input-cancel", Reason: protocol.CancelReasonUserInterrupted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), cancel); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	terminals := 0
	var observed []protocol.EventKind
	for terminals == 0 {
		select {
		case event := <-events:
			observed = append(observed, event.Kind)
			if !protocol.IsTerminalEvent(event.Kind) {
				continue
			}
			if event.Kind != protocol.EventTurnCanceled {
				t.Fatalf("input cancel terminal = %s", event.Kind)
			}
			terminals++
		case <-deadline:
			t.Fatalf(
				"input cancel terminal was not emitted; events=%v",
				observed,
			)
		}
	}
	facts, err := terminalStore.LoadDomainFacts(t.Context(), "turn-input")
	if err != nil {
		t.Fatal(err)
	}
	state := facts[len(facts)-1].State
	if state.Phase != turnkernel.PhaseCanceled || state.PendingInput != nil {
		t.Fatalf("input cancel state = %+v", state)
	}
}

func newRuntimeInputFixture(
	t *testing.T,
) (*Runtime, <-chan protocol.Event, *turnkernel.MemoryTerminalEnvelopeStore) {
	t.Helper()
	host := interacttool.NewHost(time.Minute)
	registry := tool.NewRegistry(nil, nil)
	if err := interacttool.Register(registry, interacttool.Options{
		Host: host, Workspace: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	terminalStore := turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil)
	coordinators, err := turnkernel.NewStoreCoordinatorRuntime(terminalStore)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := newTestAgentEngine(agentengine.Options{ProviderConfig: agentengine.ProviderConfig{Provider: &runtimeInputProvider{}, Route: runtimeTestRoute(t),

		MaxOutputTokens: 128}, ToolConfig: agentengine.ToolConfig{Tools: registry}, SecurityConfig: agentengine.SecurityConfig{Security: policy.DefaultRuntime(
		policy.ModeAct,
		policy.PermissionBypass,
	),
		Workspace: t.TempDir()}, TelemetryConfig: agentengine.TelemetryConfig{Metrics: telemetry.NewMetrics()}, LifecycleConfig: agentengine.LifecycleConfig{InputHost: host,

		TurnCoordinatorRuntime: coordinators},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntime(Options{
		Engine:        AdaptEngine(worker),
		TerminalStore: terminalStore,
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, events, terminalStore
}

func startRuntimeInputTurn(t *testing.T, runtime *Runtime) {
	t.Helper()
	start, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: "thread-input",
		TurnID:   "turn-input",
		ItemID:   "item-input",
		Prompt:   "ask",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), start); err != nil {
		t.Fatal(err)
	}
}

func waitRuntimeInputRequired(
	t *testing.T,
	events <-chan protocol.Event,
) *protocol.InputRequiredData {
	t.Helper()
	var required *protocol.InputRequiredData
	deadline := time.After(3 * time.Second)
	for required == nil {
		select {
		case event := <-events:
			if data, ok := event.Data.(*protocol.InputRequiredData); ok {
				required = data
			}
		case <-deadline:
			t.Fatal("input.required was not emitted")
		}
	}
	return required
}

func TestRuntimeCancelDuringProviderAndToolHasOneCanceledTerminal(
	t *testing.T,
) {
	tests := []struct {
		name       string
		build      func(*testing.T, chan struct{}) (*agentengine.Engine, *turnkernel.MemoryTerminalEnvelopeStore)
		effectKind turnkernel.EffectKind
	}{
		{
			name: "provider",
			build: func(
				t *testing.T,
				started chan struct{},
			) (*agentengine.Engine, *turnkernel.MemoryTerminalEnvelopeStore) {
				t.Helper()
				store := turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil)
				coordinators, err := turnkernel.NewStoreCoordinatorRuntime(store)
				if err != nil {
					t.Fatal(err)
				}
				worker, err := newTestAgentEngine(agentengine.Options{ProviderConfig: agentengine.ProviderConfig{Provider: &runtimeBlockingProvider{started: started},
					Route: runtimeTestRoute(t),

					MaxOutputTokens: 128}, ToolConfig: agentengine.ToolConfig{Tools: tool.NewRegistry(nil, nil)}, SecurityConfig: agentengine.SecurityConfig{Security: policy.DefaultRuntime(
					policy.ModeAct,
					policy.PermissionBypass,
				),
					Workspace: t.TempDir()}, TelemetryConfig: agentengine.TelemetryConfig{Metrics: telemetry.NewMetrics()}, LifecycleConfig: agentengine.LifecycleConfig{TurnCoordinatorRuntime: coordinators},
				})
				if err != nil {
					t.Fatal(err)
				}
				return worker, store
			},
			effectKind: turnkernel.EffectSampleProvider,
		},
		{
			name: "tool",
			build: func(
				t *testing.T,
				started chan struct{},
			) (*agentengine.Engine, *turnkernel.MemoryTerminalEnvelopeStore) {
				t.Helper()
				store := turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil)
				coordinators, err := turnkernel.NewStoreCoordinatorRuntime(store)
				if err != nil {
					t.Fatal(err)
				}
				registry := tool.NewRegistry(nil, nil)
				if err := registry.Register(
					&runtimeBlockingTool{started: started}); err != nil {
					t.Fatal(err)
				}
				worker, err := newTestAgentEngine(agentengine.Options{ProviderConfig: agentengine.ProviderConfig{Provider: &runtimeToolCancelProvider{},
					Route: runtimeTestRoute(t),

					MaxOutputTokens: 128}, ToolConfig: agentengine.ToolConfig{Tools: registry}, SecurityConfig: agentengine.SecurityConfig{Security: policy.DefaultRuntime(
					policy.ModeAct,
					policy.PermissionBypass,
				),
					Workspace: t.TempDir()}, TelemetryConfig: agentengine.TelemetryConfig{Metrics: telemetry.NewMetrics()}, LifecycleConfig: agentengine.LifecycleConfig{TurnCoordinatorRuntime: coordinators},
				})
				if err != nil {
					t.Fatal(err)
				}
				return worker, store
			},
			effectKind: turnkernel.EffectExecuteTool,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			started := make(chan struct{})
			worker, terminalStore := testCase.build(t, started)
			eventStore := NewMemoryEventStore(64)
			runtime := NewRuntime(Options{
				Engine:        AdaptEngine(worker),
				EventStore:    eventStore,
				TerminalStore: terminalStore,
			})
			t.Cleanup(func() { _ = runtime.Close(context.Background()) })
			events, err := runtime.Events(t.Context(), 0)
			if err != nil {
				t.Fatal(err)
			}
			turnID := protocol.TurnID("turn-cancel-" + testCase.name)
			start, err := protocol.NewOperation(&protocol.StartTurnPayload{
				ThreadID: protocol.ThreadID("thread-cancel-" + testCase.name),
				TurnID:   turnID,
				ItemID:   protocol.ItemID("item-cancel-" + testCase.name),
				Prompt:   "wait",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.Submit(t.Context(), start); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("cancel target did not start")
			}
			cancel, err := protocol.NewOperation(&protocol.CancelTurnPayload{
				ThreadID: start.Payload.(*protocol.StartTurnPayload).ThreadID,
				TurnID:   turnID,
				ItemID:   protocol.ItemID("item-cancel-op-" + testCase.name),
				Reason:   protocol.CancelReasonUserInterrupted,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.Submit(t.Context(), cancel); err != nil {
				t.Fatal(err)
			}
			deadline := time.After(3 * time.Second)
			for {
				select {
				case event := <-events:
					if protocol.IsTerminalEvent(event.Kind) {
						if event.Kind != protocol.EventTurnCanceled {
							t.Fatalf("terminal = %+v", event)
						}
						goto terminal
					}
				case <-deadline:
					t.Fatal("cancel did not produce terminal")
				}
			}
		terminal:
			replayed, err := eventStore.Replay(t.Context(), 0)
			if err != nil {
				t.Fatal(err)
			}
			var terminals int
			for _, event := range replayed {
				if protocol.IsTerminalEvent(event.Kind) {
					terminals++
				}
			}
			if terminals != 1 {
				t.Fatalf("terminal events = %d: %+v", terminals, replayed)
			}
			facts, err := terminalStore.LoadDomainFacts(
				t.Context(),
				string(turnID),
			)
			if err != nil {
				t.Fatal(err)
			}
			state := facts[len(facts)-1].State
			var closed int
			for _, effect := range state.CompletedEffects {
				if effect.Kind == testCase.effectKind {
					closed++
				}
			}
			if state.Phase != turnkernel.PhaseCanceled ||
				state.Terminal == nil ||
				state.Terminal.Kind != turnkernel.TerminalCanceled ||
				len(state.PendingEffects) != 0 ||
				len(state.OpenCalls) != 0 ||
				len(state.FinalOutput) != 0 ||
				closed != 1 {
				t.Fatalf("canceled kernel state = %+v", state)
			}
		})
	}
}

type runtimeCommentaryCancelProvider struct{}

func (*runtimeCommentaryCancelProvider) Stream(
	context.Context,
	provider.ModelRequest,
) (provider.Stream, error) {
	return &providerfixture.SliceStream{Events: []provider.StreamEvent{
		{Type: provider.EventTextDelta, Text: "checking the workspace first"},
		{
			Type: provider.EventToolCallDelta,
			ToolCall: &provider.ToolCallFragment{
				Index: 0,
				ID:    "call_block",
				Name:  "blocking_tool",
			},
		},
		{Type: provider.EventMessageStop},
	}}, nil
}

// A turn interrupted after live commentary is the cancel path where the
// terminal outbox re-projects events that were already published. The
// projection must dedupe against those live events and still land the
// canceled terminal, otherwise the turn row stays active forever and every
// later control operation is rejected with "turn is not active".
func TestRuntimeCancelAfterLiveCommentaryDrainsTerminalOutbox(t *testing.T) {
	started := make(chan struct{})
	terminalStore := turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil)
	coordinators, err := turnkernel.NewStoreCoordinatorRuntime(terminalStore)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&runtimeBlockingTool{started: started}); err != nil {
		t.Fatal(err)
	}
	worker, err := newTestAgentEngine(agentengine.Options{
		ProviderConfig: agentengine.ProviderConfig{
			Provider:        &runtimeCommentaryCancelProvider{},
			Route:           runtimeTestRoute(t),
			MaxOutputTokens: 128,
		},
		ToolConfig: agentengine.ToolConfig{Tools: registry},
		SecurityConfig: agentengine.SecurityConfig{
			Security:  policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
			Workspace: t.TempDir(),
		},
		TelemetryConfig: agentengine.TelemetryConfig{Metrics: telemetry.NewMetrics()},
		LifecycleConfig: agentengine.LifecycleConfig{TurnCoordinatorRuntime: coordinators},
	})
	if err != nil {
		t.Fatal(err)
	}
	eventStore := NewMemoryEventStore(64)
	runtime := NewRuntime(Options{
		Engine:        AdaptEngine(worker),
		EventStore:    eventStore,
		TerminalStore: terminalStore,
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	turnID := protocol.TurnID("turn-cancel-commentary")
	start, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: protocol.ThreadID("thread-cancel-commentary"),
		TurnID:   turnID,
		ItemID:   protocol.ItemID("item-cancel-commentary"),
		Prompt:   "wait",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel target did not start")
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-events:
			if protocol.IsTerminalEvent(event.Kind) {
				t.Fatalf("terminal before cancel: %+v", event)
			}
			if event.Kind == protocol.EventCommentaryCompleted {
				goto commentary
			}
		case <-deadline:
			t.Fatal("live commentary was not published before cancel")
		}
	}
commentary:
	cancel, err := protocol.NewOperation(&protocol.CancelTurnPayload{
		ThreadID: start.Payload.(*protocol.StartTurnPayload).ThreadID,
		TurnID:   turnID,
		ItemID:   protocol.ItemID("item-cancel-commentary-op"),
		Reason:   protocol.CancelReasonUserInterrupted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), cancel); err != nil {
		t.Fatal(err)
	}
	deadline = time.After(3 * time.Second)
	for {
		select {
		case event := <-events:
			if !protocol.IsTerminalEvent(event.Kind) {
				continue
			}
			if event.Kind != protocol.EventTurnCanceled {
				t.Fatalf("terminal = %+v", event)
			}
			if event.OperationID != start.ID {
				t.Fatalf(
					"terminal operation = %s, want the start operation %s",
					event.OperationID, start.ID,
				)
			}
			goto terminal
		case <-deadline:
			t.Fatal("cancel did not produce terminal")
		}
	}
terminal:
	replayed, err := eventStore.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	terminals, commentaries := 0, 0
	for _, event := range replayed {
		switch event.Kind {
		case protocol.EventTurnCanceled:
			terminals++
		case protocol.EventCommentaryCompleted:
			commentaries++
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal events = %d: %+v", terminals, replayed)
	}
	if commentaries != 1 {
		t.Fatalf("commentary events = %d: %+v", commentaries, replayed)
	}
	pending, err := terminalStore.PendingOutbox(t.Context(), string(turnID))
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending terminal outbox = %+v", pending)
	}
}

type runtimeWriteTool struct {
	calls atomic.Int32
	root  string
}

func (*runtimeWriteTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "write", Description: "write fixture", Visibility: tool.VisibleModel,
		Capability: tool.CapabilityWrite, AccessMode: tool.AccessWrite,
		ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
			Kind: "file", Field: "path", Access: tool.AccessWrite,
		}}},
		ParallelPolicy: tool.ParallelConcurrent, SandboxRequirement: tool.SandboxNone,
		Availability: tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "minLength": float64(1)},
			},
			"required": []string{"path"}, "additionalProperties": false,
		},
	}
}

func (t *runtimeWriteTool) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	t.calls.Add(1)
	if t.root != "" {
		if err := os.WriteFile(filepath.Join(t.root, "out.txt"), []byte("written\n"), 0o600); err != nil {
			return tool.Result{}, err
		}
	}
	return tool.Result{
		Content: "written",
		Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{
			WorkspaceChanges: []tool.WorkspaceChange{{
				Path: "out.txt", Kind: tool.WorkspaceCreated, Added: 1,
			}},
		}},
	}, nil
}

type runtimeApprovalProvider struct {
	mu    sync.Mutex
	calls int
}

type runtimeInputProvider struct {
	mu    sync.Mutex
	calls int
}

type runtimeBlockingProvider struct {
	started chan struct{}
	once    sync.Once
}

func (p *runtimeBlockingProvider) Stream(
	ctx context.Context,
	_ provider.ModelRequest,
) (provider.Stream, error) {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

type runtimeToolCancelProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *runtimeToolCancelProvider) Stream(
	ctx context.Context,
	_ provider.ModelRequest,
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
					Index: 0,
					ID:    "call_block",
					Name:  "blocking_tool",
				},
			},
			{Type: provider.EventMessageStop},
		}}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

type runtimeBlockingTool struct {
	started chan struct{}
	once    sync.Once
}

func (*runtimeBlockingTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name:        "blocking_tool",
		Description: "block until canceled",
		Visibility:  tool.VisibleModel,
		Capability:  tool.CapabilityRead,
		AccessMode:  tool.AccessRead,
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
		ParallelPolicy:     tool.ParallelSerial,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
	}
}

func (t *runtimeBlockingTool) Execute(
	ctx context.Context,
	_ json.RawMessage,
) (tool.Result, error) {
	t.once.Do(func() { close(t.started) })
	<-ctx.Done()
	return tool.Result{}, ctx.Err()
}

func (p *runtimeInputProvider) Stream(
	_ context.Context,
	_ provider.ModelRequest,
) (provider.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	switch p.calls {
	case 1:
		return &providerfixture.SliceStream{Events: []provider.StreamEvent{
			{
				Type: provider.EventToolCallDelta,
				ToolCall: &provider.ToolCallFragment{
					Index:     0,
					ID:        "call_input",
					Name:      "request_user_input",
					Arguments: `{"prompt":"continue?","options":["yes","no"]}`,
				},
			},
			{Type: provider.EventMessageStop},
		}}, nil
	case 2:
		return &providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "done"},
			{Type: provider.EventMessageStop},
		}}, nil
	default:
		return nil, errors.New("unexpected provider call")
	}
}

func (p *runtimeApprovalProvider) Stream(
	_ context.Context, _ provider.ModelRequest,
) (provider.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	switch p.calls {
	case 1:
		return &providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call_write", Name: "write", Arguments: `{"path":"out.txt"}`,
			}},
			{Type: provider.EventMessageStop},
		}}, nil
	case 2:
		return &providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "done"},
			{Type: provider.EventMessageStop},
		}}, nil
	case 3:
		return &providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "done"},
			{Type: provider.EventMessageStop},
		}}, nil
	default:
		return nil, errors.New("unexpected provider call")
	}
}

func runtimeTestRoute(t *testing.T) model.ReadyRoute {
	t.Helper()
	catalog, err := model.NewCatalog(model.Provider{
		ID: "test", Adapter: model.AdapterOpenAICompatible, Endpoint: "http://127.0.0.1:1",
		Protocol: model.ProtocolOpenAIChat, Provenance: model.ProvenanceFixture,
		Models: map[string]model.Model{"model": {
			ID: "model", CanonicalID: "model", WireID: "model",
			Limits:       model.Limits{ContextTokens: 4096, MaxOutputTokens: 1024},
			Capabilities: model.Capabilities{Streaming: true, ToolCalls: true},
			Pricing: model.Pricing{
				InputPerMillion: 1, OutputPerMillion: 1,
				Currency: "USD", Known: true, Provenance: model.ProvenanceFixture,
			},
			Provenance: model.ProvenanceFixture,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(model.RouteRequest{ProviderID: "test", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	return route
}
