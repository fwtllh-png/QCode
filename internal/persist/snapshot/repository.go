// Package snapshot persists versioned checkpoints in SQLite with content in CAS.
package snapshot

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/sqlkit"
	"github.com/fwtllh-png/QCode/internal/persist/state/cas"
	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const SchemaVersion = 1

var (
	ErrNotFound          = errors.New("snapshot not found")
	ErrIntegrity         = errors.New("snapshot integrity check failed")
	ErrUnsupportedSchema = errors.New("unsupported snapshot schema")
)

type IntegrityError struct {
	ID       string
	Expected string
	Actual   string
	Err      error
}

func (e *IntegrityError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("snapshot %q integrity check failed: %v", e.ID, e.Err)
	}
	return fmt.Sprintf(
		"snapshot %q content hash mismatch: expected %s, got %s",
		e.ID, e.Expected, e.Actual,
	)
}

func (e *IntegrityError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrIntegrity}
	}
	return []error{ErrIntegrity, e.Err}
}

type SchemaError struct {
	ID        string
	Found     int
	Supported int
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf(
		"snapshot %q schema version %d is unsupported; supported version is %d",
		e.ID, e.Found, e.Supported,
	)
}

func (e *SchemaError) Unwrap() error { return ErrUnsupportedSchema }

type Snapshot struct {
	ID            string
	ThreadID      protocol.ThreadID
	TurnID        protocol.TurnID
	Cursor        protocol.Cursor
	Kind          string
	SchemaVersion int
	ContentHash   string
	Content       []byte
	// ContentIDs are children already staged by the caller (e.g. a baseline manifest).
	ContentIDs []string
	Metadata   json.RawMessage
	CreatedAt  time.Time
}

type Repository struct {
	db      *sql.DB
	content *cas.Store
}

func NewRepository(db *sql.DB, content *cas.Store) *Repository {
	return &Repository{db: db, content: content}
}

func NewSQLiteRepository(store *sqlitestate.Store, content *cas.Store) *Repository {
	if store == nil {
		return &Repository{content: content}
	}
	return NewRepository(store.DB(), content)
}

func (r *Repository) Save(ctx context.Context, value Snapshot) (result Snapshot, resultErr error) {
	if r.db == nil || r.content == nil {
		return Snapshot{}, errors.New("snapshot database and content store are required")
	}
	if value.ID == "" || value.ThreadID == "" || value.Kind == "" {
		return Snapshot{}, errors.New("snapshot id, thread id, and kind are required")
	}
	ctx, finishStage := r.content.BeginContentStage(ctx)
	defer func() { resultErr = errors.Join(resultErr, finishStage()) }()
	if value.SchemaVersion == 0 {
		value.SchemaVersion = SchemaVersion
	}
	if value.SchemaVersion != SchemaVersion {
		return Snapshot{}, &SchemaError{
			ID: value.ID, Found: value.SchemaVersion, Supported: SchemaVersion,
		}
	}
	metadata, err := sqlkit.CanonicalObject(value.Metadata)
	if err != nil {
		return Snapshot{}, fmt.Errorf("snapshot metadata: %w", err)
	}
	value.Content = append([]byte(nil), value.Content...)
	value.ContentHash = hash(value.Content)
	if value.CreatedAt.IsZero() {
		value.CreatedAt = time.Now().UTC()
	}
	if err := r.content.Put(ctx, value.ContentHash, value.Content); err != nil {
		return Snapshot{}, fmt.Errorf("store snapshot content: %w", err)
	}
	inserted := false
	defer func() {
		if !inserted && !r.content.ManagedOwnership() {
			_ = r.content.ReleaseUnreferenced(context.Background(), value.ContentHash)
		}
	}()
	err = sqlkit.WithTx(ctx, r.db, nil, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
		INSERT INTO snapshots(
			id, thread_id, turn_id, cursor, kind, content_hash,
			schema_version, metadata_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			value.ID, value.ThreadID, nullableTurn(value.TurnID), value.Cursor, value.Kind,
			value.ContentHash, value.SchemaVersion, metadata, sqlkit.Timestamp(value.CreatedAt),
		)
		if err != nil || !r.content.ManagedOwnership() {
			return err
		}
		if err := cas.LinkTx(ctx, tx, value.ContentHash, value.ContentIDs...); err != nil {
			return err
		}
		return cas.BindTx(ctx, tx, "snapshot", value.ID, value.ContentHash)
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("persist snapshot: %w", err)
	}
	inserted = true
	value.Metadata = metadata
	return value, nil
}

func (r *Repository) Get(ctx context.Context, id string) (Snapshot, error) {
	if r.db == nil || r.content == nil {
		return Snapshot{}, errors.New("snapshot database and content store are required")
	}
	return r.read(ctx, `
		SELECT id, thread_id, turn_id, cursor, kind, content_hash,
			schema_version, metadata_json, created_at
		FROM snapshots WHERE id = ?`, id)
}

func (r *Repository) Latest(
	ctx context.Context,
	threadID protocol.ThreadID,
	kind string,
) (Snapshot, error) {
	if r.db == nil || r.content == nil {
		return Snapshot{}, errors.New("snapshot database and content store are required")
	}
	if threadID == "" || kind == "" {
		return Snapshot{}, errors.New("snapshot thread id and kind are required")
	}
	return r.read(ctx, `
		SELECT id, thread_id, turn_id, cursor, kind, content_hash,
			schema_version, metadata_json, created_at
		FROM snapshots WHERE thread_id = ? AND kind = ?
		ORDER BY cursor DESC, created_at DESC LIMIT 1`, threadID, kind)
}

// Recover returns the latest verified checkpoint. Corruption and unsupported
// schemas are returned explicitly and are never treated as a missing snapshot.
func (r *Repository) Recover(
	ctx context.Context,
	threadID protocol.ThreadID,
	kind string,
) (Snapshot, error) {
	return r.Latest(ctx, threadID, kind)
}

func (r *Repository) read(ctx context.Context, query string, arguments ...any) (Snapshot, error) {
	var value Snapshot
	var turnID sql.NullString
	var metadata, createdAt string
	err := r.db.QueryRowContext(ctx, query, arguments...).Scan(
		&value.ID, &value.ThreadID, &turnID, &value.Cursor, &value.Kind,
		&value.ContentHash, &value.SchemaVersion, &metadata, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("read snapshot: %w", err)
	}
	value.TurnID = protocol.TurnID(turnID.String)
	value.Metadata, err = sqlkit.CanonicalObject(json.RawMessage(metadata))
	if err != nil {
		return Snapshot{}, &IntegrityError{
			ID: value.ID, Err: fmt.Errorf("decode persisted snapshot metadata: %w", err),
		}
	}
	value.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return Snapshot{}, &IntegrityError{ID: value.ID, Err: err}
	}
	if value.SchemaVersion != SchemaVersion {
		return Snapshot{}, &SchemaError{
			ID: value.ID, Found: value.SchemaVersion, Supported: SchemaVersion,
		}
	}
	content, err := r.content.Get(ctx, value.ContentHash)
	if err != nil {
		return Snapshot{}, &IntegrityError{ID: value.ID, Expected: value.ContentHash, Err: err}
	}
	actual := hash(content)
	if actual != value.ContentHash {
		return Snapshot{}, &IntegrityError{
			ID: value.ID, Expected: value.ContentHash, Actual: actual,
		}
	}
	value.Content = content
	return value, nil
}

func hash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func nullableTurn(value protocol.TurnID) any {
	if value == "" {
		return nil
	}
	return value
}
