package persistence

import (
	"context"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestSessionDeletionPreservesSharedContextUntilLastOwner(t *testing.T) {
	store, err := state.Open(t.Context(), state.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.CloseAll(context.Background())
	root := t.TempDir()
	for _, id := range []string{"first", "second"} {
		if err := EnsureThread(t.Context(), store, protocol.ThreadID(id), id, root); err != nil {
			t.Fatal(err)
		}
	}
	window, err := agentcontext.NewWindowLedger("initial", 1)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := agentcontext.ContextSnapshot{
		Epoch: 1, Revision: 1, Window: window,
		History:   []provider.Message{provider.TextMessage(provider.RoleUser, "shared conversation")},
		Workspace: agentcontext.WorkspaceBinding{WorkspaceIdentity: "workspace:test"},
	}
	if err := snapshot.Seal(); err != nil {
		t.Fatal(err)
	}
	repo := NewContextRebaseRepository(store)
	for _, id := range []string{"first", "second"} {
		if err := repo.SaveTurnBaseline(t.Context(), protocol.ThreadID(id), "turn", snapshot); err != nil {
			t.Fatal(err)
		}
	}
	repositories, err := NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repositories.Sessions.DeleteLifecycle(t.Context(), "first", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, found, err := repo.TurnBaseline(t.Context(), "second", "turn")
	if err != nil || !found || got.Digest != snapshot.Digest {
		t.Fatalf("shared baseline lost: found=%v err=%v", found, err)
	}
	if _, err := repositories.Sessions.DiscardLifecycle(t.Context(), "second", 1); err != nil {
		t.Fatal(err)
	}
	var objects int
	if err := store.SQLite().DB().QueryRowContext(t.Context(), "SELECT count(*) FROM content_objects").Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if objects != 0 {
		t.Fatalf("last session deletion leaked %d content objects", objects)
	}
}
