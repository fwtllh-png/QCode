package contextstate

import (
	"context"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	sessionstate "github.com/fwtllh-png/QCode/internal/persist/session"
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
		if err := ensureContextThread(t.Context(), store, protocol.ThreadID(id), id, root); err != nil {
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
	source := agentcontext.IndexConversationAnswer("first", "report", 1, "1. 共享报告第一项\n2. 最后一个所有者仍需引用的定义")
	snapshot.Conversation = &agentcontext.ConversationState{}
	if err := snapshot.Conversation.Add(source); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Conversation.Select(agentcontext.NewConversationSelection(nil, []string{source.Items[1].ID}, 1, "report", "第二项"), nil); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Seal(); err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(store)
	for _, id := range []string{"first", "second"} {
		if err := repo.SaveTurnBaseline(t.Context(), protocol.ThreadID(id), "turn", snapshot); err != nil {
			t.Fatal(err)
		}
	}
	sessions := sessionstate.NewSQLiteRepository(store.SQLite()).WithMaintenance(store.Maintain)
	if _, err := sessions.DeleteLifecycle(t.Context(), "first", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.Maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, found, err := repo.TurnBaseline(t.Context(), "second", "turn")
	if err != nil || !found || got.Digest != snapshot.Digest {
		t.Fatalf("shared baseline lost: found=%v err=%v", found, err)
	}
	if got.Conversation == nil || got.Conversation.Sources[source.ID].Text != source.Text || got.Conversation.Selection.ItemIDs[0] != source.Items[1].ID {
		t.Fatal("shared conversation source or focus was collected")
	}
	if _, err := sessions.DiscardLifecycle(t.Context(), "second", 1); err != nil {
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
