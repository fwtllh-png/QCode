package app

import (
	"fmt"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestDurableOperationsRetainOnlyPendingRequests(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprintf("restored=%t", restored), func(t *testing.T) {
			operation := cancelInactiveTurn(t, "cache")
			canonical, err := CanonicalOperationPayload(operation)
			if err != nil {
				t.Fatal(err)
			}
			lifecycle := &faultyLifecycle{sessionID: "session-cache"}
			runtime, err := PrepareRuntime(t.Context(), Options{Lifecycle: lifecycle})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { closeRuntime(t, runtime) })
			service := runtime.OperationService
			service.open()
			if restored {
				service.restore(map[protocol.OperationID]PendingOperation{
					operation.ID: {ID: operation.ID, SessionID: "session-cache",
						IdempotencyKey: "cache-key", Canonical: canonical},
				})
			} else {
				if err := runtime.SubmitWithKey(t.Context(), operation, "cache-key"); err != nil {
					t.Fatal(err)
				}
				// Drive settlement without starting the dispatcher, so retention
				// can be checked on both sides of the commit deterministically.
				<-service.operations
			}
			pending := service.pendingOperations()
			if len(pending) != 1 || pending[0].SessionID != "session-cache" ||
				pending[0].IdempotencyKey != "cache-key" || string(pending[0].Canonical) != string(canonical) {
				t.Fatalf("pending request was not retained: %+v", pending)
			}
			if service.acceptedKeys != nil || service.committed != nil {
				t.Error("durable acceptance allocated memory-only request caches")
			}
			lifecycle.commitBroken.Store(true)
			service.commit(operation)
			if len(service.pendingOperations()) != 1 {
				t.Fatal("failed commit released the pending request")
			}
			lifecycle.commitBroken.Store(false)
			service.retryUnsettled()
			if len(service.pendingOperations()) != 0 || service.acceptedKeys != nil || service.committed != nil {
				t.Fatal("durable settlement retained request data")
			}
		})
	}
}

func TestMemoryOperationIdempotencyBeforeAndAfterCommit(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(fmt.Sprintf("restored=%t", restored), func(t *testing.T) {
			operation := cancelInactiveTurn(t, "memory-cache")
			runtime, err := PrepareRuntime(t.Context(), Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { closeRuntime(t, runtime) })
			service := runtime.OperationService
			service.open()
			if restored {
				canonical, err := CanonicalOperationPayload(operation)
				if err != nil {
					t.Fatal(err)
				}
				service.restore(map[protocol.OperationID]PendingOperation{
					operation.ID: {ID: operation.ID, IdempotencyKey: "memory-key", Canonical: canonical},
				})
			} else {
				if err := runtime.SubmitWithKey(t.Context(), operation, "memory-key"); err != nil {
					t.Fatal(err)
				}
				<-service.operations
			}
			for _, committed := range []bool{false, true} {
				if committed {
					service.commit(operation)
				}
				for _, id := range []protocol.OperationID{operation.ID, operation.ID + "-retry"} {
					duplicate := operation
					duplicate.ID = id
					if err := runtime.SubmitWithKey(t.Context(), duplicate, "memory-key"); err != nil {
						t.Fatalf("duplicate after commit=%t: %v", committed, err)
					}
					conflict := duplicate
					payload := *operation.Payload.(*protocol.CancelTurnPayload)
					payload.Reason = "different"
					conflict.Payload = &payload
					if err := runtime.SubmitWithKey(t.Context(), conflict, "memory-key"); err != ErrOperationConflict {
						t.Fatalf("conflict after commit=%t: %v", committed, err)
					}
				}
				if len(service.operations) != 0 {
					t.Fatal("duplicate or conflicting request was dispatched")
				}
			}
		})
	}
}
