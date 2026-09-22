package agentcontext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/durablecodec"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const (
	ContextManifestVersion = 1
	ContextEnvelopeVersion = 1
)

type ContentRef struct {
	Handle string `json:"handle"`
	Digest string `json:"digest"`
	Bytes  int    `json:"bytes"`
}

func (r ContentRef) Validate() error {
	decoded, err := hex.DecodeString(r.Digest)
	if r.Handle != r.Digest || len(decoded) != sha256.Size ||
		hex.EncodeToString(decoded) != r.Digest || err != nil || r.Bytes < 1 {
		return errors.New("context content reference is invalid")
	}
	return nil
}

type HistoryManifest struct {
	BaseRef  ContentRef   `json:"base_ref"`
	TailRefs []ContentRef `json:"tail_refs,omitempty"`
	Digest   string       `json:"digest"`
}

type OwnerManifest struct {
	BaseRef   ContentRef   `json:"base_ref"`
	DeltaRefs []ContentRef `json:"delta_refs,omitempty"`
	Digest    string       `json:"digest"`
}

type ContextManifest struct {
	Version         int               `json:"version"`
	ThreadID        protocol.ThreadID `json:"thread_id"`
	TurnID          protocol.TurnID   `json:"turn_id"`
	Epoch           uint64            `json:"epoch"`
	BaseRevision    uint64            `json:"base_revision"`
	Revision        uint64            `json:"revision"`
	Turn            uint64            `json:"turn,omitempty"`
	History         HistoryManifest   `json:"history"`
	Working         OwnerManifest     `json:"working"`
	Evidence        OwnerManifest     `json:"evidence"`
	Failures        OwnerManifest     `json:"failures"`
	Plan            OwnerManifest     `json:"plan"`
	Workspace       WorkspaceBinding  `json:"workspace"`
	World           WorldBaseline     `json:"world,omitempty"`
	Window          WindowLedger      `json:"window"`
	Compaction      Compaction        `json:"compaction"`
	TurnCheckpoints []TurnCheckpoint  `json:"turn_checkpoints,omitempty"`
	ContextDigest   string            `json:"context_digest"`
	Digest          string            `json:"digest"`
}

type ManifestLimits struct {
	OwnerDeltaMaxSegments int
	OwnerDeltaMaxBytes    int
}

func DefaultManifestLimits() ManifestLimits {
	return ManifestLimits{
		OwnerDeltaMaxSegments: 16,
		OwnerDeltaMaxBytes:    64 << 10,
	}
}

type BlobStore interface {
	Put(context.Context, string, []byte) error
	Get(context.Context, string) ([]byte, error)
}

// BeginContentStage pins unpublished objects until their durable owner commits
// or the attempt ends. In-memory stores need no collection coordination.
func BeginContentStage(ctx context.Context, store BlobStore) (context.Context, func() error) {
	if staging, ok := store.(interface {
		BeginContentStage(context.Context) (context.Context, func() error)
	}); ok {
		return staging.BeginContentStage(ctx)
	}
	return ctx, func() error { return nil }
}

func (m ContextManifest) ContentIDs() []string {
	ids := []string{m.History.BaseRef.Handle}
	for _, ref := range m.History.TailRefs {
		ids = append(ids, ref.Handle)
	}
	for _, owner := range []OwnerManifest{m.Working, m.Evidence, m.Failures, m.Plan} {
		ids = append(ids, owner.BaseRef.Handle)
		for _, ref := range owner.DeltaRefs {
			ids = append(ids, ref.Handle)
		}
	}
	return ids
}

type ContextEnvelope struct {
	Version    int             `json:"version"`
	Manifest   ContextManifest `json:"manifest"`
	Accounting AccountingDelta `json:"accounting"`
	Digest     string          `json:"digest"`
}

type historyState struct {
	History      []provider.Message `json:"history"`
	MessageTurns []uint64           `json:"message_turns,omitempty"`
	HistoryTurns map[string]uint64  `json:"history_turns,omitempty"`
}

