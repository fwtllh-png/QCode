package turnkernel

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestToolAdmissionRejectionClosesOnlyPendingCalls(t *testing.T) {
	store := NewMemoryTerminalEnvelopeStore(nil, nil)
	runtime, err := NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := NewRuntimeKernel(KernelIdentity{TurnID: "admission", ProfileRevision: 1}, protocol.TurnIntentAnswer, "act", nil, false, nil, nil, nil, nil, nil, DefaultPolicy(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	executed := make(map[string]tool.Result)
	cache := &tool.ResultCache{}
	completed := provider.ToolCall{ID: "completed", Name: "read", Arguments: `{}`}
	count := 0
	effect := ToolEffect{Context: t.Context(), Calls: []provider.ToolCall{completed}, Executed: executed, Cache: cache, Registry: registry,
		Execute: func(context.Context, provider.ToolCall) (tool.Result, error) {
			count++
			return tool.Result{Content: "already done"}, nil
		},
	}
	if _, err := kernel.ExecuteToolEffect(effect); err != nil {
		t.Fatal(err)
	}
	before := executed[completed.ID]
	effect.Calls = append(effect.Calls, provider.ToolCall{ID: "pending-a", Name: "write", Arguments: `{}`}, provider.ToolCall{ID: "pending-b", Name: "write", Arguments: `{}`})
	effect.Admit = func(calls []provider.ToolCall) (*tool.Result, error) {
		if len(calls) != 2 {
			t.Fatalf("admission received completed call: %v", calls)
		}
		return &tool.Result{Content: "split the batch", IsError: true, Outcome: &tool.Outcome{Status: tool.OutcomeRejected}, Metadata: map[string]any{"retry_original": false}}, nil
	}
	var starts, closes int
	effect.PublishStart = func(provider.ToolCall) error { starts++; return nil }
	effect.PublishResult = func(provider.ToolCall, tool.Result) error { closes++; return nil }
	results, err := kernel.ExecuteToolEffect(effect)
	if err != nil || count != 1 || starts != 2 || closes != 2 || len(results) != 3 || kernel.Phase() != PhaseSampling {
		t.Fatalf("calls=%d starts=%d closes=%d phase=%s err=%v", count, starts, closes, kernel.Phase(), err)
	}
	if !reflect.DeepEqual(results[0], before) || !results[1].IsError || !results[2].IsError {
		t.Fatal("lost accepted result or rejection")
	}
	results[1].Metadata["changed"] = true
	if results[2].Metadata["changed"] != nil {
		t.Fatal("rejected results share mutable metadata")
	}
	// Reopen the durable kernel: all rejected proposals must already be closed.
	if err := runtime.Release(t.Context(), "admission"); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewRuntimeKernel(KernelIdentity{TurnID: "admission", ProfileRevision: 1}, protocol.TurnIntentAnswer, "act", nil, false, nil, nil, nil, nil, nil, DefaultPolicy(), runtime)
	if err != nil || restarted.Phase() != PhaseSampling || len(restarted.Snapshot().PendingEffects) != 0 {
		t.Fatalf("restart has open calls: %v", err)
	}
}

func TestToolAdmissionStorageErrorDoesNotExecuteOrPublish(t *testing.T) {
	kernel, err := NewRuntimeKernel(KernelIdentity{TurnID: "admission-error", ProfileRevision: 1}, protocol.TurnIntentAnswer, "act", nil, false, nil, nil, nil, nil, nil, DefaultPolicy(), NewEphemeralCoordinatorRuntime())
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("admission store unavailable")
	_, err = kernel.ExecuteToolEffect(ToolEffect{Context: t.Context(), Calls: []provider.ToolCall{{ID: "call", Name: "write"}}, Executed: map[string]tool.Result{}, Cache: &tool.ResultCache{}, Registry: tool.NewRegistry(nil, nil),
		Admit: func([]provider.ToolCall) (*tool.Result, error) { return nil, want },
		Execute: func(context.Context, provider.ToolCall) (tool.Result, error) {
			t.Fatal("executed after admission error")
			return tool.Result{}, nil
		},
		PublishStart: func(provider.ToolCall) error { t.Fatal("published after admission error"); return nil },
	})
	if !errors.Is(err, want) {
		t.Fatalf("error=%v", err)
	}
}

func TestToolAdmissionRestartDuringRejectionNeverExecutesRemainder(t *testing.T) {
	store := NewMemoryTerminalEnvelopeStore(nil, nil)
	runtime, err := NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	open := func() *RuntimeKernel {
		kernel, err := NewRuntimeKernel(KernelIdentity{TurnID: "reject-restart", ProfileRevision: 1}, protocol.TurnIntentAnswer, "act", nil, false, nil, nil, nil, nil, nil, DefaultPolicy(), runtime)
		if err != nil {
			t.Fatal(err)
		}
		return kernel
	}
	kernel := open()
	registry := tool.NewRegistry(nil, nil)
	executed := map[string]tool.Result{}
	effect := ToolEffect{Context: t.Context(), Calls: []provider.ToolCall{{ID: "a", Name: "write"}, {ID: "b", Name: "write"}}, Registry: registry, Executed: executed, Cache: &tool.ResultCache{},
		Admit: func([]provider.ToolCall) (*tool.Result, error) {
			return &tool.Result{IsError: true, Content: "split batch", Metadata: map[string]any{"retry_original": false}}, nil
		},
		Execute: func(context.Context, provider.ToolCall) (tool.Result, error) {
			t.Fatal("executed rejected call")
			return tool.Result{}, nil
		},
		PublishResult: func(provider.ToolCall, tool.Result) error { panic("simulated process exit after first result") },
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("crash was not injected")
			}
		}()
		_, _ = kernel.ExecuteToolEffect(effect)
	}()
	if len(kernel.Snapshot().OpenCalls) != 1 {
		t.Fatal("fixture did not interrupt partial settlement")
	}
	if err := runtime.Release(t.Context(), "reject-restart"); err != nil {
		t.Fatal(err)
	}
	kernel = open()
	effect.Calls = kernel.PendingToolCalls()
	effect.PublishResult = nil
	effect.Admit = func([]provider.ToolCall) (*tool.Result, error) {
		t.Fatal("re-admitted previously rejected remainder")
		return nil, nil
	}
	results, err := kernel.ExecuteToolEffect(effect)
	if err != nil || len(results) != 1 || !results[0].IsError || kernel.Phase() != PhaseSampling {
		t.Fatalf("restart results=%+v phase=%s error=%v", results, kernel.Phase(), err)
	}
}

func TestToolAdmissionCannotRewriteDecisionOrRejectStartedCall(t *testing.T) {
	for _, started := range []bool{false, true} {
		kernel, err := NewRuntimeKernel(KernelIdentity{TurnID: "immutable-rejection", ProfileRevision: 1}, protocol.TurnIntentAnswer, "act", nil, false, nil, nil, nil, nil, nil, DefaultPolicy(), NewEphemeralCoordinatorRuntime())
		if err != nil {
			t.Fatal(err)
		}
		calls := []provider.ToolCall{{ID: "a", Name: "write"}}
		if err := kernel.StartTools(calls); err != nil {
			t.Fatal(err)
		}
		if started {
			if err := kernel.StartTool("a"); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := kernel.recordToolAdmissionRejection(calls, tool.Result{IsError: true, Content: "first rejection"}); err != nil {
				t.Fatal(err)
			}
		}
		before := kernel.Snapshot()
		if err := kernel.recordToolAdmissionRejection(calls, tool.Result{IsError: true, Content: "changed rejection"}); err == nil {
			t.Fatal("rewrote rejection or claimed running call never executed")
		}
		if !reflect.DeepEqual(before, kernel.Snapshot()) {
			t.Fatal("invalid rejection changed state")
		}
	}
}
