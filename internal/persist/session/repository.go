// Package session provides typed access to durable workspaces and sessions.
package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/sqlkit"
	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
)

var (
	ErrNotFound = errors.New("session record not found")
)

type Status string

const (
	StatusOpen   Status = "open"
	StatusClosed Status = "closed"
)

type Workspace struct {
	ID          string
	RootPath    string
	DisplayName string
	Metadata    json.RawMessage
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Session struct {
	ID          string
	WorkspaceID string
	Status      Status
	Metadata    json.RawMessage
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ClosedAt    *time.Time
}

type Repository struct {
	db          *sql.DB
	maintenance func(context.Context) error
}

func (r *Repository) WithMaintenance(run func(context.Context) error) *Repository {
	r.maintenance = run
	return r
}

func (r *Repository) Maintain(ctx context.Context) error {
	if r.maintenance != nil {
		return r.maintenance(ctx)
	}
	return nil
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

func (r *Repository) Get(ctx context.Context, id string) (Session, error) {
	if r.db == nil {
		return Session{}, errors.New("session repository database is required")
	}
	var value Session
	var metadata, createdAt, updatedAt string
	var closedAt sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT id, workspace_id, status, metadata_json, created_at, updated_at, closed_at
		FROM sessions WHERE id = ?`, id,
	).Scan(
		&value.ID, &value.WorkspaceID, &value.Status, &metadata,
		&createdAt, &updatedAt, &closedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	value.Metadata, err = sqlkit.CanonicalObject(json.RawMessage(metadata))
	if err != nil {
		return Session{}, fmt.Errorf("decode persisted session metadata: %w", err)
	}
	if value.CreatedAt, err = parseTime(createdAt); err != nil {
		return Session{}, err
	}
	if value.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Session{}, err
	}
	if closedAt.Valid {
		parsed, parseErr := parseTime(closedAt.String)
		if parseErr != nil {
			return Session{}, parseErr
		}
		value.ClosedAt = &parsed
	}
	return value, nil
}

func parseTime(value string) (time.Time, error) {
	result, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse persisted timestamp: %w", err)
	}
	return result, nil
}
