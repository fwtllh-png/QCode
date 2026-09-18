package turnkernel

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestPhase4R2TerminalEnvelopeCommitsAtomicallyAndIdempotently(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	store := NewMemoryTerminalEnvelopeStore(func() time.Time { return now }, nil)
	envelope := terminalEnvelopeFixture(t)
	if err := store.AppendDomainFacts(
		t.Context(),
		envelope.TurnID,
		1,
		envelope.DomainFacts[:1],
	); err != nil {
		t.Fatal(err)
	}
	first, err := store.CommitTerminal(t.Context(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CommitTerminal(t.Context(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first.Digest == "" || first.CommittedAt != now {
		t.Fatalf("markers = first:%+v second:%+v", first, second)
	}
	stored, marker, err := store.LoadTerminal(t.Context(), envelope.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if marker != first ||
		stored.OperationCommit.OperationID != envelope.OperationCommit.OperationID ||
		stored.OperationCommit.Status != envelope.OperationCommit.Status ||
		string(stored.OperationCommit.Receipt) !=
			string(envelope.OperationCommit.Receipt) ||
		len(stored.DomainFacts) != 2 ||
		len(stored.FinalOutput) != 1 {
		t.Fatalf("stored envelope = %+v marker=%+v", stored, marker)
	}
	pending, err := store.PendingOutbox(t.Context(), envelope.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending outbox = %+v", pending)
	}
	if publishErr := store.MarkOutboxPublished(
		t.Context(),
		envelope.TurnID,
		[]string{pending[0].ID},
	); publishErr != nil {
		t.Fatal(publishErr)
	}
	if publishErr := store.MarkOutboxPublished(
		t.Context(),
		envelope.TurnID,
		[]string{pending[0].ID},
	); publishErr != nil {
		t.Fatal(publishErr)
	}
	pending, err = store.PendingOutbox(t.Context(), envelope.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending outbox after idempotent publish = %+v", pending)
	}
}

func TestLatestSessionDeltaIsScopedByThreadAndRevision(t *testing.T) {
	store := NewMemoryTerminalEnvelopeStore(nil, nil)
	first := terminalEnvelopeFixture(t)
	first.SessionDelta = json.RawMessage(`{"base_revision":0,"digest":"first"}`)
	if _, err := store.CommitTerminal(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	second := terminalEnvelopeFixture(t)
	second.TurnID = "turn-2"
	second.EffectID = "effect-terminal-2"
	for index := range second.DomainFacts {
		second.DomainFacts[index].TurnID = second.TurnID
	}
	for index := range second.Outbox {
		second.Outbox[index].TurnID = protocol.TurnID(second.TurnID)
	}
	second.SessionDelta = json.RawMessage(`{"base_revision":1,"digest":"second"}`)
	if _, err := store.CommitTerminal(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	raw, err := store.LatestSessionDelta(t.Context(), "thread-1")
	if err != nil || string(raw) != string(second.SessionDelta) {
		t.Fatalf("latest delta = %s, error=%v", raw, err)
	}
	if raw, err := store.LatestSessionDelta(t.Context(), "other"); err != nil ||
		len(raw) != 0 {
		t.Fatalf("other delta = %s, error=%v", raw, err)
	}
}

func TestPhase4R2DomainFactsAreStrictlyOrderedAndTerminallySealed(t *testing.T) {
	store := NewMemoryTerminalEnvelopeStore(nil, nil)
	envelope := terminalEnvelopeFixture(t)
	if err := store.AppendDomainFacts(
		t.Context(),
		envelope.TurnID,
		1,
		envelope.DomainFacts[:1],
	); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendDomainFacts(
		t.Context(),
		envelope.TurnID,
		1,
		envelope.DomainFacts[1:],
	); err == nil {
		t.Fatal("sequence conflict was accepted")
	}
	facts, err := store.LoadDomainFacts(t.Context(), envelope.TurnID)
	if err != nil || len(facts) != 1 {
		t.Fatalf("facts = %+v error=%v", facts, err)
	}
	if _, err := store.CommitTerminal(t.Context(), envelope); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendDomainFacts(
		t.Context(),
		envelope.TurnID,
		3,
		[]DomainFact{{
			TurnID: envelope.TurnID, Sequence: 3,
			Command: "post_terminal", StateDigest: "sha256:forbidden",
		}},
	); err == nil {
		t.Fatal("post-terminal domain fact was accepted")
	}
}

func TestPhase4R2TerminalEnvelopeFailureLeaksNoPartialFacts(t *testing.T) {
	for _, stage := range []TerminalEnvelopeStage{
		StageDomainFacts,
		StageMeasurement,
		StageReceipt,
		StageFinalOutput,
		StageTerminalEvent,
		StageOperationCommit,
		StageOutbox,
		StageCommitMarker,
	} {
		t.Run(string(stage), func(t *testing.T) {
			store := NewMemoryTerminalEnvelopeStore(nil, func(
				current TerminalEnvelopeStage,
			) error {
				if current == stage {
					return errors.New("injected crash")
				}
				return nil
			})
			envelope := terminalEnvelopeFixture(t)
			if _, err := store.CommitTerminal(
				t.Context(),
				envelope,
			); err == nil {
				t.Fatal("faulted terminal commit succeeded")
			}
			if _, _, err := store.LoadTerminal(
				t.Context(),
				envelope.TurnID,
			); !errors.Is(err, ErrTerminalEnvelopeMissing) {
				t.Fatalf("partial terminal became visible: %v", err)
			}
			if _, err := store.PendingOutbox(
				t.Context(),
				envelope.TurnID,
			); !errors.Is(err, ErrTerminalEnvelopeMissing) {
				t.Fatalf("partial outbox became visible: %v", err)
			}
		})
	}
}

func TestPhase4R2TerminalEnvelopeRejectsConflictingRetry(t *testing.T) {
	store := NewMemoryTerminalEnvelopeStore(nil, nil)
	envelope := terminalEnvelopeFixture(t)
	if _, err := store.CommitTerminal(t.Context(), envelope); err != nil {
		t.Fatal(err)
	}
	envelope.EffectID = "different-effect"
	if _, err := store.CommitTerminal(
		context.Background(),
		envelope,
	); !errors.Is(err, ErrTerminalEnvelopeConflict) {
		t.Fatalf("conflicting retry error = %v", err)
	}
}

func TestSO4TerminalEnvelopeDigestCoversMeasurement(t *testing.T) {
	first := terminalEnvelopeFixture(t)
	firstDigest, err := ValidateTerminalEnvelope(first)
	if err != nil {
		t.Fatal(err)
	}
	second := cloneTerminalEnvelope(first)
	second.Measurement, err = NewTerminalMeasurementSnapshot(
		first.Measurement.FrozenAt.Add(time.Second),
		nil,
		first.Measurement.Usage,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	second.Receipt.MeasurementDigest = second.Measurement.Digest
	second.Receipt.UsageDigest = second.Measurement.UsageDigest
	secondDigest, err := ValidateTerminalEnvelope(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == secondDigest {
		t.Fatal("terminal envelope digest ignored measurement")
	}
}

func TestSO4TerminalEnvelopeRejectsReceiptUsageDrift(t *testing.T) {
	envelope := terminalEnvelopeFixture(t)
	envelope.Receipt.InputTokens++
	if _, err := ValidateTerminalEnvelope(envelope); err == nil {
		t.Fatal("receipt usage drift was accepted")
	}
}

func TestTerminalEnvelopeComparesTypedFaultsByValue(t *testing.T) {
	envelope := terminalEnvelopeFixture(t)
	fault := &protocol.FaultMetadata{
		Origin:      protocol.FaultOriginProvider,
		Disposition: protocol.FaultFailTurn,
		SideEffects: protocol.SideEffectNone,
		Stage:       protocol.FaultStageModelSample,
		Reason:      protocol.ProblemReasonProviderRateLimited,
	}
	envelope.FrozenState.Terminal.Fault = fault
	envelope.TerminalEvent.Terminal.Fault =
		protocol.CloneFaultMetadata(fault)
	if _, err := ValidateTerminalEnvelope(envelope); err != nil {
		t.Fatalf("equivalent cloned fault was rejected: %v", err)
	}
}

func TestTerminalEnvelopeRejectsTypedFaultValueDrift(t *testing.T) {
	envelope := terminalEnvelopeFixture(t)
	envelope.FrozenState.Terminal.Fault = &protocol.FaultMetadata{
		Origin:      protocol.FaultOriginProvider,
		Disposition: protocol.FaultFailTurn,
	}
	envelope.TerminalEvent.Terminal.Fault = &protocol.FaultMetadata{
		Origin:      protocol.FaultOriginProvider,
		Disposition: protocol.FaultResumeTurn,
	}
	if _, err := ValidateTerminalEnvelope(envelope); err == nil {
		t.Fatal("terminal fault value drift was accepted")
	}
}

func TestTerminalEnvelopeRejectsFaultReasonDrift(t *testing.T) {
	envelope := terminalEnvelopeFixture(t)
	envelope.FrozenState.Terminal.Fault = &protocol.FaultMetadata{
		Origin: protocol.FaultOriginRuntime, Disposition: protocol.FaultResumeTurn,
		Reason: protocol.ProblemReasonTokenBudgetExhausted,
	}
	envelope.TerminalEvent.Terminal.Fault =
		protocol.CloneFaultMetadata(envelope.FrozenState.Terminal.Fault)
	envelope.TerminalEvent.Terminal.Fault.Reason = protocol.ProblemReasonCostBudgetExhausted
	if _, err := ValidateTerminalEnvelope(envelope); err == nil {
		t.Fatal("terminal fault reason drift was accepted")
	}
}

func terminalEnvelopeFixture(t *testing.T) TerminalEnvelope {
	t.Helper()
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, ModelTextReceived{Text: "done"}).State
	state = apply(t, state, ReleaseProvisionalOutput{}).State
	state = apply(t, state, TerminalRequested{}).State
	state = apply(t, state, FinishTerminal{}).State
	digest, err := Digest(state)
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := NewTerminalMeasurementSnapshot(
		time.Unix(1, 0),
		nil,
		state.Usage,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	terminal := *state.Terminal
	return TerminalEnvelope{
		TurnID:      "turn-1",
		EffectID:    "effect-terminal-1",
		FrozenState: state,
		DomainFacts: []DomainFact{
			{
				TurnID: "turn-1", Sequence: 1, Command: "start_turn",
				Event: Event{
					Kind: EventTransition,
					From: PhaseCreated,
					To:   PhasePreparing,
				},
				State:       state,
				StateDigest: digest,
			},
			{
				TurnID: "turn-1", Sequence: 2, Command: "finish_terminal",
				Event: Event{
					Kind:     EventTerminalCommitted,
					Terminal: &terminal,
				},
				State:       state,
				StateDigest: digest,
			},
		},
		Measurement: measurement,
		Receipt: &protocol.ExecutionReceiptData{
			Goal:              "answer",
			Intent:            protocol.TurnIntentAnswer,
			Outcome:           protocol.TurnOutcomeAnswered,
			MeasurementDigest: measurement.Digest,
			UsageDigest:       measurement.UsageDigest,
		},
		FinalOutput: append([]string(nil), state.FinalOutput...),
		TerminalEvent: Event{
			Kind:     EventTerminalCommitted,
			Terminal: &terminal,
		},
		OperationCommit: OperationCommitFact{
			OperationID: "operation-1",
			Status:      "committed",
		},
		Outbox: []ProjectionOutboxEntry{
			{
				ID: "outbox-receipt", EventID: "event-receipt",
				OperationID: "operation-1", ThreadID: "thread-1",
				TurnID: "turn-1", ItemID: "item-1", Kind: "turn.receipt",
				Payload: json.RawMessage(`{"goal":"answer"}`),
			},
			{
				ID: "outbox-terminal", EventID: "event-terminal",
				OperationID: "operation-1", ThreadID: "thread-1",
				TurnID: "turn-1", ItemID: "item-1", Kind: "turn.completed",
				Payload: json.RawMessage(`{"text":"done"}`),
			},
		},
	}
}