type ownerSegment struct {
	Version int             `json:"version"`
	Owner   string          `json:"owner"`
	Mode    string          `json:"mode"`
	Data    json.RawMessage `json:"data"`
}

func BuildContextManifest(
	ctx context.Context,
	store BlobStore,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
	snapshot ContextSnapshot,
	previous *ContextManifest,
	limits ManifestLimits,
) (ContextManifest, error) {
	if store == nil {
		return ContextManifest{}, errors.New("context manifest blob store is required")
	}
	if threadID == "" || turnID == "" {
		return ContextManifest{}, errors.New("context manifest thread and turn are required")
	}
	if err := snapshot.Validate(); err != nil {
		return ContextManifest{}, err
	}
	defaults := DefaultManifestLimits()
	if limits.OwnerDeltaMaxSegments <= 0 {
		limits.OwnerDeltaMaxSegments = defaults.OwnerDeltaMaxSegments
	}
	if limits.OwnerDeltaMaxBytes <= 0 {
		limits.OwnerDeltaMaxBytes = defaults.OwnerDeltaMaxBytes
	}
	manifest := ContextManifest{
		Version: ContextManifestVersion, ThreadID: threadID, TurnID: turnID,
		Epoch: snapshot.Epoch, Revision: snapshot.Revision,
		BaseRevision: snapshot.Revision - min(snapshot.Revision, 1),
		Turn:         snapshot.Turn, Workspace: snapshot.Workspace,
		World:           CloneWorldBaseline(snapshot.World),
		Window:          CloneWindowLedger(snapshot.Window),
		Compaction:      snapshot.Compaction,
		TurnCheckpoints: CloneTurnCheckpoints(snapshot.TurnCheckpoints),
		ContextDigest:   snapshot.Digest,
	}

	history := historyState{
		History:      CloneMessages(snapshot.History),
		MessageTurns: append([]uint64(nil), snapshot.MessageTurns...),
		HistoryTurns: cloneHistoryTurns(snapshot.HistoryTurns),
	}
	var previousSnapshot ContextSnapshot
	if previous != nil {
		if err := previous.Validate(); err != nil {
			return ContextManifest{},
				fmt.Errorf("validate previous context manifest: %w", err)
		}
		if previous.ThreadID != threadID {
			return ContextManifest{},
				errors.New("previous context manifest revision is invalid")
		}
		if previous.Revision == snapshot.Revision {
			if previous.TurnID == turnID &&
				previous.Epoch == snapshot.Epoch &&
				previous.ContextDigest == snapshot.Digest {
				return *previous, nil
			}
			return ContextManifest{},
				errors.New("context manifest revision conflicts with prior state")
		}
		if previous.Revision > snapshot.Revision ||
			previous.Epoch > snapshot.Epoch {
			return ContextManifest{},
				errors.New("previous context manifest revision is invalid")
		}
		manifest.BaseRevision = previous.Revision
		loaded, err := LoadContextManifest(ctx, store, *previous)
		if err != nil {
			return ContextManifest{}, fmt.Errorf("load previous context manifest: %w", err)
		}
		previousSnapshot = loaded
	}
	if previous != nil && historyPrefix(previousSnapshot.History, snapshot.History) &&
		messageTurnsPrefix(previousSnapshot.MessageTurns, snapshot.MessageTurns) &&
		historyTurnsExtend(previousSnapshot.HistoryTurns, snapshot.HistoryTurns) {
		manifest.History = previous.History
		tail := historyState{
			History: CloneMessages(
				snapshot.History[len(previousSnapshot.History):],
			),
			MessageTurns: append(
				[]uint64(nil),
				snapshot.MessageTurns[len(previousSnapshot.MessageTurns):]...,
			),
			HistoryTurns: historyTurnDelta(
				previousSnapshot.HistoryTurns,
				snapshot.HistoryTurns,
			),
		}
		if len(tail.History) != 0 || len(tail.HistoryTurns) != 0 {
			ref, encoded, err := prepareValue("context-history-tail", tail)
			if err != nil {
				return ContextManifest{}, err
			}
			if len(manifest.History.TailRefs) >= limits.OwnerDeltaMaxSegments ||
				refsBytes(manifest.History.TailRefs)+ref.Bytes >
					limits.OwnerDeltaMaxBytes {
				base, baseErr := stageValue(
					ctx,
					store,
					"context-history-base",
					history,
				)
				if baseErr != nil {
					return ContextManifest{}, baseErr
				}
				manifest.History = HistoryManifest{BaseRef: base}
			} else {
				if err := store.Put(ctx, ref.Handle, encoded); err != nil {
					return ContextManifest{}, err
				}
				manifest.History.TailRefs = append(
					append([]ContentRef(nil), manifest.History.TailRefs...),
					ref,
				)
			}
		}
		manifest.History.Digest = historyDigest(history)
	} else {
		ref, err := stageValue(ctx, store, "context-history-base", history)
		if err != nil {
			return ContextManifest{}, err
		}
		manifest.History = HistoryManifest{
			BaseRef: ref, Digest: historyDigest(history),
		}
	}

	owners := []struct {
		name     string
		value    any
		target   *OwnerManifest
		previous OwnerManifest
	}{
		{"working", snapshot.WorkingSet, &manifest.Working, previousOwner(previous, "working")},
		{"evidence", snapshot.Evidence, &manifest.Evidence, previousOwner(previous, "evidence")},
		{"failures", snapshot.Failures, &manifest.Failures, previousOwner(previous, "failures")},
		{"plan", snapshot.Plan, &manifest.Plan, previousOwner(previous, "plan")},
	}
	for _, owner := range owners {
		next, err := appendOwner(
			ctx,
			store,
			owner.name,
			owner.value,
			owner.previous,
			limits,
		)
		if err != nil {
			return ContextManifest{}, err
		}
		*owner.target = next
	}
	manifest.Digest = manifest.digest()
	if err := manifest.Validate(); err != nil {
		return ContextManifest{}, err
	}
	return manifest, nil
}

