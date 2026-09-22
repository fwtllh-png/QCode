package turnstate

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
)

const (
	domainFactStorageVersion = 2
	domainFactSnapshotEvery  = 16
)

type factQueryer interface {
	QueryContext(
		context.Context,
		string,
		...any,
	) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type storedDomainFact struct {
	StorageVersion      uint32                     `json:"storage_version"`
	TurnID              string                     `json:"turn_id"`
	Sequence            uint64                     `json:"sequence"`
	Command             string                     `json:"command"`
	Event               turnkernel.Event           `json:"event"`
	Snapshot            json.RawMessage            `json:"snapshot,omitempty"`
	Delta               map[string]json.RawMessage `json:"delta,omitempty"`
	ObjectDelta         map[string]objectPatch     `json:"object_delta,omitempty"`
	PreviousStateDigest string                     `json:"previous_state_digest,omitempty"`
	StateDigest         string                     `json:"state_digest"`
}

// Set and Remove are separate so a JSON null remains a value, not a deletion.
type objectPatch struct {
	Set    map[string]json.RawMessage `json:"set,omitempty"`
	Remove []string                   `json:"remove,omitempty"`
}

func encodeDomainFact(
	fact turnkernel.DomainFact,
	previous *turnkernel.State,
	previousDigest string,
) ([]byte, error) {
	digest, err := turnkernel.Digest(fact.State)
	if err != nil || digest != fact.StateDigest {
		return nil, errors.New("domain fact state digest mismatch")
	}
	stored := storedDomainFact{
		StorageVersion:      domainFactStorageVersion,
		TurnID:              fact.TurnID,
		Sequence:            fact.Sequence,
		Command:             fact.Command,
		Event:               fact.Event,
		PreviousStateDigest: previousDigest,
		StateDigest:         fact.StateDigest,
	}
	if previous == nil ||
		(fact.Sequence-1)%domainFactSnapshotEvery == 0 {
		stored.Snapshot, err = json.Marshal(fact.State)
	} else {
		var left, right map[string]json.RawMessage
		if left, err = stateObject(*previous); err == nil {
			right, err = stateObject(fact.State)
		}
		if err == nil {
			stored.Delta, stored.ObjectDelta, err = fieldDelta(left, right)
		}
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(stored)
}

// encodeVerifiedDomainFact encodes a fact whose StateDigest and canonical
// encoding the turnkernel coordinator already computed (see
// turnkernel.VerifiedDomainFactStore). It reuses those encodings for the
// snapshot and the field delta instead of re-digesting the state; decode-side
// verification still re-digests every stored fact.
func encodeVerifiedDomainFact(
	fact turnkernel.DomainFact,
	stateEncoding []byte,
	previousEncoding []byte,
	previousDigest string,
) ([]byte, error) {
	stored := storedDomainFact{
		StorageVersion:      domainFactStorageVersion,
		TurnID:              fact.TurnID,
		Sequence:            fact.Sequence,
		Command:             fact.Command,
		Event:               fact.Event,
		PreviousStateDigest: previousDigest,
		StateDigest:         fact.StateDigest,
	}
	var err error
	if previousEncoding == nil ||
		(fact.Sequence-1)%domainFactSnapshotEvery == 0 {
		stored.Snapshot = stateEncoding
	} else {
		var left, right map[string]json.RawMessage
		if err = json.Unmarshal(previousEncoding, &left); err == nil {
			err = json.Unmarshal(stateEncoding, &right)
		}
		if err == nil {
			stored.Delta, stored.ObjectDelta, err = fieldDelta(left, right)
		}
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(stored)
}

func decodeDomainFacts(
	encodedFacts [][]byte,
) ([]turnkernel.DomainFact, error) {
	if len(encodedFacts) == 0 {
		return nil, nil
	}
	var first storedDomainFact
	if err := json.Unmarshal(encodedFacts[0], &first); err != nil {
		return nil, err
	}
	if first.Sequence == 0 {
		return nil, errors.New("domain fact sequence is invalid")
	}
	if len(first.Snapshot) == 0 && first.Sequence != 1 {
		return nil, errors.New("domain fact suffix must begin at a snapshot")
	}
	return decodeDomainFactSequence(
		encodedFacts,
		first.Sequence,
		first.PreviousStateDigest,
	)
}

func decodeDomainFactSuffix(
	encodedFacts [][]byte,
	startSequence uint64,
	previousDigest string,
) ([]turnkernel.DomainFact, error) {
	if startSequence == 0 {
		return nil, errors.New("domain fact suffix sequence is required")
	}
	return decodeDomainFactSequence(
		encodedFacts,
		startSequence,
		previousDigest,
	)
}

func decodeDomainFactSequence(
	encodedFacts [][]byte,
	startSequence uint64,
	previousDigest string,
) ([]turnkernel.DomainFact, error) {
	facts := make([]turnkernel.DomainFact, 0, len(encodedFacts))
	var previous *turnkernel.State
	for index, encoded := range encodedFacts {
		expectedSequence := startSequence + uint64(index)
		var version struct {
			StorageVersion uint32 `json:"storage_version"`
		}
		if err := json.Unmarshal(encoded, &version); err != nil {
			return nil, err
		}
		if version.StorageVersion != domainFactStorageVersion {
			return nil, fmt.Errorf(
				"unsupported domain fact storage version %d",
				version.StorageVersion,
			)
		}
		var stored storedDomainFact
		if err := json.Unmarshal(encoded, &stored); err != nil {
			return nil, err
		}
		if stored.Sequence != expectedSequence {
			return nil, fmt.Errorf(
				"domain fact sequence %d at index %d",
				stored.Sequence,
				index,
			)
		}
		if stored.PreviousStateDigest != previousDigest {
			return nil, fmt.Errorf(
				"domain fact previous digest mismatch at sequence %d",
				stored.Sequence,
			)
		}
		state, err := restoreState(stored, previous)
		if err != nil {
			return nil, fmt.Errorf(
				"restore domain fact %d: %w",
				stored.Sequence,
				err,
			)
		}
		fact := turnkernel.DomainFact{
			TurnID: stored.TurnID, Sequence: stored.Sequence,
			Command: stored.Command, Event: stored.Event,
			State: state, StateDigest: stored.StateDigest,
		}
		if err := validateDecodedFact(
			fact,
			expectedSequence,
			previousDigest,
			stored.PreviousStateDigest,
		); err != nil {
			return nil, err
		}
		facts = append(facts, fact)
		previous = &state
		previousDigest = fact.StateDigest
	}
	return facts, nil
}

func decodeStoredFactDigest(encoded []byte) (string, error) {
	var stored storedDomainFact
	if err := json.Unmarshal(encoded, &stored); err != nil {
		return "", err
	}
	if stored.StorageVersion != domainFactStorageVersion {
		return "", fmt.Errorf(
			"unsupported domain fact storage version %d",
			stored.StorageVersion,
		)
	}
	if stored.StateDigest == "" {
		return "", errors.New("domain fact state digest is missing")
	}
	return stored.StateDigest, nil
}

func restoreState(
	stored storedDomainFact,
	previous *turnkernel.State,
) (turnkernel.State, error) {
	var state turnkernel.State
	if len(stored.Snapshot) != 0 {
		if len(stored.Delta) != 0 || len(stored.ObjectDelta) != 0 {
			return state, errors.New("snapshot and delta are mutually exclusive")
		}
		if err := json.Unmarshal(stored.Snapshot, &state); err != nil {
			return state, err
		}
		return state, nil
	}
	if previous == nil {
		return state, errors.New("delta has no previous snapshot")
	}
	current, err := stateObject(*previous)
	if err != nil {
		return state, err
	}
	for key, value := range stored.Delta {
		if bytes.Equal(value, []byte("null")) {
			delete(current, key)
		} else {
			current[key] = value
		}
	}
	for key, patch := range stored.ObjectDelta {
		if _, replaced := stored.Delta[key]; replaced {
			return state, fmt.Errorf("object delta %q also has a replacement", key)
		}
		var object map[string]json.RawMessage
		if !isJSONObject(current[key]) {
			return state, fmt.Errorf("object delta %q has no object base", key)
		}
		if err := json.Unmarshal(current[key], &object); err != nil {
			return state, fmt.Errorf("restore object delta %q: %w", key, err)
		}
		for _, member := range patch.Remove {
			if _, set := patch.Set[member]; set {
				return state, fmt.Errorf("object delta %q both sets and removes %q", key, member)
			}
			delete(object, member)
		}
		for member, value := range patch.Set {
			object[member] = value
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			return state, err
		}
		current[key] = encoded
	}
	encoded, err := json.Marshal(current)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(encoded, &state); err != nil {
		return state, err
	}
	return state, nil
}

// fieldDelta compares every object by member, including effect/call ledgers
// and nested state records. Scalar, array and object-type changes replace the
// field. No state field name determines the storage algorithm.
func fieldDelta(
	left map[string]json.RawMessage,
	right map[string]json.RawMessage,
) (
	map[string]json.RawMessage,
	map[string]objectPatch,
	error,
) {
	delta := make(map[string]json.RawMessage)
	objectDelta := make(map[string]objectPatch)
	for key, value := range right {
		if bytes.Equal(left[key], value) {
			delete(left, key)
			continue
		}
		if isJSONObject(left[key]) && isJSONObject(value) {
			patch, patchErr := rawObjectDelta(left[key], value)
			if patchErr != nil {
				return nil, nil, patchErr
			}
			if len(patch.Set) != 0 || len(patch.Remove) != 0 {
				objectDelta[key] = patch
			}
		} else {
			delta[key] = value
		}
		delete(left, key)
	}
	for key := range left {
		delta[key] = json.RawMessage("null")
	}
	return delta, objectDelta, nil
}

func rawObjectDelta(
	previous json.RawMessage,
	current json.RawMessage,
) (objectPatch, error) {
	var left, right map[string]json.RawMessage
	if len(previous) != 0 {
		if err := json.Unmarshal(previous, &left); err != nil {
			return objectPatch{}, err
		}
	}
	if err := json.Unmarshal(current, &right); err != nil {
		return objectPatch{}, err
	}
	patch := objectPatch{Set: make(map[string]json.RawMessage)}
	for key, value := range right {
		if !bytes.Equal(left[key], value) {
			patch.Set[key] = value
		}
		delete(left, key)
	}
	for key := range left {
		patch.Remove = append(patch.Remove, key)
	}
	sort.Strings(patch.Remove)
	return patch, nil
}

func isJSONObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) != 0 && raw[0] == '{'
}

func stateObject(
	state turnkernel.State,
) (map[string]json.RawMessage, error) {
	encoded, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		return nil, err
	}
	return object, nil
}

