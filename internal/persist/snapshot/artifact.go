package snapshot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/persist/sqlkit"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/durablecodec"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const (
	KindSessionCheckpoint = "session-checkpoint"
	KindSessionPlan       = "session-plan"
)

type checkpointContent struct {
	History         []protocol.CompactedMessage   `json:"history,omitempty"`
	ContextManifest *agentcontext.ContextManifest `json:"context_manifest,omitempty"`
	Profile         protocol.SessionProfile       `json:"profile"`
}

type checkpointMetadata struct {
	Version             int                        `json:"version"`
	SessionID           string                     `json:"session_id"`
	Status              protocol.CheckpointStatus  `json:"status"`
	Summary             string                     `json:"summary"`
	ProfileRevision     uint64                     `json:"profile_revision"`
	StateEpoch          uint64                     `json:"state_epoch,omitempty"`
	ContextDigest       string                     `json:"context_digest,omitempty"`
	WorkspaceDigest     string                     `json:"workspace_digest,omitempty"`
	ParentCheckpointID  string                     `json:"parent_checkpoint_id,omitempty"`
	ChangeReceipt       *protocol.ReceiptReference `json:"change_receipt,omitempty"`
	ChangedFiles        int                        `json:"changed_files"`
	ExternalSideEffects bool                       `json:"external_side_effects"`
	SideEffectNote      string                     `json:"side_effect_note,omitempty"`
}

type planMetadata struct {
	Version                int                         `json:"version"`
	SessionID              string                      `json:"session_id"`
	Status                 protocol.PlanArtifactStatus `json:"status"`
	Purpose                protocol.PlanPurpose        `json:"purpose,omitempty"`
	ProfileRevision        uint64                      `json:"profile_revision"`
	ExecutionProfileDigest string                      `json:"execution_profile_digest,omitempty"`
	CanImplement           bool                        `json:"can_implement"`
	CanAutopilot           bool                        `json:"can_autopilot"`
}

func (r *Repository) SaveCheckpoint(
	ctx context.Context,
	checkpoint protocol.SessionCheckpoint,
	history []protocol.CompactedMessage,
	profile protocol.SessionProfile,
) (protocol.SessionCheckpoint, error) {
	return r.saveCheckpoint(ctx, checkpoint, history, profile, nil)
}

func (r *Repository) SaveContextCheckpoint(
	ctx context.Context,
	checkpoint protocol.SessionCheckpoint,
	history []protocol.CompactedMessage,
	contextSnapshot agentcontext.ContextSnapshot,
	profile protocol.SessionProfile,
) (protocol.SessionCheckpoint, error) {
	if err := contextSnapshot.Validate(); err != nil {
		return protocol.SessionCheckpoint{}, err
	}
	return r.saveCheckpoint(
		ctx,
		checkpoint,
		history,
		profile,
		&contextSnapshot,
	)
}

