package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

type archiveFixture struct {
	turnID  string
	history []provider.Message
	calls   int
}

func (a *archiveFixture) LookupTurn(
	_ context.Context, turnID string,
) ([]provider.Message, error) {
	a.calls++
	if turnID != a.turnID {
		return nil, nil
	}
	return a.history, nil
}

func TestLookupTurnHistoryFallsBackToArchive(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("ok"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	archive := &archiveFixture{
		turnID: "turn-archived",
		history: []provider.Message{
			messageWithText(provider.RoleUser, "archived question", 7),
			messageWithText(provider.RoleAssistant, "archived answer", 7),
		},
	}
	engine.options.TurnTranscriptArchive = archive
	engine.turnIDs["turn-archived"] = 7
	// The archived turn was removed from the in-memory history by a
	// replacement that only retains the current turn.
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, "current request", 8),
	}

	messages, err := engine.lookupTurnHistory(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Text() != "archived question" {
		t.Fatalf("archived messages = %+v", messages)
	}
	if archive.calls != 1 {
		t.Fatalf("archive calls = %d", archive.calls)
	}

	// In-memory hits never consult the archive.
	messages, err = engine.lookupTurnHistory(t.Context(), 8)
	if err != nil || len(messages) != 1 {
		t.Fatalf("current lookup = %+v err=%v", messages, err)
	}
	if archive.calls != 1 {
		t.Fatalf("archive consulted for in-memory turn: %d", archive.calls)
	}

	// Unknown turn numbers stay honest misses instead of probing storage.
	if messages, err = engine.lookupTurnHistory(t.Context(), 9); err != nil ||
		len(messages) != 0 {
		t.Fatalf("unknown lookup = %+v err=%v", messages, err)
	}
	if archive.calls != 1 {
		t.Fatalf("archive consulted for unknown turn: %d", archive.calls)
	}
}

func TestTurnHistoryDuringExecute(t *testing.T) {
	for _, test := range []struct {
		name         string
		arguments    string
		noArchive    bool
		wantText     string
		wantError    bool
		archiveCalls int
	}{
		{"memory", `{"turn":8}`, false, "current answer", false, 0},
		{"archive", `{"turn":7}`, false, "archived answer", false, 1},
		{"unknown", `{"turn":6}`, false, "not in durable history", true, 0},
		{"no archive", `{"turn":7}`, true, "not in durable history", true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := &scriptedProvider{streams: []provider.Stream{
				toolCallStream("history-1", "turn_history", test.arguments),
				// A successful tool also closes the failure state for honest
				// misses, so the next sample can finish normally.
				toolCallStream("echo-1", "echo", `{"text":"continue"}`),
				textStream("recovered"),
			}}
			engine := newTurnArchiveEngine(t, runtime)
			archive := &archiveFixture{
				turnID: "turn-archived",
				history: []provider.Message{
					messageWithText(provider.RoleUser, "archived question", 7),
					messageWithText(provider.RoleAssistant, "archived answer", 7),
				},
			}
			if !test.noArchive {
				engine.options.TurnTranscriptArchive = archive
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			result, err := engine.Execute(ctx, TurnRequest{Prompt: "retrieve the earlier turn"}, nil)
			if err != nil || result.State != Completed {
				t.Fatalf("state=%s err=%v", result.State, err)
			}
			if archive.calls != test.archiveCalls {
				t.Fatalf("archive calls=%d want=%d", archive.calls, test.archiveCalls)
			}
			if len(runtime.requests) < 2 {
				t.Fatal("tool result did not reach the next sample")
			}
			for _, message := range runtime.requests[1].Messages {
				for _, block := range message.Blocks {
					if result := block.ToolResult; result != nil && result.CallID == "history-1" {
						if result.IsError != test.wantError ||
							!strings.Contains(result.Content, test.wantText) {
							t.Fatalf("history result=%+v", result)
						}
						return
					}
				}
			}
			t.Fatal("history result is missing from the next sample")
		})
	}
}

func newTurnArchiveEngine(t *testing.T, runtime provider.Provider) *Engine {
	t.Helper()
	registry := tool.NewRegistry(nil, nil)
	// A retrieval-only catalog is intentionally hidden from model sampling.
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	engine.options.Workspace = t.TempDir()
	engine.turnIDs["turn-archived"] = 7
	engine.turn = 8
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, "current request", 8),
		messageWithText(provider.RoleAssistant, "current answer", 8),
	}
	return engine
}

type turnArchiveFunc func(context.Context, string) ([]provider.Message, error)

func (f turnArchiveFunc) LookupTurn(ctx context.Context, turnID string) ([]provider.Message, error) {
	return f(ctx, turnID)
}

func TestTurnHistoryArchiveCancellationReleasesExecute(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("history-1", "turn_history", `{"turn":7}`),
		toolCallStream("echo-1", "echo", `{"text":"continue"}`),
		textStream("next turn"),
	}}
	engine := newTurnArchiveEngine(t, runtime)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	lookedUp := false
	engine.options.TurnTranscriptArchive = turnArchiveFunc(func(ctx context.Context, turnID string) ([]provider.Message, error) {
		lookedUp = true
		if turnID != "turn-archived" {
			return nil, nil
		}
		control, err := engine.Control()
		if err != nil {
			return nil, err
		}
		if err := control.Cancel("cancel archive lookup"); err != nil {
			return nil, err
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	result, _ := engine.Execute(ctx, TurnRequest{Prompt: "retrieve the earlier turn"}, nil)
	if !lookedUp || result.State != Canceled || engine.runningScope() != nil {
		t.Fatalf("lookup=%v state=%s scope=%v", lookedUp, result.State, engine.runningScope())
	}
	nextCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	result, err := engine.Execute(nextCtx, TurnRequest{Prompt: "continue"}, nil)
	if err != nil || result.State != Completed {
		t.Fatalf("next turn state=%s err=%v", result.State, err)
	}
}

func TestTurnArchiveSnapshotDoesNotRetainMutableIndex(t *testing.T) {
	engine := newTurnArchiveEngine(t, &scriptedProvider{})
	archive := &archiveFixture{turnID: "turn-archived"}
	engine.options.TurnTranscriptArchive = archive
	engine.mu.Lock()
	scope := &Scope{engine: engine, state: newScopeState(engine)}
	delete(engine.turnIDs, "turn-archived")
	engine.options.TurnTranscriptArchive = nil
	engine.mu.Unlock()
	engine.publishScope(scope)
	defer engine.finishScope(scope)

	got, turnID := engine.turnArchiveSource(7)
	if got != archive || turnID != "turn-archived" {
		t.Fatalf("frozen archive=%v turn=%q", got, turnID)
	}
}
