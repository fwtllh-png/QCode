package turnstate

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestSnapshotDeltaReplayMatchesFullStateAndCutsStorage(t *testing.T) {
	database, err := sqlitestate.Open(
		t.Context(),
		filepath.Join(t.TempDir(), "state.db"),
		sqlitestate.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := NewSQLiteRepository(database)
	facts := largeDomainFacts(t, 32)
	if err := store.AppendDomainFacts(
		t.Context(),
		"turn-delta",
		1,
		facts,
	); err != nil {
		t.Fatal(err)
	}
	restored, err := store.LoadDomainFacts(t.Context(), "turn-delta")
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) == 0 ||
		restored[0].Sequence != snapshotSequence(uint64(len(facts))) {
		t.Fatalf("loaded suffix = %+v", restored)
	}
	left, _ := json.Marshal(facts[len(facts)-1].State)
	right, _ := json.Marshal(restored[len(restored)-1].State)
	if string(left) != string(right) {
		t.Fatal("snapshot/delta suffix does not match the latest state")
	}
	var storedBytes int64
	if err := database.DB().QueryRowContext(
		t.Context(),
		`SELECT SUM(LENGTH(fact_json)) FROM turn_domain_facts
		 WHERE turn_id = ?`,
		"turn-delta",
	).Scan(&storedBytes); err != nil {
		t.Fatal(err)
	}
	var fullBytes int
	for _, fact := range facts {
		encoded, err := json.Marshal(fact)
		if err != nil {
			t.Fatal(err)
		}
		fullBytes += len(encoded)
	}
	reduction := 100 * (1 - float64(storedBytes)/float64(fullBytes))
	t.Logf(
		"domain fact storage: stored=%d full=%d reduction=%.2f%%",
		storedBytes,
		fullBytes,
		reduction,
	)
	if storedBytes*2 >= int64(fullBytes) {
		t.Fatalf(
			"stored=%d full=%d reduction=%.1f%%",
			storedBytes,
			fullBytes,
			reduction,
		)
	}
}