func (r *Repository) saveCheckpoint(
	ctx context.Context,
	checkpoint protocol.SessionCheckpoint,
	history []protocol.CompactedMessage,
	profile protocol.SessionProfile,
	contextSnapshot *agentcontext.ContextSnapshot,
) (protocol.SessionCheckpoint, error) {
	if r.db == nil || r.content == nil {
		return protocol.SessionCheckpoint{},
			errors.New("snapshot database and content store are required")
	}
	sessionID, err := r.sessionForThread(ctx, checkpoint.ThreadID)
	if err != nil {
		return protocol.SessionCheckpoint{}, err
	}
	if checkpoint.SessionID == "" {
		checkpoint.SessionID = sessionID
	}
	if checkpoint.SessionID != sessionID ||
		checkpoint.ProfileRevision != profile.Revision ||
		len(history) == 0 {
		return protocol.SessionCheckpoint{},
			errors.New("checkpoint state identity is inconsistent")
	}
	if checkpoint.ParentCheckpointID == "" {
		parent, parentErr := r.latestCheckpointSummary(ctx, checkpoint.ThreadID)
		if parentErr == nil {
			checkpoint.ParentCheckpointID = parent.ID
		} else if !errors.Is(parentErr, ErrNotFound) {
			return protocol.SessionCheckpoint{}, parentErr
		}
	}
	checkpoint.CanRestore = true
	checkpoint.CanFork = true
	if err := checkpoint.Validate(); err != nil {
		return protocol.SessionCheckpoint{}, err
	}
	contentState := checkpointContent{
		History: append([]protocol.CompactedMessage(nil), history...),
		Profile: profile,
	}
	if contextSnapshot != nil {
		var previous *agentcontext.ContextManifest
		if checkpoint.ParentCheckpointID != "" {
			parent, getErr := r.Get(ctx, checkpoint.ParentCheckpointID)
			if getErr == nil {
				var parentContent checkpointContent
				if decodeCheckpointContent(parent.Content, &parentContent) == nil &&
					parentContent.ContextManifest != nil {
					value := *parentContent.ContextManifest
					previous = &value
				}
			}
		}
		manifest, buildErr := agentcontext.BuildContextManifest(
			ctx,
			r.content,
			checkpoint.ThreadID,
			checkpoint.TurnID,
			*contextSnapshot,
			previous,
			agentcontext.DefaultManifestLimits(),
		)
		if buildErr != nil {
			return protocol.SessionCheckpoint{}, buildErr
		}
		contentState.History = nil
		contentState.ContextManifest = &manifest
	}
	content, err := encodeCheckpointContent(contentState)
	if err != nil {
		return protocol.SessionCheckpoint{}, err
	}
	metadata, err := json.Marshal(checkpointMetadata{
		Version:             checkpoint.Version,
		SessionID:           checkpoint.SessionID,
		Status:              checkpoint.Status,
		Summary:             checkpoint.Summary,
		ProfileRevision:     checkpoint.ProfileRevision,
		StateEpoch:          checkpoint.StateEpoch,
		ContextDigest:       checkpoint.ContextDigest,
		WorkspaceDigest:     checkpoint.WorkspaceDigest,
		ParentCheckpointID:  checkpoint.ParentCheckpointID,
		ChangeReceipt:       checkpoint.ChangeReceipt,
		ChangedFiles:        checkpoint.ChangedFiles,
		ExternalSideEffects: checkpoint.ExternalSideEffects,
		SideEffectNote:      checkpoint.SideEffectNote,
	})
	if err != nil {
		return protocol.SessionCheckpoint{}, err
	}
	saved, err := r.Save(ctx, Snapshot{
		ID:        checkpoint.ID,
		ThreadID:  checkpoint.ThreadID,
		TurnID:    checkpoint.TurnID,
		Cursor:    checkpoint.Cursor,
		Kind:      KindSessionCheckpoint,
		Content:   content,
		Metadata:  metadata,
		CreatedAt: checkpoint.CreatedAt,
	})
	if err != nil {
		return protocol.SessionCheckpoint{}, err
	}
	checkpoint.CreatedAt = saved.CreatedAt
	return checkpoint, nil
}

func (r *Repository) GetCheckpoint(
	ctx context.Context,
	id string,
) (
	protocol.SessionCheckpoint,
	[]protocol.CompactedMessage,
	protocol.SessionProfile,
	error,
) {
	value, err := r.Get(ctx, id)
	if err != nil {
		return protocol.SessionCheckpoint{}, nil, protocol.SessionProfile{}, err
	}
	if value.Kind != KindSessionCheckpoint {
		return protocol.SessionCheckpoint{}, nil, protocol.SessionProfile{},
			ErrNotFound
	}
	checkpoint, err := decodeCheckpointSummary(value)
	if err != nil {
		return protocol.SessionCheckpoint{}, nil, protocol.SessionProfile{}, err
	}
	var content checkpointContent
	if err := decodeCheckpointContent(value.Content, &content); err != nil {
		return protocol.SessionCheckpoint{}, nil, protocol.SessionProfile{},
			&IntegrityError{ID: id, Err: err}
	}
	if content.Profile.Revision != checkpoint.ProfileRevision ||
		len(content.History) == 0 && content.ContextManifest == nil {
		return protocol.SessionCheckpoint{}, nil, protocol.SessionProfile{},
			&IntegrityError{
				ID:  id,
				Err: errors.New("checkpoint content identity is inconsistent"),
			}
	}
	if err := content.Profile.Validate(); err != nil {
		return protocol.SessionCheckpoint{}, nil, protocol.SessionProfile{},
			&IntegrityError{ID: id, Err: err}
	}
	if len(content.History) == 0 && content.ContextManifest != nil {
		snapshot, loadErr := agentcontext.LoadContextManifest(
			ctx,
			r.content,
			*content.ContextManifest,
		)
		if loadErr != nil {
			return protocol.SessionCheckpoint{}, nil, protocol.SessionProfile{},
				&IntegrityError{ID: id, Err: loadErr}
		}
		content.History, err = encodeContextHistory(snapshot.History)
		if err != nil {
			return protocol.SessionCheckpoint{}, nil, protocol.SessionProfile{},
				&IntegrityError{ID: id, Err: err}
		}
	}
	return checkpoint,
		append([]protocol.CompactedMessage(nil), content.History...),
		content.Profile,
		nil
}

