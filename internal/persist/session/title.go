package session

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/common/sqlkit"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// UpdateGeneratedTitle compares only title ownership and its revision. Pinning
// and other lifecycle changes do not invalidate an in-flight naming request.
func (r *Repository) UpdateGeneratedTitle(
	ctx context.Context, sessionID string, threadID protocol.ThreadID,
	expected uint64, source protocol.SessionTitleSource, title string,
) (protocol.SessionSummary, bool, error) {
	title = strings.TrimSpace(title)
	if err := protocol.ValidateSessionTitle(title); err != nil {
		return protocol.SessionSummary{}, false, err
	}
	if source != protocol.SessionTitleAuto && source != protocol.SessionTitleTemporary {
		return protocol.SessionSummary{}, false, errors.New("invalid generated title source")
	}
	changed := false
	err := sqlkit.WithTx(ctx, r.db, nil, func(tx *sql.Tx) error {
		var metadata []byte
		var status string
		if err := tx.QueryRowContext(ctx,
			`SELECT metadata_json, status FROM sessions WHERE id = ?`, sessionID,
		).Scan(&metadata, &status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		meta, _, _, err := decodeLifecycleMetadata(metadata)
		if err != nil {
			return err
		}
		eligible := meta.TitleSource == protocol.SessionTitleDefault ||
			(source == protocol.SessionTitleAuto && meta.TitleSource == protocol.SessionTitleTemporary)
		if status != string(StatusOpen) || meta.ActiveThreadID != threadID ||
			!eligible || meta.TitleRevision != expected {
			return nil
		}
		meta.TitleSource = source
		meta.TitleRevision++
		meta.Revision++
		encoded, err := metadataWithLifecycle(metadata, meta)
		if err != nil {
			return err
		}
		now := sqlkit.Timestamp(time.Now().UTC())
		if _, err := tx.ExecContext(ctx,
			`UPDATE threads SET title = ?, updated_at = ? WHERE id = ? AND session_id = ?`,
			title, now, threadID, sessionID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET metadata_json = ?, updated_at = ? WHERE id = ?`,
			encoded, now, sessionID,
		); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return protocol.SessionSummary{}, false, err
	}
	summary, err := r.GetLifecycle(ctx, sessionID)
	return summary, changed, err
}
