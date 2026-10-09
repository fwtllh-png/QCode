package app

import (
	"context"
	"sync"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// deferredTerminalProjections holds Turns whose terminal envelope and
// operation commit are durable but whose outbox projection failed, either
// live or during startup recovery. Their outcome is already decided, so they
// are never rejected; the outbox stays pending and the settlement retrier on
// the dispatch loop projects it before later operations and in the
// background.
type deferredTerminalProjections struct {
	mu    sync.Mutex
	turns map[protocol.TurnID]deferredTerminalProjection
}

type deferredTerminalProjection struct {
	threadID protocol.ThreadID
	publish  func() error
}

// finishTerminal projects a Turn's terminal and settles its operation.
func (s *TurnService) finishTerminal(
	sink *runtimeSink,
	operation protocol.Operation,
	payload *protocol.StartTurnPayload,
) {
	r := s.runtime
	if sink.committed != nil && sink.committed.OperationCommitted {
		// The terminal envelope already committed the operation atomically.
		// Clear the local pending projection before publishing the terminal:
		// clients may request recovery as soon as they receive that event.
		r.commitLocal(operation.ID)
	}
	err := sink.publishTerminal()
	if err == nil {
		s.settleProjectedTerminal(payload.ThreadID, payload.TurnID)
		sink.commitOperation()
		return
	}
	if sink.committed != nil && sink.committed.OperationCommitted {
		s.deferTerminalProjection(payload.ThreadID, payload.TurnID, sink.publishTerminal, err)
		sink.commitOperation()
		return
	}
	r.rejectAndCommit(operation, err)
}

func (s *TurnService) settleProjectedTerminal(
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
) {
	r := s.runtime
	r.ArtifactService.PersistTerminalArtifactForTurn(
		context.Background(), threadID, turnID,
	)
	r.TurnQueueService.Drain(threadID)
}

// deferRecoveredTerminalProjection parks an outbox that startup recovery
// could not project.
func (s *TurnService) deferRecoveredTerminalProjection(
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
	err error,
) {
	s.deferTerminalProjection(threadID, turnID, func() error {
		return s.runtime.terminal.PublishPending(context.Background(), turnID)
	}, err)
}

func (s *TurnService) deferTerminalProjection(
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
	publish func() error,
	err error,
) {
	s.deferred.mu.Lock()
	if s.deferred.turns == nil {
		s.deferred.turns = make(map[protocol.TurnID]deferredTerminalProjection)
	}
	s.deferred.turns[turnID] = deferredTerminalProjection{
		threadID: threadID, publish: publish,
	}
	s.deferred.mu.Unlock()
	s.runtime.metrics.Error()
	if logger := s.runtime.logger; logger != nil {
		logger.Error(
			"committed turn terminal projection deferred",
			"turn_id", turnID, "error", err,
		)
	}
	s.runtime.OperationService.wakeSettlement()
}

func (s *TurnService) hasDeferredTerminalProjections() bool {
	s.deferred.mu.Lock()
	defer s.deferred.mu.Unlock()
	return len(s.deferred.turns) != 0
}

// retryDeferredTerminalProjections runs on the dispatch loop, so a pending
// terminal is projected before any later operation for its thread.
func (s *TurnService) retryDeferredTerminalProjections() {
	s.deferred.mu.Lock()
	pending := make(map[protocol.TurnID]deferredTerminalProjection, len(s.deferred.turns))
	for turnID, projection := range s.deferred.turns {
		pending[turnID] = projection
	}
	s.deferred.mu.Unlock()
	for turnID, projection := range pending {
		if err := projection.publish(); err != nil {
			if logger := s.runtime.logger; logger != nil {
				logger.Error(
					"committed turn terminal projection retry failed",
					"turn_id", turnID, "error", err,
				)
			}
			continue
		}
		s.deferred.mu.Lock()
		delete(s.deferred.turns, turnID)
		s.deferred.mu.Unlock()
		s.settleProjectedTerminal(projection.threadID, turnID)
	}
}
