package app

import (
	"context"
	"errors"
	"strconv"
	"sync"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// unsettledOperation is an accepted operation whose outcome is decided but
// whose durable settlement has not finished. An accepted operation must end
// committed or rejected, so a failed step keeps the remainder here and the
// dispatch loop retries it until the commit receipt is durable.
type unsettledOperation struct {
	operation protocol.Operation
	// rejection is emitted as OperationRejected before the commit.
	rejection error
	// events are the committed outcome events; emitted counts those already
	// projected.
	events  []protocol.EventData
	emitted int
	// drain advances the Thread's turn queue once the operation commits.
	drain protocol.ThreadID
}

type operationSettlements struct {
	mu      sync.Mutex
	pending []unsettledOperation
	wake    chan struct{}
}

// settle drives an accepted operation to its durable commit, parking the
// remainder for retry when a step fails.
func (s *OperationService) settle(settlement unsettledOperation) {
	if err := s.advance(&settlement); err != nil {
		s.park(settlement, err)
	}
}

// advance resumes from the first unfinished step. Settlement events carry
// identities derived from the operation, so a retry after an append whose
// projection failed re-projects that event instead of appending a duplicate.
func (s *OperationService) advance(settlement *unsettledOperation) error {
	sink := &runtimeSink{runtime: s.runtime, operation: settlement.operation}
	operationID := settlement.operation.ID
	if settlement.rejection != nil {
		if err := sink.emitSettlement(
			SettlementEventID(operationID, "rejected"),
			operationRejection(settlement.rejection),
		); err != nil {
			return err
		}
		settlement.rejection = nil
	}
	for settlement.emitted < len(settlement.events) {
		slot := "outcome/" + strconv.Itoa(settlement.emitted)
		if err := sink.emitSettlement(
			SettlementEventID(operationID, slot),
			settlement.events[settlement.emitted],
		); err != nil {
			return err
		}
		settlement.emitted++
	}
	if s.runtime.lifecycle != nil {
		if err := s.runtime.lifecycle.Commit(
			context.Background(),
			s.operationCommitReceipt(operationID),
		); err != nil {
			return err
		}
	}
	s.commitLocal(operationID)
	if settlement.drain != "" {
		s.runtime.TurnQueueService.Drain(settlement.drain)
	}
	return nil
}

func (s *OperationService) park(settlement unsettledOperation, err error) {
	s.settlements.mu.Lock()
	s.settlements.pending = append(s.settlements.pending, settlement)
	s.settlements.mu.Unlock()
	s.runtime.metrics.Error()
	if s.runtime.logger != nil {
		s.runtime.logger.Error(
			"runtime operation settlement deferred",
			"operation_id", settlement.operation.ID,
			"error", err,
		)
	}
	s.wakeSettlement()
}

// retryUnsettled runs on the dispatch loop, so a parked settlement lands
// before any later operation is dispatched.
func (s *OperationService) retryUnsettled() {
	s.settlements.mu.Lock()
	pending := s.settlements.pending
	s.settlements.pending = nil
	s.settlements.mu.Unlock()
	var failed []unsettledOperation
	for _, settlement := range pending {
		if err := s.advance(&settlement); err != nil {
			if s.runtime.logger != nil {
				s.runtime.logger.Error(
					"runtime operation settlement retry failed",
					"operation_id", settlement.operation.ID,
					"error", err,
				)
			}
			failed = append(failed, settlement)
		}
	}
	if len(failed) == 0 {
		return
	}
	s.settlements.mu.Lock()
	s.settlements.pending = append(failed, s.settlements.pending...)
	s.settlements.mu.Unlock()
}

func (s *OperationService) hasUnsettled() bool {
	s.settlements.mu.Lock()
	defer s.settlements.mu.Unlock()
	return len(s.settlements.pending) != 0
}

// wakeSettlement lets the dispatch loop arm its retry timer for work parked
// outside the loop, such as by a Turn worker.
func (s *OperationService) wakeSettlement() {
	select {
	case s.settlements.wake <- struct{}{}:
	default:
	}
}

// commit settles an operation whose outcome needs no further events.
func (s *OperationService) commit(operation protocol.Operation) {
	s.settle(unsettledOperation{operation: operation})
}

// rejectAndCommit settles a rejected operation. A rejected queued StartTurn
// frees its Thread without a terminal, so the queue advances once it commits.
func (s *OperationService) rejectAndCommit(
	operation protocol.Operation,
	err error,
) {
	if err == nil {
		err = errors.New("operation rejected without a cause")
	}
	settlement := unsettledOperation{operation: operation, rejection: err}
	if payload, ok := operation.Payload.(*protocol.StartTurnPayload); ok &&
		payload.QueueID != "" {
		settlement.drain = payload.ThreadID
	}
	s.settle(settlement)
}

// emitSettlement publishes under a stable identity when the event store can
// resolve one; a store without identities cannot deduplicate a retry.
func (s *runtimeSink) emitSettlement(
	eventID protocol.EventID,
	data protocol.EventData,
) error {
	if _, ok := s.runtime.events.(EventIdentityStore); ok {
		return s.EmitStable(eventID, data)
	}
	return s.Emit(data)
}
