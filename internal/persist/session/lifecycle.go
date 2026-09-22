package session

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/sqlkit"
	"github.com/fwtllh-png/QCode/internal/persist/state/cas"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

var ErrLifecycleRevisionConflict = errors.New("session lifecycle revision conflict")

type LifecycleRevisionConflictError struct {
	Expected uint64
	Current  uint64
}

func (e *LifecycleRevisionConflictError) Error() string {
	return fmt.Sprintf(
		"session lifecycle revision conflict: expected %d, current %d",
		e.Expected,
		e.Current,
	)
}

func (e *LifecycleRevisionConflictError) Unwrap() error {
	return ErrLifecycleRevisionConflict
}

type LifecycleQuery = protocol.SessionListQuery

type lifecycleMetadata struct {
	Version        int                         `json:"version"`
	Revision       uint64                      `json:"revision"`
	Pinned         bool                        `json:"pinned"`
	ActiveThreadID protocol.ThreadID           `json:"active_thread_id,omitempty"`
	TitleSource    protocol.SessionTitleSource `json:"title_source,omitempty"`
	TitleRevision  uint64                      `json:"title_revision,omitempty"`
}

type profileRoute struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	Mode            string `json:"mode"`
	ExecutionTarget string `json:"execution_target"`
}

type lifecycleQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (r *Repository) CreateLifecycle(
	ctx context.Context,
	seed protocol.SessionCreateSeed,
) (protocol.SessionSummary, error) {
	if r.db == nil {
		return protocol.SessionSummary{}, errors.New("session repository database is required")
	}
	if err := seed.Validate(); err != nil {
		return protocol.SessionSummary{}, err
	}
	workspaceRoot, err := NormalizeWorkspaceRoot(seed.WorkspaceRoot)
	if err != nil {
		return protocol.SessionSummary{}, fmt.Errorf("resolve session workspace: %w", err)
	}
	seed.WorkspaceRoot = workspaceRoot
	now := time.Now().UTC()
	persistedIsolation := seed.Isolation
	if persistedIsolation == "shared" {
		persistedIsolation = ""
	}
	metadata, err := json.Marshal(map[string]any{
		"lifecycle": lifecycleMetadata{
			Version:        protocol.SessionLifecycleVersion,
			Revision:       1,
			ActiveThreadID: seed.ThreadID,
			TitleSource:    seed.TitleSource,
			TitleRevision:  1,
		},
		"provider":  seed.Provider,
		"model":     seed.Model,
		"isolation": persistedIsolation,
	})
	if err != nil {
		return protocol.SessionSummary{}, err
	}
	err = sqlkit.WithTx(ctx, r.db, nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workspaces(
				id, root_path, display_name, metadata_json, created_at, updated_at
			) VALUES (?, ?, ?, '{}', ?, ?)
			ON CONFLICT(root_path) DO NOTHING`,
			seed.WorkspaceID,
			seed.WorkspaceRoot,
			seed.WorkspaceLabel,
			sqlkit.Timestamp(now),
			sqlkit.Timestamp(now),
		); err != nil {
			return fmt.Errorf("create session workspace: %w", err)
		}
		var workspaceID string
		if err := tx.QueryRowContext(
			ctx,
			`SELECT id FROM workspaces WHERE root_path = ?`,
			seed.WorkspaceRoot,
		).Scan(&workspaceID); err != nil {
			return fmt.Errorf("resolve session workspace: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sessions(
				id, workspace_id, status, metadata_json, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?)`,
			seed.SessionID,
			workspaceID,
			StatusOpen,
			metadata,
			sqlkit.Timestamp(now),
			sqlkit.Timestamp(now),
		); err != nil {
			return fmt.Errorf("create session lifecycle: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO threads(
				id, session_id, parent_thread_id, title, status, created_at, updated_at
			) VALUES (?, ?, NULL, ?, 'open', ?, ?)`,
			seed.ThreadID,
			seed.SessionID,
			seed.Title,
			sqlkit.Timestamp(now),
			sqlkit.Timestamp(now),
		); err != nil {
			return fmt.Errorf("create session thread: %w", err)
		}
		return nil
	})
	if err != nil {
		return protocol.SessionSummary{}, err
	}
	return r.GetLifecycle(ctx, seed.SessionID)
}

func (r *Repository) ListLifecycle(
	ctx context.Context,
	filter LifecycleQuery,
) (protocol.SessionList, error) {
	if r.db == nil {
		return protocol.SessionList{}, errors.New("session repository database is required")
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 || len(filter.Query) > 256 ||
		strings.ContainsRune(filter.Query, '\x00') {
		return protocol.SessionList{}, errors.New("session lifecycle query is invalid")
	}
	sqlLimit := limit
	if filter.Status != "" {
		sqlLimit = 1000
	}
	if filter.WorkspaceRoot != "" {
		workspaceRoot, err := NormalizeWorkspaceRoot(filter.WorkspaceRoot)
		if err != nil {
			return protocol.SessionList{}, fmt.Errorf("resolve session workspace: %w", err)
		}
		filter.WorkspaceRoot = workspaceRoot
	}
	query := `
		SELECT s.id, s.status, s.metadata_json, s.created_at, s.updated_at,
		       w.root_path, w.display_name,
		       root.id, root.parent_thread_id, root.title, root.status, root.updated_at
		FROM sessions s
		JOIN workspaces w ON w.id = s.workspace_id
		JOIN threads root ON root.id = COALESCE(
			NULLIF(json_extract(
				s.metadata_json, '$.lifecycle.active_thread_id'
			), ''),
			(
				SELECT id FROM threads
				WHERE session_id = s.id AND parent_thread_id IS NULL
				ORDER BY created_at, id LIMIT 1
			)
		)
		WHERE 1 = 1`
	var arguments []any
	add := func(clause string, value any) {
		query += " AND " + clause
		arguments = append(arguments, value)
	}
	if filter.WorkspaceRoot != "" {
		add("w.root_path = ?", filter.WorkspaceRoot)
	}
	if !filter.IncludeArchived {
		add("s.status = ?", StatusOpen)
	}
	if filter.PinnedOnly {
		query += ` AND COALESCE(
			json_extract(s.metadata_json, '$.lifecycle.pinned'), 0
		) = 1`
	}
	needle := strings.TrimSpace(filter.Query)
	if needle != "" {
		query += ` AND (
			instr(lower(root.title), lower(?)) > 0 OR
			EXISTS (
				SELECT 1
				FROM items i
				JOIN turns tr ON tr.id = i.turn_id
				JOIN threads st ON st.id = tr.thread_id
				WHERE st.session_id = s.id
				  AND instr(lower(CAST(i.payload_json AS TEXT)), lower(?)) > 0
			)
		)`
		arguments = append(arguments, needle, needle)
	}
	query += ` ORDER BY
		COALESCE(json_extract(s.metadata_json, '$.lifecycle.pinned'), 0) DESC,
		COALESCE((
			SELECT MAX(tr.updated_at)
			FROM turns tr JOIN threads st ON st.id = tr.thread_id
			WHERE st.session_id = s.id
		), root.updated_at, s.updated_at) DESC,
		s.id
		LIMIT ?`
	arguments = append(arguments, sqlLimit)
	rows, err := r.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return protocol.SessionList{}, fmt.Errorf("list session lifecycle: %w", err)
	}
	defer rows.Close()
	var summaries []protocol.SessionSummary
	for rows.Next() {
		summary, err := scanLifecycle(rows)
		if err != nil {
			return protocol.SessionList{}, err
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return protocol.SessionList{}, err
	}
	if err := rows.Close(); err != nil {
		return protocol.SessionList{}, err
	}
	if err := r.projectLifecyclePage(ctx, summaries); err != nil {
		return protocol.SessionList{}, err
	}
	result := make([]protocol.SessionSummary, 0, len(summaries))
	for _, summary := range summaries {
		if filter.Status != "" && summary.Status != filter.Status {
			continue
		}
		result = append(result, summary)
		if len(result) == limit {
			break
		}
	}
	matches, err := r.matchLifecyclePage(ctx, result, needle)
	if err != nil {
		return protocol.SessionList{}, err
	}
	return protocol.SessionList{
		Version: protocol.SessionLifecycleVersion,
		Query:   needle, Sessions: result, Matches: matches,
	}, nil
}

func (r *Repository) GetLifecycle(
	ctx context.Context,
	sessionID string,
) (protocol.SessionSummary, error) {
	if r.db == nil {
		return protocol.SessionSummary{}, errors.New("session repository database is required")
	}
	return getLifecycle(ctx, r.db, sessionID)
}

func getLifecycle(
	ctx context.Context,
	queryer lifecycleQueryer,
	sessionID string,
) (protocol.SessionSummary, error) {
	summary, err := scanLifecycle(queryer.QueryRowContext(ctx, `
		SELECT s.id, s.status, s.metadata_json, s.created_at, s.updated_at,
		       w.root_path, w.display_name,
		       t.id, t.parent_thread_id, t.title, t.status, t.updated_at
		FROM sessions s
		JOIN workspaces w ON w.id = s.workspace_id
		JOIN threads t ON t.id = COALESCE(
			NULLIF(json_extract(
				s.metadata_json, '$.lifecycle.active_thread_id'
			), ''),
			(
				SELECT id FROM threads
				WHERE session_id = s.id AND parent_thread_id IS NULL
				ORDER BY created_at, id LIMIT 1
			)
		)
		WHERE s.id = ?`,
		sessionID,
	))
	if err != nil {
		return protocol.SessionSummary{}, err
	}
	if err := projectLatestTurn(ctx, queryer, &summary); err != nil {
		return protocol.SessionSummary{}, err
	}
	if err := projectUsage(ctx, queryer, &summary); err != nil {
		return protocol.SessionSummary{}, err
	}
	if err := summary.Validate(); err != nil {
		return protocol.SessionSummary{}, err
	}
	return summary, nil
}

func scanLifecycle(row interface{ Scan(...any) error }) (protocol.SessionSummary, error) {
	var (
		summary                                  protocol.SessionSummary
		sessionStatus, threadStatus              string
		metadata                                 []byte
		workspaceLabel                           string
		createdAt, sessionUpdated, threadUpdated string
		parent                                   sql.NullString
	)
	err := row.Scan(
		&summary.SessionID, &sessionStatus, &metadata, &createdAt, &sessionUpdated,
		&summary.WorkspaceRoot, &workspaceLabel,
		&summary.ThreadID, &parent, &summary.Title, &threadStatus, &threadUpdated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.SessionSummary{}, ErrNotFound
	}
	if err != nil {
		return protocol.SessionSummary{}, fmt.Errorf("get session lifecycle: %w", err)
	}
	summary.Version = protocol.SessionLifecycleVersion
	summary.Archived = sessionStatus == string(StatusClosed) ||
		threadStatus == "archived"
	if parent.Valid {
		summary.ParentThreadID = protocol.ThreadID(parent.String)
	}
	if summary.CreatedAt, err = parseTime(createdAt); err != nil {
		return protocol.SessionSummary{}, err
	}
	sessionTime, err := parseTime(sessionUpdated)
	if err != nil {
		return protocol.SessionSummary{}, err
	}
	threadTime, err := parseTime(threadUpdated)
	if err != nil {
		return protocol.SessionSummary{}, err
	}
	summary.UpdatedAt = laterTime(sessionTime, threadTime)
	meta, route, isolation, err := decodeLifecycleMetadata(metadata)
	if err != nil {
		return protocol.SessionSummary{}, err
	}
	summary.Revision = meta.Revision
	summary.TitleSource = meta.TitleSource
	summary.TitleRevision = meta.TitleRevision
	summary.Pinned = meta.Pinned
	summary.Provider = route.Provider
	summary.Model = route.Model
	summary.Mode = route.Mode
	summary.ExecutionTarget = route.ExecutionTarget
	if summary.ExecutionTarget == "" {
		summary.ExecutionTarget = "local"
	}
	summary.Isolation = isolation
	summary.WorkspaceLabel = strings.TrimSpace(workspaceLabel)
	if summary.WorkspaceLabel == "" {
		summary.WorkspaceLabel = filepath.Base(summary.WorkspaceRoot)
	}
	if summary.WorkspaceLabel == "" || summary.WorkspaceLabel == "." {
		summary.WorkspaceLabel = summary.WorkspaceRoot
	}
	return summary, nil
}

func (r *Repository) ThreadIDs(
	ctx context.Context,
	sessionID string,
) ([]protocol.ThreadID, error) {
	return threadIDs(ctx, r.db, sessionID)
}

func (r *Repository) TurnIDs(
	ctx context.Context,
	sessionID string,
) ([]protocol.TurnID, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("session repository database is required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT turn.id
		FROM turns AS turn
		JOIN threads AS thread ON thread.id = turn.thread_id
		WHERE thread.session_id = ?
		ORDER BY turn.created_at, turn.id`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []protocol.TurnID
	for rows.Next() {
		var id protocol.TurnID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func threadIDs(
	ctx context.Context,
	queryer lifecycleQueryer,
	sessionID string,
) ([]protocol.ThreadID, error) {
	rows, err := queryer.QueryContext(ctx, `
		SELECT id FROM threads WHERE session_id = ? ORDER BY created_at, id`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []protocol.ThreadID
	for rows.Next() {
		var id protocol.ThreadID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func (r *Repository) PresentationReadFence(
	ctx context.Context,
	sessionID string,
) (protocol.SessionReadFence, error) {
	if r.db == nil {
		return protocol.SessionReadFence{},
			errors.New("session repository database is required")
	}
	var fence protocol.SessionReadFence
	err := sqlkit.WithTx(
		ctx,
		r.db,
		&sql.TxOptions{ReadOnly: true},
		func(tx *sql.Tx) error {
			if err := tx.QueryRowContext(ctx, `
				SELECT sequence FROM event_watermark WHERE id = 1`,
			).Scan(&fence.ThroughSequence); err != nil {
				return fmt.Errorf("read presentation event watermark: %w", err)
			}
			summary, err := getLifecycle(ctx, tx, sessionID)
			if err != nil {
				return err
			}
			ids, err := threadIDs(ctx, tx, sessionID)
			if err != nil {
				return err
			}
			fence.Session = summary
			fence.ThreadIDs = ids
			return nil
		},
	)
	if err != nil {
		return protocol.SessionReadFence{}, err
	}
	return fence, nil
}

func (r *Repository) SessionForThread(
	ctx context.Context,
	threadID protocol.ThreadID,
) (string, error) {
	var sessionID string
	err := r.db.QueryRowContext(
		ctx,
		`SELECT session_id FROM threads WHERE id = ?`,
		threadID,
	).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return sessionID, err
}

func (r *Repository) ActivateThread(
	ctx context.Context,
	sessionID string,
	threadID protocol.ThreadID,
) (protocol.SessionSummary, error) {
	err := sqlkit.WithTx(ctx, r.db, nil, func(tx *sql.Tx) error {
		var metadata []byte
		err := tx.QueryRowContext(ctx, `
			SELECT s.metadata_json
			FROM sessions s
			JOIN threads t ON t.session_id = s.id
			WHERE s.id = ? AND t.id = ?`,
			sessionID, threadID,
		).Scan(&metadata)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		lifecycle, _, _, err := decodeLifecycleMetadata(metadata)
		if err != nil {
			return err
		}
		if lifecycle.ActiveThreadID == threadID {
			return nil
		}
		lifecycle.Version = protocol.SessionLifecycleVersion
		lifecycle.Revision++
		lifecycle.ActiveThreadID = threadID
		updated, err := metadataWithLifecycle(metadata, lifecycle)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE sessions SET metadata_json = ?, updated_at = ? WHERE id = ?`,
			updated, sqlkit.Timestamp(time.Now().UTC()), sessionID,
		)
		return err
	})
	if err != nil {
		return protocol.SessionSummary{}, err
	}
	return r.GetLifecycle(ctx, sessionID)
}

func (r *Repository) UpdateLifecycle(
	ctx context.Context,
	sessionID string,
	expectedRevision uint64,
	patch protocol.SessionLifecyclePatch,
) (protocol.SessionSummary, error) {
	if expectedRevision == 0 {
		return protocol.SessionSummary{}, errors.New("expected lifecycle revision is required")
	}
	if err := patch.Validate(); err != nil {
		return protocol.SessionSummary{}, err
	}
	err := sqlkit.WithTx(ctx, r.db, nil, func(tx *sql.Tx) error {
		var metadata []byte
		var threadID protocol.ThreadID
		var currentTitle, sessionStatus, threadStatus string
		err := tx.QueryRowContext(ctx, `
			SELECT s.metadata_json, s.status, t.id, t.title, t.status
			FROM sessions s
			JOIN threads t ON t.id = COALESCE(
				NULLIF(json_extract(
					s.metadata_json, '$.lifecycle.active_thread_id'
				), ''),
				(
					SELECT id FROM threads
					WHERE session_id = s.id AND parent_thread_id IS NULL
					ORDER BY created_at, id LIMIT 1
				)
			)
			WHERE s.id = ?`,
			sessionID,
		).Scan(&metadata, &sessionStatus, &threadID, &currentTitle, &threadStatus)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		lifecycle, _, _, err := decodeLifecycleMetadata(metadata)
		if err != nil {
			return err
		}
		if lifecycle.Revision != expectedRevision {
			return &LifecycleRevisionConflictError{
				Expected: expectedRevision,
				Current:  lifecycle.Revision,
			}
		}
		title := currentTitle
		claimTitle := patch.Title != nil && lifecycle.TitleSource != protocol.SessionTitleManual
		if patch.Title != nil {
			title = strings.TrimSpace(*patch.Title)
		}
		pinned := lifecycle.Pinned
		if patch.Pinned != nil {
			pinned = *patch.Pinned
		}
		archived := sessionStatus == string(StatusClosed) || threadStatus == "archived"
		if patch.Archived != nil {
			archived = *patch.Archived
		}
		if archived &&
			!(sessionStatus == string(StatusClosed) || threadStatus == "archived") {
			if err := requireNoActiveTurns(ctx, tx, sessionID, "archive"); err != nil {
				return err
			}
		}
		if !claimTitle && title == currentTitle && pinned == lifecycle.Pinned &&
			archived == (sessionStatus == string(StatusClosed) || threadStatus == "archived") {
			return nil
		}
		lifecycle.Version = protocol.SessionLifecycleVersion
		lifecycle.Revision++
		if patch.Title != nil {
			lifecycle.TitleSource = protocol.SessionTitleManual
			lifecycle.TitleRevision++
		}
		lifecycle.Pinned = pinned
		nextMetadata, err := metadataWithLifecycle(metadata, lifecycle)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		closedAt := any(nil)
		nextSessionStatus := StatusOpen
		nextThreadStatus := "open"
		if archived {
			nextSessionStatus = StatusClosed
			nextThreadStatus = "archived"
			closedAt = sqlkit.Timestamp(now)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET status = ?, metadata_json = ?, updated_at = ?, closed_at = ?
			WHERE id = ?`,
			nextSessionStatus, nextMetadata, sqlkit.Timestamp(now), closedAt, sessionID,
		); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE threads
			SET title = ?, status = ?, updated_at = ?
			WHERE id = ?`,
			title, nextThreadStatus, sqlkit.Timestamp(now), threadID,
		)
		return err
	})
	if err != nil {
		return protocol.SessionSummary{}, err
	}
	return r.GetLifecycle(ctx, sessionID)
}

func (r *Repository) DeleteLifecycle(
	ctx context.Context,
	sessionID string,
	expectedRevision uint64,
) (protocol.SessionDeleteResult, error) {
	return r.deleteLifecycle(ctx, sessionID, expectedRevision, false)
}

func (r *Repository) DiscardLifecycle(
	ctx context.Context,
	sessionID string,
	expectedRevision uint64,
) (protocol.SessionDeleteResult, error) {
	return r.deleteLifecycle(ctx, sessionID, expectedRevision, true)
}

func (r *Repository) deleteLifecycle(
	ctx context.Context,
	sessionID string,
	expectedRevision uint64,
	discard bool,
) (protocol.SessionDeleteResult, error) {
	if expectedRevision == 0 {
		return protocol.SessionDeleteResult{},
			errors.New("expected lifecycle revision is required")
	}
	var threadID protocol.ThreadID
	err := sqlkit.WithTx(ctx, r.db, nil, func(tx *sql.Tx) error {
		var (
			metadata      []byte
			workspaceRoot string
			status        string
		)
		err := tx.QueryRowContext(ctx, `
			SELECT s.metadata_json, s.status, w.root_path, t.id
			FROM sessions s
			JOIN workspaces w ON w.id = s.workspace_id
			JOIN threads t ON t.id = COALESCE(
				NULLIF(json_extract(
					s.metadata_json, '$.lifecycle.active_thread_id'
				), ''),
				(
					SELECT id FROM threads
					WHERE session_id = s.id AND parent_thread_id IS NULL
					ORDER BY created_at, id LIMIT 1
				)
			)
			WHERE s.id = ?`,
			sessionID,
		).Scan(&metadata, &status, &workspaceRoot, &threadID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		lifecycle, _, _, err := decodeLifecycleMetadata(metadata)
		if err != nil {
			return err
		}
		if lifecycle.Revision != expectedRevision {
			return &LifecycleRevisionConflictError{
				Expected: expectedRevision,
				Current:  lifecycle.Revision,
			}
		}
		if !discard {
			if err := requireNoActiveTurns(ctx, tx, sessionID, "delete"); err != nil {
				return err
			}
			var count int
			if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM sessions s
			JOIN workspaces w ON w.id = s.workspace_id
			WHERE w.root_path = ?`,
				workspaceRoot,
			).Scan(&count); err != nil {
				return err
			}
			if count <= 1 {
				return protocol.NewProblem(
					protocol.CodeConflict,
					"cannot delete the last session in a workspace",
					false,
					nil,
				)
			}
		}
		// These kernel tables have no lifecycle foreign keys. Remove their
		// rows before the session cascade removes the owning turns.
		for _, statement := range []string{
			`DELETE FROM turn_terminal_outbox WHERE turn_id IN (
				SELECT t.id FROM turns t JOIN threads th ON th.id = t.thread_id
				WHERE th.session_id = ?)`,
			`DELETE FROM turn_terminal_envelopes WHERE turn_id IN (
				SELECT t.id FROM turns t JOIN threads th ON th.id = t.thread_id
				WHERE th.session_id = ?)`,
			`DELETE FROM turn_domain_facts WHERE turn_id IN (
				SELECT t.id FROM turns t JOIN threads th ON th.id = t.thread_id
				WHERE th.session_id = ?)`,
		} {
			if _, err := tx.ExecContext(ctx, statement, sessionID); err != nil {
				return fmt.Errorf("delete session turn state: %w", err)
			}
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID)
		if err != nil {
			return fmt.Errorf("delete session: %w", err)
		}
		if err := sqlkit.RequireAffected(result, 1); err != nil {
			return ErrLifecycleRevisionConflict
		}
		_, err = cas.CollectTx(ctx, tx)
		return err
	})
	if err != nil {
		return protocol.SessionDeleteResult{}, err
	}
	return protocol.SessionDeleteResult{
		Version:   protocol.SessionLifecycleVersion,
		SessionID: sessionID,
		ThreadID:  threadID,
		DeletedAt: time.Now().UTC(),
	}, nil
}

func requireNoActiveTurns(
	ctx context.Context,
	tx *sql.Tx,
	sessionID, action string,
) error {
	var count int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM turns tr
		JOIN threads th ON th.id = tr.thread_id
		WHERE th.session_id = ? AND tr.status = 'active'`,
		sessionID,
	).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return protocol.NewProblem(
			protocol.CodeConflict,
			fmt.Sprintf("cannot %s session with an active turn", action),
			true,
			nil,
		)
	}
	return nil
}

func decodeLifecycleMetadata(
	metadata []byte,
) (lifecycleMetadata, profileRoute, string, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &values); err != nil {
		return lifecycleMetadata{}, profileRoute{}, "", fmt.Errorf(
			"decode durable session metadata: %w",
			err,
		)
	}
	lifecycle := lifecycleMetadata{
		Version:       protocol.SessionLifecycleVersion,
		Revision:      1,
		TitleSource:   protocol.SessionTitleManual,
		TitleRevision: 1,
	}
	if raw := values["lifecycle"]; len(raw) != 0 {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&lifecycle); err != nil {
			return lifecycleMetadata{}, profileRoute{}, "", fmt.Errorf(
				"decode session lifecycle metadata: %w",
				err,
			)
		}
		if lifecycle.Version != protocol.SessionLifecycleVersion ||
			lifecycle.Revision == 0 {
			return lifecycleMetadata{}, profileRoute{}, "",
				errors.New("session lifecycle metadata is invalid")
		}
		if lifecycle.ActiveThreadID != "" &&
			len(lifecycle.ActiveThreadID) > 256 {
			return lifecycleMetadata{}, profileRoute{}, "",
				errors.New("session lifecycle active Thread identity is invalid")
		}
	}
	var route profileRoute
	if raw := values["profile"]; len(raw) != 0 {
		_ = json.Unmarshal(raw, &route)
	}
	if route.Provider == "" {
		_ = json.Unmarshal(values["provider"], &route.Provider)
	}
	if route.Model == "" {
		_ = json.Unmarshal(values["model"], &route.Model)
	}
	var isolation string
	_ = json.Unmarshal(values["isolation"], &isolation)
	if isolation == "" {
		isolation = "shared"
	}
	if isolation != "shared" && isolation != "worktree" {
		return lifecycleMetadata{}, profileRoute{}, "",
			fmt.Errorf("unsupported durable session isolation %q", isolation)
	}
	return lifecycle, route, isolation, nil
}

func metadataWithLifecycle(
	metadata []byte,
	lifecycle lifecycleMetadata,
) ([]byte, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &values); err != nil {
		return nil, err
	}
	if values == nil {
		values = make(map[string]json.RawMessage)
	}
	encoded, err := json.Marshal(lifecycle)
	if err != nil {
		return nil, err
	}
	values["lifecycle"] = encoded
	return json.Marshal(values)
}

func projectLatestTurn(
	ctx context.Context,
	queryer lifecycleQueryer,
	summary *protocol.SessionSummary,
) error {
	var turnID, status, updatedAt string
	err := queryer.QueryRowContext(ctx, `
		SELECT tr.id, tr.status, tr.updated_at
		FROM turns tr
		JOIN threads t ON t.id = tr.thread_id
		WHERE t.session_id = ?
		ORDER BY tr.updated_at DESC, tr.ordinal DESC
		LIMIT 1`,
		summary.SessionID,
	).Scan(&turnID, &status, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		summary.Status = protocol.SessionStatusIdle
		return nil
	}
	if err != nil {
		return err
	}
	if err := applyLatestTurn(summary, turnID, status, updatedAt); err != nil {
		return err
	}
	var sequence uint64
	if err := queryer.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(e.sequence), 0)
		FROM event_index e
		JOIN threads t ON t.id = e.thread_id
		WHERE t.session_id = ?`,
		summary.SessionID,
	).Scan(&sequence); err != nil {
		return err
	}
	summary.LatestSequence = protocol.Cursor(sequence)
	return nil
}

func applyLatestTurn(summary *protocol.SessionSummary, turnID, status, updatedAt string) error {
	summary.LatestTurnID = protocol.TurnID(turnID)
	summary.Status = protocol.SessionStatusIdle
	if turnID == "" {
		return nil
	}
	turnTime, err := parseTime(updatedAt)
	if err != nil {
		return err
	}
	summary.UpdatedAt = laterTime(summary.UpdatedAt, turnTime)
	switch status {
	case "active":
		summary.Status = protocol.SessionStatusRunning
	case "completed", "reverted":
		summary.Status = protocol.SessionStatusCompleted
	case "blocked":
		summary.Status = protocol.SessionStatusBlocked
	case "failed":
		summary.Status = protocol.SessionStatusFailed
	case "canceled":
		summary.Status = protocol.SessionStatusInterrupted
	default:
		summary.Status = protocol.SessionStatusIdle
	}
	return nil
}

func projectUsage(
	ctx context.Context,
	queryer lifecycleQueryer,
	summary *protocol.SessionSummary,
) error {
	var unpriced, calls uint64
	err := queryer.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(input_tokens + output_tokens + reasoning_tokens), 0),
			COALESCE(SUM(CASE WHEN cost_known THEN cost_microunits ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN cost_known THEN 0 ELSE 1 END), 0),
			COUNT(*)
		FROM usage WHERE session_id = ?`,
		summary.SessionID,
	).Scan(&summary.TotalTokens, &summary.CostMicrounits, &unpriced, &calls)
	if err != nil {
		return err
	}
	summary.CostKnown = calls > 0 && unpriced == 0
	return nil
}

func laterTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}