func (r *Repository) GetContextCheckpoint(
	ctx context.Context,
	id string,
) (
	protocol.SessionCheckpoint,
	agentcontext.ContextSnapshot,
	protocol.SessionProfile,
	error,
) {
	value, err := r.Get(ctx, id)
	if err != nil {
		return protocol.SessionCheckpoint{}, agentcontext.ContextSnapshot{},
			protocol.SessionProfile{}, err
	}
	if value.Kind != KindSessionCheckpoint {
		return protocol.SessionCheckpoint{}, agentcontext.ContextSnapshot{},
			protocol.SessionProfile{}, ErrNotFound
	}
	checkpoint, err := decodeCheckpointSummary(value)
	if err != nil {
		return protocol.SessionCheckpoint{}, agentcontext.ContextSnapshot{},
			protocol.SessionProfile{}, err
	}
	var content checkpointContent
	if err := decodeCheckpointContent(value.Content, &content); err != nil {
		return protocol.SessionCheckpoint{}, agentcontext.ContextSnapshot{},
			protocol.SessionProfile{}, &IntegrityError{ID: id, Err: err}
	}
	if content.ContextManifest == nil {
		return protocol.SessionCheckpoint{}, agentcontext.ContextSnapshot{},
			protocol.SessionProfile{}, &IntegrityError{
				ID: id, Err: errors.New("checkpoint has no context snapshot"),
			}
	}
	contextSnapshot, err := agentcontext.LoadContextManifest(
		ctx,
		r.content,
		*content.ContextManifest,
	)
	if err != nil {
		return protocol.SessionCheckpoint{}, agentcontext.ContextSnapshot{},
			protocol.SessionProfile{}, &IntegrityError{ID: id, Err: err}
	}
	if contextSnapshot.Digest != checkpoint.ContextDigest ||
		contextSnapshot.Epoch != checkpoint.StateEpoch ||
		contextSnapshot.Workspace.SparseDigest != checkpoint.WorkspaceDigest ||
		content.Profile.Revision != checkpoint.ProfileRevision {
		return protocol.SessionCheckpoint{}, agentcontext.ContextSnapshot{},
			protocol.SessionProfile{}, &IntegrityError{
				ID: id, Err: errors.New("checkpoint context identity is inconsistent"),
			}
	}
	return checkpoint, contextSnapshot, content.Profile, nil
}

func encodeContextHistory(
	messages []provider.Message,
) ([]protocol.CompactedMessage, error) {
	result := make([]protocol.CompactedMessage, 0, len(messages))
	for _, message := range messages {
		content, err := json.Marshal(message.Blocks)
		if err != nil {
			return nil, err
		}
		result = append(result, protocol.CompactedMessage{
			Role: string(message.Role), Content: content, Turn: message.Turn,
		})
	}
	return result, nil
}

func encodeCheckpointContent(value checkpointContent) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return durablecodec.Compress(raw)
}

func decodeCheckpointContent(raw []byte, target *checkpointContent) error {
	if !durablecodec.IsCompressed(raw) {
		return decodeStrict(raw, target)
	}
	decoded, err := durablecodec.Decompress(raw)
	if err != nil {
		return err
	}
	return decodeStrict(decoded, target)
}

