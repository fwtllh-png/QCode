package app

import (
	"context"

	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (r *Runtime) TurnWithdrawn(ctx context.Context, thread protocol.ThreadID, turn protocol.TurnID) (bool, error) {
	store, ok := r.contextRebaseStore.(agentcontext.TurnContextStore)
	if !ok {
		return false, nil
	}
	return store.TurnWithdrawn(ctx, thread, turn)
}

// WithdrawTurn holds admission until execution and terminal publication settle.
// The durable tombstone is authoritative even if event publication is retried.
func (r *Runtime) WithdrawTurn(ctx context.Context, request protocol.TurnWithdrawRequest) error {
	if err := request.Validate(); err != nil {
		return runtimeProblem(protocol.CodeInvalidArgument, err.Error(), err)
	}
	manager, ok := r.engine.(interface {
		WithdrawTurn(context.Context, protocol.ThreadID, protocol.TurnID) error
	})
	store, stored := r.contextRebaseStore.(agentcontext.TurnContextStore)
	if !ok || !stored {
		return runtimeProblem(protocol.CodeUnavailable, "Turn withdrawal is unavailable", nil)
	}
	unlockMutations := r.SessionService.lockMutations()
	current, err := r.SessionStatus(ctx, request.SessionID)
	if err != nil {
		unlockMutations()
		return err
	}
	thread := current.ThreadID
	threadIDs, err := r.sessionLifecycle.ThreadIDs(ctx, request.SessionID)
	if err != nil {
		unlockMutations()
		return err
	}
	owned := make(map[protocol.ThreadID]bool, len(threadIDs))
	for _, id := range threadIDs {
		owned[id] = true
	}
	active, running := r.active.LookupThread(thread)
	if current.Archived || current.LatestTurnID != request.TurnID ||
		(running && active.TurnID != request.TurnID) {
		unlockMutations()
		return runtimeProblem(protocol.CodeConflict, "Only the latest Turn in the active Session Thread can be withdrawn", nil)
	}
	endWithdrawal, err := r.OperationService.beginWithdrawal(thread, request.TurnID, owned)
	unlockMutations()
	if err != nil {
		return err
	}
	defer endWithdrawal()
	if _, found, err := store.TurnBaseline(ctx, thread, request.TurnID); err != nil || !found {
		if err != nil {
			return err
		}
		return runtimeProblem(protocol.CodeConflict, "This Turn has no saved pre-Turn context; it cannot be withdrawn", nil)
	}
	for _, owner := range threadIDs {
		handle, active := r.active.LookupThread(owner)
		if !active {
			continue
		}
		operation, createErr := protocol.NewOperation(&protocol.CancelTurnPayload{
			ThreadID: owner, TurnID: handle.TurnID,
			ItemID: protocol.ItemID(sessionDerivedID("item", string(handle.TurnID), "withdraw-cancel")),
			Reason: protocol.CancelReasonUserInterrupted,
		})
		if createErr != nil {
			return createErr
		}
		outcome := r.TurnService.Cancel(operation, operation.Payload.(*protocol.CancelTurnPayload))
		if _, stillActive := r.active.LookupThread(owner); outcome.Problem != nil && stillActive {
			return outcome.Problem
		}
	}
	for {
		pending, changed, err := r.OperationService.pendingForThreads(owned)
		if err != nil {
			return err
		}
		if !pending {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-r.ctx.Done():
			return ErrClosed
		}
	}
	if _, active := r.active.LookupThread(thread); active {
		return retryableProblem(protocol.CodeConflict, "Turn is still settling")
	}
	releaseWorkspace, err := r.active.acquireWorkspace()
	if err != nil {
		return err
	}
	defer releaseWorkspace()
	if err := manager.WithdrawTurn(ctx, thread, request.TurnID); err != nil {
		return err
	}
	key := string(thread) + ":" + string(request.TurnID)
	return r.publishStable(
		protocol.OperationID(sessionDerivedID("op", key, "withdraw")),
		thread, request.TurnID,
		protocol.ItemID(sessionDerivedID("item", key, "withdraw")),
		protocol.EventID(sessionDerivedID("event", key, "withdraw")),
		&protocol.TurnWithdrawnData{},
	)
}

func (r *Runtime) requireRetainedTurn(ctx context.Context, thread protocol.ThreadID, turn protocol.TurnID) error {
	withdrawn, err := r.TurnWithdrawn(ctx, thread, turn)
	if err != nil {
		return err
	}
	if withdrawn {
		return runtimeProblem(protocol.CodeConflict, "Turn was withdrawn and cannot be recovered", nil)
	}
	return nil
}
