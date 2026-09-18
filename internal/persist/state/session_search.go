package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/fwtllh-png/QCode/internal/persist/history"
	"github.com/fwtllh-png/QCode/internal/persist/state/eventlog"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// This cache is rebuilt from the durable log, not a versioned source of truth.
// Keeping it beside event_index makes append and recovery one atomic projection.
func (s *Store) ensureSessionSearch(ctx context.Context) error {
	_, err := s.sqlite.DB().ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS session_event_search (
			sequence INTEGER PRIMARY KEY REFERENCES event_index(sequence) ON DELETE CASCADE,
			thread_id TEXT NOT NULL,
			turn_id TEXT NOT NULL,
			fields_json TEXT NOT NULL,
			folded_text TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS session_event_search_thread
			ON session_event_search(thread_id, sequence DESC);
	`)
	return err
}

const upsertSessionSearch = `
	INSERT INTO session_event_search(sequence, thread_id, turn_id, fields_json, folded_text)
	VALUES (?, ?, ?, ?, ?)
	ON CONFLICT(sequence) DO UPDATE SET
		thread_id=excluded.thread_id, turn_id=excluded.turn_id,
		fields_json=excluded.fields_json, folded_text=excluded.folded_text
	WHERE thread_id != excluded.thread_id OR turn_id != excluded.turn_id
		OR fields_json != excluded.fields_json OR folded_text != excluded.folded_text`

func searchProjection(event protocol.Event) ([]any, error) {
	fields := history.SearchFields(event)
	if len(fields) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	texts := make([]string, len(fields))
	for index, field := range fields {
		texts[index] = strings.ToLower(field.Text)
	}
	// SessionListQuery forbids NUL, so no query can match across fields.
	return []any{event.Sequence, event.ThreadID, event.TurnID,
		string(encoded), strings.Join(texts, "\x00")}, nil
}

func projectSessionSearchTx(ctx context.Context, tx *sql.Tx, event protocol.Event) error {
	args, err := searchProjection(event)
	if err != nil || args == nil {
		return err
	}
	_, err = tx.ExecContext(ctx, upsertSessionSearch, args...)
	return err
}

func (s *Store) reconcileSessionSearch(ctx context.Context, records []eventlog.Record) error {
	rows, err := s.sqlite.DB().QueryContext(ctx, `
		SELECT sequence, thread_id, turn_id, fields_json, folded_text FROM session_event_search`)
	if err != nil {
		return err
	}
	existing := make(map[protocol.Cursor][4]string)
	for rows.Next() {
		var sequence protocol.Cursor
		var fields [4]string
		if err := rows.Scan(&sequence, &fields[0], &fields[1], &fields[2], &fields[3]); err != nil {
			_ = rows.Close()
			return err
		}
		existing[sequence] = fields
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var changed [][]any
	for _, record := range records {
		args, err := searchProjection(record.Event)
		if err != nil {
			return err
		}
		if args == nil {
			continue
		}
		fields := [4]string{string(record.Event.ThreadID), string(record.Event.TurnID),
			args[3].(string), args[4].(string)}
		if current, ok := existing[record.Event.Sequence]; !ok || current != fields {
			changed = append(changed, args)
		}
		delete(existing, record.Event.Sequence)
	}
	// No write transaction or per-event SQL for a healthy cache.
	if len(changed) == 0 && len(existing) == 0 {
		return nil
	}
	return s.sqlite.Transaction(ctx, func(tx *sql.Tx) error {
		statement, err := tx.PrepareContext(ctx, upsertSessionSearch)
		if err != nil {
			return err
		}
		defer statement.Close()
		for _, args := range changed {
			if _, err := statement.ExecContext(ctx, args...); err != nil {
				return err
			}
		}
		for sequence := range existing {
			if _, err := tx.ExecContext(ctx,
				"DELETE FROM session_event_search WHERE sequence = ?", sequence); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) SearchSessionEvents(ctx context.Context, byThread map[protocol.ThreadID]string, query string) ([]protocol.SessionSearchMatch, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	if len(byThread) == 0 || query == "" {
		return nil, nil
	}
	requested, err := json.Marshal(byThread)
	if err != nil {
		return nil, err
	}
	// JSON binds the ownership set in one parameter, independent of the
	// number of threads. The thread index bounds substring scanning to the
	// requested sessions; only the newest match per session is decoded.
	rows, err := s.sqlite.DB().QueryContext(ctx, `
		SELECT newest.session_id, p.turn_id, p.fields_json
		FROM (
			SELECT t.session_id, MAX(p.sequence) AS sequence
			FROM json_each(?) AS requested
			JOIN threads AS t ON t.id = requested.key AND t.session_id = requested.value
			JOIN session_event_search AS p ON p.thread_id = t.id
			WHERE instr(p.folded_text, ?) > 0
			GROUP BY t.session_id
		) AS newest
		JOIN session_event_search AS p ON p.sequence = newest.sequence`,
		string(requested), strings.ToLower(query))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var matches []protocol.SessionSearchMatch
	for rows.Next() {
		var match protocol.SessionSearchMatch
		var encoded string
		if err := rows.Scan(&match.SessionID, &match.TurnID, &encoded); err != nil {
			return nil, err
		}
		var fields []history.SearchField
		if err := json.Unmarshal([]byte(encoded), &fields); err != nil {
			return nil, err
		}
		if kind, snippet, ok := history.MatchSearchFields(fields, query); ok {
			match.Kind, match.Snippet = kind, snippet
			matches = append(matches, match)
		}
	}
	return matches, rows.Err()
}

func (s *WorkspaceEventStore) SearchSessionEvents(ctx context.Context, byThread map[protocol.ThreadID]string, query string) ([]protocol.SessionSearchMatch, error) {
	if s.workspaceRoot == "" {
		return s.store.SearchSessionEvents(ctx, byThread, query)
	}
	threads, err := s.store.workspaceThreadIDs(ctx, physicalWorkspaceRoot(s.workspaceRoot))
	if err != nil {
		return nil, err
	}
	owned := make(map[protocol.ThreadID]string, len(byThread))
	for threadID, sessionID := range byThread {
		if _, ok := threads[threadID]; ok {
			owned[threadID] = sessionID
		}
	}
	return s.store.SearchSessionEvents(ctx, owned, query)
}