func validateDecodedFact(
	fact turnkernel.DomainFact,
	expectedSequence uint64,
	previousDigest string,
	storedPreviousDigest string,
) error {
	if fact.Sequence != expectedSequence {
		return fmt.Errorf("domain fact sequence mismatch at %d", expectedSequence)
	}
	if storedPreviousDigest != "" &&
		storedPreviousDigest != previousDigest {
		return fmt.Errorf(
			"domain fact chain mismatch at sequence %d",
			expectedSequence,
		)
	}
	if err := turnkernel.Validate(fact.State); err != nil {
		return fmt.Errorf("domain fact state %d: %w", expectedSequence, err)
	}
	digest, err := turnkernel.Digest(fact.State)
	if err != nil || digest != fact.StateDigest {
		return fmt.Errorf(
			"domain fact digest mismatch at sequence %d",
			expectedSequence,
		)
	}
	return nil
}

func snapshotSequence(sequence uint64) uint64 {
	if sequence == 0 {
		return 1
	}
	return sequence - (sequence-1)%domainFactSnapshotEvery
}

func lastSnapshotSequence(
	ctx context.Context,
	queryer factQueryer,
	turnID string,
) (uint64, error) {
	var last uint64
	if err := queryer.QueryRowContext(
		ctx,
		`SELECT COALESCE(MAX(sequence), 0) FROM turn_domain_facts WHERE turn_id = ?`,
		turnID,
	).Scan(&last); err != nil {
		return 0, err
	}
	if last == 0 {
		return 1, nil
	}
	return snapshotSequence(last), nil
}

