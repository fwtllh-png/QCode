package state_test

import (
	"context"
	"testing"

	threadstate "github.com/fwtllh-png/QCode/internal/host/runtimeapi/thread"
	sessionstate "github.com/fwtllh-png/QCode/internal/persist/session"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestWorkspaceEventStoreIsolatesReplayAndDoesNotOwnSharedStore(
	t *testing.T,
) {
	store, err := state.Open(
		t.Context(),
		state.Options{DataDir: t.TempDir()},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	sessions := sessionstate.NewSQLiteRepository(store.SQLite())
	threads := threadstate.NewSQLiteRepository(store.SQLite())
	rootA := t.TempDir()
	rootB := t.TempDir()
	seedWorkspaceEventThread(t, sessions, threads, rootA, "session-a", "thread-a")
	seedWorkspaceEventThread(t, sessions, threads, rootB, "session-b", "thread-b")

	eventsA := state.NewWorkspaceEventStore(store, rootA)
	eventsB := state.NewWorkspaceEventStore(store, rootB)
	eventA := workspaceScopedEvent(t, 1, "thread-a", "turn-a", "workspace A")
	if err := eventsA.Append(t.Context(), eventA); err != nil {
		t.Fatal(err)
	}
	eventB := workspaceScopedEvent(t, 2, "thread-b", "turn-b", "workspace B")
	if err := eventsB.Append(t.Context(), eventB); err != nil {
		t.Fatal(err)
	}
	if err := eventsA.Append(t.Context(), workspaceScopedEvent(
		t,
		3,
		"thread-b",
		"turn-foreign",
		"foreign",
	)); err == nil {
		t.Fatal("Workspace A accepted a Workspace B event")
	}

	assertWorkspaceReplay := func(
		eventStore *state.WorkspaceEventStore,
		want protocol.EventID,
	) {
		t.Helper()
		events, err := eventStore.Replay(t.Context(), 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].ID != want {
			t.Fatalf("events = %+v, want only %s", events, want)
		}
	}
	assertWorkspaceReplay(eventsA, eventA.ID)
	assertWorkspaceReplay(eventsB, eventB.ID)
	// Even explicitly supplied foreign threads cannot expand Workspace access.
	page, more, err := eventsA.ReplaySessionBefore(t.Context(), "session-a",
		[]protocol.ThreadID{"thread-a", "thread-b"}, 0, 2, 1)
	if err != nil || more || len(page) != 1 || page[0].ID != eventA.ID {
		t.Fatalf("indexed Workspace replay=%+v more=%v err=%v", page, more, err)
	}

	if err := eventsA.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventBAfterClose := workspaceScopedEvent(
		t,
		3,
		"thread-b",
		"turn-b-next",
		"workspace B remains open",
	)
	if err := eventsB.Append(t.Context(), eventBAfterClose); err != nil {
		t.Fatalf("shared Store closed with Workspace A: %v", err)
	}
}

func seedWorkspaceEventThread(
	t *testing.T,
	sessions *sessionstate.Repository,
	threads *threadstate.Repository,
	root, sessionID string,
	threadID protocol.ThreadID,
) {
	t.Helper()
	if err := sessions.EnsureSeed(t.Context(), sessionID, root); err != nil {
		t.Fatal(err)
	}
	if _, err := threads.Create(
		t.Context(),
		threadstate.Thread{
			ID: threadID, SessionID: sessionID, Status: threadstate.ThreadOpen,
		},
	); err != nil {
		t.Fatal(err)
	}
}

func workspaceScopedEvent(
	t *testing.T,
	sequence protocol.Cursor,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
	text string,
) protocol.Event {
	t.Helper()
	event, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: sequence, OperationID: protocol.OperationID("operation-" + turnID),
		ThreadID: threadID, TurnID: turnID,
		ItemID: protocol.ItemID("item-" + turnID),
	}, &protocol.TurnCompletedData{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return event
}
