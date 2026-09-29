// Package thread provides transport-independent persistence for threads,
// turns, items, and accepted runtime operations.
package thread

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	sessionstate "github.com/fwtllh-png/QCode/internal/persist/session"
	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

var (
	ErrNotFound          = errors.New("thread record not found")
	ErrActiveTurn        = app.ErrActiveTurn
	ErrTerminal          = errors.New("turn already has a terminal state")
	ErrOperationConflict = errors.New("operation identity was reused with a different payload")
)

type ThreadStatus string
type TurnStatus string
type OperationStatus string

const (
	ThreadOpen     ThreadStatus = "open"
	ThreadArchived ThreadStatus = "archived"

	TurnActive    TurnStatus = "active"
	TurnCompleted TurnStatus = "completed"
	TurnBlocked   TurnStatus = "blocked"
	TurnFailed    TurnStatus = "failed"
	TurnCanceled  TurnStatus = "canceled"
	TurnReverted  TurnStatus = "reverted"

	OperationAccepted  OperationStatus = "accepted"
	OperationCommitted OperationStatus = "committed"
)

type Thread struct {
	ID             protocol.ThreadID
	SessionID      string
	ParentThreadID protocol.ThreadID
	Title          string
	Status         ThreadStatus
	LatestCursor   protocol.Cursor
	SourceCursor   protocol.Cursor
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Turn struct {
	ID          protocol.TurnID
	ThreadID    protocol.ThreadID
	OperationID protocol.OperationID
	Ordinal     uint64
	Status      TurnStatus
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

type Item struct {
	ID        protocol.ItemID
	TurnID    protocol.TurnID
	Ordinal   uint64
	Kind      string
	Payload   json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

type OperationRecord struct {
	ID             protocol.OperationID
	SessionID      string
	IdempotencyKey string
	Kind           protocol.OperationKind
	Status         OperationStatus
	Request        json.RawMessage
	Response       json.RawMessage
	Error          json.RawMessage
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Filter struct {
	SessionID     string
	WorkspaceRoot string
	Status        ThreadStatus
}

type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

func NewSQLiteRepository(store *sqlitestate.Store) *Repository {
	if store == nil {
		return &Repository{}
	}
	return NewRepository(store.DB())
}

func (r *Repository) Create(ctx context.Context, value Thread) (Thread, error) {
	if r.db == nil {
		return Thread{}, errors.New("thread repository database is required")
	}
	if value.ID == "" || value.SessionID == "" {
		return Thread{}, errors.New("thread id and session id are required")
	}
	if value.Status == "" {
		value.Status = ThreadOpen
	}
	now := time.Now().UTC()
	if value.CreatedAt.IsZero() {
		value.CreatedAt = now
	}
	if value.UpdatedAt.IsZero() {
		value.UpdatedAt = value.CreatedAt
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO threads(
			id, session_id, parent_thread_id, title, status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		value.ID, value.SessionID, nullString(value.ParentThreadID), value.Title,
		value.Status, timestamp(value.CreatedAt), timestamp(value.UpdatedAt),
	)
	if err != nil {
		return Thread{}, fmt.Errorf("create thread: %w", err)
	}
	return value, nil
}

func (r *Repository) Rename(
	ctx context.Context,
	id protocol.ThreadID,
	title string,
) error {
	if r.db == nil {
		return errors.New("thread repository database is required")
	}
	if id == "" || title == "" {
		return errors.New("thread id and title are required")
	}
	result, err := r.db.ExecContext(ctx, `
		UPDATE threads SET title = ?, updated_at = ? WHERE id = ?`,
		title, timestamp(time.Now().UTC()), id,
	)
	if err != nil {
		return fmt.Errorf("rename thread: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect renamed thread: %w", err)
	}
	if updated != 1 {
		return ErrNotFound
	}
	return nil
}

// HistoryCursor returns the cursor immediately before the earliest durable
// event belonging to the newest limit turns of a thread. Hosts use it to
// hydrate a bounded UI projection without changing the client's live cursor.
func (r *Repository) HistoryCursor(
	ctx context.Context,
	threadID protocol.ThreadID,
	limit int,
) (protocol.Cursor, error) {
	if r.db == nil {
		return 0, errors.New("thread repository database is required")
	}
	if threadID == "" || limit <= 0 || limit > 1000 {
		return 0, errors.New("thread id and history limit between 1 and 1000 are required")
	}
	var first sql.NullInt64
	err := r.db.QueryRowContext(ctx, `
		SELECT MIN(sequence)
		FROM event_index
		WHERE turn_id IN (
			SELECT id FROM turns
			WHERE thread_id = ?
			ORDER BY ordinal DESC
			LIMIT ?
		)`, threadID, limit,
	).Scan(&first)
	if err != nil {
		return 0, fmt.Errorf("query thread history cursor: %w", err)
	}
	if !first.Valid || first.Int64 <= 1 {
		return 0, nil
	}
	return protocol.Cursor(first.Int64 - 1), nil
}

// CreateSeed atomically creates the workspace, session, and initial thread
// needed by transport-level thread administration.
func (r *Repository) CreateSeed(
	ctx context.Context,
	workspace sessionstate.Workspace,
	session sessionstate.Session,
	value Thread,
) (Thread, error) {
	if r.db == nil {
		return Thread{}, errors.New("thread repository database is required")
	}
	if workspace.ID == "" || workspace.RootPath == "" ||
		session.ID == "" || session.WorkspaceID != workspace.ID ||
		value.ID == "" || value.SessionID != session.ID {
		return Thread{}, errors.New("workspace, session, and thread seed identities are inconsistent")
	}
	if value.Status == "" {
		value.Status = ThreadOpen
	}
	if session.Status == "" {
		session.Status = sessionstate.StatusOpen
	}
	workspaceMetadata, err := normalizeObject(workspace.Metadata)
	if err != nil {
		return Thread{}, fmt.Errorf("workspace metadata: %w", err)
	}
	sessionMetadata, err := normalizeObject(session.Metadata)
	if err != nil {
		return Thread{}, fmt.Errorf("session metadata: %w", err)
	}
	now := time.Now().UTC()
	if workspace.CreatedAt.IsZero() {
		workspace.CreatedAt = now
	}
	if workspace.UpdatedAt.IsZero() {
		workspace.UpdatedAt = workspace.CreatedAt
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = session.CreatedAt
	}
	if value.CreatedAt.IsZero() {
		value.CreatedAt = now
	}
	if value.UpdatedAt.IsZero() {
		value.UpdatedAt = value.CreatedAt
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Thread{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO workspaces(id, root_path, display_name, metadata_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(root_path) DO NOTHING`,
		workspace.ID, workspace.RootPath, workspace.DisplayName, workspaceMetadata,
		timestamp(workspace.CreatedAt), timestamp(workspace.UpdatedAt))
	if err != nil {
		return Thread{}, fmt.Errorf("create seed workspace: %w", err)
	}
	created, err := result.RowsAffected()
	if err != nil {
		return Thread{}, fmt.Errorf("inspect seed workspace: %w", err)
	}
	if created == 0 {
		if err := tx.QueryRowContext(ctx, `
			SELECT id FROM workspaces WHERE root_path = ?`,
			workspace.RootPath,
		).Scan(&workspace.ID); err != nil {
			return Thread{}, fmt.Errorf("reuse seed workspace: %w", err)
		}
	}
	session.WorkspaceID = workspace.ID
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions(
			id, workspace_id, status, metadata_json, created_at, updated_at, closed_at
		) VALUES (?, ?, ?, ?, ?, ?, NULL)`,
		session.ID, session.WorkspaceID, session.Status, sessionMetadata,
		timestamp(session.CreatedAt), timestamp(session.UpdatedAt),
	); err != nil {
		return Thread{}, fmt.Errorf("create seed session: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO threads(
			id, session_id, parent_thread_id, title, status, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		value.ID, value.SessionID, nullString(value.ParentThreadID), value.Title,
		value.Status, timestamp(value.CreatedAt), timestamp(value.UpdatedAt),
	); err != nil {
		return Thread{}, fmt.Errorf("create seed thread: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Thread{}, err
	}
	return value, nil
}

func (r *Repository) Get(ctx context.Context, id protocol.ThreadID) (Thread, error) {
	var value Thread
	var parent sql.NullString
	var createdAt, updatedAt string
	var cursor uint64
	var sourceCursor int64
	err := r.db.QueryRowContext(ctx, `
		SELECT t.id, t.session_id, t.parent_thread_id, t.title, t.status,
		       t.source_cursor, t.created_at, t.updated_at, COALESCE(MAX(e.sequence), 0)
		FROM threads t
		LEFT JOIN event_index e ON e.thread_id = t.id
		WHERE t.id = ?
		GROUP BY t.id`, id,
	).Scan(
		&value.ID, &value.SessionID, &parent, &value.Title, &value.Status,
		&sourceCursor, &createdAt, &updatedAt, &cursor,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Thread{}, ErrNotFound
	}
	if err != nil {
		return Thread{}, fmt.Errorf("get thread: %w", err)
	}
	if parent.Valid {
		value.ParentThreadID = protocol.ThreadID(parent.String)
	}
	value.LatestCursor = protocol.Cursor(cursor)
	value.SourceCursor = protocol.Cursor(sourceCursor)
	if value.CreatedAt, err = parseTime(createdAt); err != nil {
		return Thread{}, err
	}
	if value.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Thread{}, err
	}
	return value, nil
}

func (r *Repository) GetInWorkspace(
	ctx context.Context,
	id protocol.ThreadID,
	workspaceRoot string,
) (Thread, error) {
	if workspaceRoot == "" {
		return r.Get(ctx, id)
	}
	normalized, err := sessionstate.NormalizeWorkspaceRoot(workspaceRoot)
	if err != nil {
		return Thread{}, fmt.Errorf("resolve thread Workspace: %w", err)
	}
	workspaceRoot = normalized
	var found int
	err = r.db.QueryRowContext(ctx, `
		SELECT 1
		FROM threads t
		JOIN sessions s ON s.id = t.session_id
		JOIN workspaces w ON w.id = s.workspace_id
		WHERE t.id = ? AND w.root_path = ?`,
		id, workspaceRoot,
	).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return Thread{}, ErrNotFound
	}
	if err != nil {
		return Thread{}, fmt.Errorf("get thread workspace: %w", err)
	}
	return r.Get(ctx, id)
}

// List returns threads newest-first, optionally constrained to one session or
// canonical workspace root. Hosts must pass a limit so an old state directory
// cannot become an unbounded response.
func (r *Repository) List(
	ctx context.Context,
	filter Filter,
	limit int,
) ([]Thread, error) {
	if r.db == nil {
		return nil, errors.New("thread repository database is required")
	}
	if limit <= 0 || limit > 1000 {
		return nil, errors.New("thread list limit must be between 1 and 1000")
	}
	if filter.WorkspaceRoot != "" {
		workspaceRoot, err := sessionstate.NormalizeWorkspaceRoot(filter.WorkspaceRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve thread Workspace: %w", err)
		}
		filter.WorkspaceRoot = workspaceRoot
	}
	query := `
		SELECT t.id
		FROM threads t
		JOIN sessions s ON s.id = t.session_id
		JOIN workspaces w ON w.id = s.workspace_id
		WHERE 1 = 1`
	var arguments []any
	add := func(clause string, value any) {
		query += " AND " + clause
		arguments = append(arguments, value)
	}
	if filter.SessionID != "" {
		add("t.session_id = ?", filter.SessionID)
	}
	if filter.WorkspaceRoot != "" {
		add("w.root_path = ?", filter.WorkspaceRoot)
	}
	if filter.Status != "" {
		add("t.status = ?", filter.Status)
	}
	query += " ORDER BY t.updated_at DESC, t.id LIMIT ?"
	arguments = append(arguments, limit)
	rows, err := r.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	defer rows.Close()
	var ids []protocol.ThreadID
	for rows.Next() {
		var id protocol.ThreadID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	values := make([]Thread, 0, len(ids))
	for _, id := range ids {
		value, err := r.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func (r *Repository) GetTurn(ctx context.Context, id protocol.TurnID) (Turn, error) {
	var value Turn
	var operationID sql.NullString
	var createdAt, updatedAt string
	var completedAt sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT id, thread_id, operation_id, ordinal, status, created_at, updated_at, completed_at
		FROM turns WHERE id = ?`, id,
	).Scan(
		&value.ID, &value.ThreadID, &operationID, &value.Ordinal, &value.Status,
		&createdAt, &updatedAt, &completedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("get turn: %w", err)
	}
	if operationID.Valid {
		value.OperationID = protocol.OperationID(operationID.String)
	}
	if value.CreatedAt, err = parseTime(createdAt); err != nil {
		return Turn{}, err
	}
	if value.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Turn{}, err
	}
	if completedAt.Valid {
		parsed, parseErr := parseTime(completedAt.String)
		if parseErr != nil {
			return Turn{}, parseErr
		}
		value.CompletedAt = &parsed
	}
	return value, nil
}

func (r *Repository) ListTurns(ctx context.Context, threadID protocol.ThreadID) ([]Turn, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id FROM turns WHERE thread_id = ? ORDER BY ordinal`, threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("list turns: %w", err)
	}
	defer rows.Close()
	var ids []protocol.TurnID
	for rows.Next() {
		var id protocol.TurnID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]Turn, 0, len(ids))
	for _, id := range ids {
		value, err := r.GetTurn(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}

func (r *Repository) GetItem(ctx context.Context, id protocol.ItemID) (Item, error) {
	var value Item
	var payload, createdAt, updatedAt string
	err := r.db.QueryRowContext(ctx, `
		SELECT id, turn_id, ordinal, kind, payload_json, created_at, updated_at
		FROM items WHERE id = ?`, id,
	).Scan(
		&value.ID, &value.TurnID, &value.Ordinal, &value.Kind,
		&payload, &createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("get item: %w", err)
	}
	value.Payload = json.RawMessage(payload)
	if value.CreatedAt, err = parseTime(createdAt); err != nil {
		return Item{}, err
	}
	if value.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Item{}, err
	}
	return value, nil
}

func timestamp(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	result, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse persisted timestamp: %w", err)
	}
	return result, nil
}

func nullString[T ~string](value T) any {
	if value == "" {
		return nil
	}
	return string(value)
}

func normalizeObject(value json.RawMessage) (json.RawMessage, error) {
	if len(value) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}
