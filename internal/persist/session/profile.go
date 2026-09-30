package session

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fwtllh-png/QCode/internal/common/sqlkit"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

var ErrProfileRevisionConflict = errors.New("session profile revision conflict")

type ProfileRevisionConflictError struct {
	Expected uint64
	Current  uint64
}

func (e *ProfileRevisionConflictError) Error() string {
	return fmt.Sprintf(
		"session profile revision conflict: expected %d, current %d",
		e.Expected,
		e.Current,
	)
}

func (e *ProfileRevisionConflictError) Unwrap() error {
	return ErrProfileRevisionConflict
}

func (r *Repository) Profile(
	ctx context.Context,
	sessionID string,
	defaults protocol.SessionProfile,
) (protocol.SessionProfile, error) {
	if r.db == nil {
		return protocol.SessionProfile{}, errors.New("session repository database is required")
	}
	if err := defaults.Validate(); err != nil {
		return protocol.SessionProfile{}, fmt.Errorf("default session profile: %w", err)
	}
	var metadata []byte
	err := r.db.QueryRowContext(
		ctx,
		"SELECT metadata_json FROM sessions WHERE id = ?",
		sessionID,
	).Scan(&metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.SessionProfile{}, ErrNotFound
	}
	if err != nil {
		return protocol.SessionProfile{}, fmt.Errorf("get session profile: %w", err)
	}
	return profileFromMetadata(metadata, defaults)
}

