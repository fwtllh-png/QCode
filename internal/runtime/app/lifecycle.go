package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

var (
	ErrOperationConflict = protocol.NewProblem(
		protocol.CodeConflict,
		"operation identity was reused with a different payload",
		false,
		nil,
	)
	ErrActiveTurn = protocol.NewProblem(
		protocol.CodeConflict,
		"thread already has an active turn",
		true,
		nil,
	)
)

// Acceptance is the durable result of accepting an operation. Duplicate
// operations are deliberately not dispatched to Engine.
type Acceptance struct {
	OperationID protocol.OperationID
	Duplicate   bool
	Committed   bool
}

type PendingOperation struct {
	ID             protocol.OperationID
	SessionID      string
	IdempotencyKey string
	Canonical      json.RawMessage
}

type EventItemOwner struct {
	TurnID  protocol.TurnID
	LocalID string
}

func eventItemOwner(turnID protocol.TurnID, localID string) EventItemOwner {
	return EventItemOwner{TurnID: turnID, LocalID: localID}
}

// RecoveryState is explicit input to Runtime's state machine. It contains only
// state required to continue allocating events and reject unsafe replays.
type RecoveryState struct {
	LastSequence       protocol.Cursor
	Terminals          map[protocol.TurnID]protocol.EventKind
	PendingApprovals   map[string]PendingApproval
	PendingInputs      map[string]PendingInput
	PendingQueuedTurns map[string]protocol.QueuedTurn
	PendingOperations  map[protocol.OperationID]PendingOperation
	// ToolItems remaps Turn-owned call IDs to ItemIDs so post-recovery
	// tool.result events keep a stable item identity (F5).
	ToolItems map[EventItemOwner]protocol.ItemID
}

type CommitReceipt struct {
	OperationID  protocol.OperationID `json:"operation_id"`
	Status       string               `json:"status"`
	LastSequence protocol.Cursor      `json:"last_sequence"`
	CompletedAt  time.Time            `json:"completed_at"`
}

func (r *Runtime) BindThreadSession(
	threadID protocol.ThreadID,
	sessionID string,
) error {
	manager, ok := r.engine.(*ThreadManager)
	if !ok {
		return errors.New("runtime engine does not support thread session binding")
	}
	return manager.BindSession(threadID, sessionID)
}

func (r *Runtime) RegisterChildThread(
	threadID protocol.ThreadID,
	spec ChildSpec,
) error {
	manager, ok := r.engine.(*ThreadManager)
	if !ok {
		return errors.New("runtime engine does not support child thread registration")
	}
	return manager.RegisterChild(threadID, spec)
}

func (r *Runtime) ReleaseThread(threadID protocol.ThreadID) {
	if manager, ok := r.engine.(*ThreadManager); ok {
		manager.Release(threadID)
	}
}

// DurableLifecycle persists operation acceptance and relational projections.
// Runtime remains the sole state machine; implementations only enforce durable
// compare-and-set transitions and rebuild recovery input.
type DurableLifecycle interface {
	Recover(context.Context) (RecoveryState, error)
	Accept(context.Context, protocol.Operation, string, json.RawMessage) (Acceptance, error)
	Project(context.Context, protocol.Event) error
	Commit(context.Context, CommitReceipt) error
}

func CanonicalOperationPayload(operation protocol.Operation) (json.RawMessage, error) {
	raw, err := json.Marshal(struct {
		Kind    protocol.OperationKind    `json:"kind"`
		Payload protocol.OperationPayload `json:"payload"`
	}{Kind: operation.Kind, Payload: operation.Payload})
	if err != nil {
		return nil, fmt.Errorf("encode operation payload: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("canonicalize operation payload: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonicalize operation payload: %w", err)
	}
	return canonical, nil
}
