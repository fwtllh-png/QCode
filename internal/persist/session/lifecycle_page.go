package session

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func lifecyclePageIDs(summaries []protocol.SessionSummary) []string {
	ids := make([]string, len(summaries))
	for i := range summaries {
		ids[i] = summaries[i].SessionID
	}
	return ids
}

// Aggregate only the selected page, without multiplying usage by event or
// thread counts. The JSON parameter also avoids SQLite's bind-variable limit.
func (r *Repository) projectLifecyclePage(ctx context.Context, summaries []protocol.SessionSummary) error {
	if len(summaries) == 0 {
		return nil
	}
	ids, err := json.Marshal(lifecyclePageIDs(summaries))
	if err != nil {
		return err
	}
	rows, err := r.db.QueryContext(ctx, `
		WITH page AS (SELECT value AS id FROM json_each(?)),
		page_threads AS (
			SELECT t.id, t.session_id FROM threads t JOIN page p ON p.id = t.session_id
		),
		latest AS (
			SELECT t.session_id, tr.id, tr.status, tr.updated_at,
			       ROW_NUMBER() OVER (
			           PARTITION BY t.session_id ORDER BY tr.updated_at DESC, tr.ordinal DESC
			       ) AS rank
			FROM page_threads t JOIN turns tr ON tr.thread_id = t.id
		),
		watermarks AS (
			SELECT t.session_id, MAX(e.sequence) AS sequence
			FROM page_threads t JOIN event_index e ON e.thread_id = t.id
			GROUP BY t.session_id
		),
		totals AS (
			SELECT u.session_id, SUM(input_tokens + output_tokens + reasoning_tokens) AS tokens,
			       SUM(CASE WHEN cost_known THEN cost_microunits ELSE 0 END) AS cost,
			       SUM(CASE WHEN cost_known THEN 0 ELSE 1 END) AS unpriced, COUNT(*) AS calls
			FROM usage u JOIN page p ON p.id = u.session_id GROUP BY u.session_id
		)
		SELECT p.id, l.id, l.status, l.updated_at, COALESCE(w.sequence, 0),
		       COALESCE(u.tokens, 0), COALESCE(u.cost, 0), COALESCE(u.unpriced, 0), COALESCE(u.calls, 0)
		FROM page p
		LEFT JOIN latest l ON l.session_id = p.id AND l.rank = 1
		LEFT JOIN watermarks w ON w.session_id = p.id
		LEFT JOIN totals u ON u.session_id = p.id`, string(ids))
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[string]*protocol.SessionSummary, len(summaries))
	for i := range summaries {
		byID[summaries[i].SessionID] = &summaries[i]
	}
	for rows.Next() {
		var id string
		var turnID, status, updatedAt sql.NullString
		var sequence protocol.Cursor
		var tokens, cost, unpriced, calls uint64
		if err := rows.Scan(&id, &turnID, &status, &updatedAt, &sequence, &tokens, &cost, &unpriced, &calls); err != nil {
			return err
		}
		summary := byID[id]
		if err := applyLatestTurn(summary, turnID.String, status.String, updatedAt.String); err != nil {
			return err
		}
		if turnID.Valid {
			summary.LatestSequence = sequence
		}
		summary.TotalTokens = tokens
		summary.CostMicrounits = cost
		summary.CostKnown = calls > 0 && unpriced == 0
		if err := summary.Validate(); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (r *Repository) matchLifecyclePage(ctx context.Context, summaries []protocol.SessionSummary, query string) ([]protocol.SessionSearchMatch, error) {
	matches := make([]protocol.SessionSearchMatch, 0)
	if len(summaries) == 0 || query == "" {
		return matches, nil
	}
	ids, err := json.Marshal(lifecyclePageIDs(summaries))
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
		WITH matches AS (
			SELECT t.session_id, tr.id,
			       ROW_NUMBER() OVER (
			           PARTITION BY t.session_id ORDER BY tr.ordinal DESC, i.ordinal DESC
			       ) AS rank
			FROM json_each(?) p
			JOIN threads t ON t.session_id = p.value
			JOIN turns tr ON tr.thread_id = t.id
			JOIN items i ON i.turn_id = tr.id
			WHERE instr(lower(CAST(i.payload_json AS TEXT)), lower(?)) > 0
		)
		SELECT session_id, id FROM matches WHERE rank = 1`, string(ids), query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := make(map[string]protocol.TurnID, len(summaries))
	for rows.Next() {
		var id string
		var turn protocol.TurnID
		if err := rows.Scan(&id, &turn); err != nil {
			return nil, err
		}
		byID[id] = turn
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, summary := range summaries {
		if turn := byID[summary.SessionID]; turn != "" {
			matches = append(matches, protocol.SessionSearchMatch{
				SessionID: summary.SessionID, TurnID: turn, Kind: "content",
			})
		}
	}
	return matches, nil
}

func (r *Repository) ThreadIDsForSessions(ctx context.Context, sessionIDs []string) (map[string][]protocol.ThreadID, error) {
	result := make(map[string][]protocol.ThreadID, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return result, nil
	}
	ids, err := json.Marshal(sessionIDs)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT session_id, id FROM threads
		WHERE session_id IN (SELECT value FROM json_each(?))
		ORDER BY session_id, created_at, id`, string(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID string
		var threadID protocol.ThreadID
		if err := rows.Scan(&sessionID, &threadID); err != nil {
			return nil, err
		}
		result[sessionID] = append(result[sessionID], threadID)
	}
	return result, rows.Err()
}
