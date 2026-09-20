package turnkernel

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

var ErrEffectResultIdentity = errors.New("effect result identity mismatch")

type DomainFactStore interface {
	AppendDomainFacts(context.Context, string, uint64, []DomainFact) error
	LoadDomainFacts(context.Context, string) ([]DomainFact, error)
}

// VerifiedDomainFactStore is a DomainFactStore that accepts batches whose
// digests and canonical encodings the coordinator already computed. Skipping
// the redundant per-fact digest recomputation is safe because stores still
// anchor every batch against the durable tail and decode re-verifies each
// stored fact.
type VerifiedDomainFactStore interface {
	DomainFactStore
	AppendVerifiedDomainFacts(context.Context, DomainFactBatch) error
}

type DomainFactBatch struct {
	TurnID       string
	ExpectedNext uint64
	// Facts share one immutable state per command; commits must not retain or
	// mutate them beyond the call.
	Facts []DomainFact
	// StateEncoding is the canonical JSON encoding of the facts' shared
	// state. PreviousDigest and PreviousEncoding describe the state of the
	// previous committed fact and are zero when ExpectedNext is 1. Verified
	// stores use them to encode without re-digesting or re-reading the
	// durable tail; other stores may ignore them.
	StateEncoding    []byte
	PreviousDigest   string
	PreviousEncoding []byte
}

type DomainFactCommit func(context.Context, DomainFactBatch) error

type EffectExecutor interface {
	ExecuteEffect(context.Context, Effect) (Command, error)
}

type EffectDispatcher interface {
	Dispatch(context.Context, Effect, func(Command) error) error
}

// DomainFactObserver receives facts only after their authoritative append
// succeeds. It is diagnostics-only: errors cannot flow back into the Kernel.
type DomainFactObserver func(context.Context, []DomainFact)

type SynchronousEffectDispatcher struct {
	Executors map[EffectKind]EffectExecutor
}

func (d SynchronousEffectDispatcher) Dispatch(
	ctx context.Context,
	effect Effect,
	submit func(Command) error,
) error {
	executor := d.Executors[effect.Kind]
	if executor == nil {
		return fmt.Errorf("no executor for effect kind %q", effect.Kind)
	}
	result, err := executor.ExecuteEffect(ctx, effect)
	if err != nil {
		result = EffectResultReceived{
			EffectID: effect.ID,
			Success:  false,
			Error:    err.Error(),
		}
	}
	if result == nil {
		return errors.New("effect executor returned no result command")
	}
	if resultEffectID(result) != effect.ID {
		return ErrEffectResultIdentity
	}
	return submit(result)
}

type TurnCoordinator struct {
	mu          sync.Mutex
	turnID      string
	state       State
	reducer     Reducer
	store       DomainFactStore
	dispatcher  EffectDispatcher
	observer    DomainFactObserver
	nextFact    uint64
	lastDigest  string
	lastEncoded []byte
}

func (c *TurnCoordinator) SetDomainFactObserver(observer DomainFactObserver) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observer = observer
}

func NewTurnCoordinator(
	turnID string,
	state State,
	store DomainFactStore,
	dispatcher EffectDispatcher,
) (*TurnCoordinator, error) {
	if turnID == "" {
		return nil, errors.New("turn coordinator turn id is empty")
	}
	if state.ProfileRevision == 0 {
		return nil, errors.New("turn coordinator requires a frozen profile revision")
	}
	if store == nil {
		return nil, errors.New("turn coordinator domain fact store is nil")
	}
	if dispatcher == nil {
		return nil, errors.New("turn coordinator effect dispatcher is nil")
	}
	if err := Validate(state); err != nil {
		return nil, err
	}
	facts, err := store.LoadDomainFacts(context.Background(), turnID)
	if err != nil {
		return nil, err
	}
	if len(facts) != 0 {
		return nil, errors.New(
			"turn coordinator requires RestoreTurnCoordinator for existing facts",
		)
	}
	return &TurnCoordinator{
		turnID: turnID, state: cloneState(state), store: store,
		dispatcher: dispatcher, nextFact: 1,
	}, nil
}

