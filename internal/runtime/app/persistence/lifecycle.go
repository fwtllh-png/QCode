package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/persist/state"
	threadstate "github.com/fwtllh-png/QCode/internal/persist/thread"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// Lifecycle adapts durable thread records to Runtime recovery and operations.
type Lifecycle struct{ store *threadstate.Lifecycle }

var _ app.DurableLifecycle = (*Lifecycle)(nil)

func NewLifecycle(store *state.Store) *Lifecycle {
	return &Lifecycle{store: threadstate.NewLifecycle(store)}
}

func NewWorkspaceLifecycle(store *state.Store, workspaceRoot string) *Lifecycle {
	return &Lifecycle{store: threadstate.NewWorkspaceLifecycle(store, workspaceRoot)}
}

func (l *Lifecycle) Accept(ctx context.Context, operation protocol.Operation, idempotencyKey string, canonical json.RawMessage) (app.Acceptance, error) {
	accepted, err := l.store.Accept(ctx, operation, idempotencyKey, canonical)
	if errors.Is(err, threadstate.ErrOperationConflict) {
		return app.Acceptance{}, app.ErrOperationConflict
	}
	if errors.Is(err, threadstate.ErrActiveTurn) {
		return app.Acceptance{}, app.ErrActiveTurn
	}
	return app.Acceptance(accepted), err
}

func (l *Lifecycle) Commit(ctx context.Context, receipt app.CommitReceipt) error {
	return l.store.Commit(ctx, threadstate.CommitReceipt(receipt))
}

func (l *Lifecycle) Project(ctx context.Context, event protocol.Event) error {
	return l.store.Project(ctx, event)
}

func (l *Lifecycle) Recover(ctx context.Context) (app.RecoveryState, error) {
	if l == nil || l.store == nil {
		return app.RecoveryState{}, errors.New("thread lifecycle state store is required")
	}
	events, err := l.store.RecoveryEvents(ctx)
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
					threadstate.ErrTerminal, event.TurnID, existing, event.Kind,
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
	last, err := l.store.LastSequence(ctx)
	if err != nil {
		return app.RecoveryState{}, err
	}
	recovery.LastSequence = max(recovery.LastSequence, last)
	for _, receipt := range confirmed {
		if err := l.Commit(ctx, receipt); err != nil && !errors.Is(err, threadstate.ErrNotFound) {
			return app.RecoveryState{}, fmt.Errorf(
				"recover operation %s commit receipt: %w", receipt.OperationID, err,
			)
		}
	}

	pending, err := l.store.PendingOperations(ctx)
	if err != nil {
		return app.RecoveryState{}, err
	}
	for _, operation := range pending {
		recovery.PendingOperations[operation.ID] = app.PendingOperation{
			ID: operation.ID, SessionID: operation.SessionID,
			IdempotencyKey: operation.IdempotencyKey, Canonical: operation.Request,
		}
	}
	return recovery, nil
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
