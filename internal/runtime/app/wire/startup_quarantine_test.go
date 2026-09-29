package wire

import (
	"context"
	"testing"
	"time"

	turnstate "github.com/fwtllh-png/QCode/internal/persist/state/turnstate"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	apppersistence "github.com/fwtllh-png/QCode/internal/runtime/app/persistence"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// A Turn whose durable fact chain no longer restores used to stay active
// forever: its operation was never dispatched, the Thread's single active Turn
// slot stayed taken, and the Session could not be deleted. Production startup
// must quarantine it through the SQLite terminal store and settle the
// operation.
func TestPersistentStartupQuarantinesUnrestorableTurn(t *testing.T) {
	store := seedPersistentState(t, t.TempDir())
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	repositories, err := apppersistence.NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	operation := persistentStartOperation(t, "turn-quarantine", "item-quarantine")
	canonical, err := app.CanonicalOperationPayload(operation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repositories.Lifecycle.Accept(
		t.Context(),
		operation,
		"request-quarantine",
		canonical,
	); err != nil {
		t.Fatal(err)
	}
	facts := turnstate.NewSQLiteRepository(store.SQLite())
	seed, err := turnkernel.NewStoreCoordinatorRuntime(facts)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := seed.Open(
		t.Context(),
		"turn-quarantine",
		turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Coordinator.Submit(t.Context(), turnkernel.StartTurn{}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Release(t.Context(), "turn-quarantine"); err != nil {
		t.Fatal(err)
	}
	if err := facts.ClaimTurn(
		t.Context(),
		"turn-quarantine",
		"crashed-owner",
		time.Minute,
	); err != nil {
		t.Fatal(err)
	}
	db := store.SQLite().DB()
	corrupted, err := db.ExecContext(t.Context(), `
		UPDATE turn_domain_facts
		SET fact_json = json_set(fact_json, '$.state_digest', 'sha256:corrupt')
		WHERE turn_id = ?`, "turn-quarantine",
	)
	if err != nil {
		t.Fatal(err)
	}
	var seededLeases int
	if err := db.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM turn_coordinator_leases WHERE turn_id = ?`,
		"turn-quarantine",
	).Scan(&seededLeases); err != nil {
		t.Fatal(err)
	}
	if rows, _ := corrupted.RowsAffected(); rows == 0 || seededLeases != 1 {
		t.Fatalf("seeded corrupt facts=%d leases=%d", rows, seededLeases)
	}

	engine := &persistentTestEngine{}
	runtime, err := newPersistentRuntime(t.Context(), PersistentRuntimeOptions{
		Store: store, Engine: engine,
	})
	if err != nil {
		t.Fatalf("unrestorable turn blocked startup: %v", err)
	}
	t.Cleanup(func() { closePersistentRuntime(t, runtime) })
	waitForPersistentCondition(t, func() bool {
		return runtime.Snapshot(t.Context()).PendingOperations == 0
	})
	if starts := engine.starts.Load(); starts != 0 {
		t.Fatalf("unrestorable turn reached the engine %d times", starts)
	}
	var turnStatus, operationStatus string
	var leases int
	if err := db.QueryRowContext(t.Context(), `
		SELECT
			(SELECT status FROM turns WHERE id = ?),
			(SELECT status FROM operations WHERE id = ?),
			(SELECT COUNT(*) FROM turn_coordinator_leases WHERE turn_id = ?)`,
		"turn-quarantine", operation.ID, "turn-quarantine",
	).Scan(&turnStatus, &operationStatus, &leases); err != nil {
		t.Fatal(err)
	}
	if turnStatus != "failed" || operationStatus != "committed" || leases != 0 {
		t.Fatalf(
			"quarantine left turn=%q operation=%q leases=%d",
			turnStatus, operationStatus, leases,
		)
	}

	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	next := persistentStartOperation(t, "turn-after-quarantine", "item-after-quarantine")
	if err := runtime.Submit(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	waitForTerminal(t, events, next.ID)
}

func waitForPersistentCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for persistent runtime condition")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
