package engine

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestCompactionHeadroomPreservesActualProviderToolLoopPrefix(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("headroom-a", "echo", `{"text":"first result"}`),
		toolCallStream("headroom-b", "echo", `{"text":"second result"}`),
		textStream("done"),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	e, err := newTestEngine(Options{
		ProviderConfig: ProviderConfig{Provider: runtime, Route: mustTestRouteWithContext(t, 32768), MaxOutputTokens: 128},
		ToolConfig:     ToolConfig{Tools: registry, Authorize: func(provider.ToolCall) bool { return true }},
		SecurityConfig: SecurityConfig{Workspace: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.options.TokenEstimator = selectionTextEstimator{}
	for turn := uint64(1); turn <= 64; turn++ {
		e.history = append(e.history, messageWithText(provider.RoleUser, strings.Repeat("x", 512), turn))
	}
	e.history = append(e.history, messageWithText(provider.RoleUser, strings.Repeat("recent work ", 400), 65))
	e.turn = 65
	var samples []protocol.SampleContextData
	var folds int
	_, err = e.Execute(t.Context(), TurnRequest{Prompt: "continue"}, func(event Event) error {
		if event.InputContext != nil {
			samples = append(samples, *event.InputContext)
		}
		if event.Compaction != nil && event.Compaction.Mode == "view" {
			folds++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 3 || folds != 1 || samples[0].CompactionHeadroomTokens == 0 {
		t.Fatalf("samples=%d folds=%d", len(samples), folds)
	}
	for index := 1; index < len(samples); index++ {
		if !samples[index].PrefixMonotonic || samples[index].PairingVisibleOrphans != 0 {
			t.Fatalf("sample %d lost cacheable prefix: %+v", index, samples[index])
		}
		assertRequestMessagePrefix(t, runtime.requests[index-1], runtime.requests[index])
	}
}

func TestCompactionHeadroomUsesWorkloadNotUnobservedFullInput(t *testing.T) {
	e := newEngine(t, &scriptedProvider{}, nil)
	e.options.TokenEstimator = selectionTextEstimator{}
	history := []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("x", 100), 1),
		messageWithText(provider.RoleAssistant, strings.Repeat("x", 200), 1),
		messageWithText(provider.RoleUser, strings.Repeat("x", 50), 2),
	}
	window := tokenWindow{hardLimit: 1000, compactLimit: 900, active: 800,
		accounting: agentcontext.WindowProjection{FullActiveTokens: 800, PendingTokens: 800}}
	margin, target := e.compactionHeadroom(history, window, 100, 0)
	if margin != 300 || target != 600 {
		t.Fatalf("first request headroom=%d target=%d", margin, target)
	}
	window.accounting.Observed = true
	window.accounting.PendingTokens = 400
	ledger := e.context.Window()
	ledger.Observe(protocol.SampleContextData{EstimatedTokens: 800,
		WindowObserved: true, WindowProjectedTokens: 800}, 820, 0)
	e.context.SetWindow(ledger)
	margin, target = e.compactionHeadroom(history, window, 100, 0)
	if margin != 420 || target != 480 {
		t.Fatalf("observed headroom=%d target=%d", margin, target)
	}
	margin, target = e.compactionHeadroom(history, window, 100, 200)
	if margin != 200 || target != 0 {
		t.Fatalf("bounded headroom=%d target=%d", margin, target)
	}
	window.active, window.compactLimit = 100, 150
	margin, target = e.compactionHeadroom(history, window, 100, 0)
	if margin != 420 || target != 430 {
		t.Fatalf("body ceiling headroom=%d target=%d", margin, target)
	}
}

func TestCompactionHeadroomKeepsPrefixAcrossSamplesAndRestoration(t *testing.T) {
	for _, initiallyTrimmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "hard_gate", true: "capacity_selection"}[initiallyTrimmed], func(t *testing.T) {
			e := newEngine(t, &scriptedProvider{}, nil)
			e.options.TokenEstimator = selectionTextEstimator{}
			e.options.Context.RecentTailTurns = 0
			e.options.MaxOutputTokens = 128
			e.options.Route = mustTestRouteWithContext(t, 1_000_000)
			var history []provider.Message
			for turn := uint64(1); turn <= 24; turn++ {
				history = append(history, messageWithText(provider.RoleUser, strings.Repeat("x", 512), turn))
			}
			history = append(history,
				messageWithText(provider.RoleUser, strings.Repeat("recent work ", 400), 25),
				messageWithText(provider.RoleUser, "continue", 26))
			original := cloneMessages(history)
			project := selectionProjector(t, e)
			input := agentcontext.NewMessageLedger(agentcontext.LedgerInput{
				Stable: e.promptMessages(), History: project(history),
			}).Snapshot()
			before, err := e.measureTokenWindow(input, 128, 0)
			if err != nil {
				t.Fatal(err)
			}
			capacity := before.total - 1
			if initiallyTrimmed {
				capacity -= 1024
			}
			e.options.Route = mustTestRouteWithContext(t, capacity)
			var receipts []*CompactionReceipt
			send := func(_ State, event Event) error {
				if event.Compaction != nil {
					receipts = append(receipts, event.Compaction)
				}
				return nil
			}
			window, err := e.runCompactGate(t.Context(), &history, input, 128,
				CompactionPhasePreSampling, true, send, 0, project)
			if err != nil || window.accounting.FullActiveTokens > e.viewFold.target ||
				len(receipts) != 1 || len(receipts[0].RemovedTurns) < 2 || !reflect.DeepEqual(history, original) {
				t.Fatalf("window=%+v fold=%+v receipts=%+v error=%v", window, e.viewFold, receipts, err)
			}
			start := e.contextProjection(history).TailStart
			prefix := cloneMessages(project(history))
			for sample := 0; sample < 4; sample++ {
				history = append(history, messageWithText(provider.RoleAssistant, strings.Repeat("growth ", 70), 26))
				window, err = e.runCompactGate(t.Context(), &history, input, 128,
					CompactionPhaseMidTurn, true, send, 0, project)
				if err != nil || window.total > window.hardLimit || e.contextProjection(history).TailStart != start || len(receipts) != 1 {
					t.Fatalf("sample %d re-folded: window=%+v start=%d receipts=%d err=%v", sample, window, e.contextProjection(history).TailStart, len(receipts), err)
				}
				after := project(history)
				// The retrieval hint follows the history; all retained source
				// messages remain an identical cacheable prefix.
				if !reflect.DeepEqual(after[:len(prefix)-1], prefix[:len(prefix)-1]) {
					t.Fatal("retained prefix changed")
				}
			}
			// Terminal maintenance and a new window must not refill the margin.
			e.resetViewFold()
			e.advanceTokenWindow()
			history = append(history, messageWithText(provider.RoleUser, "next turn", 27))
			if e.contextProjection(history).TailStart != start {
				t.Fatal("next turn refilled omitted history")
			}
			delta, err := prepareSessionDeltaForTest("headroom", 0, history, provider.Usage{}, 0,
				SessionStateDelta{Turn: 27, Window: e.context.Window()})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(delta)
			if err != nil {
				t.Fatal(err)
			}
			restored := newEngine(t, &scriptedProvider{}, nil)
			restored.options.TokenEstimator = selectionTextEstimator{}
			restored.options.Route = e.options.Route
			if err := restored.RestoreSessionDelta(raw); err != nil {
				t.Fatal(err)
			}
			if restored.contextProjection(restored.history).TailStart != start {
				t.Fatal("restoration refilled omitted history")
			}
			snapshot, err := restored.ExportContextSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := restored.RestoreContextSnapshot(snapshot); err != nil {
				t.Fatal(err)
			}
			if restored.contextProjection(restored.history).TailStart != start {
				t.Fatal("checkpoint restore refilled omitted history")
			}
			fork, _, err := restored.ForkFromContextSnapshot(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			fork.options.TokenEstimator = selectionTextEstimator{}
			if fork.context.Window().HistoryFloorTurn != snapshot.Window.HistoryFloorTurn ||
				fork.contextProjection(fork.history).TailStart < start {
				t.Fatal("checkpoint fork refilled omitted history")
			}
			restored.ReplaceHistory(original)
			restored.options.Route = mustTestRouteWithContext(t, 1_000_000)
			if restored.contextProjection(original).TailStart != 0 {
				t.Fatal("explicit replacement retained a stale boundary")
			}
		})
	}
}

func TestCompactionHeadroomIsSoftWhenOnlyCurrentRequestRemains(t *testing.T) {
	e := newEngine(t, &scriptedProvider{}, nil)
	e.options.Context.RecentTailTurns = 0
	e.options.TokenEstimator = selectionTextEstimator{}
	e.options.Route = mustTestRouteWithContext(t, 4096)
	e.options.MaxOutputTokens = 128
	e.options.Context.Window.AutoTokens = 400
	history := []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("old ", 100), 1),
		messageWithText(provider.RoleUser, strings.Repeat("required request ", 100), 2),
	}
	current := history[1]
	input := agentcontext.NewMessageLedger(agentcontext.LedgerInput{History: history}).Snapshot()
	window, err := e.runCompactGate(t.Context(), &history, input, 128,
		CompactionPhasePreSampling, true, func(State, Event) error { return nil }, 0, nil)
	if err != nil || window.total > window.hardLimit || window.accounting.FullActiveTokens <= e.viewFold.target || !reflect.DeepEqual(current, history[1]) {
		t.Fatalf("soft target damaged request or blocked: window=%+v fold=%+v err=%v", window, e.viewFold, err)
	}
}
