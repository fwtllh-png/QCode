package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/common/sqlkit"
)

// EnsureSeed creates the workspace and session rows required by background
// orchestration before a Runtime has accepted a turn.
func (r *Repository) EnsureSeed(
	ctx context.Context,
	sessionID string,
	workspaceRoot string,
) error {
	if r == nil || r.db == nil {
		return errors.New("session repository database is required")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("session id is required")
	}
	root, err := NormalizeWorkspaceRoot(workspaceRoot)
	if err != nil {
		return fmt.Errorf("resolve workspace root: %w", err)
	}
	now := sqlkit.Timestamp(time.Now().UTC())
	return sqlkit.WithTx(ctx, r.db, nil, func(tx *sql.Tx) error {
		workspaceID := "workspace-" + sessionID
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workspaces(
				id, root_path, display_name, created_at, updated_at
			) VALUES (?, ?, 'qcode', ?, ?)
			ON CONFLICT(root_path) DO NOTHING`,
			workspaceID, root, now, now,
		); err != nil {
			return fmt.Errorf("ensure workspace: %w", err)
		}
		if err := tx.QueryRowContext(
			ctx,
			`SELECT id FROM workspaces WHERE root_path = ?`,
			root,
		).Scan(&workspaceID); err != nil {
			return fmt.Errorf("resolve workspace: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sessions(id, workspace_id, status, created_at, updated_at)
			VALUES (?, ?, 'open', ?, ?)
			ON CONFLICT(id) DO NOTHING`,
			sessionID, workspaceID, now, now,
		); err != nil {
			return fmt.Errorf("ensure session: %w", err)
		}
		var persistedWorkspaceID string
		if err := tx.QueryRowContext(
			ctx,
			`SELECT workspace_id FROM sessions WHERE id = ?`,
			sessionID,
		).Scan(&persistedWorkspaceID); err != nil {
			return fmt.Errorf("resolve session: %w", err)
		}
		if persistedWorkspaceID != workspaceID {
			return errors.New("session belongs to a different workspace")
		}
		return nil
	})
}

// NormalizeWorkspaceRoot returns the physical workspace identity persisted in
// workspaces.root_path. Missing paths retain their cleaned absolute identity.
func NormalizeWorkspaceRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "."
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(absolute)
	resolved, err := filepath.EvalSymlinks(clean)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return clean, nil
	}
	return "", err
}
