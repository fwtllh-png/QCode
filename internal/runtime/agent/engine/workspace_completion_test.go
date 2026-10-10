package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// workspaceCompletionFixture wires real file tools and a journal to exercise
// workspace changes, completion, and rollback.
type workspaceCompletionFixture struct {
	engine   *Engine
	provider *scriptedProvider
	path     string
	journal  *workspacejournal.Manager
}

func newWorkspaceCompletionFixture(t *testing.T, extraReplies, maxSteps int) workspaceCompletionFixture {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := contentstore.NewMemory(contentstore.Options{})
	registry := tool.NewRegistry(nil, tool.NewResultStoreWithStore(32<<10, store))
	files, err := filetool.NewWithBackend(root, engineSandboxBackend{root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Register(registry); err != nil {
		t.Fatal(err)
	}
	journal, err := workspacejournal.New(root, store)
	if err != nil {
		t.Fatal(err)
	}
	streams := []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				ID: "read", Name: "file_read", Arguments: `{"path":"value.txt"}`,
			}},
			{Type: provider.EventMessageStop},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				ID: "edit", Name: "file_edit",
				Arguments: `{"path":"value.txt","old":"before","new":"after"}`,
			}},
			{Type: provider.EventMessageStop},
		}},
	}
	for range 1 + extraReplies {
		streams = append(streams, &providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "done"},
			{Type: provider.EventMessageStop},
		}})
	}
	runtime := &scriptedProvider{streams: streams}
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: runtime, Route: testRoute(t),
		MaxOutputTokens: 128, MaxSteps: maxSteps}, ToolConfig: ToolConfig{Tools: registry,

		Diagnostics: fakeDiagnosticRunner{}}, SecurityConfig: SecurityConfig{Workspace: root,
		Journal: journal},
	})
	if err != nil {
		t.Fatal(err)
	}
	return workspaceCompletionFixture{
		engine: engine, provider: runtime, path: path,
		journal: journal,
	}
}