func loadEncodedFacts(
	ctx context.Context,
	queryer factQueryer,
	turnID string,
) ([][]byte, error) {
	start, err := lastSnapshotSequence(ctx, queryer, turnID)
	if err != nil {
		return nil, err
	}
	return loadEncodedFactsFrom(ctx, queryer, turnID, start)
}

func loadEncodedFactsForReplay(
	ctx context.Context,
	queryer factQueryer,
	turnID string,
	expectedNext uint64,
) ([][]byte, error) {
	return loadEncodedFactsFrom(
		ctx,
		queryer,
		turnID,
		snapshotSequence(expectedNext),
	)
}

func loadEncodedFactsFrom(
	ctx context.Context,
	queryer factQueryer,
	turnID string,
	startSequence uint64,
) ([][]byte, error) {
	rows, err := queryer.QueryContext(
		ctx,
		`SELECT fact_json FROM turn_domain_facts
		 WHERE turn_id = ? AND sequence >= ? ORDER BY sequence`,
		turnID,
		startSequence,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var encodedFacts [][]byte
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		encodedFacts = append(
			encodedFacts,
			append([]byte(nil), encoded...),
		)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for index, encoded := range encodedFacts {
		hydrated, err := hydrateContent(ctx, queryer, encoded)
		if err != nil {
			return nil, err
		}
		encodedFacts[index] = hydrated
	}
	return encodedFacts, nil
}
