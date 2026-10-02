package thread

import (
	"path/filepath"
	"testing"
	"time"

	sessionstate "github.com/fwtllh-png/QCode/internal/persist/session"
	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestCreateSeedReusesWorkspaceRoot(t *testing.T) {
	store, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repository := NewRepository(store.DB())
	for _, value := range []struct {
		workspace string
		session   string
		thread    string
	}{
		{"workspace-one", "session-one", "thread-one"},
		{"workspace-two", "session-two", "thread-two"},
	} {
		if _, err := repository.CreateSeed(
			t.Context(),
			sessionstate.Workspace{ID: value.workspace, RootPath: "/workspace"},
			sessionstate.Session{ID: value.session, WorkspaceID: value.workspace},
			Thread{ID: protocol.ThreadID(value.thread), SessionID: value.session},
		); err != nil {
			t.Fatal(err)
		}
	}
	var workspaceCount int
	if err := store.DB().QueryRowContext(
		t.Context(), `SELECT COUNT(*) FROM workspaces`,
	).Scan(&workspaceCount); err != nil {
		t.Fatal(err)
	}
	if workspaceCount != 1 {
		t.Fatalf("workspace count = %d, want 1", workspaceCount)
	}
	var distinctWorkspaceCount int
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT COUNT(DISTINCT workspace_id) FROM sessions`,
	).Scan(&distinctWorkspaceCount); err != nil {
		t.Fatal(err)
	}
	if distinctWorkspaceCount != 1 {
		t.Fatalf(
			"session workspace count = %d, want 1",
			distinctWorkspaceCount,
		)
	}
	if err := repository.Rename(t.Context(), "thread-two", "修复登录问题"); err != nil {
		t.Fatal(err)
	}
	renamed, err := repository.Get(t.Context(), "thread-two")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Title != "修复登录问题" {
		t.Fatalf("renamed title = %q", renamed.Title)
	}
}

func TestListIsBoundedAndWorkspaceScoped(t *testing.T) {
	store, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, statement := range []string{
		`INSERT INTO workspaces(id, root_path, created_at, updated_at)
		 VALUES ('workspace-a', '/workspace/a', ?, ?)`,
		`INSERT INTO workspaces(id, root_path, created_at, updated_at)
		 VALUES ('workspace-b', '/workspace/b', ?, ?)`,
		`INSERT INTO sessions(id, workspace_id, status, created_at, updated_at)
		 VALUES ('session-a', 'workspace-a', 'open', ?, ?)`,
		`INSERT INTO sessions(id, workspace_id, status, created_at, updated_at)
		 VALUES ('session-b', 'workspace-b', 'open', ?, ?)`,
		`INSERT INTO threads(id, session_id, title, status, created_at, updated_at)
		 VALUES ('thread-a', 'session-a', 'a', 'open', ?, ?)`,
		`INSERT INTO threads(id, session_id, title, status, created_at, updated_at)
		 VALUES ('thread-b', 'session-b', 'b', 'open', ?, ?)`,
	} {
		if _, err := store.DB().ExecContext(t.Context(), statement, now, now); err != nil {
			t.Fatal(err)
		}
	}
	repository := NewRepository(store.DB())
	values, err := repository.List(t.Context(), Filter{WorkspaceRoot: "/workspace/a"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].ID != "thread-a" {
		t.Fatalf("workspace-scoped threads = %+v", values)
	}
	if _, err := repository.GetInWorkspace(
		t.Context(), "thread-b", "/workspace/a",
	); err != ErrNotFound {
		t.Fatalf("foreign workspace get error = %v", err)
	}
	if _, err := repository.List(t.Context(), Filter{}, 1001); err == nil {
		t.Fatal("oversized thread list succeeded")
	}
}

func TestHistoryCursorBoundsNewestTurns(t *testing.T) {
	store, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	statements := []string{
		`INSERT INTO workspaces(id, root_path, created_at, updated_at)
		 VALUES ('workspace', '/workspace', ?, ?)`,
		`INSERT INTO sessions(id, workspace_id, status, created_at, updated_at)
		 VALUES ('session', 'workspace', 'open', ?, ?)`,
		`INSERT INTO threads(id, session_id, title, status, created_at, updated_at)
		 VALUES ('thread', 'session', 'chat', 'open', ?, ?)`,
		`INSERT INTO turns(id, thread_id, ordinal, status, created_at, updated_at)
		 VALUES ('turn-1', 'thread', 1, 'completed', ?, ?)`,
		`INSERT INTO turns(id, thread_id, ordinal, status, created_at, updated_at)
		 VALUES ('turn-2', 'thread', 2, 'completed', ?, ?)`,
	}
	for _, statement := range statements {
		if _, err := store.DB().ExecContext(t.Context(), statement, now, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []struct {
		sequence int
		eventID  string
		turnID   string
	}{
		{5, "event-5", "turn-1"},
		{10, "event-10", "turn-2"},
	} {
		if _, err := store.DB().ExecContext(t.Context(), `
			INSERT INTO event_index(
				sequence, event_id, thread_id, turn_id, kind,
				log_offset, log_length, sha256, created_at
			) VALUES (?, ?, 'thread', ?, 'turn.started', 0, 1, ?, ?)`,
			value.sequence, value.eventID, value.turnID,
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			now,
		); err != nil {
			t.Fatal(err)
		}
	}
	repository := NewRepository(store.DB())
	cursor, err := repository.HistoryCursor(t.Context(), "thread", 1)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 9 {
		t.Fatalf("HistoryCursor() = %d, want 9", cursor)
	}
}