func RestoreTurnCoordinator(
	ctx context.Context,
	turnID string,
	store DomainFactStore,
	dispatcher EffectDispatcher,
) (*TurnCoordinator, error) {
	if turnID == "" || store == nil || dispatcher == nil {
		return nil, errors.New("turn coordinator restore dependencies are incomplete")
	}
	facts, err := store.LoadDomainFacts(ctx, turnID)
	if err != nil {
		return nil, err
	}
	if len(facts) == 0 {
		return nil, errors.New("turn coordinator restore has no domain facts")
	}
	start := facts[0].Sequence
	if start == 0 {
		return nil, errors.New("domain fact sequence is invalid")
	}
	var restored State
	for index, fact := range facts {
		if fact.TurnID != turnID || fact.Sequence != start+uint64(index) {
			return nil, fmt.Errorf("domain fact sequence is invalid at index %d", index)
		}
		if err := Validate(fact.State); err != nil {
			return nil, fmt.Errorf("domain fact state %d: %w", index+1, err)
		}
		digest, digestErr := Digest(fact.State)
		if digestErr != nil || digest != fact.StateDigest {
			return nil, fmt.Errorf("domain fact digest mismatch at sequence %d", fact.Sequence)
		}
		restored = cloneState(fact.State)
	}
	// Cache the digest and encoding of the durable tail so the next
	// transition can anchor its batch without re-reading stored facts. The
	// loop above already verified this digest against the last fact.
	lastDigest, lastEncoded, err := digestValidated(restored)
	if err != nil || lastDigest != facts[len(facts)-1].StateDigest {
		return nil, fmt.Errorf(
			"restored state diverges from digest at sequence %d: %w",
			facts[len(facts)-1].Sequence,
			err,
		)
	}
	coordinator := &TurnCoordinator{
		turnID: turnID, state: restored, store: store,
		dispatcher: dispatcher,
		nextFact:   facts[len(facts)-1].Sequence + 1,
		lastDigest: lastDigest, lastEncoded: lastEncoded,
	}
	for _, id := range sortedEffectIDs(restored.PendingEffects) {
		if restored.PendingEffects[id].Status != EffectRunning {
			continue
		}
		if err := coordinator.Submit(ctx, EffectRequeued{EffectID: id}); err != nil {
			return nil, fmt.Errorf("requeue running effect %q: %w", id, err)
		}
	}
	restored = coordinator.Snapshot()
	for _, id := range sortedEffectIDs(restored.PendingEffects) {
		effect := restored.PendingEffects[id]
		if err := dispatcher.Dispatch(ctx, effect, func(result Command) error {
			return coordinator.Submit(ctx, result)
		}); err != nil {
			return nil, err
		}
	}
	return coordinator, nil
}

func (c *TurnCoordinator) Snapshot() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneState(c.state)
}

// LastDigest returns the digest of the most recently committed state. It is
// unavailable before the first committed transition of a new turn.
func (c *TurnCoordinator) LastDigest() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastDigest == "" {
		return "", false
	}
	return c.lastDigest, true
}

func (c *TurnCoordinator) TurnID() string {
	return c.turnID
}

func (c *TurnCoordinator) DomainFacts(
	ctx context.Context,
) ([]DomainFact, error) {
	return c.store.LoadDomainFacts(ctx, c.turnID)
}

func (c *TurnCoordinator) Submit(ctx context.Context, command Command) error {
	return c.SubmitWithCommit(ctx, command, nil)
}

