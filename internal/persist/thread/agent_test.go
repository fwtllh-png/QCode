package thread

import (
	"path/filepath"
	"testing"

	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
)

func TestIsAgentThreadKeepsTerminalDelegationAndSessionBoundary(t *testing.T) {
	db, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.DB().ExecContext(t.Context(), `INSERT INTO agent_nodes (
		workspace_root, session_id, agent_id, path, thread_id, status, revision, role,
		operation_id, actor, event_id, source_sequence, updated_at
	) VALUES ('/workspace', 'session', 'child', '/root/child', 'thread', 'completed', 1,
		'worker', 'operation', 'runtime', 'event', 1, '2026-10-10T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(db.DB())
	for _, session := range []string{"session", "other-session"} {
		got, err := repository.IsAgentThread(t.Context(), session, "thread")
		if err != nil || got != (session == "session") {
			t.Fatalf("session=%s delegated=%v err=%v", session, got, err)
		}
	}
	if got, err := repository.IsAgentThread(t.Context(), "session", "fork"); err != nil || got {
		t.Fatalf("ordinary fork marked delegated: %v %v", got, err)
	}
}
