package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type TerminalPublisher struct{ runtime *Runtime }

func NewTerminalPublisher(runtime *Runtime) *TerminalPublisher {
	return &TerminalPublisher{runtime: runtime}
}

func (p *TerminalPublisher) Commit(ctx context.Context, request TerminalRequest) (result CommittedTerminal, resultErr error) {
	ctx, finishStage := agentcontext.BeginContentStage(ctx, p.runtime.content)
	defer func() { resultErr = errors.Join(resultErr, finishStage()) }()
	material := request.Material
	if !material.FrozenState.Phase.Terminal() ||
		material.FrozenState.Terminal == nil ||
		material.Receipt == nil ||
		material.Terminal == nil ||
		len(material.DomainFacts) == 0 {
		return CommittedTerminal{}, errors.New("terminal material is incomplete")
	}
	threadID, turnID, itemID := protocol.OperationReferences(request.Operation)
	if string(turnID) != material.DomainFacts[0].TurnID {
		return CommittedTerminal{}, errors.New("terminal material turn identity mismatch")
	}
	sessionDelta, manifest, staged, err := p.prepareContextManifest(
		ctx,
		threadID,
		turnID,
		material.SessionDelta,
	)
	if err != nil {
		return CommittedTerminal{}, err
	}
	releaseStaged := func() {
		releaseStagedContent(p.runtime.content, staged)
	}
	receiptPayload, err := json.Marshal(material.Receipt)
	if err != nil {
		releaseStaged()
		return CommittedTerminal{}, err
	}
	terminalPayload, err := json.Marshal(material.Terminal)
	if err != nil {
		releaseStaged()
		return CommittedTerminal{}, err
	}
	operationReceipt, err := json.Marshal(
		p.runtime.OperationService.operationCommitReceipt(request.Operation.ID),
	)
	if err != nil {
		releaseStaged()
		return CommittedTerminal{}, err
	}
	// Outbox entries re-project live events (commentary, output deltas) that
	// were emitted under the operation that started the turn, so every entry
	// must carry that emission identity. Swapping in a later operation (for
	// example the cancel operation) makes stable re-projection collide with
	// the already-published event and blocks the terminal projection.
	projectionOperationID := request.Operation.ID
	entry := func(id string, kind protocol.EventKind, payload json.RawMessage) turnkernel.ProjectionOutboxEntry {
		return turnkernel.ProjectionOutboxEntry{
			ID: id, EventID: TerminalOutboxEventID(turnID, id),
			OperationID: projectionOperationID,
			ThreadID:    threadID, TurnID: turnID, ItemID: itemID,
			Kind: string(kind), Payload: payload,
		}
	}
	outbox := make([]turnkernel.ProjectionOutboxEntry, 0,
		len(material.FrozenState.Commentary)+len(material.FrozenState.FinalOutput)+2)
	for _, message := range material.FrozenState.Commentary {
		data := message.ProtocolData(string(turnID))
		payload, marshalErr := json.Marshal(&data)
		if marshalErr != nil {
			releaseStaged()
			return CommittedTerminal{}, marshalErr
		}
		projected := entry("commentary:"+message.SampleID, protocol.EventCommentaryCompleted, payload)
		projected.EventID = CommentaryEventID(data.MessageID)
		outbox = append(outbox, projected)
	}
	for index, text := range material.FrozenState.FinalOutput {
		payload, marshalErr := json.Marshal(&protocol.OutputDeltaData{Text: text})
		if marshalErr != nil {
			return CommittedTerminal{}, marshalErr
		}
		outbox = append(outbox, entry(fmt.Sprintf("output:%06d", index+1),
			protocol.EventOutputDelta, payload))
	}
	outbox = append(outbox,
		entry("receipt", protocol.EventExecutionReceipt, receiptPayload),
		entry("terminal", EventKind(material.Terminal), terminalPayload),
	)
	decision := *material.FrozenState.Terminal
	envelope := turnkernel.TerminalEnvelope{
		TurnID: string(turnID), EffectID: "terminal:" + string(turnID),
		FrozenState: material.FrozenState, DomainFacts: material.DomainFacts,
		Measurement:  material.Measurement,
		Receipt:      material.Receipt,
		SessionDelta: append(json.RawMessage(nil), sessionDelta...),
		FinalOutput:  append([]string(nil), material.FrozenState.FinalOutput...),
		TerminalEvent: turnkernel.Event{
			Kind: turnkernel.EventTerminalCommitted, Terminal: &decision,
		},
		OperationCommit: turnkernel.OperationCommitFact{
			OperationID: request.Operation.ID,
			Status:      "committed", Receipt: operationReceipt,
		},
		Outbox: outbox,
	}
	committed := CommittedTerminal{
		Operation: request.Operation, OperationID: projectionOperationID, ItemID: itemID,
	}
	if p.runtime.lifecycle != nil {
		atomicStore, ok := p.runtime.terminalStore.(turnkernel.AtomicTerminalOperationStore)
		if !ok {
			return CommittedTerminal{}, errors.New(
				"durable lifecycle requires atomic terminal operation store",
			)
		}
		_, err = atomicStore.CommitTerminalOperation(ctx, envelope)
		committed.OperationCommitted = err == nil
	} else {
		_, err = p.runtime.terminalStore.CommitTerminal(ctx, envelope)
		committed.OperationCommitted = err == nil
	}
	if err == nil {
		if manifest != nil {
			p.runtime.contextManifests.Store(threadID, *manifest)
		}
	}
	if err != nil {
		releaseStaged()
	}
	return committed, err
}

