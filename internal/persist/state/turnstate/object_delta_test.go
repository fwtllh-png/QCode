package turnstate

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestObjectDeltaPreservesNullAndSeparatesRemoval(t *testing.T) {
	left := json.RawMessage(`{"keep":{"value":1},"remove":2,"changed":3}`)
	right := json.RawMessage(`{"keep":{"value":1},"changed":null,"added":4}`)
	patch, err := rawObjectDelta(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(patch.Remove, []string{"remove"}) ||
		string(patch.Set["changed"]) != "null" ||
		string(patch.Set["added"]) != "4" || len(patch.Set) != 2 {
		t.Fatalf("patch = %+v", patch)
	}
}

func TestObjectDeltaIsIndependentOfStateFieldName(t *testing.T) {
	for _, field := range []string{"completed_effects", "closed_calls", "sample_ledger", "future_record"} {
		t.Run(field, func(t *testing.T) {
			before := map[string]json.RawMessage{
				field: json.RawMessage(`{"kept":{"x":1},"changed":{"x":2},"removed":{}}`),
			}
			after := map[string]json.RawMessage{
				field: json.RawMessage(`{"kept":{"x":1},"changed":{"x":3},"added":{}}`),
			}
			delta, objects, err := fieldDelta(before, after)
			if err != nil {
				t.Fatal(err)
			}
			patch := objects[field]
			if len(delta) != 0 || len(patch.Set) != 2 ||
				!reflect.DeepEqual(patch.Remove, []string{"removed"}) {
				t.Fatalf("delta = %s, objects = %+v", delta, objects)
			}
			if _, rewritten := patch.Set["kept"]; rewritten {
				t.Fatal("unchanged member was rewritten")
			}
		})
	}
}

func TestObjectDeltaTypeChangesUseReplacement(t *testing.T) {
	for _, pair := range [][2]string{
		{"null", "{}"}, {"{}", "null"}, {"[]", "{}"}, {"{}", "[]"},
		{"1", "{}"}, {"{}", "1"},
	} {
		t.Run(pair[0]+" to "+pair[1], func(t *testing.T) {
			delta, objects, err := fieldDelta(
				map[string]json.RawMessage{"field": json.RawMessage(pair[0])},
				map[string]json.RawMessage{"field": json.RawMessage(pair[1])},
			)
			if err != nil || len(objects) != 0 || string(delta["field"]) != pair[1] {
				t.Fatalf("delta=%s objects=%+v err=%v", delta, objects, err)
			}
		})
	}
}

func TestObjectDeltaRestoresAddedUpdatedAndRemovedCalls(t *testing.T) {
	store := openVerifiedTestStore(t)
	state := turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1)
	var facts []turnkernel.DomainFact
	for seq := 1; seq <= 36; seq++ {
		state.ClosedCalls = maps.Clone(state.ClosedCalls)
		if seq > 1 {
			delete(state.ClosedCalls, fmt.Sprintf("call-%d", seq-1))
		}
		state.ClosedCalls[fmt.Sprintf("call-%d", seq)] = turnkernel.ToolResultState{
			ID: fmt.Sprintf("call-%d", seq), Name: "fixture",
		}
		state.Context.HistoryBytes = seq
		digest, err := turnkernel.Digest(state)
		if err != nil {
			t.Fatal(err)
		}
		facts = append(facts, turnkernel.DomainFact{
			TurnID: "object-roundtrip", Sequence: uint64(seq),
			Command: "fixture", State: state, StateDigest: digest,
		})
	}
	if err := store.AppendDomainFacts(t.Context(), "object-roundtrip", 1, facts); err != nil {
		t.Fatal(err)
	}
	// Decode from the first snapshot to cover every boundary, not only the tail.
	restored, err := decodeDomainFacts(storedFactRows(t, store, "object-roundtrip"))
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != len(facts) {
		t.Fatalf("restored %d facts, want %d", len(restored), len(facts))
	}
	for index := range facts {
		got, _ := json.Marshal(restored[index].State)
		want, _ := json.Marshal(facts[index].State)
		if string(got) != string(want) {
			t.Fatalf("state differs at sequence %d", index+1)
		}
	}
}

func TestObjectDeltaRejectsConflictingOrMissingBase(t *testing.T) {
	state := turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1)
	for _, stored := range []storedDomainFact{
		{Snapshot: json.RawMessage(`{}`), ObjectDelta: map[string]objectPatch{"context": {}}},
		{Delta: map[string]json.RawMessage{"context": json.RawMessage(`{}`)},
			ObjectDelta: map[string]objectPatch{"context": {}}},
		{ObjectDelta: map[string]objectPatch{"unknown": {}}},
		{ObjectDelta: map[string]objectPatch{"context": {
			Set:    map[string]json.RawMessage{"digest": json.RawMessage(`"x"`)},
			Remove: []string{"digest"},
		}}},
	} {
		if _, err := restoreState(stored, &state); err == nil {
			t.Fatalf("invalid object delta accepted: %+v", stored)
		}
	}
}