func (r *Repository) ListCheckpoints(
	ctx context.Context,
	sessionID string,
	limit int,
) ([]protocol.SessionCheckpoint, error) {
	if r.db == nil {
		return nil, errors.New("snapshot database is required")
	}
	if sessionID == "" || limit < 1 || limit > 1000 {
		return nil, errors.New("checkpoint Session and limit are invalid")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, s.thread_id, s.turn_id, s.cursor, s.metadata_json, s.created_at
		FROM snapshots s
		JOIN threads t ON t.id = s.thread_id
		WHERE t.session_id = ? AND s.kind = ?
		ORDER BY s.cursor DESC, s.created_at DESC
		LIMIT ?`,
		sessionID, KindSessionCheckpoint, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list Session checkpoints: %w", err)
	}
	defer rows.Close()
	result := make([]protocol.SessionCheckpoint, 0)
	for rows.Next() {
		checkpoint, err := scanCheckpointSummary(rows)
		if err != nil {
			return nil, err
		}
		if checkpoint.SessionID != sessionID {
			return nil, &IntegrityError{
				ID:  checkpoint.ID,
				Err: errors.New("checkpoint crosses Session identity"),
			}
		}
		result = append(result, checkpoint)
	}
	return result, rows.Err()
}

func scanCheckpointSummary(row interface{ Scan(...any) error }, extra ...any) (protocol.SessionCheckpoint, error) {
	var value Snapshot
	var turnID sql.NullString
	var metadata, createdAt string
	dest := []any{&value.ID, &value.ThreadID, &turnID, &value.Cursor, &metadata, &createdAt}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return protocol.SessionCheckpoint{}, err
	}
	value.TurnID = protocol.TurnID(turnID.String)
	value.Kind = KindSessionCheckpoint
	var err error
	value.Metadata, err = sqlkit.CanonicalObject(json.RawMessage(metadata))
	if err != nil {
		return protocol.SessionCheckpoint{}, &IntegrityError{
			ID: value.ID, Err: fmt.Errorf("decode persisted checkpoint metadata: %w", err),
		}
	}
	if value.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return protocol.SessionCheckpoint{}, &IntegrityError{ID: value.ID, Err: err}
	}
	return decodeCheckpointSummary(value)
}

func (r *Repository) CountCheckpoints(
	ctx context.Context,
	sessionID string,
) (int, error) {
	if r.db == nil {
		return 0, errors.New("snapshot database is required")
	}
	var count int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM snapshots s
		JOIN threads t ON t.id = s.thread_id
		WHERE t.session_id = ? AND s.kind = ?`,
		sessionID, KindSessionCheckpoint,
	).Scan(&count)
	return count, err
}

func (r *Repository) SavePlan(
	ctx context.Context,
	artifact protocol.SessionPlanArtifact,
) (protocol.SessionPlanArtifact, error) {
	sessionID, err := r.sessionForThread(ctx, artifact.ThreadID)
	if err != nil {
		return protocol.SessionPlanArtifact{}, err
	}
	if artifact.SessionID == "" {
		artifact.SessionID = sessionID
	}
	if artifact.SessionID != sessionID {
		return protocol.SessionPlanArtifact{},
			errors.New("Plan Artifact crosses Session identity")
	}
	if err := artifact.Validate(); err != nil {
		return protocol.SessionPlanArtifact{}, err
	}
	metadata, err := json.Marshal(planMetadata{
		Version:                artifact.Version,
		SessionID:              artifact.SessionID,
		Status:                 artifact.Status,
		Purpose:                artifact.Purpose,
		ProfileRevision:        artifact.ProfileRevision,
		ExecutionProfileDigest: artifact.ExecutionProfileDigest,
		CanImplement:           artifact.CanImplement,
		CanAutopilot:           artifact.CanAutopilot,
	})
	if err != nil {
		return protocol.SessionPlanArtifact{}, err
	}
	saved, err := r.Save(ctx, Snapshot{
		ID:        artifact.ID,
		ThreadID:  artifact.ThreadID,
		TurnID:    artifact.TurnID,
		Cursor:    artifact.Cursor,
		Kind:      KindSessionPlan,
		Content:   []byte(artifact.Body),
		Metadata:  metadata,
		CreatedAt: artifact.CreatedAt,
	})
	if err != nil {
		return protocol.SessionPlanArtifact{}, err
	}
	artifact.CreatedAt = saved.CreatedAt
	return artifact, nil
}

func (r *Repository) GetPlan(
	ctx context.Context,
	id string,
) (protocol.SessionPlanArtifact, error) {
	value, err := r.Get(ctx, id)
	if err != nil {
		return protocol.SessionPlanArtifact{}, err
	}
	if value.Kind != KindSessionPlan {
		return protocol.SessionPlanArtifact{}, ErrNotFound
	}
	return decodePlanArtifact(value)
}