// SubmitWithCommit lets a persistence owner atomically append the resulting
// facts with another state transition. A nil commit uses the coordinator's
// configured DomainFactStore.
func (c *TurnCoordinator) SubmitWithCommit(
	ctx context.Context,
	command Command,
	commit DomainFactCommit,
) error {
	effects, err := c.transition(ctx, command, commit)
	if err != nil {
		return err
	}
	for _, effect := range effects {
		if err := c.dispatcher.Dispatch(ctx, effect, func(result Command) error {
			return c.Submit(ctx, result)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (c *TurnCoordinator) transition(
	ctx context.Context,
	command Command,
	commit DomainFactCommit,
) ([]Effect, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	transition, err := c.reducer.Apply(c.state, command)
	if err != nil {
		return nil, err
	}
	// Apply validates every state it publishes, so digesting skips
	// re-validation and reuses the canonical encoding for persistence.
	digest, encoded, err := digestValidated(transition.State)
	if err != nil {
		return nil, err
	}
	// Every state published by Apply is immutable, so one defensive copy per
	// command separates the batch and observer from the coordinator's
	// working state.
	stateSnapshot := cloneState(transition.State)
	facts := make([]DomainFact, 0, max(1, len(transition.Events)))
	if len(transition.Events) == 0 {
		facts = append(facts, DomainFact{
			TurnID: c.turnID, Sequence: c.nextFact,
			Command: CommandName(command), State: stateSnapshot,
			StateDigest: digest,
		})
	} else {
		for index, event := range transition.Events {
			facts = append(facts, DomainFact{
				TurnID: c.turnID, Sequence: c.nextFact + uint64(index),
				Command: CommandName(command), Event: event,
				State: stateSnapshot, StateDigest: digest,
			})
		}
	}
	previousDigest, previousEncoding := c.lastDigest, c.lastEncoded
	if c.nextFact == 1 {
		previousDigest, previousEncoding = "", nil
	}
	if commit == nil {
		commit = c.defaultCommit()
	}
	if err := commit(ctx, DomainFactBatch{
		TurnID:           c.turnID,
		ExpectedNext:     c.nextFact,
		Facts:            facts,
		StateEncoding:    encoded,
		PreviousDigest:   previousDigest,
		PreviousEncoding: previousEncoding,
	}); err != nil {
		return nil, protocol.NewFault(
			protocol.CodeUnavailable,
			"turn domain facts could not be persisted",
			true,
			protocol.FaultMetadata{
				Origin:         protocol.FaultOriginPersistence,
				Disposition:    protocol.FaultResumeTurn,
				SideEffects:    protocol.SideEffectUnknown,
				RecoveryAction: "restore persistence and resume from durable facts",
			},
			err,
		)
	}
	c.state = transition.State
	c.lastDigest = digest
	c.lastEncoded = encoded
	c.nextFact += uint64(len(facts))
	c.observeFacts(facts)
	return append([]Effect(nil), transition.Effects...), nil
}

func (c *TurnCoordinator) defaultCommit() DomainFactCommit {
	if verified, ok := c.store.(VerifiedDomainFactStore); ok {
		return verified.AppendVerifiedDomainFacts
	}
	return func(ctx context.Context, batch DomainFactBatch) error {
		return c.store.AppendDomainFacts(
			ctx,
			batch.TurnID,
			batch.ExpectedNext,
			batch.Facts,
		)
	}
}

func (c *TurnCoordinator) observeFacts(facts []DomainFact) {
	if c.observer == nil || len(facts) == 0 {
		return
	}
	func() {
		defer func() {
			if recover() != nil {
				_ = debug.Stack()
			}
		}()
		c.observer(context.Background(), facts)
	}()
}

func resultEffectID(command Command) string {
	switch value := command.(type) {
	case EffectResultReceived:
		return value.EffectID
	case PersistenceResultReceived:
		return value.EffectID
	case ApprovalResultReceived:
		return value.EffectID
	case InputResultReceived:
		return value.EffectID
	case ToolResultReceived:
		return value.EffectID
	case ModelSampleResultReceived:
		return value.EffectID
	case VerificationFinished:
		return value.EffectID
	case JournalResultReceived:
		return value.EffectID
	default:
		return ""
	}
}
