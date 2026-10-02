package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestCompactWindowChainAndResume(t *testing.T) {
	store := NewMemoryEventStore(256)
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
	manager.SetWindowRestorer(func(ctx context.Context, threadID protocol.ThreadID) (*protocol.ThreadCompactedData, error) {
		return agentcontext.LatestThreadHistorySeed(ctx, store, threadID)
	})
	manager.SetSequenceReader(func(ctx context.Context) (protocol.Cursor, error) {
		return store.LastSequence(ctx)
	})
	runtime := NewRuntime(Options{Engine: manager, EventStore: store})
	defer runtime.Close(context.Background())

	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range []struct {
		turnID, itemID, prompt string
	}{
		{"turn-1", "item-1", "first window turn"},
		{"turn-2", "item-2", "second window turn"},
	} {
		start, err := protocol.NewOperation(&protocol.StartTurnPayload{
			ThreadID: "thread-window", TurnID: protocol.TurnID(turn.turnID),
			ItemID: protocol.ItemID(turn.itemID), Prompt: turn.prompt,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.Submit(t.Context(), start); err != nil {
			t.Fatal(err)
		}
		waitTerminal(t, events, 2*time.Second)
	}

	compact, err := protocol.NewOperation(&protocol.CompactThreadPayload{
		ThreadID: "thread-window", TurnID: "turn-2", ItemID: "compact-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), compact); err != nil {
		t.Fatal(err)
	}
	var compacted *protocol.ThreadCompactedData
	deadline := time.After(2 * time.Second)
	for compacted == nil {
		select {
		case event := <-events:
			if event.Kind == protocol.EventThreadCompacted {
				data, ok := event.Data.(*protocol.ThreadCompactedData)
				if !ok || data.WindowID == "" || len(data.ReplacementHistory) == 0 {
					t.Fatalf("compacted window = %#v", event.Data)
				}
				if data.WindowNumber != 2 ||
					data.FirstWindowID == "" ||
					data.PreviousWindowID != data.FirstWindowID ||
					data.WindowID == data.PreviousWindowID {
					t.Fatalf("window chain = %#v", data)
				}
				compacted = data
			}
		case <-deadline:
			t.Fatal("thread.compacted was not emitted")
		}
	}

	before, err := manager.History("thread-window")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate process restart: new ThreadManager + same event store.
	resumed := NewThreadManager(func() (*EngineAdapter, error) {
		clone, err := seed.CloneEmpty()
		if err != nil {
			return nil, err
		}
		return AdaptEngine(clone), nil
	})
	resumed.SetWindowRestorer(func(ctx context.Context, threadID protocol.ThreadID) (*protocol.ThreadCompactedData, error) {
		return agentcontext.LatestThreadHistorySeed(ctx, store, threadID)
	})
	hist, err := resumed.History("thread-window")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != len(before) {
		t.Fatalf("resumed history len=%d want %d\nbefore=%+v\nafter=%+v", len(hist), len(before), before, hist)
	}
	for i := range hist {
		if hist[i].Role != before[i].Role || hist[i].Text() != before[i].Text() || hist[i].Turn != before[i].Turn {
			t.Fatalf("resumed history mismatch at %d:\n%+v\n%+v", i, hist[i], before[i])
		}
	}
	resumedAdapter, err := resumed.forThread("thread-window")
	if err != nil {
		t.Fatal(err)
	}
	windowID, windowNumber := resumedAdapter.Underlying().TokenWindowIdentity()
	if windowID != compacted.WindowID || windowNumber != compacted.WindowNumber {
		t.Fatalf(
			"resumed token window=%s/%d, want %s/%d",
			windowID, windowNumber, compacted.WindowID, compacted.WindowNumber,
		)
	}
}

func TestThreadResumeKeepsCompactedWindowNewerThanTerminalDelta(t *testing.T) {
	seed, err := newTestAgentEngine(agentengine.Options{
		ProviderConfig: agentengine.ProviderConfig{
			Provider:        &threadEchoProvider{},
			Route:           runtimeTestRoute(t),
			MaxOutputTokens: 128,
		},
		ToolConfig: agentengine.ToolConfig{
			Tools: tool.NewRegistry(nil, nil),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	oldWindow, err := agentcontext.NewWindowLedger("window-old", 1)
	if err != nil {
		t.Fatal(err)
	}
	binding := agentcontext.WorkspaceBinding{
		WorkspaceIdentity: "workspace:test",
	}
	binding.Seal()
	oldMessage := provider.TextMessage(provider.RoleUser, "old terminal history")
	oldMessage.Turn = 1
	snapshot := agentcontext.ContextSnapshot{
		Version:   agentcontext.ContextSnapshotVersion,
		Epoch:     1,
		Revision:  1,
		Turn:      1,
		History:   []provider.Message{oldMessage},
		Workspace: binding,
		Window:    oldWindow,
	}
	if err := snapshot.Seal(); err != nil {
		t.Fatal(err)
	}
	accounting := agentcontext.AccountingDelta{TurnID: "turn-1"}
	accounting.Seal()
	delta, err := agentcontext.NewSessionDelta(snapshot, accounting)
	if err != nil {
		t.Fatal(err)
	}
	rawDelta, err := json.Marshal(delta)
	if err != nil {
		t.Fatal(err)
	}
	newMessage := provider.TextMessage(
		provider.RoleSystem,
		"new compacted history",
	)
	encoded, err := agentcontext.EncodeCompactedHistory(
		[]provider.Message{newMessage},
	)
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
	manager.SetWindowRestorer(func(
		context.Context,
		protocol.ThreadID,
	) (*protocol.ThreadCompactedData, error) {
		return &protocol.ThreadCompactedData{
			ReplacementHistory: encoded,
			WindowNumber:       2,
			FirstWindowID:      "window-old",
			WindowID:           "window-compacted",
		}, nil
	})
	manager.SetSessionDeltaRestorer(func(
		context.Context,
		protocol.ThreadID,
	) (json.RawMessage, error) {
		return rawDelta, nil
	})

	history, err := manager.History("thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Text() != "new compacted history" {
		t.Fatalf("restored history = %+v", history)
	}
	adapter, err := manager.forThread("thread-1")
	if err != nil {
		t.Fatal(err)
	}
	windowID, windowNumber := adapter.Underlying().TokenWindowIdentity()
	if windowID != "window-compacted" || windowNumber != 2 {
		t.Fatalf("restored window = %s/%d", windowID, windowNumber)
	}
}

func TestCompactForkResume(t *testing.T) {
	store := NewMemoryEventStore(256)
	seed, err := newTestAgentEngine(agentengine.Options{ProviderConfig: agentengine.ProviderConfig{Provider: &threadEchoProvider{}, Route: runtimeTestRoute(t),

		MaxOutputTokens: 128}, ToolConfig: agentengine.ToolConfig{Tools: tool.NewRegistry(nil, nil)}, TelemetryConfig: agentengine.TelemetryConfig{Metrics: telemetry.NewMetrics()},
	})
	if err != nil {
		t.Fatal(err)
	}
	factory := func() (*EngineAdapter, error) {
		clone, err := seed.CloneEmpty()
		if err != nil {
			return nil, err
		}
		return AdaptEngine(clone), nil
	}
	manager := NewThreadManager(factory)
	manager.SetWindowRestorer(func(ctx context.Context, threadID protocol.ThreadID) (*protocol.ThreadCompactedData, error) {
		return agentcontext.LatestThreadHistorySeed(ctx, store, threadID)
	})
	manager.SetSequenceReader(func(ctx context.Context) (protocol.Cursor, error) {
		return store.LastSequence(ctx)
	})
	runtime := NewRuntime(Options{Engine: manager, EventStore: store})
	defer runtime.Close(context.Background())

	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	start, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: "thread-parent", TurnID: "turn-1", ItemID: "item-1", Prompt: "parent turn",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, events, 2*time.Second)

	compact, err := protocol.NewOperation(&protocol.CompactThreadPayload{
		ThreadID: "thread-parent", TurnID: "turn-1", ItemID: "compact-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), compact); err != nil {
		t.Fatal(err)
	}
	waitKind(t, events, protocol.EventThreadCompacted, 2*time.Second)

	beforeFork, err := store.LastSequence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fork, err := protocol.NewOperation(&protocol.ForkThreadPayload{
		ThreadID: "thread-parent", TurnID: "turn-1", ItemID: "fork-1",
		NewThreadID: "thread-child",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), fork); err != nil {
		t.Fatal(err)
	}
	var forked *protocol.ThreadForkedData
	deadline := time.After(2 * time.Second)
	for forked == nil {
		select {
		case event := <-events:
			if event.Kind == protocol.EventThreadForked {
				data, ok := event.Data.(*protocol.ThreadForkedData)
				if !ok || data.NewThreadID != "thread-child" {
					t.Fatalf("forked = %#v", event.Data)
				}
				if data.SourceCursor != beforeFork {
					t.Fatalf("source cursor = %d, want flushed %d", data.SourceCursor, beforeFork)
				}
				if len(data.ReplacementHistory) == 0 {
					t.Fatal("fork must carry replacement history")
				}
				if data.WindowID == "" || data.WindowNumber != 1 ||
					data.FirstWindowID != data.WindowID {
					t.Fatalf("fork window = %#v", data)
				}
				forked = data
			}
		case <-deadline:
			t.Fatal("thread.forked was not emitted")
		}
	}

	parentHist, err := manager.History("thread-parent")
	if err != nil {
		t.Fatal(err)
	}
	childHist, err := manager.History("thread-child")
	if err != nil {
		t.Fatal(err)
	}
	if len(childHist) != len(parentHist) {
		t.Fatalf("child history len=%d want %d", len(childHist), len(parentHist))
	}

	resumed := NewThreadManager(factory)
	resumed.SetWindowRestorer(func(ctx context.Context, threadID protocol.ThreadID) (*protocol.ThreadCompactedData, error) {
		return agentcontext.LatestThreadHistorySeed(ctx, store, threadID)
	})
	restored, err := resumed.History("thread-child")
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != len(childHist) {
		t.Fatalf("resumed child history len=%d want %d", len(restored), len(childHist))
	}
	resumedChild, err := resumed.forThread("thread-child")
	if err != nil {
		t.Fatal(err)
	}
	windowID, windowNumber := resumedChild.Underlying().TokenWindowIdentity()
	if windowID != forked.WindowID || windowNumber != forked.WindowNumber {
		t.Fatalf(
			"resumed fork window=%s/%d, want %s/%d",
			windowID, windowNumber, forked.WindowID, forked.WindowNumber,
		)
	}
}

func waitKind(t *testing.T, events <-chan protocol.Event, kind protocol.EventKind, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case event := <-events:
			if event.Kind == kind {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", kind)
		}
	}
}

func TestEncodeDecodeCompactedHistoryRoundTrip(t *testing.T) {
	assistant := provider.Message{
		Role: provider.RoleAssistant, Turn: 1,
		Blocks: []provider.ContentBlock{{
			Type: provider.ContentText, Text: "world",
		}},
		Provenance: &provider.AssistantProvenance{
			Adapter: "openai", Provider: "openai", Model: "model",
			Replay: &provider.ReplayState{
				Version: provider.ReplayVersion,
				Data:    json.RawMessage(`{"items":[]}`),
			},
		},
	}
	assistant.Provenance.Replay.ContentDigest =
		provider.MessageContentDigest(assistant)
	input := []provider.Message{
		{Role: provider.RoleUser, Turn: 1, Blocks: []provider.ContentBlock{{
			Type: provider.ContentText, Text: "hello",
		}}},
		assistant,
	}
	encoded, err := agentcontext.EncodeCompactedHistory(input)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := agentcontext.DecodeCompactedHistory(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 || decoded[0].Turn != 1 || decoded[1].Text() != "world" {
		t.Fatalf("decoded = %+v", decoded)
	}
	if decoded[1].Provenance != nil {
		t.Fatalf("rewritten compacted history retained private replay: %+v", decoded[1])
	}
}
