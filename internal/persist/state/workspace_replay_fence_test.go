package state_test

import (
	"context"
	"testing"

	threadstate "github.com/fwtllh-png/QCode/internal/host/runtimeapi/thread"
	sessionstate "github.com/fwtllh-png/QCode/internal/persist/session"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestWorkspaceReplayFenceExcludesLaterEventsAcrossForeignPages(t *testing.T) {
	store, err := state.Open(t.Context(), state.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	sessions := sessionstate.NewSQLiteRepository(store.SQLite())
	threads := threadstate.NewSQLiteRepository(store.SQLite())
	rootA, rootB := t.TempDir(), t.TempDir()
	seedWorkspaceEventThread(t, sessions, threads, rootA, "session-a", "thread-a")
	seedWorkspaceEventThread(t, sessions, threads, rootB, "session-b", "thread-b")
	// Cross the global 512-record replay page with foreign events. Leave a
	// sequence gap at the fence, then place another matching event beyond it.
	for sequence := protocol.Cursor(1); sequence <= 515; sequence++ {
		if sequence == 514 {
			continue
		}
		thread := protocol.ThreadID("thread-b")
		if sequence == 513 || sequence == 515 {
			thread = "thread-a"
		}
		event := workspaceScopedEvent(t, sequence, thread, "turn", "done")
		if err := store.Append(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	workspace := state.NewWorkspaceEventStore(store, rootA)
	for _, limit := range []int{0, 1, 2} {
		page, more, err := workspace.ReplayThrough(t.Context(), 0, 514, limit)
		if err != nil || more || len(page) != 1 || page[0].Sequence != 513 {
			t.Fatalf("limit=%d page=%v more=%v err=%v", limit, page, more, err)
		}
	}
	page, more, err := workspace.ReplayThrough(t.Context(), 0, 512, 1)
	if err != nil || more || len(page) != 0 {
		t.Fatalf("foreign-only fence: page=%v more=%v err=%v", page, more, err)
	}
	page, more, err = workspace.ReplayThrough(t.Context(), 0, 515, 1)
	if err != nil || !more || len(page) != 1 || page[0].Sequence != 513 {
		t.Fatalf("full fence: page=%v more=%v err=%v", page, more, err)
	}
}
