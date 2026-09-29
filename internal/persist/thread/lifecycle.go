package thread

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/observability/usage"
	sessionstate "github.com/fwtllh-png/QCode/internal/persist/session"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type Lifecycle struct {
	state         *state.Store
	db            *sql.DB
	workspaceRoot string
}

func NewLifecycle(store *state.Store) *Lifecycle {
	if store == nil {
		return &Lifecycle{}
	}
	return &Lifecycle{state: store, db: store.SQLite().DB()}
}

// NewWorkspaceLifecycle binds durable recovery to one Workspace while sharing
// the process-wide SQLite store and event sequence.
func NewWorkspaceLifecycle(
	store *state.Store,
	workspaceRoot string,
) *Lifecycle {
	lifecycle := NewLifecycle(store)
	if strings.TrimSpace(workspaceRoot) != "" {
		if normalized, err := sessionstate.NormalizeWorkspaceRoot(workspaceRoot); err == nil {
			workspaceRoot = normalized
		}
	}
	lifecycle.workspaceRoot = workspaceRoot
	return lifecycle
}

func (l *Lifecycle) Recover(ctx context.Context) (app.RecoveryState, error) {
	if l.state == nil || l.db == nil {
		return app.RecoveryState{}, errors.New("thread lifecycle state store is required")
	}
	events, err := l.recoveryEvents(ctx)
	if err != nil {
		return app.RecoveryState{}, fmt.Errorf("replay lifecycle events: %w", err)
	}
	recovery := app.RecoveryState{
		Terminals:          make(map[protocol.TurnID]protocol.EventKind),
		PendingApprovals:   make(map[string]app.PendingApproval),
		PendingInputs:      make(map[string]app.PendingInput),
		PendingQueuedTurns: make(map[string]protocol.QueuedTurn),
		PendingOperations:  make(map[protocol.OperationID]app.PendingOperation),
		ToolItems:          make(map[app.EventItemOwner]protocol.ItemID),
	}
	confirmed := make(map[protocol.OperationID]app.CommitReceipt)
	for _, event := range events {
		if err := l.Project(ctx, event); err != nil {
			return app.RecoveryState{}, fmt.Errorf("recover event %d projection: %w", event.Sequence, err)
		}
		if err := app.ApplyTurnQueueEvent(recovery.PendingQueuedTurns, event); err != nil {
			return app.RecoveryState{}, fmt.Errorf(
				"recover event %d turn queue: %w",
				event.Sequence,
				err,
			)
		}
		recovery.LastSequence = max(recovery.LastSequence, event.Sequence)
		if protocol.IsTerminalEvent(event.Kind) {
			if existing, exists := recovery.Terminals[event.TurnID]; exists {
				return app.RecoveryState{}, fmt.Errorf(
					"%w: turn %s has terminal events %s and %s",
					ErrTerminal, event.TurnID, existing, event.Kind,
				)
			}
			recovery.Terminals[event.TurnID] = event.Kind
			for requestID, approval := range recovery.PendingApprovals {
				if approval.TurnID == event.TurnID {
					delete(recovery.PendingApprovals, requestID)
				}
			}
			for requestID, input := range recovery.PendingInputs {
				if input.TurnID == event.TurnID {
					delete(recovery.PendingInputs, requestID)
				}
			}
		}
		switch data := event.Data.(type) {
		case *protocol.ApprovalRequiredData:
			recovery.PendingApprovals[data.RequestID] = app.PendingApproval{
				RequestID: data.RequestID,
				ThreadID:  event.ThreadID,
				TurnID:    event.TurnID,
				ItemID:    event.ItemID,
				Data:      *data,
			}
		case *protocol.ApprovalResolvedData:
			delete(recovery.PendingApprovals, data.RequestID)
		case *protocol.InputRequiredData:
			recovery.PendingInputs[data.RequestID] = app.PendingInput{
				RequestID: data.RequestID,
				ThreadID:  event.ThreadID,
				TurnID:    event.TurnID,
				ItemID:    event.ItemID,
				Data:      *data,
			}
		case *protocol.InputResolvedData:
			delete(recovery.PendingInputs, data.RequestID)
		case *protocol.ToolResultData:
			if data.CallID != "" && event.ItemID != "" {
				recovery.ToolItems[app.EventItemOwner{
					TurnID:  event.TurnID,
					LocalID: data.CallID,
				}] = event.ItemID
			}
		}
		if confirmsOperation(event.Kind) {
			confirmed[event.OperationID] = app.CommitReceipt{
				OperationID:  event.OperationID,
				Status:       "committed",
				LastSequence: event.Sequence,
				CompletedAt:  event.CreatedAt,
			}
		}
	}
	last, err := l.state.LastSequence(ctx)
	if err != nil {
		return app.RecoveryState{}, err
	}
	recovery.LastSequence = max(recovery.LastSequence, last)
	for _, receipt := range confirmed {
		if err := l.Commit(ctx, receipt); err != nil && !errors.Is(err, ErrNotFound) {
			return app.RecoveryState{}, fmt.Errorf(
				"recover operation %s commit receipt: %w", receipt.OperationID, err,
			)
		}
	}

	pendingQuery := `
		SELECT operation.id, operation.session_id,
			COALESCE(operation.idempotency_key, ''), operation.request_json
		FROM operations AS operation`
	pendingArguments := []any{OperationAccepted}
	if l.workspaceRoot != "" {
		pendingQuery += `
		JOIN sessions AS session ON session.id = operation.session_id
		JOIN workspaces AS workspace ON workspace.id = session.workspace_id`
	}
	pendingQuery += ` WHERE operation.status = ?`
	if l.workspaceRoot != "" {
		pendingQuery += ` AND workspace.root_path = ?`
		pendingArguments = append(pendingArguments, l.workspaceRoot)
	}
	rows, err := l.db.QueryContext(ctx, pendingQuery, pendingArguments...)
	if err != nil {
		return app.RecoveryState{}, fmt.Errorf("read pending accepted operations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pending app.PendingOperation
		var canonical string
		if err := rows.Scan(
			&pending.ID,
			&pending.SessionID,
			&pending.IdempotencyKey,
			&canonical,
		); err != nil {
			return app.RecoveryState{}, err
		}
		pending.Canonical = json.RawMessage(canonical)
		recovery.PendingOperations[pending.ID] = pending
	}
	if err := rows.Err(); err != nil {
		return app.RecoveryState{}, err
	}
	if err := rows.Close(); err != nil {
		return app.RecoveryState{}, err
	}
	interruptedQuery := `
		SELECT operation.id, operation.session_id,
			COALESCE(operation.idempotency_key, ''), operation.request_json
		FROM turns AS turn
		JOIN operations AS operation ON operation.id = turn.operation_id
		JOIN sessions AS session ON session.id = operation.session_id
		JOIN workspaces AS workspace ON workspace.id = session.workspace_id
		LEFT JOIN turn_terminal_envelopes AS terminal ON terminal.turn_id = turn.id
		WHERE turn.status = 'active'
			AND operation.kind = ?
			AND operation.status = ?
			AND terminal.turn_id IS NULL`
	interruptedArguments := []any{
		protocol.OperationStartTurn,
		OperationCommitted,
	}
	if l.workspaceRoot != "" {
		interruptedQuery += ` AND workspace.root_path = ?`
		interruptedArguments = append(interruptedArguments, l.workspaceRoot)
	}
	interruptedQuery += ` ORDER BY turn.created_at, turn.id`
	rows, err = l.db.QueryContext(
		ctx,
		interruptedQuery,
		interruptedArguments...,
	)
	if err != nil {
		return app.RecoveryState{}, fmt.Errorf(
			"read interrupted active turns: %w",
			err,
		)
	}
	defer rows.Close()
	for rows.Next() {
		var pending app.PendingOperation
		var canonical string
		if err := rows.Scan(
			&pending.ID,
			&pending.SessionID,
			&pending.IdempotencyKey,
			&canonical,
		); err != nil {
			return app.RecoveryState{}, err
		}
		pending.Canonical = json.RawMessage(canonical)
		recovery.PendingOperations[pending.ID] = pending
	}
	if err := rows.Err(); err != nil {
		return app.RecoveryState{}, err
	}
	return recovery, nil
}

func (l *Lifecycle) recoveryEvents(
	ctx context.Context,
) ([]protocol.Event, error) {
	if l.workspaceRoot == "" {
		return l.state.Replay(ctx, 0)
	}
	return l.state.ReplayWorkspace(ctx, 0, l.workspaceRoot)
}

func (l *Lifecycle) Accept(
	ctx context.Context,
	operation protocol.Operation,
	idempotencyKey string,
	canonical json.RawMessage,
) (app.Acceptance, error) {
	if l.db == nil {
		return app.Acceptance{}, errors.New("thread lifecycle database is required")
	}
	var acceptance app.Acceptance
	err := withTx(ctx, l.db, func(tx *sql.Tx) error {
		threadID, turnID, itemID := protocol.OperationReferences(operation)
		var sessionID, sessionStatus, threadStatus string
		query := `
				SELECT t.session_id, s.status, t.status
				FROM threads t
				JOIN sessions s ON s.id = t.session_id
				JOIN workspaces w ON w.id = s.workspace_id
				WHERE t.id = ?`
		arguments := []any{threadID}
		if l.workspaceRoot != "" {
			query += ` AND w.root_path = ?`
			arguments = append(arguments, l.workspaceRoot)
		}
		if err := tx.QueryRowContext(
			ctx,
			query,
			arguments...,
		).Scan(&sessionID, &sessionStatus, &threadStatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}

		existing, found, err := lookupOperation(ctx, tx, "id = ?", operation.ID)
		if err != nil {
			return err
		}
		if found {
			if !bytes.Equal(existing.Request, canonical) {
				return ErrOperationConflict
			}
			acceptance = app.Acceptance{
				OperationID: existing.ID,
				Duplicate:   true,
				Committed:   existing.Status == OperationCommitted,
			}
			return nil
		}
		if idempotencyKey != "" {
			existing, found, err = lookupOperation(
				ctx, tx, "session_id = ? AND idempotency_key = ?", sessionID, idempotencyKey,
			)
			if err != nil {
				return err
			}
			if found {
				if !bytes.Equal(existing.Request, canonical) || existing.Kind != operation.Kind {
					return ErrOperationConflict
				}
				acceptance = app.Acceptance{
					OperationID: existing.ID,
					Duplicate:   true,
					Committed:   existing.Status == OperationCommitted,
				}
				return nil
			}
		}
		if sessionStatus != "open" || threadStatus != string(ThreadOpen) {
			return protocol.NewProblem(
				protocol.CodeConflict,
				"session or thread is archived",
				false,
				nil,
			)
		}

		now := time.Now().UTC()
		_, err = tx.ExecContext(ctx, `
			INSERT INTO operations(
				id, session_id, idempotency_key, kind, status, request_json, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			operation.ID, sessionID, nullString(idempotencyKey), operation.Kind,
			OperationAccepted, canonical, timestamp(operation.CreatedAt), timestamp(now),
		)
		if err != nil {
			return fmt.Errorf("persist accepted operation: %w", err)
		}
		if operation.Kind == protocol.OperationStartTurn {
			if err := acceptTurn(ctx, tx, operation, threadID, turnID, itemID, canonical, now); err != nil {
				return err
			}
		} else {
			if err := acceptItem(ctx, tx, turnID, itemID, string(operation.Kind), canonical, now); err != nil {
				return err
			}
		}
		acceptance = app.Acceptance{OperationID: operation.ID}
		return nil
	})
	if errors.Is(err, ErrOperationConflict) {
		return app.Acceptance{}, app.ErrOperationConflict
	}
	if errors.Is(err, ErrActiveTurn) {
		return app.Acceptance{}, ErrActiveTurn
	}
	return acceptance, err
}

func (l *Lifecycle) Commit(ctx context.Context, receipt app.CommitReceipt) error {
	if receipt.OperationID == "" {
		return errors.New("commit receipt operation id is required")
	}
	if receipt.CompletedAt.IsZero() {
		receipt.CompletedAt = time.Now().UTC()
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return withTx(ctx, l.db, func(tx *sql.Tx) error {
		var status OperationStatus
		var response sql.NullString
		err := tx.QueryRowContext(
			ctx, "SELECT status, response_json FROM operations WHERE id = ?", receipt.OperationID,
		).Scan(&status, &response)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status == OperationCommitted {
			return nil
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE operations SET status = ?, response_json = ?, updated_at = ?
			WHERE id = ? AND status = ?`,
			OperationCommitted, encoded, timestamp(receipt.CompletedAt),
			receipt.OperationID, OperationAccepted,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrOperationConflict
		}
		return nil
	})
}

func (l *Lifecycle) Project(ctx context.Context, event protocol.Event) error {
	if l.db == nil {
		return errors.New("thread lifecycle database is required")
	}
	return withTx(ctx, l.db, func(tx *sql.Tx) error {
		var threadExists int
		if err := tx.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM threads WHERE id = ?",
			event.ThreadID,
		).Scan(&threadExists); err != nil {
			return err
		}
		// Session deletion intentionally keeps the durable event log for audit.
		// Recovery must not recreate or re-project relational state for events
		// whose owning Thread has since been deleted.
		if threadExists == 0 {
			return nil
		}
		if err := usage.ProjectTx(ctx, tx, event); err != nil {
			return fmt.Errorf("project usage lifecycle: %w", err)
		}
		_, err := tx.ExecContext(
			ctx, "UPDATE threads SET updated_at = ? WHERE id = ?",
			timestamp(event.CreatedAt), event.ThreadID,
		)
		if err != nil {
			return err
		}
		switch event.Kind {
		case protocol.EventThreadCompacted, protocol.EventThreadForked, protocol.EventTurnReverted,
			protocol.EventTurnCanceled:
			if err := updateExistingItem(ctx, tx, event); err != nil {
				return err
			}
		case protocol.EventToolResult, protocol.EventApprovalRequired, protocol.EventApprovalResolved,
			protocol.EventInputRequired, protocol.EventInputResolved,
			protocol.EventCommandExecution, protocol.EventHostCommand:
			if err := upsertEventItem(ctx, tx, event); err != nil {
				return err
			}
		}
		switch event.Kind {
		case protocol.EventTurnCompleted, protocol.EventTurnFailed, protocol.EventTurnCanceled:
			status := terminalStatus(event)
			result, err := tx.ExecContext(ctx, `
				UPDATE turns SET status = ?, updated_at = ?, completed_at = ?
				WHERE id = ? AND status = ?`,
				status, timestamp(event.CreatedAt), timestamp(event.CreatedAt),
				event.TurnID, TurnActive,
			)
			if err != nil {
				return err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if affected == 0 {
				var current TurnStatus
				if err := tx.QueryRowContext(
					ctx, "SELECT status FROM turns WHERE id = ?", event.TurnID,
				).Scan(&current); err != nil {
					return err
				}
				if current == TurnFailed && status == TurnBlocked {
					_, err := tx.ExecContext(ctx, `
						UPDATE turns SET status = ?, updated_at = ?, completed_at = ?
						WHERE id = ? AND status = ?`,
						TurnBlocked,
						timestamp(event.CreatedAt),
						timestamp(event.CreatedAt),
						event.TurnID,
						TurnFailed,
					)
					return err
				}
				if current != status {
					return ErrTerminal
				}
			}
		case protocol.EventOperationRejected:
			_, err := tx.ExecContext(ctx, `
				UPDATE turns SET status = ?, updated_at = ?, completed_at = ?
				WHERE id = ? AND operation_id = ? AND status = ?`,
				TurnFailed, timestamp(event.CreatedAt), timestamp(event.CreatedAt),
				event.TurnID, event.OperationID, TurnActive,
			)
			if err != nil {
				return err
			}
		case protocol.EventThreadForked:
			data, ok := event.Data.(*protocol.ThreadForkedData)
			if !ok {
				return errors.New("thread fork event has unexpected data")
			}
			var sessionID string
			if err := tx.QueryRowContext(
				ctx, "SELECT session_id FROM threads WHERE id = ?", event.ThreadID,
			).Scan(&sessionID); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO threads(
					id, session_id, parent_thread_id, title, status,
					source_cursor, created_at, updated_at
				) VALUES (?, ?, ?, '', ?, ?, ?, ?)
				ON CONFLICT(id) DO NOTHING`,
				data.NewThreadID, sessionID, event.ThreadID, ThreadOpen,
				int64(data.SourceCursor),
				timestamp(event.CreatedAt), timestamp(event.CreatedAt),
			)
			if err != nil {
				return err
			}
		case protocol.EventCheckpointForked:
			data, ok := event.Data.(*protocol.CheckpointForkedData)
			if !ok {
				return errors.New("checkpoint fork event has unexpected data")
			}
			var sessionID string
			if err := tx.QueryRowContext(
				ctx, "SELECT session_id FROM threads WHERE id = ?", event.ThreadID,
			).Scan(&sessionID); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO threads(
					id, session_id, parent_thread_id, title, status,
					source_cursor, created_at, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(id) DO NOTHING`,
				data.NewThreadID, sessionID, event.ThreadID, data.Title,
				ThreadOpen, int64(data.SourceCursor),
				timestamp(event.CreatedAt), timestamp(event.CreatedAt),
			)
			if err != nil {
				return err
			}
		case protocol.EventThreadCompacted:
			_, err := tx.ExecContext(
				ctx, "UPDATE threads SET updated_at = ? WHERE id = ?",
				timestamp(event.CreatedAt), event.ThreadID,
			)
			return err
		case protocol.EventTurnReverted:
			data, ok := event.Data.(*protocol.TurnRevertedData)
			if !ok {
				return errors.New("turn revert event has unexpected data")
			}
			var targetCount int
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM turns WHERE id = ? AND thread_id = ?`,
				data.TargetTurnID, event.ThreadID,
			).Scan(&targetCount); err != nil {
				return err
			}
			if targetCount != 1 {
				return ErrNotFound
			}
			_, err := tx.ExecContext(ctx, `
				UPDATE threads SET updated_at = ? WHERE id = ?`,
				timestamp(event.CreatedAt), event.ThreadID,
			)
			return err
		}
		return nil
	})
}

func acceptTurn(
	ctx context.Context,
	tx *sql.Tx,
	operation protocol.Operation,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
	itemID protocol.ItemID,
	canonical json.RawMessage,
	now time.Time,
) error {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO turns(
			id, thread_id, operation_id, ordinal, status, created_at, updated_at
		)
		SELECT ?, ?, ?,
			COALESCE((SELECT MAX(ordinal) + 1 FROM turns WHERE thread_id = ?), 0),
			?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM turns WHERE thread_id = ? AND status = ?
		)`,
		turnID, threadID, operation.ID, threadID, TurnActive,
		timestamp(operation.CreatedAt), timestamp(now), threadID, TurnActive,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrActiveTurn
	}
	return acceptItem(ctx, tx, turnID, itemID, string(operation.Kind), canonical, now)
}

func acceptItem(
	ctx context.Context,
	tx *sql.Tx,
	turnID protocol.TurnID,
	itemID protocol.ItemID,
	kind string,
	canonical json.RawMessage,
	now time.Time,
) error {
	var ordinal uint64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(ordinal) + 1, 0) FROM items WHERE turn_id = ?`,
		turnID,
	).Scan(&ordinal); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO items(id, turn_id, ordinal, kind, payload_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		itemID, turnID, ordinal, kind, canonical, timestamp(now), timestamp(now),
	)
	if err != nil {
		return fmt.Errorf("persist operation item: %w", err)
	}
	return nil
}

func updateExistingItem(ctx context.Context, tx *sql.Tx, event protocol.Event) error {
	payload, err := json.Marshal(event.Data)
	if err != nil {
		return fmt.Errorf("encode projected lifecycle event: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE items SET kind = ?, payload_json = ?, updated_at = ?
		WHERE id = ? AND turn_id = ?`,
		event.Kind, payload, timestamp(event.CreatedAt), event.ItemID, event.TurnID,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	// TurnCanceled may race Close()-style cancels that never created a
	// cancel item; turn status update below still applies.
	if affected != 1 && event.Kind != protocol.EventTurnCanceled {
		return ErrNotFound
	}
	return nil
}

// upsertEventItem projects durable tool/approval/input events onto first-class
// items (F5). Creates the item when absent, otherwise updates payload/kind.
func upsertEventItem(ctx context.Context, tx *sql.Tx, event protocol.Event) error {
	if event.ItemID == "" || event.TurnID == "" {
		return errors.New("event item and turn are required for item projection")
	}
	payload, err := json.Marshal(event.Data)
	if err != nil {
		return fmt.Errorf("encode projected lifecycle event: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE items SET kind = ?, payload_json = ?, updated_at = ?
		WHERE id = ? AND turn_id = ?`,
		event.Kind, payload, timestamp(event.CreatedAt), event.ItemID, event.TurnID,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 1 {
		return nil
	}
	return acceptItem(ctx, tx, event.TurnID, event.ItemID, string(event.Kind), payload, event.CreatedAt)
}

func lookupOperation(
	ctx context.Context,
	tx *sql.Tx,
	predicate string,
	arguments ...any,
) (OperationRecord, bool, error) {
	var value OperationRecord
	var key sql.NullString
	var request string
	err := tx.QueryRowContext(ctx, `
		SELECT id, session_id, idempotency_key, kind, status, request_json
		FROM operations WHERE `+predicate, arguments...,
	).Scan(
		&value.ID, &value.SessionID, &key, &value.Kind, &value.Status, &request,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return OperationRecord{}, false, nil
	}
	if err != nil {
		return OperationRecord{}, false, err
	}
	if key.Valid {
		value.IdempotencyKey = key.String
	}
	value.Request = json.RawMessage(request)
	return value, true, nil
}

func terminalStatus(event protocol.Event) TurnStatus {
	switch event.Kind {
	case protocol.EventTurnCompleted:
		return TurnCompleted
	case protocol.EventTurnFailed:
		if data, ok := event.Data.(*protocol.TurnFailedData); ok &&
			(data.Convergence != nil || recoverableTurnFault(data.Fault)) {
			return TurnBlocked
		}
		return TurnFailed
	default:
		return TurnCanceled
	}
}

func recoverableTurnFault(fault *protocol.FaultMetadata) bool {
	return protocol.FaultAllowsTurnRecovery(fault)
}

func confirmsOperation(kind protocol.EventKind) bool {
	return protocol.IsTerminalEvent(kind) ||
		kind == protocol.EventOperationRejected ||
		kind == protocol.EventTurnSteered ||
		kind == protocol.EventTurnQueued ||
		kind == protocol.EventQueuedTurnUpdated ||
		kind == protocol.EventQueuedTurnRemoved ||
		kind == protocol.EventApprovalResolved ||
		kind == protocol.EventThreadCompacted ||
		kind == protocol.EventThreadForked ||
		kind == protocol.EventTurnReverted
}

func withTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) (err error) {
	if db == nil {
		return errors.New("database is required")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
