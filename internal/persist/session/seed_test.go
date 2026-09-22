package session_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/persist/session"
	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
)

func TestEnsureSeedUsesPhysicalWorkspaceIdentity(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "workspace-link")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestate.Open(
		t.Context(),
		filepath.Join(t.TempDir(), "state.db"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repository := session.NewSQLiteRepository(store)
	if err := repository.EnsureSeed(t.Context(), "session-a", root); err != nil {
		t.Fatal(err)
	}
	if err := repository.EnsureSeed(t.Context(), "session-b", alias); err != nil {
		t.Fatal(err)
	}
	var workspaces, sessionWorkspaces int
	if err := store.DB().QueryRowContext(
		t.Context(),
		`SELECT COUNT(*) FROM workspaces`,
	).Scan(&workspaces); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(
		t.Context(),
		`SELECT COUNT(DISTINCT workspace_id) FROM sessions`,
	).Scan(&sessionWorkspaces); err != nil {
		t.Fatal(err)
	}
	if workspaces != 1 || sessionWorkspaces != 1 {
		t.Fatalf(
			"workspaces=%d session_workspaces=%d, want 1/1",
			workspaces,
			sessionWorkspaces,
		)
	}
}