func LoadContextManifest(
	ctx context.Context,
	store BlobStore,
	manifest ContextManifest,
) (ContextSnapshot, error) {
	if store == nil {
		return ContextSnapshot{}, errors.New("context manifest blob store is required")
	}
	if err := manifest.Validate(); err != nil {
		return ContextSnapshot{}, err
	}
	var history historyState
	if err := readValue(ctx, store, manifest.History.BaseRef, &history); err != nil {
		return ContextSnapshot{}, err
	}
	for _, ref := range manifest.History.TailRefs {
		var tail historyState
		if err := readValue(ctx, store, ref, &tail); err != nil {
			return ContextSnapshot{}, err
		}
		history.History = append(history.History, tail.History...)
		history.MessageTurns = append(
			history.MessageTurns,
			tail.MessageTurns...,
		)
		for id, turn := range tail.HistoryTurns {
			if history.HistoryTurns == nil {
				history.HistoryTurns = make(map[string]uint64)
			}
			history.HistoryTurns[id] = turn
		}
	}
	if historyDigest(history) != manifest.History.Digest {
		return ContextSnapshot{}, errors.New("context history manifest digest mismatch")
	}
	snapshot := ContextSnapshot{
		Version: ContextSnapshotVersion,
		Epoch:   manifest.Epoch, Revision: manifest.Revision, Turn: manifest.Turn,
		History: history.History, MessageTurns: history.MessageTurns,
		HistoryTurns:    history.HistoryTurns,
		Workspace:       manifest.Workspace,
		World:           CloneWorldBaseline(manifest.World),
		Window:          CloneWindowLedger(manifest.Window),
		Compaction:      manifest.Compaction,
		TurnCheckpoints: CloneTurnCheckpoints(manifest.TurnCheckpoints),
	}
	for _, owner := range []struct {
		name     string
		manifest OwnerManifest
		target   any
	}{
		{"working", manifest.Working, &snapshot.WorkingSet},
		{"evidence", manifest.Evidence, &snapshot.Evidence},
		{"failures", manifest.Failures, &snapshot.Failures},
		{"plan", manifest.Plan, &snapshot.Plan},
	} {
		if err := loadOwner(ctx, store, owner.name, owner.manifest, owner.target); err != nil {
			return ContextSnapshot{}, err
		}
	}
	for index, turn := range snapshot.MessageTurns {
		if index < len(snapshot.History) {
			snapshot.History[index].Turn = turn
		}
	}
	if err := snapshot.Seal(); err != nil {
		return ContextSnapshot{}, err
	}
	if snapshot.Digest != manifest.ContextDigest {
		return ContextSnapshot{}, fmt.Errorf(
			"context manifest snapshot digest mismatch: got %s want %s",
			snapshot.Digest,
			manifest.ContextDigest,
		)
	}
	return snapshot, nil
}