func (f workspaceCompletionFixture) contents(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRetainedDraftConflictTerminalizesNewTurn(t *testing.T) {
	fixture := newWorkspaceCompletionFixture(
		t,
		0,
		4,
	)
	if err := fixture.journal.Begin("turn-source-draft"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.journal.Before(t.Context(), fixture.path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.path, []byte("draft\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.journal.After(fixture.path); err != nil {
		t.Fatal(err)
	}
	if err := fixture.journal.Suspend("turn-source-draft"); err != nil {
		t.Fatal(err)
	}

	var states []State
	result, err := fixture.engine.RunForTurnWithIntentAndAttachments(
		t.Context(),
		"turn-journal-conflict",
		"unrelated request",
		protocol.TurnIntentAnswer,
		nil,
		func(event Event) error {
			states = append(states, event.State)
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "retained draft") {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	if len(states) == 0 || states[0] != Preparing {
		t.Fatalf("journal admission states = %v, want Preparing first", states)
	}
	handle, err := fixture.engine.options.TurnCoordinatorRuntime.Restore(
		t.Context(),
		"turn-journal-conflict",
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = fixture.engine.options.TurnCoordinatorRuntime.Release(
			context.Background(),
			"turn-journal-conflict",
		)
	})
	state := handle.Coordinator.Snapshot()
	if state.Phase != turnkernel.PhaseFailed ||
		state.Terminal == nil ||
		state.Terminal.Kind != turnkernel.TerminalFailed ||
		state.Terminal.Fault == nil ||
		state.Terminal.Fault.Disposition != protocol.FaultResumeTurn ||
		state.Journal != turnkernel.JournalSuspended ||
		!fixture.journal.HasDraft("turn-source-draft") {
		t.Fatalf("terminal state = %+v", state)
	}
}

func TestOrphanedDraftContinueAdoptsDeletedSessionDraft(t *testing.T) {
	fixture := newWorkspaceCompletionFixture(
		t,
		0,
		4,
	)
	fixture.engine.options.SessionForTurn = func(
		context.Context,
		string,
	) (string, bool) {
		return "", false
	}
	if err := fixture.journal.Begin("turn-deleted-session"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.journal.Before(t.Context(), fixture.path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.path, []byte("draft\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.journal.After(fixture.path); err != nil {
		t.Fatal(err)
	}
	if err := fixture.journal.Suspend("turn-deleted-session"); err != nil {
		t.Fatal(err)
	}

	blocked, err := fixture.engine.RunForTurnWithIntentAndAttachments(
		t.Context(),
		"turn-new-session",
		"unrelated request",
		protocol.TurnIntentAnswer,
		nil,
		func(Event) error { return nil },
	)
	if err == nil ||
		protocol.DispositionOf(err) != protocol.FaultRetryTurn ||
		!fixture.journal.HasDraft("turn-deleted-session") {
		t.Fatalf("orphan block = %+v error = %v", blocked, err)
	}

	fixture.provider.streams = []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "done"},
			{Type: provider.EventMessageStop},
		}},
	}
	recovery := protocol.TurnRecoveryContext{
		Action: protocol.TurnRecoveryContinue, SourceTurnID: "turn-new-session",
	}
	continued, err := fixture.engine.RunForTurnWithRequest(
		t.Context(),
		"turn-adopted",
		TurnRequest{
			Prompt: "continue orphaned draft", Intent: protocol.TurnIntentAnswer,
			Recovery: &recovery,
		},
		func(Event) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if continued.State != Completed ||
		fixture.contents(t) != "draft\n" ||
		fixture.journal.HasDraft("turn-deleted-session") ||
		fixture.journal.HasDraft("turn-new-session") {
		t.Fatalf(
			"adopted = %+v contents=%q deleted_draft=%v new_draft=%v",
			continued,
			fixture.contents(t),
			fixture.journal.HasDraft("turn-deleted-session"),
			fixture.journal.HasDraft("turn-new-session"),
		)
	}
}

func TestOrphanedDraftRetryRevertsDeletedSessionDraft(t *testing.T) {
	fixture := newWorkspaceCompletionFixture(
		t,
		0,
		4,
	)
	fixture.engine.options.SessionForTurn = func(
		context.Context,
		string,
	) (string, bool) {
		return "", false
	}
	if err := fixture.journal.Begin("turn-deleted-session"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.journal.Before(t.Context(), fixture.path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.path, []byte("draft\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.journal.After(fixture.path); err != nil {
		t.Fatal(err)
	}
	if err := fixture.journal.Suspend("turn-deleted-session"); err != nil {
		t.Fatal(err)
	}

	fixture.provider.streams = []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "done"},
			{Type: provider.EventMessageStop},
		}},
	}
	recovery := protocol.TurnRecoveryContext{
		Action: protocol.TurnRecoveryRetry, SourceTurnID: "turn-unrelated",
	}
	retried, err := fixture.engine.RunForTurnWithRequest(
		t.Context(),
		"turn-reverted",
		TurnRequest{
			Prompt: "retry after delete", Intent: protocol.TurnIntentAnswer,
			Recovery: &recovery,
		},
		func(Event) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if retried.State != Completed ||
		fixture.contents(t) != "before\n" ||
		fixture.journal.HasDraft("turn-deleted-session") {
		t.Fatalf(
			"reverted = %+v contents=%q draft=%v",
			retried,
			fixture.contents(t),
			fixture.journal.HasDraft("turn-deleted-session"),
		)
	}
}

func TestWorkspaceChangesCompleteWithoutVerificationGate(t *testing.T) {
	fixture := newWorkspaceCompletionFixture(t, 0, 6)
	result, err := fixture.engine.RunForTurnWithIntentAndAttachments(t.Context(), "ordinary-edit", "change value", protocol.TurnIntentWorkspaceChange, nil, func(event Event) error {
		if event.State == Verifying {
			t.Fatal("ordinary edit entered a verification gate")
		}
		return nil
	})
	if err != nil || result.State != Completed || fixture.contents(t) != "after\n" {
		t.Fatalf("edit did not complete: %+v %v", result, err)
	}
	if len(fixture.provider.requests) != 3 {
		t.Fatalf("unexpected repair samples: %d", len(fixture.provider.requests))
	}
}