func (p *TerminalPublisher) prepareContextManifest(
	ctx context.Context,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
	raw json.RawMessage,
) (
	json.RawMessage,
	*agentcontext.ContextManifest,
	[]agentcontext.ContentRef,
	error,
) {
	if len(raw) == 0 {
		return nil, nil, nil, nil
	}
	var delta agentcontext.SessionDelta
	if err := json.Unmarshal(raw, &delta); err != nil {
		return nil, nil, nil, fmt.Errorf("decode prepared session delta: %w", err)
	}
	if delta.Version != agentcontext.ContextEnvelopeVersion {
		return nil, nil, nil, fmt.Errorf(
			"unsupported session delta version %d",
			delta.Version,
		)
	}
	snapshot, err := delta.ContextSnapshot()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build context snapshot: %w", err)
	}
	previous, hasPrevious := p.loadContextManifest(threadID)
	var prior *agentcontext.ContextManifest
	if hasPrevious {
		prior = &previous
	}
	manifest, err := agentcontext.BuildContextManifest(
		ctx,
		p.runtime.content,
		threadID,
		turnID,
		snapshot,
		prior,
		delta.ManifestLimits,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stage context manifest: %w", err)
	}
	encoded, err := agentcontext.EncodeContextEnvelope(
		manifest,
		delta.AccountingDelta(),
	)
	if err != nil {
		return nil, nil, nil, err
	}
	staged := newManifestRefs(prior, manifest)
	return encoded, &manifest, staged, nil
}

func newManifestRefs(
	previous *agentcontext.ContextManifest,
	current agentcontext.ContextManifest,
) []agentcontext.ContentRef {
	existing := make(map[string]struct{})
	if previous != nil {
		for _, ref := range contextManifestRefs(*previous) {
			existing[ref.Handle] = struct{}{}
		}
	}
	var result []agentcontext.ContentRef
	for _, ref := range contextManifestRefs(current) {
		if _, reused := existing[ref.Handle]; !reused {
			result = append(result, ref)
		}
	}
	return result
}

func contextManifestRefs(
	manifest agentcontext.ContextManifest,
) []agentcontext.ContentRef {
	result := []agentcontext.ContentRef{manifest.History.BaseRef}
	result = append(result, manifest.History.TailRefs...)
	for _, owner := range []agentcontext.OwnerManifest{
		manifest.Working,
		manifest.Evidence,
		manifest.Failures,
		manifest.Plan,
	} {
		result = append(result, owner.BaseRef)
		result = append(result, owner.DeltaRefs...)
	}
	return result
}

func (p *TerminalPublisher) Publish(ctx context.Context, committed CommittedTerminal) error {
	_, turnID, _ := protocol.OperationReferences(committed.Operation)
	entries, err := p.runtime.terminalStore.PendingOutbox(ctx, string(turnID))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.OperationID != committed.OperationID || entry.ItemID != committed.ItemID {
			return errors.New("terminal outbox projection identity mismatch")
		}
		if err := p.publishEntry(ctx, string(turnID), entry); err != nil {
			return err
		}
	}
	return nil
}

