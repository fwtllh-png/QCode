package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// turnStatusLifecycle mirrors the durable lifecycle's turn status rule: an
// operation rejection fails the turn, and a later, different terminal for the
// same turn is a conflict.
type turnStatusLifecycle struct {
	mu       sync.Mutex
	rejected map[protocol.TurnID]bool
}

func (*turnStatusLifecycle) Recover(context.Context) (RecoveryState, error) {
	return RecoveryState{}, nil
}

func (*turnStatusLifecycle) Accept(
	_ context.Context,
	operation protocol.Operation,
	_ string,
	_ json.RawMessage,
) (Acceptance, error) {
	return Acceptance{OperationID: operation.ID}, nil
}

func (l *turnStatusLifecycle) Project(_ context.Context, event protocol.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case event.Kind == protocol.EventOperationRejected:
		l.rejected[event.TurnID] = true
	case protocol.IsTerminalEvent(event.Kind) && l.rejected[event.TurnID]:
		return errors.New("turn already has a terminal state")
	}
	return nil
}

func (*turnStatusLifecycle) Commit(context.Context, CommitReceipt) error { return nil }

// terminalOutageStore refuses terminal appends until healed.
type terminalOutageStore struct {
	*MemoryEventStore
	broken   atomic.Bool
	attempts atomic.Int32
}

func newTerminalOutageStore() *terminalOutageStore {
	store := &terminalOutageStore{MemoryEventStore: NewMemoryEventStore(32)}
	store.broken.Store(true)
	return store
}

func (s *terminalOutageStore) Append(ctx context.Context, event protocol.Event) error {
	if protocol.IsTerminalEvent(event.Kind) {
		s.attempts.Add(1)
		if s.broken.Load() {
			return errors.New("injected terminal append outage")
		}
	}
	return s.MemoryEventStore.Append(ctx, event)
}

func durableOutageOptions(events *terminalOutageStore) (Options, *c5AtomicTerminalStore) {
	terminals := &c5AtomicTerminalStore{
		MemoryTerminalEnvelopeStore: turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil),
	}
	return Options{
		Engine:        &startupFailureEngine{},
		EventStore:    events,
		ContentStore:  NewMemoryContentStore(),
		TerminalStore: terminals,
		Lifecycle:     &turnStatusLifecycle{rejected: make(map[protocol.TurnID]bool)},
	}, terminals
}

func submitDuringTerminalOutage(
	t *testing.T,
	runtime *Runtime,
	events *terminalOutageStore,
	index int,
) protocol.Operation {
	t.Helper()
	operation := startOperation(t, index)
	if err := runtime.Submit(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, func() bool {
		snapshot := runtime.Snapshot(t.Context())
		return events.attempts.Load() > 0 &&
			snapshot.ActiveTurns == 0 && snapshot.PendingOperations == 0
	})
	return operation
}

func assertNoRejection(t *testing.T, events EventStore) []protocol.Event {
	t.Helper()
	replayed, err := events.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range replayed {
		if event.Kind == protocol.EventOperationRejected {
			t.Fatalf("durably committed Turn was rejected: %+v", event.Data)
		}
	}
	return replayed
}

func TestCommittedTerminalProjectionIsRetriedBeforeTheNextOperation(t *testing.T) {
	events := newTerminalOutageStore()
	options, _ := durableOutageOptions(events)
	runtime, err := newRuntimeWithRecovery(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	first := submitDuringTerminalOutage(t, runtime, events, 811)
	assertNoRejection(t, events)

	events.broken.Store(false)
	if err := runtime.Submit(t.Context(), startOperation(t, 812)); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, func() bool {
		replayed, _ := events.Replay(t.Context(), 0)
		terminals := 0
		for _, event := range replayed {
			if protocol.IsTerminalEvent(event.Kind) {
				terminals++
			}
		}
		return terminals == 2
	})
	replayed := assertNoRejection(t, events)
	_, firstTurn, _ := protocol.OperationReferences(first)
	for _, event := range replayed {
		if protocol.IsTerminalEvent(event.Kind) {
			if event.TurnID != firstTurn {
				t.Fatalf("next operation ran before the pending terminal projection: %+v", replayed)
			}
			break
		}
	}
}

func TestCommittedTerminalProjectionOutageKeepsRestartRecoverable(t *testing.T) {
	events := newTerminalOutageStore()
	options, terminals := durableOutageOptions(events)
	runtime, err := newRuntimeWithRecovery(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	operation := submitDuringTerminalOutage(t, runtime, events, 813)
	assertNoRejection(t, events)

	// The replacement Runtime starts over the same durable stores, as after a
	// crash; closing the first would close the shared memory event store.
	events.broken.Store(false)
	options.ContentStore = NewMemoryContentStore()
	restarted, err := newRuntimeWithRecovery(t.Context(), options)
	if err != nil {
		t.Fatalf("restart after terminal projection outage: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close(context.Background()) })
	_, turnID, _ := protocol.OperationReferences(operation)
	pending, err := terminals.PendingOutbox(t.Context(), string(turnID))
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending outbox after restart = %+v", pending)
	}
	failed := 0
	for _, event := range assertNoRejection(t, events) {
		if event.Kind == protocol.EventTurnFailed && event.TurnID == turnID {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("turn.failed events = %d, want 1", failed)
	}
}
