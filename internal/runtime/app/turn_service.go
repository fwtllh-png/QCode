package app

import (
	"context"
	"errors"
	"fmt"

	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (s *TurnService) Start(operation protocol.Operation, payload *protocol.StartTurnPayload) OperationOutcome {
	r := s.runtime
	if err := errors.Join(r.ArtifactService.PrepareStartPayload(r.ctx, r.workspaceRoot, payload), (StartTurnHandler{Runtime: r}).validateStart(payload)); err != nil {
		return finishOutcome(err)
	}
	r.EventService.mu.Lock()
	_, finished := r.terminals[payload.TurnID]
	r.EventService.mu.Unlock()
	if finished {
		return finishOutcome(errors.New("turn already has a terminal event"))
	}
	turnContext, cancel := context.WithCancel(r.ctx)
	lease, err := r.active.Reserve(payload.ThreadID, payload.TurnID, operation.ID, payload.ItemID)
	if err != nil {
		cancel()
		return finishOutcome(err)
	}
	if err := r.active.BindControl(payload.TurnID, cancel); err != nil {
		_ = r.active.Release(lease)
		cancel()
		return finishOutcome(err)
	}
	r.workers.Add(1)
	go s.run(turnContext, cancel, lease, operation, payload)
	return OperationOutcome{
		Kind: OutcomeAsync, CommitMode: CommitDeferred,
		Async: &AsyncTurn{
			ThreadID: payload.ThreadID, TurnID: payload.TurnID,
			OperationID: operation.ID, ItemID: payload.ItemID,
		},
	}
}

func (s *TurnService) run(
	turnContext context.Context,
	cancel context.CancelFunc,
	lease ActiveTurnLease,
	operation protocol.Operation,
	payload *protocol.StartTurnPayload,
) {
	r := s.runtime
	defer r.workers.Done()
	released := false
	releaseActive := func() {
		if released {
			return
		}
		_ = r.active.Release(lease)
		released = true
	}
	defer releaseActive()
	defer cancel()
	sink := &runtimeSink{
		runtime: r, operation: operation, deferTerminal: true,
	}
	err := startTurnSafely(r.engine, turnContext, payload, sink)
	if r.lifecycle != nil && !sink.terminalCommitAttempted {
		if !turnkernel.HasTerminalFacts(context.Background(), r.terminalStore, string(payload.TurnID)) &&
			r.rejectResumableOperation(operation, err, releaseActive) {
			return
		}
		if err == nil {
			err = errors.New("durable turn returned without atomic terminal commit")
		}
		if terminalErr := r.commitStartupTerminal(payload, sink, err); terminalErr != nil {
			releaseActive()
			err = errors.Join(err, terminalErr)
			if rejectErr := r.reject(operation, err); rejectErr == nil {
				r.commit(operation.ID)
			}
			return
		}
	}
	if sink.terminalCommitAttempted && sink.terminal == nil {
		releaseActive()
		if err == nil {
			err = errors.New("terminal envelope commit failed")
		}
		if rejectErr := r.reject(operation, err); rejectErr == nil {
			r.commit(operation.ID)
		}
		return
	}
	if errors.Is(turnContext.Err(), context.Canceled) {
		itemID, opID := payload.ItemID, operation.ID
		if stored, ok := r.active.LookupTurn(payload.TurnID); ok {
			if stored.ItemID != "" {
				itemID = stored.ItemID
			}
			if stored.OperationID != "" {
				opID = stored.OperationID
			}
		}
		releaseActive()
		// Engine owns the decision; TurnService binds cancel identities.
		if sink.publishTerminalAs(opID, itemID) == nil {
			r.ArtifactService.PersistTerminalArtifactForTurn(
				context.Background(), payload.ThreadID, payload.TurnID,
			)
			sink.commitOperation()
			r.TurnQueueService.Drain(payload.ThreadID)
		}
		return
	}
	if sink.terminal == nil {
		releaseActive()
		if err == nil {
			err = errors.New("turn engine returned without terminal material")
		}
		if rejectErr := r.reject(operation, err); rejectErr == nil {
			r.commit(operation.ID)
		}
		return
	}
	releaseActive()
	if terminalErr := sink.publishTerminal(); terminalErr == nil {
		r.ArtifactService.PersistTerminalArtifactForTurn(
			context.Background(), payload.ThreadID, payload.TurnID,
		)
		sink.commitOperation()
		r.TurnQueueService.Drain(payload.ThreadID)
	} else if rejectErr := r.reject(operation, terminalErr); rejectErr == nil {
		r.commit(operation.ID)
	}
}

func startTurnSafely(
	engine Engine,
	ctx context.Context,
	payload *protocol.StartTurnPayload,
	sink EngineSink,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = protocol.NewProblem(
				protocol.CodeInternal,
				"turn engine panicked",
				false,
				fmt.Errorf("turn engine panic: %v", recovered),
			)
		}
	}()
	return engine.StartTurn(ctx, payload, sink)
}

func (s *TurnService) Cancel(operation protocol.Operation, payload *protocol.CancelTurnPayload) OperationOutcome {
	r := s.runtime
	if _, active := r.active.LookupTurn(payload.TurnID); !active {
		return finishOutcome(turnNotActiveProblem())
	}
	sink := &runtimeSink{runtime: r, operation: operation}
	if err := r.engine.CancelTurn(r.ctx, payload, sink); err != nil {
		if !errors.Is(err, agentengine.ErrTurnCoordinatorNotActive) {
			return finishOutcome(err)
		}
	}
	cancel, err := r.active.RecordCancel(
		payload.TurnID,
		operation.ID,
		payload.ItemID,
	)
	if err != nil {
		return finishOutcome(err)
	}
	cancel()
	return OperationOutcome{Kind: OutcomeCommitted, CommitMode: CommitNow}
}
