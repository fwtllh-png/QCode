package persistence

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/state"
	turnstate "github.com/fwtllh-png/QCode/internal/persist/state/turnstate"
	threadstate "github.com/fwtllh-png/QCode/internal/persist/thread"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestC1DurableCoordinatorRuntimeScansRestoresAndLeasesActiveTurn(
	t *testing.T,
) {
	store := seedCoordinatorState(t, t.TempDir())
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	repositories, err := NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	operation := coordinatorStartOperation(t, "turn-c1", "item-c1")
	canonical, err := app.CanonicalOperationPayload(operation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repositories.Lifecycle.Accept(
		t.Context(),
		operation,
		"request-c1",
		canonical,
	); err != nil {
		t.Fatal(err)
	}
	factStore := turnstate.NewSQLiteRepository(store.SQLite())
	seed, err := turnkernel.NewStoreCoordinatorRuntime(factStore)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := seed.Open(
		t.Context(),
		"turn-c1",
		turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []turnkernel.Command{
		turnkernel.StartTurn{},
		turnkernel.PreparationFinished{},
		turnkernel.ModelTextReceived{Text: "partial"},
	} {
		if err := handle.Coordinator.Submit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	wantDigest, err := turnkernel.Digest(handle.Coordinator.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Release(t.Context(), "turn-c1"); err != nil {
		t.Fatal(err)
	}

	first, err := NewCoordinatorRuntime(
		factStore,
		"owner-c1-first",
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close(context.Background()) })
	restored, err := first.RecoverActiveTurns(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || restored[0].TurnID != "turn-c1" {
		t.Fatalf("restored turns = %+v", restored)
	}
	if _, err := first.RecoverActiveTurns(
		t.Context(),
	); !errors.Is(err, turnkernel.ErrCoordinatorAlreadyActive) {
		t.Fatalf("duplicate recovery error = %v", err)
	}

	facts, err := factStore.LoadDomainFacts(t.Context(), "turn-c1")
	if err != nil {
		t.Fatal(err)
	}
	gotDigest := facts[len(facts)-1].StateDigest
	if gotDigest != wantDigest {
		t.Fatalf("restored digest = %s, want %s", gotDigest, wantDigest)
	}

	second, err := NewCoordinatorRuntime(
		factStore,
		"owner-c1-second",
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	concurrent, err := second.RecoverActiveTurns(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(concurrent) != 0 {
		t.Fatalf("concurrent recovery claimed %+v", concurrent)
	}
	if err := first.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	afterShutdown, err := second.RecoverActiveTurns(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(afterShutdown) != 1 ||
		afterShutdown[0].TurnID != "turn-c1" {
		t.Fatalf("post-shutdown recovery = %+v", afterShutdown)
	}
}

func TestDurableCoordinatorOpenWaitsForInterruptedTurnLease(t *testing.T) {
	store := seedCoordinatorState(t, t.TempDir())
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	repositories, err := NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	operation := coordinatorStartOperation(t, "turn-reload", "item-reload")
	canonical, err := app.CanonicalOperationPayload(operation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repositories.Lifecycle.Accept(
		t.Context(),
		operation,
		"request-reload",
		canonical,
	); err != nil {
		t.Fatal(err)
	}
	factStore := turnstate.NewSQLiteRepository(store.SQLite())
	seed, err := turnkernel.NewStoreCoordinatorRuntime(factStore)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := seed.Open(
		t.Context(),
		"turn-reload",
		turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Coordinator.Submit(
		t.Context(),
		turnkernel.StartTurn{},
	); err != nil {
		t.Fatal(err)
	}
	if err := seed.Release(t.Context(), "turn-reload"); err != nil {
		t.Fatal(err)
	}
	lease := 80 * time.Millisecond
	if err := factStore.ClaimTurn(
		t.Context(),
		"turn-reload",
		"interrupted-owner",
		lease,
	); err != nil {
		t.Fatal(err)
	}

	runtime, err := NewCoordinatorRuntime(
		factStore,
		"replacement-owner",
		lease,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	started := time.Now()
	restored, err := runtime.Open(
		t.Context(),
		"turn-reload",
		turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.Restored {
		t.Fatal("interrupted turn was opened as a new coordinator")
	}
	if elapsed := time.Since(started); elapsed < lease/2 {
		t.Fatalf("restored before stale lease expired: %s", elapsed)
	}
}

func TestDurableCoordinatorRetriesReleaseWithoutRenewingLease(t *testing.T) {
	store := seedCoordinatorState(t, t.TempDir())
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	repositories, err := NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	operation := coordinatorStartOperation(
		t,
		"turn-release-retry",
		"item-release-retry",
	)
	canonical, err := app.CanonicalOperationPayload(operation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repositories.Lifecycle.Accept(
		t.Context(),
		operation,
		"request-release-retry",
		canonical,
	); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewCoordinatorRuntime(
		turnstate.NewSQLiteRepository(store.SQLite()),
		"owner-release-retry",
		60*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	handle, err := runtime.Open(
		t.Context(),
		"turn-release-retry",
		turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Coordinator.Submit(
		t.Context(),
		turnkernel.StartTurn{},
	); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runtime.Release(
		canceled,
		"turn-release-retry",
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("release error = %v", err)
	}
	if got := runtime.activeTurnIDs(); len(got) != 0 {
		t.Fatalf("releasing turn is still renewed: %v", got)
	}
	deadline := time.Now().Add(time.Second)
	for len(runtime.trackedTurnIDs()) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.trackedTurnIDs(); len(got) != 0 {
		t.Fatalf("release retry did not drain: %v", got)
	}
}

func TestC1DurableCoordinatorRuntimeQuarantinesIncompleteFacts(
	t *testing.T,
) {
	store := seedCoordinatorState(t, t.TempDir())
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	repositories, err := NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	operation := coordinatorStartOperation(
		t,
		"turn-c1-incomplete",
		"item-c1-incomplete",
	)
	canonical, err := app.CanonicalOperationPayload(operation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repositories.Lifecycle.Accept(
		t.Context(),
		operation,
		"request-c1-incomplete",
		canonical,
	); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewCoordinatorRuntime(
		turnstate.NewSQLiteRepository(store.SQLite()),
		"owner-c1-incomplete",
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	restored, err := runtime.RecoverActiveTurns(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 {
		t.Fatalf("unrestorable turn was restored: %+v", restored)
	}
}

func seedCoordinatorState(t *testing.T, root string) *state.Store {
	t.Helper()
	store, err := state.Open(t.Context(), state.Options{DataDir: root})
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := repositories.Sessions.EnsureSeed(
		t.Context(),
		"session-1",
		filepath.Join(root, "workspace"),
	); err != nil {
		t.Fatal(err)
	}
	_, err = repositories.Threads.Create(t.Context(), threadstate.Thread{
		ID: "thread-1", SessionID: "session-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func coordinatorStartOperation(
	t *testing.T,
	turnID protocol.TurnID,
	itemID protocol.ItemID,
) protocol.Operation {
	t.Helper()
	operation, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: "thread-1", TurnID: turnID, ItemID: itemID, Prompt: "persist me",
	})
	if err != nil {
		t.Fatal(err)
	}
	return operation
}
