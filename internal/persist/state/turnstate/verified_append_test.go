package turnstate

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// legacyFactStore hides AppendVerifiedDomainFacts so the coordinator falls
// back to the re-digesting append path.
type legacyFactStore struct {
	store *Store
}

func (l legacyFactStore) AppendDomainFacts(
	ctx context.Context,
	turnID string,
	expectedNext uint64,
	facts []turnkernel.DomainFact,
) error {
	return l.store.AppendDomainFacts(ctx, turnID, expectedNext, facts)
}

func (l legacyFactStore) LoadDomainFacts(
	ctx context.Context,
	turnID string,
) ([]turnkernel.DomainFact, error) {
	return l.store.LoadDomainFacts(ctx, turnID)
}

func openVerifiedTestStore(t *testing.T) *Store {
	t.Helper()
	database, err := sqlitestate.Open(
		t.Context(),
		filepath.Join(t.TempDir(), "state.db"),
		sqlitestate.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return NewSQLiteRepository(database)
}

func storedFactRows(t *testing.T, store *Store, turnID string) [][]byte {
	t.Helper()
	rows, err := store.database.DB().Query(
		`SELECT fact_json FROM turn_domain_facts
		 WHERE turn_id = ? ORDER BY sequence`,
		turnID,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var encoded [][]byte
	for rows.Next() {
		var fact []byte
		if err := rows.Scan(&fact); err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, append([]byte(nil), fact...))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return encoded
}

// The verified append path must be byte-identical to the re-digesting path:
// same snapshots, same deltas, same digest chain. Forty-two commands cross
// the snapshot cadence twice (sequences 17 and 33), so both encodings run.
func TestVerifiedAppendMatchesLegacyEncoding(t *testing.T) {
	commands := []turnkernel.Command{
		turnkernel.StartTurn{},
		turnkernel.PreparationFinished{},
	}
	for index := 0; index < 40; index++ {
		commands = append(commands, turnkernel.ModelTextReceived{
			Text: fmt.Sprintf("line-%d", index),
		})
	}
	run := func(
		store turnkernel.DomainFactStore,
		readable *Store,
	) [][]byte {
		runtime, err := turnkernel.NewStoreCoordinatorRuntime(store)
		if err != nil {
			t.Fatal(err)
		}
		handle, err := runtime.Open(
			t.Context(),
			"turn-encoding",
			turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1),
		)
		if err != nil {
			t.Fatal(err)
		}
		for _, command := range commands {
			if err := handle.Coordinator.Submit(t.Context(), command); err != nil {
				t.Fatal(err)
			}
		}
		return storedFactRows(t, readable, "turn-encoding")
	}
	verifiedStore := openVerifiedTestStore(t)
	legacyStore := openVerifiedTestStore(t)
	verified := run(verifiedStore, verifiedStore)
	legacy := run(legacyFactStore{store: legacyStore}, legacyStore)
	if len(verified) != len(commands) || len(legacy) != len(commands) {
		t.Fatalf(
			"fact counts = verified %d, legacy %d, want %d",
			len(verified), len(legacy), len(commands),
		)
	}
	for index := range verified {
		if string(verified[index]) != string(legacy[index]) {
			t.Fatalf(
				"fact %d differs between verified and legacy encoding:\n"+
					"verified: %s\nlegacy:   %s",
				index+1,
				verified[index],
				legacy[index],
			)
		}
	}
}

// Decoding a verified-appended journal must pass the full re-digesting
// verification and restore the exact committed state.
func TestVerifiedAppendRestoresThroughVerification(t *testing.T) {
	store := openVerifiedTestStore(t)
	runtime, err := turnkernel.NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Open(
		t.Context(),
		"turn-verified-restore",
		turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []turnkernel.Command{
		turnkernel.StartTurn{},
		turnkernel.PreparationFinished{},
		turnkernel.ModelTextReceived{Text: "one"},
		turnkernel.ModelTextReceived{Text: "two"},
	} {
		if err := handle.Coordinator.Submit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	want, err := turnkernel.Digest(handle.Coordinator.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	cached, ok := handle.Coordinator.LastDigest()
	if !ok || cached != want {
		t.Fatalf("cached digest = %s (ok %t), want %s", cached, ok, want)
	}
	if err := runtime.Release(t.Context(), "turn-verified-restore"); err != nil {
		t.Fatal(err)
	}
	restored, err := runtime.Restore(t.Context(), "turn-verified-restore")
	if err != nil {
		t.Fatal(err)
	}
	got, err := turnkernel.Digest(restored.Coordinator.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("restored digest = %s, want %s", got, want)
	}
	restoredCached, ok := restored.Coordinator.LastDigest()
	if !ok || restoredCached != want {
		t.Fatalf(
			"restored cached digest = %s (ok %t), want %s",
			restoredCached, ok, want,
		)
	}
}

// Per-command cost as the turn grows: every ModelTextReceived appends to the
// state, so the benchmark shows how each command scales with accumulated
// state. Run against both append paths to keep the gap honest.
func benchmarkCoordinatorAppend(b *testing.B, store turnkernel.DomainFactStore) {
	runtime, err := turnkernel.NewStoreCoordinatorRuntime(store)
	if err != nil {
		b.Fatal(err)
	}
	handle, err := runtime.Open(
		b.Context(),
		"turn-bench",
		turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1),
	)
	if err != nil {
		b.Fatal(err)
	}
	for _, command := range []turnkernel.Command{
		turnkernel.StartTurn{},
		turnkernel.PreparationFinished{},
	} {
		if err := handle.Coordinator.Submit(b.Context(), command); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		err := handle.Coordinator.Submit(
			b.Context(),
			turnkernel.ModelTextReceived{Text: fmt.Sprintf("line-%d", index)},
		)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCoordinatorVerifiedAppend(b *testing.B) {
	database, err := sqlitestate.Open(
		b.Context(),
		filepath.Join(b.TempDir(), "state.db"),
		sqlitestate.Options{},
	)
	if err != nil {
		b.Fatal(err)
	}
	defer database.Close()
	benchmarkCoordinatorAppend(b, NewSQLiteRepository(database))
}

func BenchmarkCoordinatorLegacyAppend(b *testing.B) {
	database, err := sqlitestate.Open(
		b.Context(),
		filepath.Join(b.TempDir(), "state.db"),
		sqlitestate.Options{},
	)
	if err != nil {
		b.Fatal(err)
	}
	defer database.Close()
	benchmarkCoordinatorAppend(b, legacyFactStore{
		store: NewSQLiteRepository(database),
	})
}