// Recover projects every pending terminal outbox after a restart. A Turn whose
// projection fails is handed to deferred and the remaining Turns continue, so
// one bad outbox cannot block startup; only an unreadable outbox aborts.
func (p *TerminalPublisher) Recover(
	ctx context.Context,
	deferred func(threadID protocol.ThreadID, turnID protocol.TurnID, err error),
) error {
	store, ok := p.runtime.terminalStore.(turnkernel.TerminalProjectionRecoveryStore)
	if !ok {
		if p.runtime.lifecycle != nil {
			return errors.New(
				"durable lifecycle requires terminal projection recovery store",
			)
		}
		return nil
	}
	projections, err := store.PendingTerminalProjections(ctx)
	if err != nil {
		return err
	}
	for _, projection := range projections {
		for _, entry := range projection.Entries {
			if err := p.publishEntry(ctx, projection.Envelope.TurnID, entry); err != nil {
				deferred(entry.ThreadID, protocol.TurnID(projection.Envelope.TurnID), fmt.Errorf(
					"project terminal outbox %s/%s: %w",
					projection.Envelope.TurnID,
					entry.ID,
					err,
				))
				break
			}
		}
	}
	return nil
}

// PublishPending projects whatever remains in one Turn's terminal outbox.
func (p *TerminalPublisher) PublishPending(ctx context.Context, turnID protocol.TurnID) error {
	entries, err := p.runtime.terminalStore.PendingOutbox(ctx, string(turnID))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := p.publishEntry(ctx, string(turnID), entry); err != nil {
			return err
		}
	}
	return nil
}

func (p *TerminalPublisher) publishEntry(
	ctx context.Context,
	turnID string,
	entry turnkernel.ProjectionOutboxEntry,
) error {
	data, err := DecodeTerminalOutboxEntry(entry)
	if err != nil {
		return err
	}
	if entry.EventID == "" || entry.OperationID == "" || entry.ThreadID == "" ||
		entry.TurnID == "" || entry.ItemID == "" {
		return errors.New("terminal outbox event identity is incomplete")
	}
	err = p.runtime.PublishTerminalProjection(ctx, entry, data)
	if err != nil {
		return err
	}
	return p.runtime.terminalStore.MarkOutboxPublished(ctx, turnID, []string{entry.ID})
}

func TerminalOutboxEventID(turnID protocol.TurnID, entryID string) protocol.EventID {
	sum := sha256.Sum256([]byte("terminal-outbox\x00" + string(turnID) + "\x00" + entryID))
	return protocol.EventID(fmt.Sprintf("evt_%x", sum[:16]))
}

func CommentaryEventID(messageID string) protocol.EventID {
	sum := sha256.Sum256([]byte("commentary\x00" + messageID))
	return protocol.EventID(fmt.Sprintf("evt_%x", sum[:16]))
}

// SettlementEventID names an event that settles an accepted operation, so a
// retried settlement re-projects the appended event instead of duplicating it.
func SettlementEventID(operationID protocol.OperationID, slot string) protocol.EventID {
	sum := sha256.Sum256([]byte("operation-settlement\x00" + string(operationID) + "\x00" + slot))
	return protocol.EventID(fmt.Sprintf("evt_%x", sum[:16]))
}

func releaseStagedContent(store ContentStore, staged []agentcontext.ContentRef) {
	if store == nil {
		return
	}
	if managed, ok := store.(interface{ ManagedOwnership() bool }); ok && managed.ManagedOwnership() {
		return // The enclosing stage releases all staged content, including partial failures.
	}
	for _, ref := range staged {
		_ = store.Release(context.Background(), ref.Handle)
		if collector, ok := store.(interface {
			CollectIfUnreferenced(context.Context, string) error
		}); ok {
			_ = collector.CollectIfUnreferenced(context.Background(), ref.Handle)
		}
	}
}

func (p *TerminalPublisher) loadContextManifest(
	threadID protocol.ThreadID,
) (agentcontext.ContextManifest, bool) {
	value, ok := p.runtime.contextManifests.Load(threadID)
	if !ok {
		return agentcontext.ContextManifest{}, false
	}
	manifest, ok := value.(agentcontext.ContextManifest)
	return manifest, ok
}