func (r *Repository) LatestPlan(
	ctx context.Context,
	sessionID string,
	threadID protocol.ThreadID,
) (protocol.SessionPlanArtifact, bool, error) {
	if r.db == nil || r.content == nil {
		return protocol.SessionPlanArtifact{}, false,
			errors.New("snapshot database and content store are required")
	}
	query := `
		SELECT s.id
		FROM snapshots s
		JOIN threads t ON t.id = s.thread_id
		WHERE t.session_id = ? AND s.kind = ?`
	arguments := []any{sessionID, KindSessionPlan}
	if threadID != "" {
		query += " AND s.thread_id = ?"
		arguments = append(arguments, threadID)
	}
	query += " ORDER BY s.cursor DESC, s.created_at DESC LIMIT 1"
	var id string
	if err := r.db.QueryRowContext(ctx, query, arguments...).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return protocol.SessionPlanArtifact{}, false, nil
		}
		return protocol.SessionPlanArtifact{}, false, err
	}
	artifact, err := r.GetPlan(ctx, id)
	return artifact, err == nil, err
}

func (r *Repository) sessionForThread(
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

func (r *Repository) latestCheckpointSummary(
	ctx context.Context,
	threadID protocol.ThreadID,
) (protocol.SessionCheckpoint, error) {
	var id string
	err := r.db.QueryRowContext(ctx, `
		SELECT id FROM snapshots
		WHERE thread_id = ? AND kind = ?
		ORDER BY cursor DESC, created_at DESC LIMIT 1`,
		threadID, KindSessionCheckpoint,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.SessionCheckpoint{}, ErrNotFound
	}
	if err != nil {
		return protocol.SessionCheckpoint{}, err
	}
	value, err := r.Get(ctx, id)
	if err != nil {
		return protocol.SessionCheckpoint{}, err
	}
	return decodeCheckpointSummary(value)
}

func decodeCheckpointSummary(
	value Snapshot,
) (protocol.SessionCheckpoint, error) {
	var metadata checkpointMetadata
	if err := decodeStrict(value.Metadata, &metadata); err != nil {
		return protocol.SessionCheckpoint{},
			&IntegrityError{ID: value.ID, Err: err}
	}
	checkpoint := protocol.SessionCheckpoint{
		Version:             metadata.Version,
		ID:                  value.ID,
		SessionID:           metadata.SessionID,
		ThreadID:            value.ThreadID,
		TurnID:              value.TurnID,
		Cursor:              value.Cursor,
		Status:              metadata.Status,
		Summary:             metadata.Summary,
		ProfileRevision:     metadata.ProfileRevision,
		StateEpoch:          metadata.StateEpoch,
		ContextDigest:       metadata.ContextDigest,
		WorkspaceDigest:     metadata.WorkspaceDigest,
		ParentCheckpointID:  metadata.ParentCheckpointID,
		ChangeReceipt:       metadata.ChangeReceipt,
		ChangedFiles:        metadata.ChangedFiles,
		ExternalSideEffects: metadata.ExternalSideEffects,
		SideEffectNote:      metadata.SideEffectNote,
		CanRestore:          true,
		CanFork:             true,
		CreatedAt:           value.CreatedAt,
	}
	if err := checkpoint.Validate(); err != nil {
		return protocol.SessionCheckpoint{},
			&IntegrityError{ID: value.ID, Err: err}
	}
	return checkpoint, nil
}

func decodePlanArtifact(
	value Snapshot,
) (protocol.SessionPlanArtifact, error) {
	var metadata planMetadata
	if err := decodeStrict(value.Metadata, &metadata); err != nil {
		return protocol.SessionPlanArtifact{},
			&IntegrityError{ID: value.ID, Err: err}
	}
	artifact := protocol.SessionPlanArtifact{
		Version:                metadata.Version,
		ID:                     value.ID,
		SessionID:              metadata.SessionID,
		ThreadID:               value.ThreadID,
		TurnID:                 value.TurnID,
		Cursor:                 value.Cursor,
		Status:                 metadata.Status,
		Purpose:                metadata.Purpose,
		Body:                   string(value.Content),
		ProfileRevision:        metadata.ProfileRevision,
		ExecutionProfileDigest: metadata.ExecutionProfileDigest,
		CanImplement:           metadata.CanImplement,
		CanAutopilot:           metadata.CanAutopilot,
		CreatedAt:              value.CreatedAt,
	}
	if err := artifact.Validate(); err != nil {
		return protocol.SessionPlanArtifact{},
			&IntegrityError{ID: value.ID, Err: err}
	}
	return artifact, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("snapshot contains trailing JSON")
		}
		return err
	}
	return nil
}
