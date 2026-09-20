package turnkernel

import (
	"encoding/json"
	"testing"
)

// The set must marshal exactly like the historical []string encoding; state
// digests over stored facts depend on those bytes.
func TestObservationSetEncodesLikeSlice(t *testing.T) {
	type legacy struct {
		Seen []string `json:"seen_observations,omitempty"`
	}
	type current struct {
		Seen *ObservationSet `json:"seen_observations,omitempty"`
	}
	keys := []string{"sha256:a", "sha256:b", "sha256:c"}
	set := &ObservationSet{}
	for _, key := range keys {
		set.add(key)
	}
	encoded, err := json.Marshal(current{Seen: set})
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(legacy{Seen: keys})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(want) {
		t.Fatalf(
			"encoded = %s, want legacy slice encoding %s",
			encoded, want,
		)
	}
	omitted, err := json.Marshal(current{})
	if err != nil {
		t.Fatal(err)
	}
	if string(omitted) != "{}" {
		t.Fatalf("empty set encoded = %s, want {}", omitted)
	}
	var restored current
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	reencoded, err := json.Marshal(restored)
	if err != nil {
		t.Fatal(err)
	}
	if string(reencoded) != string(want) {
		t.Fatalf(
			"round trip encoded = %s, want %s",
			reencoded, want,
		)
	}
}

func TestObservationSetMembershipAndClone(t *testing.T) {
	var set *ObservationSet
	if set.contains("sha256:a") {
		t.Fatal("nil set must not contain keys")
	}
	progress := ProgressState{}
	if seenObservation(progress, "sha256:a") {
		t.Fatal("empty progress must not observe keys")
	}
	rememberObservation(&progress, "sha256:a")
	rememberObservation(&progress, "sha256:a")
	rememberObservation(&progress, "")
	if !seenObservation(progress, "sha256:a") {
		t.Fatal("remembered key is missing")
	}
	if len(progress.SeenObservations.order) != 1 {
		t.Fatalf(
			"remembered %d keys, want 1",
			len(progress.SeenObservations.order),
		)
	}
	isolated := cloneProgressState(progress)
	rememberObservation(&isolated, "sha256:b")
	if seenObservation(progress, "sha256:b") {
		t.Fatal("clone leaked a remembered key back to the source")
	}
	if !seenObservation(isolated, "sha256:b") {
		t.Fatal("clone lost its remembered key")
	}
}
