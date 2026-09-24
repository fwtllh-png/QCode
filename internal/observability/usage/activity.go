package usage

import (
	"context"
	"fmt"

	sessionstate "github.com/fwtllh-png/QCode/internal/persist/session"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// Activity counts durable execution events independently of provider billing.
// ToolCalls counts settled calls, including control and failed calls, once per
// turn/item. An unfinished call is not yet part of this count.
type Activity struct {
	Turns     uint64 `json:"turns"`
	Completed uint64 `json:"completed"`
	Failed    uint64 `json:"failed"`
	Canceled  uint64 `json:"canceled"`
	ToolCalls uint64 `json:"tool_calls"`
}

func (r *Repository) queryActivity(ctx context.Context, filter Query) (*Activity, error) {
	// Provider/model and billing-time filters cannot describe execution events.
	if filter.Provider != "" || filter.Model != "" ||
		!filter.Start.IsZero() || !filter.End.IsZero() {
		return nil, nil
	}
	query := `
		SELECT e.kind, e.turn_id, e.item_id
		FROM event_index e
		JOIN threads th ON th.id = e.thread_id
		JOIN sessions s ON s.id = th.session_id
		JOIN workspaces w ON w.id = s.workspace_id
		WHERE e.kind IN (?, ?, ?, ?, ?)`
	args := []any{
		protocol.EventTurnStarted, protocol.EventTurnCompleted,
		protocol.EventTurnFailed, protocol.EventTurnCanceled, protocol.EventToolResult,
	}
	add := func(clause string, values ...any) {
		query += " AND " + clause
		args = append(args, values...)
	}
	if filter.SessionID != "" {
		if filter.IncludeChildren {
			add(`(th.session_id = ? OR EXISTS (
				SELECT 1 FROM agent_nodes child
				WHERE child.session_id = ? AND child.thread_id = th.id
			))`, filter.SessionID, filter.SessionID)
		} else {
			add("th.session_id = ?", filter.SessionID)
		}
	}
	if filter.ThreadID != "" {
		add("e.thread_id = ?", filter.ThreadID)
	}
	if filter.TurnID != "" {
		add("e.turn_id = ?", filter.TurnID)
	}
	if filter.WorkspaceRoot != "" {
		root, err := sessionstate.NormalizeWorkspaceRoot(filter.WorkspaceRoot)
		if err != nil {
			return nil, err
		}
		add("w.root_path = ?", root)
	}
	// Turn lifecycle events have one identity per kind/turn; tool results use
	// the protocol item identity, so duplicate delivery never inflates totals.
	query = `SELECT kind, COUNT(*) FROM (
		SELECT kind, turn_id,
			CASE WHEN kind = 'tool.result' THEN item_id ELSE '' END AS identity
		FROM (` + query + `)
		GROUP BY kind, turn_id, identity
	) GROUP BY kind`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query execution activity: %w", err)
	}
	defer rows.Close()
	value := &Activity{}
	for rows.Next() {
		var kind protocol.EventKind
		var count uint64
		if err := rows.Scan(&kind, &count); err != nil {
			return nil, err
		}
		switch kind {
		case protocol.EventTurnStarted:
			value.Turns = count
		case protocol.EventTurnCompleted:
			value.Completed = count
		case protocol.EventTurnFailed:
			value.Failed = count
		case protocol.EventTurnCanceled:
			value.Canceled = count
		case protocol.EventToolResult:
			value.ToolCalls = count
		}
	}
	return value, rows.Err()
}
