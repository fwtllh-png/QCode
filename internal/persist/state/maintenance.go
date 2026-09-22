package state

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/state/eventlog"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// Maintain removes expired deleted-thread audit events. Zero retention means
// immediate cleanup; archived exports, when requested, are retained separately.
// It runs at startup and after session deletion, and can be called explicitly.
func (s *Store) Maintain(ctx context.Context) error {
	s.readers.Lock()
	defer s.readers.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := s.queueExpiredEvents(ctx, time.Now()); err != nil {
		return err
	}
	if err := s.finishEventPruning(ctx); err != nil {
		return err
	}
	_, err := s.content.CollectUnreferenced(ctx)
	return err
}

func (s *Store) queueExpiredEvents(ctx context.Context, now time.Time) error {
	rows, err := s.sqlite.DB().QueryContext(ctx,
		`SELECT thread_id, deleted_at FROM deleted_event_threads
		 WHERE EXISTS(SELECT 1 FROM event_index WHERE thread_id = deleted_event_threads.thread_id)`)
	if err != nil {
		return err
	}
	var expired []string
	for rows.Next() {
		var thread, deleted string
		if err := rows.Scan(&thread, &deleted); err != nil {
			_ = rows.Close()
			return err
		}
		at, err := time.Parse(time.RFC3339Nano, deleted)
		if err != nil {
			_ = rows.Close()
			return err
		}
		if !now.Before(at.Add(s.deletedEventRetention)) {
			expired = append(expired, thread)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	return s.sqlite.Transaction(ctx, func(tx *sql.Tx) error {
		for _, thread := range expired {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO event_prune_queue SELECT sequence FROM event_index
				 WHERE thread_id = ? ON CONFLICT DO NOTHING`, thread); err != nil {
				return err
			}
		}
		return nil
	})
}

// A durable queue makes both sides of the filesystem/SQLite boundary
// recoverable: before rename, repeat compaction; after rename, repair offsets
// and remove the queued projections. Clear the queue only with that commit.
func (s *Store) finishEventPruning(ctx context.Context) error {
	rows, err := s.sqlite.DB().QueryContext(ctx, "SELECT sequence FROM event_prune_queue")
	if err != nil {
		return err
	}
	remove := make(map[protocol.Cursor]bool)
	for rows.Next() {
		var sequence protocol.Cursor
		if err := rows.Scan(&sequence); err != nil {
			_ = rows.Close()
			return err
		}
		remove[sequence] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(remove) == 0 {
		return nil
	}
	archive := ""
	if s.archiveDeletedEvents {
		archive = filepath.Join(s.root, "event-archives")
	}
	if _, err := s.events.Compact(ctx, remove, archive); err != nil {
		return err
	}
	records, err := s.events.ReplayRecords(ctx, 0)
	if err != nil {
		return err
	}
	return s.sqlite.Transaction(ctx, func(tx *sql.Tx) error {
		for _, statement := range []string{
			"DELETE FROM event_index WHERE sequence IN (SELECT sequence FROM event_prune_queue)",
			"DELETE FROM event_reservations WHERE sequence IN (SELECT sequence FROM event_prune_queue)",
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		for _, record := range records {
			if err := updateEventOffset(ctx, tx, record); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM event_prune_queue")
		return err
	})
}

func updateEventOffset(ctx context.Context, tx *sql.Tx, record eventlog.Record) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE event_index SET log_offset = ?, log_length = ?, sha256 = ?
		 WHERE sequence = ? AND event_id = ?`,
		record.Evidence.Offset, record.Evidence.Length, record.Evidence.SHA256,
		record.Event.Sequence, record.Event.ID)
	return err
}
