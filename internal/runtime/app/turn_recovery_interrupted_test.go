package app

import (
	"context"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// A StartTurn accepted before a crash that never reached the engine has no
// domain facts. Its accepted Turn row keeps the Thread busy, so recovery must
// settle it durably instead of leaving it pending forever.
func TestRecoverySettlesAcceptedStartTurnThatNeverReachedTheEngine(t *testing.T) {
	operation := startOperation(t, 81)
	canonical, err := CanonicalOperationPayload(operation)
	if err != nil {
		t.Fatal(err)
	}
	_, turnID, _ := protocol.OperationReferences(operation)
	events := NewMemoryEventStore(16)
	engine := &c5RecoveryEngine{}
	runtime, err := newRuntimeWithRecovery(t.Context(), Options{
		Engine:       engine,
		EventStore:   events,
		ContentStore: NewMemoryContentStore(),
		TerminalStore: &c5AtomicTerminalStore{
			MemoryTerminalEnvelopeStore: turnkernel.NewMemoryTerminalEnvelopeStore(
				nil,
				nil,
			),
		},
		Lifecycle: &c5RecoveryLifecycle{recovery: RecoveryState{
			PendingOperations: map[protocol.OperationID]PendingOperation{
				operation.ID: {ID: operation.ID, Canonical: canonical},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	waitForCondition(t, func() bool {
		return runtime.Snapshot(t.Context()).PendingOperations == 0
	})
	if starts := engine.starts.Load(); starts != 0 {
		t.Fatalf("interrupted Turn was silently re-run %d times", starts)
	}
	replayed, err := events.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var failed *protocol.TurnFailedData
	for _, event := range replayed {
		if event.TurnID != turnID {
			continue
		}
		if data, ok := event.Data.(*protocol.TurnFailedData); ok {
			failed = data
		}
	}
	if failed == nil {
		t.Fatalf("interrupted Turn has no terminal event: %+v", replayed)
	}
	if !protocol.FaultAllowsTurnRecovery(failed.Fault) {
		t.Fatalf("interrupted Turn cannot be retried: %+v", failed.Fault)
	}
}