func (r *Repository) EnsureProfile(
	ctx context.Context,
	sessionID string,
	defaults protocol.SessionProfile,
) (protocol.SessionProfile, error) {
	if r.db == nil {
		return protocol.SessionProfile{}, errors.New("session repository database is required")
	}
	if err := defaults.Validate(); err != nil {
		return protocol.SessionProfile{}, fmt.Errorf("default session profile: %w", err)
	}
	for range 3 {
		var metadata []byte
		err := r.db.QueryRowContext(
			ctx,
			"SELECT metadata_json FROM sessions WHERE id = ?",
			sessionID,
		).Scan(&metadata)
		if errors.Is(err, sql.ErrNoRows) {
			return protocol.SessionProfile{}, ErrNotFound
		}
		if err != nil {
			return protocol.SessionProfile{}, fmt.Errorf("get session profile: %w", err)
		}
		var values map[string]json.RawMessage
		if err := json.Unmarshal(metadata, &values); err != nil {
			return protocol.SessionProfile{}, fmt.Errorf(
				"decode durable session metadata: %w",
				err,
			)
		}
		if len(values["profile"]) != 0 {
			current, err := profileFromMetadata(metadata, defaults)
			if err != nil {
				return protocol.SessionProfile{}, err
			}
			migrated, ok, err := migrateLegacyProfileDefaults(current, defaults)
			if err != nil {
				return protocol.SessionProfile{}, err
			}
			if !ok {
				return current, nil
			}
			next, err := metadataWithProfile(metadata, migrated)
			if err != nil {
				return protocol.SessionProfile{}, err
			}
			result, err := r.db.ExecContext(ctx, `
				UPDATE sessions
				SET metadata_json = ?, updated_at = ?
				WHERE id = ? AND metadata_json = ?`,
				next,
				sqlkit.Timestamp(time.Now().UTC()),
				sessionID,
				metadata,
			)
			if err != nil {
				return protocol.SessionProfile{}, fmt.Errorf(
					"migrate session profile defaults: %w",
					err,
				)
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return protocol.SessionProfile{}, err
			}
			if affected == 1 {
				return migrated, nil
			}
			continue
		}
		next, err := metadataWithProfile(metadata, defaults)
		if err != nil {
			return protocol.SessionProfile{}, err
		}
		result, err := r.db.ExecContext(ctx, `
			UPDATE sessions
			SET metadata_json = ?, updated_at = ?
			WHERE id = ? AND metadata_json = ?`,
			next,
			sqlkit.Timestamp(time.Now().UTC()),
			sessionID,
			metadata,
		)
		if err != nil {
			return protocol.SessionProfile{}, fmt.Errorf("initialize session profile: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return protocol.SessionProfile{}, err
		}
		if affected == 1 {
			return defaults, nil
		}
	}
	return protocol.SessionProfile{}, ErrProfileRevisionConflict
}

func migrateLegacyProfileDefaults(
	current, defaults protocol.SessionProfile,
) (protocol.SessionProfile, bool, error) {
	var patch protocol.SessionProfilePatch
	if current.Revision == 1 && defaults.MaxSteps == 0 &&
		(current.MaxSteps == 8 ||
			current.MaxSteps == 64 ||
			current.MaxSteps == 256) {
		maxSteps := defaults.MaxSteps
		patch.MaxSteps = &maxSteps
	}
	if defaults.PlanningPolicy == "adaptive" &&
		current.PlanningPolicy != "adaptive" {
		planning := defaults.PlanningPolicy
		patch.PlanningPolicy = &planning
	}
	if patch.MaxSteps == nil && patch.PlanningPolicy == nil {
		return current, false, nil
	}
	updated, err := protocol.ApplySessionProfilePatch(
		current,
		patch,
	)
	if err != nil {
		return protocol.SessionProfile{}, false, err
	}
	return updated.Profile, true, nil
}

func (r *Repository) UpdateProfile(
	ctx context.Context,
	sessionID string,
	expectedRevision uint64,
	defaults protocol.SessionProfile,
	patch protocol.SessionProfilePatch,
) (protocol.SessionProfileUpdateResult, error) {
	if r.db == nil {
		return protocol.SessionProfileUpdateResult{},
			errors.New("session repository database is required")
	}
	if expectedRevision == 0 {
		return protocol.SessionProfileUpdateResult{},
			errors.New("expected session profile revision must be positive")
	}
	if err := defaults.Validate(); err != nil {
		return protocol.SessionProfileUpdateResult{},
			fmt.Errorf("default session profile: %w", err)
	}
	if err := patch.Validate(); err != nil {
		return protocol.SessionProfileUpdateResult{}, err
	}
	var updated protocol.SessionProfileUpdateResult
	err := sqlkit.WithTx(ctx, r.db, nil, func(tx *sql.Tx) error {
		var metadata []byte
		err := tx.QueryRowContext(
			ctx,
			"SELECT metadata_json FROM sessions WHERE id = ?",
			sessionID,
		).Scan(&metadata)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read session profile: %w", err)
		}
		current, err := profileFromMetadata(metadata, defaults)
		if err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return &ProfileRevisionConflictError{
				Expected: expectedRevision,
				Current:  current.Revision,
			}
		}
		updated, err = protocol.ApplySessionProfilePatch(current, patch)
		if err != nil {
			return err
		}
		if updated.Profile.Revision == current.Revision {
			return nil
		}
		nextMetadata, err := metadataWithProfile(metadata, updated.Profile)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET metadata_json = ?, updated_at = ?
			WHERE id = ? AND metadata_json = ?`,
			nextMetadata,
			sqlkit.Timestamp(time.Now().UTC()),
			sessionID,
			metadata,
		)
		if err != nil {
			return fmt.Errorf("update session profile: %w", err)
		}
		if err := sqlkit.RequireAffected(result, 1); err != nil {
			var mismatch *sqlkit.AffectedRowsError
			if !errors.As(err, &mismatch) {
				return err
			}
			return &ProfileRevisionConflictError{
				Expected: expectedRevision,
				Current:  current.Revision,
			}
		}
		return nil
	})
	if err != nil {
		return protocol.SessionProfileUpdateResult{}, err
	}
	return updated, nil
}

func (r *Repository) RebindWorkspaceProfiles(
	ctx context.Context,
	workspaceRoots []string,
	defaults protocol.SessionProfile,
) error {
	if r.db == nil {
		return errors.New("session repository database is required")
	}
	if err := defaults.Validate(); err != nil {
		return fmt.Errorf("default session profile: %w", err)
	}
	roots := make([]string, 0, len(workspaceRoots))
	seen := make(map[string]struct{}, len(workspaceRoots))
	for _, root := range workspaceRoots {
		normalized, err := NormalizeWorkspaceRoot(root)
		if err != nil {
			return fmt.Errorf("resolve session workspace: %w", err)
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		roots = append(roots, normalized)
	}
	return sqlkit.WithTx(ctx, r.db, nil, func(tx *sql.Tx) error {
		type profileMetadata struct {
			id       string
			metadata []byte
		}
		var records []profileMetadata
		for _, root := range roots {
			rows, err := tx.QueryContext(ctx, `
				SELECT s.id, s.metadata_json
				FROM sessions s
				JOIN workspaces w ON w.id = s.workspace_id
				WHERE w.root_path = ?`,
				root,
			)
			if err != nil {
				return fmt.Errorf("list Workspace session profiles: %w", err)
			}
			for rows.Next() {
				var record profileMetadata
				if err := rows.Scan(&record.id, &record.metadata); err != nil {
					_ = rows.Close()
					return err
				}
				records = append(records, record)
			}
			if err := rows.Close(); err != nil {
				return err
			}
			if err := rows.Err(); err != nil {
				return err
			}
		}
		now := sqlkit.Timestamp(time.Now().UTC())
		for _, record := range records {
			var values map[string]json.RawMessage
			if err := json.Unmarshal(record.metadata, &values); err != nil {
				return fmt.Errorf("decode durable session metadata: %w", err)
			}
			if len(values["profile"]) == 0 {
				continue
			}
			current, err := profileFromMetadata(record.metadata, defaults)
			if err != nil {
				return err
			}
			provider := defaults.Provider
			model := defaults.Model
			reasoning := defaults.ReasoningEffort
			updated, err := protocol.ApplySessionProfilePatch(
				current,
				protocol.SessionProfilePatch{
					Provider:        &provider,
					Model:           &model,
					ReasoningEffort: &reasoning,
				},
			)
			if err != nil {
				return err
			}
			if updated.Profile.Revision == current.Revision {
				continue
			}
			next, err := metadataWithProfile(record.metadata, updated.Profile)
			if err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, `
				UPDATE sessions
				SET metadata_json = ?, updated_at = ?
				WHERE id = ? AND metadata_json = ?`,
				next,
				now,
				record.id,
				record.metadata,
			)
			if err != nil {
				return fmt.Errorf("rebind session profile: %w", err)
			}
			if err := sqlkit.RequireAffected(result, 1); err != nil {
				return err
			}
		}
		return nil
	})
}

func metadataWithProfile(
	metadata []byte,
	profile protocol.SessionProfile,
) ([]byte, error) {
	var values map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(metadata))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("decode durable session metadata: %w", err)
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return nil, fmt.Errorf("encode session profile: %w", err)
	}
	values["profile"] = encoded
	result, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("encode durable session metadata: %w", err)
	}
	return result, nil
}

func profileFromMetadata(
	metadata []byte,
	defaults protocol.SessionProfile,
) (protocol.SessionProfile, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &values); err != nil {
		return protocol.SessionProfile{}, fmt.Errorf("decode durable session metadata: %w", err)
	}
	raw := values["profile"]
	if len(raw) == 0 {
		return defaults, nil
	}
	var profile protocol.SessionProfile
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profile); err != nil {
		return protocol.SessionProfile{}, fmt.Errorf("decode session profile: %w", err)
	}
	if err := profile.Validate(); err != nil {
		return protocol.SessionProfile{}, fmt.Errorf("validate session profile: %w", err)
	}
	return profile, nil
}