func EncodeContextEnvelope(
	manifest ContextManifest,
	accounting AccountingDelta,
) ([]byte, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	if err := accounting.Validate(); err != nil {
		return nil, err
	}
	envelope := ContextEnvelope{
		Version:  ContextEnvelopeVersion,
		Manifest: manifest, Accounting: accounting,
	}
	envelope.Digest = envelope.digest()
	return json.Marshal(envelope)
}

func DecodeContextEnvelope(raw []byte) (ContextEnvelope, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var envelope ContextEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return ContextEnvelope{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return ContextEnvelope{}, errors.New("context envelope has trailing JSON")
		}
		return ContextEnvelope{}, err
	}
	if envelope.Version != ContextEnvelopeVersion ||
		envelope.Digest == "" || envelope.Digest != envelope.digest() {
		return ContextEnvelope{}, errors.New("context envelope identity or digest is invalid")
	}
	if err := envelope.Manifest.Validate(); err != nil {
		return ContextEnvelope{}, err
	}
	if err := envelope.Accounting.Validate(); err != nil {
		return ContextEnvelope{}, err
	}
	return envelope, nil
}

func (e ContextEnvelope) digest() string {
	e.Digest = ""
	encoded, _ := json.Marshal(e)
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (m ContextManifest) Validate() error {
	if m.Version != ContextManifestVersion || m.ThreadID == "" ||
		m.TurnID == "" || m.Epoch == 0 || m.Revision == 0 ||
		m.BaseRevision >= m.Revision ||
		m.ContextDigest == "" || m.History.Digest == "" ||
		m.Working.Digest == "" || m.Evidence.Digest == "" ||
		m.Failures.Digest == "" || m.Plan.Digest == "" ||
		m.Digest == "" ||
		m.Digest != m.digest() || !m.Window.Valid() {
		return errors.New("context manifest identity or digest is invalid")
	}
	if err := m.Workspace.Validate(); err != nil {
		return err
	}
	if err := m.History.BaseRef.Validate(); err != nil {
		return err
	}
	for _, ref := range m.History.TailRefs {
		if err := ref.Validate(); err != nil {
			return err
		}
	}
	for _, owner := range []OwnerManifest{
		m.Working, m.Evidence, m.Failures, m.Plan,
	} {
		if err := owner.BaseRef.Validate(); err != nil {
			return err
		}
		for _, ref := range owner.DeltaRefs {
			if err := ref.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m ContextManifest) digest() string {
	m.Digest = ""
	encoded, _ := json.Marshal(m)
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func appendOwner(
	ctx context.Context,
	store BlobStore,
	name string,
	value any,
	previous OwnerManifest,
	limits ManifestLimits,
) (OwnerManifest, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return OwnerManifest{}, err
	}
	digest := digestBytes(raw)
	if previous.Digest == digest {
		return previous, nil
	}
	segment := ownerSegment{
		Version: 1, Owner: name, Mode: "replace", Data: raw,
	}
	ref, err := stageValue(ctx, store, "context-"+name+"-segment", segment)
	if err != nil {
		return OwnerManifest{}, err
	}
	if previous.BaseRef.Handle == "" ||
		len(previous.DeltaRefs) >= limits.OwnerDeltaMaxSegments ||
		refsBytes(previous.DeltaRefs)+ref.Bytes > limits.OwnerDeltaMaxBytes {
		return OwnerManifest{BaseRef: ref, Digest: digest}, nil
	}
	return OwnerManifest{
		BaseRef: previous.BaseRef,
		DeltaRefs: append(
			append([]ContentRef(nil), previous.DeltaRefs...),
			ref,
		),
		Digest: digest,
	}, nil
}

func loadOwner(
	ctx context.Context,
	store BlobStore,
	name string,
	manifest OwnerManifest,
	target any,
) error {
	refs := append(
		[]ContentRef{manifest.BaseRef},
		manifest.DeltaRefs...,
	)
	var latest ownerSegment
	for _, ref := range refs {
		if err := readValue(ctx, store, ref, &latest); err != nil {
			return err
		}
		if latest.Version != 1 || latest.Owner != name ||
			latest.Mode != "replace" || len(latest.Data) == 0 {
			return fmt.Errorf("context owner segment %q is invalid", name)
		}
	}
	if digestBytes(latest.Data) != manifest.Digest {
		return fmt.Errorf("context owner %q digest mismatch", name)
	}
	decoder := json.NewDecoder(bytes.NewReader(latest.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("context owner has trailing JSON")
		}
		return err
	}
	return nil
}

func stageValue(
	ctx context.Context,
	store BlobStore,
	prefix string,
	value any,
) (ContentRef, error) {
	ref, encoded, err := prepareValue(prefix, value)
	if err != nil {
		return ContentRef{}, err
	}
	if err := store.Put(ctx, ref.Handle, encoded); err != nil {
		return ContentRef{}, err
	}
	return ref, nil
}

func prepareValue(
	_ string,
	value any,
) (ContentRef, []byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return ContentRef{}, nil, err
	}
	encoded, err := durablecodec.Compress(raw)
	if err != nil {
		return ContentRef{}, nil, err
	}
	digest := sha256.Sum256(encoded)
	id := hex.EncodeToString(digest[:])
	handle := id
	return ContentRef{Handle: handle, Digest: id, Bytes: len(encoded)}, encoded, nil
}

func readValue(
	ctx context.Context,
	store BlobStore,
	ref ContentRef,
	target any,
) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	raw, err := store.Get(ctx, ref.Handle)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != ref.Digest || len(raw) != ref.Bytes {
		return errors.New("context content reference digest mismatch")
	}
	decoded, err := durablecodec.Decompress(raw)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("context content has trailing JSON")
		}
		return err
	}
	return nil
}

