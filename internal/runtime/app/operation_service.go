package app

import (
	"context"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// OperationService owns operation admission, idempotency, queueing, and
// lifecycle commit state. Runtime exposes its methods as a compatibility
// facade, but does not synchronize or mutate these fields directly.
// acceptedKeys and committed provide local deduplication only without a
// durable lifecycle.
type OperationService struct {
	runtime *Runtime

	mu                 sync.Mutex
	operations         chan acceptedOperation
	processed          uint64
	accepted           map[protocol.OperationID]PendingOperation
	acceptedKeys       map[string]protocol.OperationID
	committed          map[protocol.OperationID]PendingOperation
	accepting          bool
	workspaceOperation bool
	withdrawing        map[protocol.ThreadID]bool
	changed            chan struct{}
	settlements        operationSettlements
}

func (s *OperationService) snapshot() (processed uint64, pending int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending = len(s.accepted)
	if s.workspaceOperation {
		pending++
	}
	return s.processed, pending
}

func (s *OperationService) hasPendingSession(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, operation := range s.accepted {
		if operation.SessionID == sessionID {
			return true
		}
	}
	return false
}

func (s *OperationService) pendingSessions(wanted map[string]struct{}) map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := make(map[string]struct{})
	for _, operation := range s.accepted {
		if _, ok := wanted[operation.SessionID]; ok {
			sessions[operation.SessionID] = struct{}{}
		}
	}
	return sessions
}

func (s *OperationService) hasWorkspaceOperation() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workspaceOperation || len(s.withdrawing) != 0
}

func (s *OperationService) pendingOperations() []PendingOperation {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := make([]PendingOperation, 0, len(s.accepted))
	for _, operation := range s.accepted {
		operation.Canonical = append([]byte(nil), operation.Canonical...)
		pending = append(pending, operation)
	}
	return pending
}

func (s *OperationService) pendingSnapshot() map[protocol.OperationID]PendingOperation {
	pending := s.pendingOperations()
	result := make(map[protocol.OperationID]PendingOperation, len(pending))
	for _, operation := range pending {
		result[operation.ID] = operation
	}
	return result
}

// restore reinstalls accepted operations from a predecessor Runtime before
// activation.
func (s *OperationService) restore(pending map[protocol.OperationID]PendingOperation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for operationID, operation := range pending {
		s.accepted[operationID] = operation
		if s.runtime.lifecycle == nil && operation.IdempotencyKey != "" {
			s.acceptedKeys[operation.IdempotencyKey] = operationID
		}
	}
}

func (s *OperationService) open() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepting = true
}

// shutdown stops admission and closes the dispatch channel exactly once.
// neverOpened closes it for a Runtime whose admission was never opened.
func (s *OperationService) shutdown(neverOpened bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accepting {
		s.accepting = false
		close(s.operations)
	} else if neverOpened {
		close(s.operations)
	}
}

// serve dispatches accepted operations in admission order until shutdown.
// Pending settlements are retried before every dispatch and, while any remain
// and retryEvery is positive, on a timer so they land without new traffic.
func (s *OperationService) serve(
	dispatch func(acceptedOperation),
	retry func(),
	pending func() bool,
	retryEvery time.Duration,
) {
	var timer *time.Timer
	var due <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case accepted, open := <-s.operations:
			if !open {
				return
			}
			retry()
			dispatch(accepted)
			s.mu.Lock()
			s.processed++
			s.runtime.metrics.OperationProcessed()
			s.mu.Unlock()
		case <-due:
			due = nil
			retry()
		case <-s.settlements.wake:
		}
		if due == nil && retryEvery > 0 && pending() {
			timer = time.NewTimer(retryEvery)
			due = timer.C
		}
	}
}

// enqueueRecovered replays an already accepted operation; it bypasses
// admission because acceptance was durable before the restart.
func (s *OperationService) enqueueRecovered(ctx context.Context, accepted acceptedOperation) error {
	select {
	case s.operations <- accepted:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// trackWhileAccepting registers work with workers only while admission is
// open. Shutdown takes the same lock, so its Wait cannot miss the work.
func (s *OperationService) trackWhileAccepting(workers *sync.WaitGroup) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.accepting {
		return false
	}
	workers.Add(1)
	return true
}

func (s *OperationService) withdrawalInProgress() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.withdrawing) != 0
}

// beginWorkspaceOperation claims exclusive workspace access while no
// operation is accepted, queued, withdrawing, or already holding it.
func (s *OperationService) beginWorkspaceOperation() (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.accepting {
		return nil, ErrClosed
	}
	if s.workspaceOperation || len(s.withdrawing) != 0 || len(s.accepted) != 0 {
		return nil, retryableProblem(protocol.CodeConflict, "finish pending work before changing Git state")
	}
	if s.runtime.TurnQueueService.hasItems() {
		return nil, retryableProblem(protocol.CodeConflict, "finish queued work before changing Git state")
	}
	release, err := s.runtime.active.acquireWorkspace()
	if err != nil {
		return nil, err
	}
	s.workspaceOperation = true
	done := s.runtime.TurnService.trackWorker()
	return func() {
		s.mu.Lock()
		release()
		s.workspaceOperation = false
		s.mu.Unlock()
		done()
	}, nil
}

// beginWithdrawal blocks admission for the withdrawn Thread. Accepted
// operations may only be the withdrawn StartTurn itself; anything else owned
// by the Session must finish first.
func (s *OperationService) beginWithdrawal(
	thread protocol.ThreadID,
	turn protocol.TurnID,
	owned map[protocol.ThreadID]bool,
) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.accepting || s.workspaceOperation || len(s.withdrawing) != 0 {
		return nil, retryableProblem(protocol.CodeConflict, "Runtime is busy")
	}
	// An already accepted newer start cannot be overtaken by withdrawal.
	for _, pending := range s.accepted {
		operation, err := decodePendingOperation(pending)
		if err != nil {
			return nil, err
		}
		owner, pendingTurn, _ := protocol.OperationReferences(operation)
		if !owned[owner] || owner == thread &&
			(pendingTurn != turn || operation.Kind != protocol.OperationStartTurn) {
			return nil, retryableProblem(protocol.CodeConflict, "Finish pending operations before withdrawing")
		}
	}
	s.withdrawing[thread] = true
	s.changed = make(chan struct{})
	done := s.runtime.TurnService.trackWorker()
	return func() {
		s.mu.Lock()
		delete(s.withdrawing, thread)
		s.changed = nil
		s.mu.Unlock()
		done()
	}, nil
}

// pendingForThreads reports whether accepted operations still reference any
// owned Thread, with a channel closed on the next commit.
func (s *OperationService) pendingForThreads(
	owned map[protocol.ThreadID]bool,
) (bool, <-chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, value := range s.accepted {
		operation, err := decodePendingOperation(value)
		if err != nil {
			return false, nil, err
		}
		if owner, _, _ := protocol.OperationReferences(operation); owned[owner] {
			return true, s.changed, nil
		}
	}
	return false, s.changed, nil
}
