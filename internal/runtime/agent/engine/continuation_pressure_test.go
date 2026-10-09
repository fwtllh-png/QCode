package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestContinuationPressureRetainsFullOutputAndRegeneratesCalls(t *testing.T) {
	for _, window := range []uint64{4096, 8192} {
		for _, fragments := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/fragments=%t", window, fragments), func(t *testing.T) {
				body := strings.Repeat("confirmed partial progress ", int(window))
				events := []provider.StreamEvent{{Type: provider.EventTextDelta, Text: body}}
				if fragments {
					events = append(events, provider.StreamEvent{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
						ID: "incomplete", Name: "echo", Arguments: `{"text":"` + strings.Repeat("unfinished argument ", int(window)),
					}})
				}
				events = append(events, provider.StreamEvent{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens})
				runtime := &scriptedProvider{streams: []provider.Stream{&providerfixture.SliceStream{Events: events}}}
				if fragments {
					runtime.streams = append(runtime.streams, toolCallStream("fresh", "echo", `{"text":"fresh valid call"}`))
				}
				runtime.streams = append(runtime.streams, textStream(" finished"))
				results := tool.NewResultStore(32 << 10)
				registry := tool.NewRegistry(nil, results)
				echo := &echoTool{}
				if err := registry.Register(echo); err != nil {
					t.Fatal(err)
				}
				e := newEngine(t, runtime, registry)
				e.options.Route = mustTestRouteWithContext(t, window)
				e.options.Routes, _ = model.NewRouteSet(e.options.Route, nil, false)
				var samples []protocol.SampleContextData
				var commentary string
				result, err := e.RunForTurn(t.Context(), "pressure", "continue this requested task", func(event Event) error {
					if event.SampleContext != nil {
						samples = append(samples, *event.SampleContext)
					}
					if event.Commentary != nil {
						commentary += event.Commentary.Text
					}
					return nil
				})
				if err != nil || result.State != Completed || commentary+result.Text != body+" finished" {
					t.Fatalf("state=%s text bytes=%d error=%v", result.State, len(result.Text), err)
				}
				visible := joinMessageText(runtime.requests[1].Messages)
				if !strings.Contains(visible, "[continuation_context_projection]") || !strings.Contains(visible, "continue this requested task") {
					t.Fatal("missing compact projection or user request")
				}
				handle := regexp.MustCompile(`result_[a-f0-9]{64}`).FindString(visible)
				full, ok := results.Get(handle)
				var archived []provider.Message
				if !ok || json.Unmarshal([]byte(full), &archived) != nil || !strings.Contains(joinMessageText(archived), body) {
					t.Fatal("full partial output cannot be recovered")
				}
				wantCalls := int32(0)
				if fragments {
					wantCalls = 1
				}
				if echo.calls.Load() != wantCalls {
					t.Fatalf("calls=%d", echo.calls.Load())
				}
				for _, sample := range samples {
					if sample.WindowFullActiveTokens+sample.WindowOutputReserve > window {
						t.Fatal("sample exceeded context window")
					}
				}
			})
		}
	}
}

func TestContinuationPressureRestartRebuildsProjectionWithoutReplayingTools(t *testing.T) {
	body := strings.Repeat("retained partial output ", 8192)
	runtime, engine, echo, _ := restartTestEngines(t, "workspace-restart", func(engine *Engine, runtime *scriptedProvider) {
		engine.options.Route = mustTestRouteWithContext(t, 4096)
		engine.options.Routes, _ = model.NewRouteSet(engine.options.Route, nil, false)
		runtime.streams = []provider.Stream{runtime.streams[0], &providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: body},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens},
		}}, runtime.streams[1]}
	})
	engine.options.Route = mustTestRouteWithContext(t, 4096)
	engine.options.Routes, _ = model.NewRouteSet(engine.options.Route, nil, false)
	result, err := engine.RunForTurn(t.Context(), "turn-restart", "inspect the parser", nil)
	if err != nil || result.State != Completed || result.Text != body+"resumed" || echo.calls.Load() != 1 {
		t.Fatalf("state=%s calls=%d text bytes=%d err=%v", result.State, echo.calls.Load(), len(result.Text), err)
	}
	if !strings.Contains(joinMessageText(runtime.requests[0].Messages), "[continuation_context_projection]") {
		t.Fatal("restart did not rebuild the bounded projection")
	}
}

func TestContinuationPressureStorageFailurePreservesOriginal(t *testing.T) {
	store := contentstore.NewMemory(contentstore.Options{})
	if err := store.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, tool.NewResultStoreWithStore(4096, store)))
	messages := []provider.Message{provider.TextMessage(provider.RoleAssistant, strings.Repeat("partial ", 4096))}
	before := cloneMessages(messages)
	after, changed, err := e.compactModelContinuation(messages, tokenWindow{total: 9000, hardLimit: 4096}, func([]provider.Message) (tokenWindow, error) {
		t.Fatal("measured unpersisted projection")
		return tokenWindow{}, nil
	})
	if err != nil || changed || !reflect.DeepEqual(before, after) {
		t.Fatalf("lost output after spill failure: %v", err)
	}
}

func TestContinuationPressureKeepsMandatoryRequestIrreducible(t *testing.T) {
	e := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	e.options.Route = mustTestRouteWithContext(t, 4096)
	e.options.Routes, _ = model.NewRouteSet(e.options.Route, nil, false)
	user := strings.Repeat("required user instruction ", 4096)
	messages := []provider.Message{provider.TextMessage(provider.RoleAssistant, strings.Repeat("partial ", 4096))}
	measure := func(continuation []provider.Message) (tokenWindow, error) {
		input := agentcontext.NewMessageLedger(agentcontext.LedgerInput{History: []provider.Message{provider.TextMessage(provider.RoleUser, user)}, Continuation: continuation}).Snapshot()
		return e.measureTokenWindow(input, 128, 0)
	}
	before, err := measure(messages)
	if err != nil {
		t.Fatal(err)
	}
	compacted, changed, err := e.compactModelContinuation(messages, before, measure)
	if err != nil || !changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
	after, err := measure(compacted)
	if err != nil || after.total <= after.hardLimit {
		t.Fatal("mandatory user request was silently discarded")
	}
}