func previousOwner(
	manifest *ContextManifest,
	name string,
) OwnerManifest {
	if manifest == nil {
		return OwnerManifest{}
	}
	switch name {
	case "working":
		return manifest.Working
	case "evidence":
		return manifest.Evidence
	case "failures":
		return manifest.Failures
	case "plan":
		return manifest.Plan
	default:
		return OwnerManifest{}
	}
}

func historyPrefix(previous, current []provider.Message) bool {
	if len(previous) > len(current) {
		return false
	}
	for index := range previous {
		left, _ := json.Marshal(previous[index])
		right, _ := json.Marshal(current[index])
		if !bytes.Equal(left, right) {
			return false
		}
	}
	return true
}

func messageTurnsPrefix(previous, current []uint64) bool {
	if len(previous) > len(current) {
		return false
	}
	for index, turn := range previous {
		if current[index] != turn {
			return false
		}
	}
	return true
}

func historyTurnDelta(left, right map[string]uint64) map[string]uint64 {
	var result map[string]uint64
	for id, turn := range right {
		if left[id] == turn {
			continue
		}
		if result == nil {
			result = make(map[string]uint64)
		}
		result[id] = turn
	}
	return result
}

func historyTurnsExtend(left, right map[string]uint64) bool {
	for id, turn := range left {
		if right[id] != turn {
			return false
		}
	}
	return true
}

func historyDigest(value historyState) string {
	raw, _ := json.Marshal(value)
	return digestBytes(raw)
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func refsBytes(refs []ContentRef) int {
	total := 0
	for _, ref := range refs {
		total += ref.Bytes
	}
	return total
}
