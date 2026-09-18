package snapshot

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/fwtllh-png/QCode/internal/persist/artifact"
)

// CheckpointSummaries reads counts and the latest checkpoint for the requested
// sessions together, retaining the same metadata validation as ListCheckpoints.
func (r *Repository) CheckpointSummaries(ctx context.Context, sessionIDs []string) (map[string]artifact.SessionCheckpointSummary, error) {
	if r.db == nil {
		return nil, errors.New("snapshot database is required")
	}
	result := make(map[string]artifact.SessionCheckpointSummary, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return result, nil
	}
	ids, err := json.Marshal(sessionIDs)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
		WITH ranked AS (
			SELECT s.id, s.thread_id, s.turn_id, s.cursor, s.metadata_json, s.created_at,
			       t.session_id, COUNT(*) OVER (PARTITION BY t.session_id) AS count,
			       ROW_NUMBER() OVER (
			           PARTITION BY t.session_id ORDER BY s.cursor DESC, s.created_at DESC
			       ) AS rank
			FROM snapshots s JOIN threads t ON t.id = s.thread_id
			WHERE t.session_id IN (SELECT value FROM json_each(?)) AND s.kind = ?
		)
		SELECT id, thread_id, turn_id, cursor, metadata_json, created_at, session_id, count
		FROM ranked WHERE rank = 1`, string(ids), KindSessionCheckpoint)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID string
		var count int
		checkpoint, err := scanCheckpointSummary(rows, &sessionID, &count)
		if err != nil {
			return nil, err
		}
		if checkpoint.SessionID != sessionID {
			return nil, &IntegrityError{
				ID: checkpoint.ID, Err: errors.New("checkpoint crosses Session identity"),
			}
		}
		result[sessionID] = artifact.SessionCheckpointSummary{
			Count: count, ChangedFiles: checkpoint.ChangedFiles,
		}
	}
	return result, rows.Err()
}
