package app

import (
	"context"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/artifact"
	sessionhistory "github.com/fwtllh-png/QCode/internal/persist/history"
	"github.com/fwtllh-png/QCode/internal/runtime/app/eventhub"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// SessionService owns Session lifecycle mutations and background title jobs.
type SessionService struct {
	runtime *Runtime
	// mutationMu serializes Session mutations with admission-changing
	// workspace operations; others take it only through lockMutations.
	mutationMu   sync.Mutex
	titleWorkers sync.WaitGroup
	// titleMu is a leaf lock guarding titleJobs.
	titleMu   sync.Mutex
	titleJobs map[string]sessionTitleJob
}

// lockMutations excludes Session mutations until the returned func runs.
func (s *SessionService) lockMutations() func() {
	s.mutationMu.Lock()
	return s.mutationMu.Unlock
}

func (s *SessionService) waitTitleWorkers() { s.titleWorkers.Wait() }

type AgentPresetService struct{ runtime *Runtime }
type ArtifactService = artifact.Service

// EventService owns in-memory event projections and observer delivery. The
// durable EventStore and Hub remain injected runtime resources.
type EventService struct {
	runtime *Runtime

	// publishMu serializes event publication end to end: identity and
	// terminal dedupe, durable append, and synchronous projection. It is the
	// only Runtime lock held across that I/O; readers never take it.
	publishMu sync.Mutex
	// mu guards the in-memory indexes below and the turn queue maps. It is
	// held only around map reads and writes, never across I/O or callbacks.
	mu                  sync.Mutex
	terminals           map[protocol.TurnID]protocol.EventKind
	approvals           map[string]PendingApproval
	inputs              map[string]PendingInput
	observerMu          sync.Mutex
	observers           map[uint64]func(protocol.Event)
	nextObserver        uint64
	observerQueue       []protocol.Event
	observerDispatching bool
	toolItems           map[EventItemOwner]protocol.ItemID
	approvalItems       map[EventItemOwner]protocol.ItemID
	inputItems          map[EventItemOwner]protocol.ItemID
}

// RecoveryService owns reconstruction of volatile indexes and replay of
// accepted operations after durable resources are ready.
type RecoveryService struct{ runtime *Runtime }

func installRuntimeServices(runtime *Runtime, operationBuffer int) {
	runtime.EventService = &EventService{
		runtime:       runtime,
		terminals:     make(map[protocol.TurnID]protocol.EventKind),
		approvals:     make(map[string]PendingApproval),
		inputs:        make(map[string]PendingInput),
		observers:     make(map[uint64]func(protocol.Event)),
		toolItems:     make(map[EventItemOwner]protocol.ItemID),
		approvalItems: make(map[EventItemOwner]protocol.ItemID),
		inputItems:    make(map[EventItemOwner]protocol.ItemID),
	}
	runtime.TurnService = &TurnService{
		runtime: runtime,
		active:  NewActiveTurnRegistry(),
	}
	runtime.TurnQueueService = newTurnQueueService(runtime)
	runtime.SessionService = &SessionService{runtime: runtime}
	runtime.AgentPresetService = &AgentPresetService{runtime: runtime}
	runtime.OperationService = &OperationService{
		runtime:      runtime,
		operations:   make(chan acceptedOperation, operationBuffer),
		accepted:     make(map[protocol.OperationID]PendingOperation),
		acceptedKeys: make(map[string]protocol.OperationID),
		committed:    make(map[protocol.OperationID]PendingOperation),
		withdrawing:  make(map[protocol.ThreadID]bool),
		settlements:  operationSettlements{wake: make(chan struct{}, 1)},
	}
	runtime.RecoveryService = &RecoveryService{runtime: runtime}
	runtime.HistoryService = sessionhistory.NewService(runtime)
	runtime.ArtifactService = artifact.NewArtifactService(runtime)
	runtime.TraceQuery = runtime.opts.Observability.TraceQuery
}

// OperationService owns operation admission, idempotency, queueing, and
// lifecycle commit state. Runtime exposes its methods as a compatibility
// facade, but does not synchronize or mutate these fields directly.
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
		if operation.IdempotencyKey != "" {
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

func newEventHub(ctx context.Context, runtime *Runtime) *eventhub.Hub {
	return eventhub.New(eventhub.Config{
		Store: runtime.events, Buffer: runtime.opts.SubscriberBuffer,
		Context: ctx, Closed: ErrClosed, CursorAhead: ErrCursorAhead,
		ReplayOverflow: func(cursor protocol.Cursor, limit int) error {
			return &ReplayLimitError{Requested: cursor, Limit: limit}
		},
		OnPublished: runtime.metrics.EventPublished, OnDropped: runtime.metrics.SubscriberDropped,
		OnEvent: runtime.observeEvent,
	})
}

func runtimeProblem(code protocol.ErrorCode, message string, cause error) *protocol.Problem {
	return protocol.NewProblem(code, message, false, cause)
}
func retryableProblem(code protocol.ErrorCode, message string) *protocol.Problem {
	return protocol.NewProblem(code, message, true, nil)
}
func turnNotActiveProblem() *protocol.Problem {
	return runtimeProblem(protocol.CodeInvalidArgument, "turn is not active", nil)
}
func sessionBusyProblem(message string, summary protocol.SessionSummary) *protocol.Problem {
	return protocol.NewProblemWithDetails(protocol.CodeConflict, message, true,
		protocol.ProblemDetails{Reason: protocol.ProblemReasonSessionBusy,
			ResourceID: summary.SessionID, SessionStatus: string(summary.Status)}, nil)
}
