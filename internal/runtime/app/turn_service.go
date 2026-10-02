package app

import (
	"context"
	"errors"
	"fmt"
	"sync"

	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// TurnService owns active Turn leases and execution goroutine lifetime. The
// Runtime facade delegates Turn admission and control to this owner.
type TurnService struct {
	runtime  *Runtime
	active   *ActiveTurnRegistry
	workers  sync.WaitGroup
	deferred deferredTerminalProjections
}

// trackWorker registers work outside Turn execution that Runtime shutdown
// must wait for before closing the engine.
func (s *TurnService) trackWorker() func() {
	s.workers.Add(1)
	return s.workers.Done
}

func (s *TurnService) waitWorkers() { s.workers.Wait() }

type turnExecution func(context.Context, *protocol.StartTurnPayload, EngineSink) error

func (s *TurnService) Start(operation protocol.Operation, payload *protocol.StartTurnPayload) OperationOutcome {
	engine := s.runtime.engine
	return s.start(operation, payload, func(
		ctx context.Context,
		payload *protocol.StartTurnPayload,
		sink EngineSink,
	) error {
		return startTurnSafely(engine, ctx, payload, sink)
	})
}

// SettleInterruptedStart closes a recovered StartTurn that was accepted but
// never reached its engine. It commits a retryable failed terminal instead of
// re-running a prompt the user may no longer expect to execute.
func (s *TurnService) SettleInterruptedStart(
	operation protocol.Operation,
	payload *protocol.StartTurnPayload,
) OperationOutcome {
	return s.start(operation, payload, func(
		context.Context,
		*protocol.StartTurnPayload,
		EngineSink,
	) error {
		return protocol.NewFault(
			protocol.CodeUnavailable,
			"turn was interrupted before it started; start it again",
			true,
			protocol.FaultMetadata{
				Origin:      protocol.FaultOriginRuntime,
				Disposition: protocol.FaultRetryTurn,
				SideEffects: protocol.SideEffectNone,
			},
			nil,
		)
	})
}

func (s *TurnService) start(
	operation protocol.Operation,
	payload *protocol.StartTurnPayload,
	execute turnExecution,
) OperationOutcome {
	r := s.runtime
	if err := errors.Join(r.ArtifactService.PrepareStartPayload(r.ctx, r.workspaceRoot, payload), (StartTurnHandler{Runtime: r}).validateStart(payload)); err != nil {
		return finishOutcome(err)
	}
	if _, finished := r.EventService.terminalKind(payload.TurnID); finished {
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
	s.workers.Add(1)
	go s.run(turnContext, cancel, lease, operation, payload, execute)
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
	execute turnExecution,
) {
	r := s.runtime
	defer s.workers.Done()
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
	err := execute(turnContext, payload, sink)
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
			r.rejectAndCommit(operation, errors.Join(err, terminalErr))
			return
		}
	}
	if sink.terminalCommitAttempted && sink.terminal == nil {
		releaseActive()
		if err == nil {
			err = errors.New("terminal envelope commit failed")
		}
		r.rejectAndCommit(operation, err)
		return
	}
	if errors.Is(turnContext.Err(), context.Canceled) {
		// The engine owns the decision; the terminal projection re-projects
		// live events, so it must stay under the operation that emitted them.
		// Attributing it to the cancel operation makes stable commentary
		// re-projection collide with its live event and silently strands the
		// outbox (turn row stays active, queue never drains).
		releaseActive()
		s.finishTerminal(sink, operation, payload)
		return
	}
	if sink.terminal == nil {
		releaseActive()
		if err == nil {
			err = errors.New("turn engine returned without terminal material")
		}
		r.rejectAndCommit(operation, err)
		return
	}
	releaseActive()
	s.finishTerminal(sink, operation, payload)
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
