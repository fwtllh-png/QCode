package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// settlementRetry keeps background settlement retries fast in tests.
const settlementRetry = 5 * time.Millisecond

// faultyLifecycle accepts every operation and injects durable write faults:
// a commit receipt IO error, or a projection failure for one event kind after
// the event was already appended to the log.
type faultyLifecycle struct {
	sessionID     string
	commitBroken  atomic.Bool
	projectBroken atomic.Bool
	projectKind   protocol.EventKind
}

func (*faultyLifecycle) Recover(context.Context) (RecoveryState, error) {
	return RecoveryState{}, nil
}

func (l *faultyLifecycle) Accept(
	_ context.Context,
	operation protocol.Operation,
	_ string,
	_ json.RawMessage,
) (Acceptance, error) {
	return Acceptance{OperationID: operation.ID, SessionID: l.sessionID}, nil
}

func (l *faultyLifecycle) Project(_ context.Context, event protocol.Event) error {
	if l.projectBroken.Load() && event.Kind == l.projectKind {
		return errors.New("injected projection failure")
	}
	return nil
}

func (l *faultyLifecycle) Commit(context.Context, CommitReceipt) error {
	if l.commitBroken.Load() {
		return errors.New("injected commit receipt IO error")
	}
	return nil
}

func newSettlementRuntime(
	t *testing.T,
	lifecycle DurableLifecycle,
	events EventStore,
) *Runtime {
	t.Helper()
	runtime, err := newRuntimeWithRecovery(t.Context(), Options{
		Engine:       &testEngine{},
		EventStore:   events,
		ContentStore: NewMemoryContentStore(),
		TerminalStore: &c5AtomicTerminalStore{
			MemoryTerminalEnvelopeStore: turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil),
		},
		Lifecycle:       lifecycle,
		SettlementRetry: settlementRetry,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	return runtime
}

func cancelInactiveTurn(t *testing.T, index string) protocol.Operation {
	t.Helper()
	operation, err := protocol.NewOperation(&protocol.CancelTurnPayload{
		ThreadID: protocol.ThreadID("thread-settle-" + index),
		TurnID:   protocol.TurnID("turn-settle-" + index),
		ItemID:   protocol.ItemID("item-settle-" + index),
		Reason:   "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func rejectionsFor(
	t *testing.T,
	events EventStore,
	operationID protocol.OperationID,
) []*protocol.OperationRejectedData {
	t.Helper()
	replayed, err := events.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var rejections []*protocol.OperationRejectedData
	for _, event := range replayed {
		if data, ok := event.Data.(*protocol.OperationRejectedData); ok &&
			event.OperationID == operationID {
			rejections = append(rejections, data)
		}
	}
	return rejections
}

func TestCommitReceiptIOErrorIsSettledWithoutNewTraffic(t *testing.T) {
	lifecycle := &faultyLifecycle{}
	lifecycle.commitBroken.Store(true)
	events := NewMemoryEventStore(32)
	runtime := newSettlementRuntime(t, lifecycle, events)
	operation := cancelInactiveTurn(t, "commit")
	if err := runtime.Submit(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, func() bool {
		return len(rejectionsFor(t, events, operation.ID)) == 1
	})
	if pending := runtime.Snapshot(t.Context()).PendingOperations; pending != 1 {
		t.Fatalf("operation without a durable commit receipt pending = %d, want 1", pending)
	}

	lifecycle.commitBroken.Store(false)
	waitForCondition(t, func() bool {
		return runtime.Snapshot(t.Context()).PendingOperations == 0
	})
	if got := len(rejectionsFor(t, events, operation.ID)); got != 1 {
		t.Fatalf("commit retry re-emitted the rejection: %d events", got)
	}
}

func TestDiscardSessionRejectsOperationAwaitingCommit(t *testing.T) {
	operation := cancelInactiveTurn(t, "discard")
	threadID, _, _ := protocol.OperationReferences(operation)
	store := &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session-settle", ThreadID: threadID,
		Title: "Settlement", Status: protocol.SessionStatusCompleted,
		Isolation: "shared", WorkspaceRoot: "/workspace",
		ExecutionTarget: "local", WorkspaceLabel: "workspace",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	lifecycle := &faultyLifecycle{sessionID: store.summary.SessionID}
	lifecycle.commitBroken.Store(true)
	events := NewMemoryEventStore(32)
	runtime, err := newRuntimeWithRecovery(t.Context(), Options{
		Engine: &testEngine{}, EventStore: events,
		ContentStore: NewMemoryContentStore(),
		TerminalStore: &c5AtomicTerminalStore{
			MemoryTerminalEnvelopeStore: turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil),
		},
		Lifecycle: lifecycle, SessionLifecycle: store, SettlementRetry: settlementRetry,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		lifecycle.commitBroken.Store(false)
		closeRuntime(t, runtime)
	})
	if err := runtime.Submit(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, func() bool {
		return len(rejectionsFor(t, events, operation.ID)) == 1
	})
	if _, active := runtime.active.LookupThread(threadID); active {
		t.Fatal("active Turn would mask the pending-operation guard")
	}
	if runtime.Snapshot(t.Context()).PendingOperations != 1 {
		t.Fatal("unsettled operation was not retained")
	}
	if _, err := runtime.DiscardSession(t.Context(), store.summary.SessionID, store.summary.Revision); !protocol.IsCode(err, protocol.CodeConflict) || store.deleted {
		t.Fatalf("discard of unsettled operation = %v, deleted=%t", err, store.deleted)
	}
	lifecycle.commitBroken.Store(false)
	waitForCondition(t, func() bool {
		return runtime.Snapshot(t.Context()).PendingOperations == 0
	})
	if _, err := runtime.DiscardSession(t.Context(), store.summary.SessionID, store.summary.Revision); err != nil {
		t.Fatal(err)
	}
	if !store.discarded {
		t.Fatal("settled session was not discarded")
	}
}

func TestRejectionProjectionFailureIsRetriedWithoutDuplicateEvent(t *testing.T) {
	lifecycle := &faultyLifecycle{projectKind: protocol.EventOperationRejected}
	lifecycle.projectBroken.Store(true)
	events := NewMemoryEventStore(32)
	runtime := newSettlementRuntime(t, lifecycle, events)
	operation := cancelInactiveTurn(t, "projection")
	if err := runtime.Submit(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	// The rejection reaches the log, but its projection keeps failing.
	waitForCondition(t, func() bool {
		return len(rejectionsFor(t, events, operation.ID)) == 1
	})
	time.Sleep(4 * settlementRetry)
	if pending := runtime.Snapshot(t.Context()).PendingOperations; pending != 1 {
		t.Fatalf("operation with an unprojected rejection pending = %d, want 1", pending)
	}

	lifecycle.projectBroken.Store(false)
	waitForCondition(t, func() bool {
		return runtime.Snapshot(t.Context()).PendingOperations == 0
	})
	if got := len(rejectionsFor(t, events, operation.ID)); got != 1 {
		t.Fatalf("projection retry appended %d rejections, want 1", got)
	}
}

func TestCommittedTerminalProjectionIsRetriedWithoutNewTraffic(t *testing.T) {
	events := newTerminalOutageStore()
	options, _ := durableOutageOptions(events)
	options.SettlementRetry = settlementRetry
	runtime, err := newRuntimeWithRecovery(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	operation := submitDuringTerminalOutage(t, runtime, events, 831)
	_, turnID, _ := protocol.OperationReferences(operation)

	events.broken.Store(false)
	waitForCondition(t, func() bool {
		replayed, _ := events.Replay(t.Context(), 0)
		return slices.ContainsFunc(replayed, func(event protocol.Event) bool {
			return event.TurnID == turnID && protocol.IsTerminalEvent(event.Kind)
		})
	})
	assertNoRejection(t, events)
}

func TestStartupDefersAnUnprojectableTerminalOutbox(t *testing.T) {
	envelope := c5TerminalEnvelope(t)
	terminals := &c5AtomicTerminalStore{
		MemoryTerminalEnvelopeStore: turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil),
	}
	if _, err := terminals.CommitTerminal(t.Context(), envelope); err != nil {
		t.Fatal(err)
	}
	events := newTerminalOutageStore()
	runtime, err := newRuntimeWithRecovery(t.Context(), Options{
		EventStore:      events,
		TerminalStore:   terminals,
		SettlementRetry: settlementRetry,
	})
	if err != nil {
		t.Fatalf("one unprojectable terminal outbox blocked startup: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	events.broken.Store(false)
	waitForCondition(t, func() bool {
		pending, err := terminals.PendingOutbox(t.Context(), envelope.TurnID)
		return err == nil && len(pending) == 0
	})
	replayed, err := events.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	completed := 0
	for _, event := range replayed {
		if event.Kind == protocol.EventTurnCompleted {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("recovered terminal events = %d, want 1", completed)
	}
}

// A crash between acceptance and the first durable fact leaves operations of
// every kind accepted. Restart must settle each of them, not only StartTurn.
func TestRestartSettlesEveryInterruptedOperationKind(t *testing.T) {
	payloads := map[protocol.OperationKind]protocol.OperationPayload{
		protocol.OperationCancelTurn: &protocol.CancelTurnPayload{
			ThreadID: "thread-crash", TurnID: "turn-crash",
			ItemID: "item-crash-cancel", Reason: "test",
		},
		protocol.OperationApprovalDecision: &protocol.ApprovalDecisionPayload{
			ThreadID: "thread-crash", TurnID: "turn-crash",
			ItemID: "item-crash-approval", RequestID: "approval-crash",
			Decision: protocol.ApprovalApprove,
		},
		protocol.OperationRevertTurn: &protocol.RevertTurnPayload{
			ThreadID: "thread-crash", TurnID: "turn-crash-revert",
			ItemID: "item-crash-revert", TargetTurnID: "turn-crash",
		},
	}
	pending := make(map[protocol.OperationID]PendingOperation)
	operations := make(map[protocol.OperationKind]protocol.Operation)
	for kind, payload := range payloads {
		operation, err := protocol.NewOperation(payload)
		if err != nil {
			t.Fatal(err)
		}
		canonical, err := CanonicalOperationPayload(operation)
		if err != nil {
			t.Fatal(err)
		}
		pending[operation.ID] = PendingOperation{ID: operation.ID, Canonical: canonical}
		operations[kind] = operation
	}
	events := NewMemoryEventStore(32)
	runtime, err := newRuntimeWithRecovery(t.Context(), Options{
		Engine:       &c5RecoveryEngine{},
		EventStore:   events,
		ContentStore: NewMemoryContentStore(),
		TerminalStore: &c5AtomicTerminalStore{
			MemoryTerminalEnvelopeStore: turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil),
		},
		Lifecycle: &c5RecoveryLifecycle{recovery: RecoveryState{
			PendingOperations: pending,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	waitForCondition(t, func() bool {
		return runtime.Snapshot(t.Context()).PendingOperations == 0
	})
	for kind, operation := range operations {
		rejections := rejectionsFor(t, events, operation.ID)
		if len(rejections) != 1 {
			t.Fatalf("%s rejections = %d, want 1", kind, len(rejections))
		}
		fault := rejections[0].Fault
		if fault == nil || fault.Disposition != protocol.FaultReject ||
			fault.RetryOwner != protocol.FaultRetryOwnerHost {
			t.Fatalf("%s restart fault = %+v", kind, fault)
		}
		wantEffects := protocol.SideEffectNone
		if kind == protocol.OperationRevertTurn {
			wantEffects = protocol.SideEffectUnknown
		}
		if fault.SideEffects != wantEffects {
			t.Fatalf("%s side effects = %q, want %q", kind, fault.SideEffects, wantEffects)
		}
	}
}

// quarantineTerminalStore serves an unrestorable fact chain and records the
// Turns recovery quarantines.
type quarantineTerminalStore struct {
	*c5AtomicTerminalStore
	loadErr     error
	facts       []turnkernel.DomainFact
	mu          sync.Mutex
	quarantined []string
}

func (s *quarantineTerminalStore) LoadDomainFacts(
	ctx context.Context,
	turnID string,
) ([]turnkernel.DomainFact, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	if s.facts != nil {
		return s.facts, nil
	}
	return s.c5AtomicTerminalStore.LoadDomainFacts(ctx, turnID)
}

func (s *quarantineTerminalStore) QuarantineActiveTurn(_ context.Context, turnID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quarantined = append(s.quarantined, turnID)
	return nil
}

func TestRestartQuarantinesUnrestorableTurn(t *testing.T) {
	state := turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1)
	started, err := (turnkernel.Reducer{}).Apply(state, turnkernel.StartTurn{})
	if err != nil {
		t.Fatal(err)
	}
	for name, store := range map[string]*quarantineTerminalStore{
		"unreadable facts": {loadErr: errors.New("injected fact decode failure")},
		"digest mismatch": {facts: []turnkernel.DomainFact{{
			Sequence: 1, Command: "start_turn",
			State: started.State, StateDigest: "sha256:corrupt",
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			operation := startOperation(t, 841)
			_, turnID, _ := protocol.OperationReferences(operation)
			for index := range store.facts {
				store.facts[index].TurnID = string(turnID)
			}
			canonical, err := CanonicalOperationPayload(operation)
			if err != nil {
				t.Fatal(err)
			}
			store.c5AtomicTerminalStore = &c5AtomicTerminalStore{
				MemoryTerminalEnvelopeStore: turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil),
			}
			events := NewMemoryEventStore(32)
			engine := &c5RecoveryEngine{}
			runtime, err := newRuntimeWithRecovery(t.Context(), Options{
				Engine:        engine,
				EventStore:    events,
				ContentStore:  NewMemoryContentStore(),
				TerminalStore: store,
				Lifecycle: &c5RecoveryLifecycle{recovery: RecoveryState{
					PendingOperations: map[protocol.OperationID]PendingOperation{
						operation.ID: {ID: operation.ID, Canonical: canonical},
					},
				}},
			})
			if err != nil {
				t.Fatalf("unrestorable turn blocked startup: %v", err)
			}
			t.Cleanup(func() { _ = runtime.Close(context.Background()) })
			waitForCondition(t, func() bool {
				return runtime.Snapshot(t.Context()).PendingOperations == 0
			})
			if starts := engine.starts.Load(); starts != 0 {
				t.Fatalf("unrestorable turn reached the engine %d times", starts)
			}
			store.mu.Lock()
			quarantined := slices.Clone(store.quarantined)
			store.mu.Unlock()
			if !slices.Equal(quarantined, []string{string(turnID)}) {
				t.Fatalf("quarantined turns = %v", quarantined)
			}
			rejections := rejectionsFor(t, events, operation.ID)
			if len(rejections) != 1 {
				t.Fatalf("quarantine rejections = %d, want 1", len(rejections))
			}
			fault := rejections[0].Fault
			if fault == nil || fault.Origin != protocol.FaultOriginKernel ||
				!protocol.FaultAllowsTurnRecovery(fault) {
				t.Fatalf("quarantine fault = %+v", fault)
			}
		})
	}
}