func TestSampleLedgerUsesMemberDeltas(t *testing.T) {
	database, err := sqlitestate.Open(
		t.Context(),
		filepath.Join(t.TempDir(), "state.db"),
		sqlitestate.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := NewSQLiteRepository(database)
	facts := sampleLedgerDomainFacts(t, 32)
	if err := store.AppendDomainFacts(
		t.Context(),
		"turn-sample-delta",
		1,
		facts,
	); err != nil {
		t.Fatal(err)
	}
	restored, err := store.LoadDomainFacts(
		t.Context(),
		"turn-sample-delta",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) == 0 ||
		restored[0].Sequence != snapshotSequence(uint64(len(facts))) {
		t.Fatalf("loaded suffix = %+v", restored)
	}
	left, _ := json.Marshal(facts[len(facts)-1].State)
	right, _ := json.Marshal(restored[len(restored)-1].State)
	if string(left) != string(right) {
		t.Fatal("sample ledger suffix does not match the latest state")
	}
	var second []byte
	if err := database.DB().QueryRowContext(
		t.Context(),
		`SELECT fact_json FROM turn_domain_facts
		 WHERE turn_id = ? AND sequence = 2`,
		"turn-sample-delta",
	).Scan(&second); err != nil {
		t.Fatal(err)
	}
	var stored storedDomainFact
	if err := json.Unmarshal(second, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.ObjectDelta["sample_ledger"].Set) != 1 ||
		stored.Delta["sample_ledger"] != nil {
		t.Fatalf("sample ledger delta = %+v", stored)
	}
	var storedBytes int64
	if err := database.DB().QueryRowContext(
		t.Context(),
		`SELECT SUM(LENGTH(fact_json)) FROM turn_domain_facts
		 WHERE turn_id = ?`,
		"turn-sample-delta",
	).Scan(&storedBytes); err != nil {
		t.Fatal(err)
	}
	var fullBytes int
	for _, fact := range facts {
		encoded, err := json.Marshal(fact)
		if err != nil {
			t.Fatal(err)
		}
		fullBytes += len(encoded)
	}
	if storedBytes*3 >= int64(fullBytes) {
		t.Fatalf(
			"sample ledger stored=%d full=%d, want >66%% reduction",
			storedBytes,
			fullBytes,
		)
	}
}

func TestLoadDomainFactsStartsAtLastSnapshot(t *testing.T) {
	database, err := sqlitestate.Open(
		t.Context(),
		filepath.Join(t.TempDir(), "state.db"),
		sqlitestate.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := NewSQLiteRepository(database)
	facts := largeDomainFacts(t, 20)
	if err := store.AppendDomainFacts(
		t.Context(),
		"turn-delta",
		1,
		facts,
	); err != nil {
		t.Fatal(err)
	}
	restored, err := store.LoadDomainFacts(t.Context(), "turn-delta")
	if err != nil {
		t.Fatal(err)
	}
	if restored[0].Sequence != 17 || len(restored) != 4 {
		t.Fatalf(
			"suffix start=%d count=%d, want sequence 17 and 4 facts",
			restored[0].Sequence,
			len(restored),
		)
	}
	left, _ := json.Marshal(facts[19].State)
	right, _ := json.Marshal(restored[3].State)
	if string(left) != string(right) {
		t.Fatal("last snapshot suffix does not restore the latest state")
	}
}

func TestSnapshotDeltaDigestChainRejectsTampering(t *testing.T) {
	database, err := sqlitestate.Open(
		t.Context(),
		filepath.Join(t.TempDir(), "state.db"),
		sqlitestate.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := NewSQLiteRepository(database)
	facts := largeDomainFacts(t, 3)
	if err := store.AppendDomainFacts(
		t.Context(),
		"turn-delta",
		1,
		facts,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB().ExecContext(
		t.Context(),
		`UPDATE turn_domain_facts
		 SET fact_json = json_set(
		     fact_json,
		     '$.previous_state_digest',
		     'sha256:tampered'
		 )
		 WHERE turn_id = ? AND sequence = 2`,
		"turn-delta",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadDomainFacts(
		t.Context(),
		"turn-delta",
	); err == nil {
		t.Fatal("tampered delta chain was accepted")
	}
}

func BenchmarkSO7DomainFactCodec(b *testing.B) {
	facts := largeDomainFacts(b, 32)
	encoded := make([][]byte, 0, len(facts))
	var previous *turnkernel.State
	var previousDigest string
	for _, fact := range facts {
		value, err := encodeDomainFact(fact, previous, previousDigest)
		if err != nil {
			b.Fatal(err)
		}
		encoded = append(encoded, value)
		state := fact.State
		previous = &state
		previousDigest = fact.StateDigest
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := decodeDomainFacts(encoded); err != nil {
			b.Fatal(err)
		}
	}
}

func largeDomainFacts(tb testing.TB, count int) []turnkernel.DomainFact {
	tb.Helper()
	state := turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1)
	for index := 0; index < 128; index++ {
		id := fmt.Sprintf("call-%03d", index)
		state.ClosedCalls[id] = turnkernel.ToolResultState{
			ID: id, Name: "large_result_fixture",
		}
	}
	facts := make([]turnkernel.DomainFact, 0, count)
	for index := 1; index <= count; index++ {
		state.Context.HistoryBytes = index * 100
		digest, err := turnkernel.Digest(state)
		if err != nil {
			tb.Fatal(err)
		}
		facts = append(facts, turnkernel.DomainFact{
			TurnID: "turn-delta", Sequence: uint64(index),
			Command: "context_updated",
			Event: turnkernel.Event{
				Kind: turnkernel.EventTransition,
				From: turnkernel.PhaseCreated,
				To:   turnkernel.PhaseCreated,
			},
			State: state, StateDigest: digest,
		})
	}
	return facts
}

func sampleLedgerDomainFacts(
	tb testing.TB,
	count int,
) []turnkernel.DomainFact {
	tb.Helper()
	facts := make([]turnkernel.DomainFact, 0, count)
	for index := 1; index <= count; index++ {
		state := turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1)
		for sample := 1; sample <= index; sample++ {
			id := fmt.Sprintf("sample-%03d", sample)
			state.SampleLedger[id] = turnkernel.ModelSampleState{
				ID:      id,
				Attempt: 1,
				Status:  turnkernel.SampleFailed,
				Error:   strings.Repeat("e", 2<<10),
			}
		}
		digest, err := turnkernel.Digest(state)
		if err != nil {
			tb.Fatal(err)
		}
		facts = append(facts, turnkernel.DomainFact{
			TurnID: "turn-sample-delta", Sequence: uint64(index),
			Command: "model_sample_progress_recorded",
			State:   state, StateDigest: digest,
		})
	}
	return facts
}
