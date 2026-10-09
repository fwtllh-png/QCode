package turnkernel

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerassembly "github.com/fwtllh-png/QCode/internal/adapter/provider/assembly"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestC1CoordinatorRuntimeRestoresAndDurablyRequeuesRunningEffect(
	t *testing.T,
) {
	store := NewMemoryTerminalEnvelopeStore(nil, nil)
	first, err := NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := first.Open(
		t.Context(),
		"turn-c1-runtime",
		NewState(protocol.TurnIntentAnswer, "act", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []Command{
		StartTurn{},
		PreparationFinished{},
		ToolCallsProposed{
			Calls: []ToolCallState{{ID: "call-1", Name: "read"}},
		},
	} {
		if err := handle.Coordinator.Submit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	effect, err := handle.Dispatcher.Start(EffectExecuteTool, "call-1")
	if err != nil {
		t.Fatal(err)
	}
	running := handle.Coordinator.Snapshot()
	runningDigest, err := Digest(running)
	if err != nil {
		t.Fatal(err)
	}
	if running.PendingEffects[effect.ID].Status != EffectRunning {
		t.Fatalf("running state = %+v", running.PendingEffects[effect.ID])
	}
	if err := first.Release(t.Context(), "turn-c1-runtime"); err != nil {
		t.Fatal(err)
	}

	second, err := NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := second.Restore(t.Context(), "turn-c1-runtime")
	if err != nil {
		t.Fatal(err)
	}
	state := restored.Coordinator.Snapshot()
	requeued := state.PendingEffects[effect.ID]
	if requeued.Status != EffectRequested || requeued.Attempt != 1 {
		t.Fatalf("requeued effect = %+v", requeued)
	}
	pending := restored.Dispatcher.PendingRouted(EffectExecuteTool)
	if len(pending) != 1 || pending[0].ID != effect.ID {
		t.Fatalf("restored pending effects = %+v", pending)
	}
	facts, err := store.LoadDomainFacts(t.Context(), "turn-c1-runtime")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) < 2 ||
		facts[len(facts)-2].StateDigest != runningDigest ||
		facts[len(facts)-1].Command != "effect_requeued" {
		t.Fatalf("restored facts = %+v", facts)
	}
	restarted, err := restored.Dispatcher.Start(
		EffectExecuteTool,
		"call-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Attempt != 2 {
		t.Fatalf("restarted attempt = %d, want 2", restarted.Attempt)
	}
	if err := restored.Dispatcher.Resolve(ToolResultReceived{
		EffectID: restarted.ID,
		CallID:   "call-1",
	}); err != nil {
		t.Fatal(err)
	}
	closed := restored.Coordinator.Snapshot()
	if len(closed.PendingEffects) != 0 ||
		closed.CompletedEffects[effect.ID].Attempt != 2 {
		t.Fatalf("restored closure = %+v", closed)
	}
	if _, err := second.Restore(
		t.Context(),
		"turn-c1-runtime",
	); !errors.Is(err, ErrCoordinatorAlreadyActive) {
		t.Fatalf("duplicate restore error = %v", err)
	}
}

func TestC1CoordinatorRuntimeRejectsIncompleteRestore(t *testing.T) {
	runtime, err := NewStoreCoordinatorRuntime(
		NewMemoryTerminalEnvelopeStore(nil, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Restore(
		t.Context(),
		"turn-without-facts",
	); err == nil {
		t.Fatal("restore without facts succeeded")
	}
}

func TestC3CoordinatorRuntimeRequeuesRunningModelEffect(t *testing.T) {
	store := NewMemoryTerminalEnvelopeStore(nil, nil)
	first, err := NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := first.Open(
		t.Context(),
		"turn-c3-model-restore",
		NewState(protocol.TurnIntentAnswer, "act", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []Command{
		StartTurn{},
		PreparationFinished{},
		ModelSampleRequested{SampleID: "sample-restore"},
	} {
		if err := handle.Coordinator.Submit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	effect, err := handle.Dispatcher.Start(
		EffectSampleProvider,
		"sample-restore",
	)
	if err != nil {
		t.Fatal(err)
	}
	assembly := providerassembly.NewResponseAssembly("sample-restore")
	if err := assembly.BeginTransport(provider.TransportMetadata{
		LogicalRequestID:   "sample-restore",
		TransportRequestID: "transport-1",
		Attempt:            1,
	}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []provider.StreamEvent{
		{Type: provider.EventTextDelta, Text: "confirmed"},
		{
			Type: provider.EventToolCallDelta,
			ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call-1", Name: "read",
				Arguments: `{"path":`,
			},
		},
	} {
		if _, err := assembly.Apply(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := handle.Coordinator.Submit(
		t.Context(),
		ModelSampleProgressRecorded{
			EffectID: effect.ID,
			SampleID: "sample-restore",
			Attempt:  effect.Attempt,
			Assembly: *assembly,
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(
		t.Context(),
		"turn-c3-model-restore",
	); err != nil {
		t.Fatal(err)
	}
	second, err := NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := second.Restore(
		t.Context(),
		"turn-c3-model-restore",
	)
	if err != nil {
		t.Fatal(err)
	}
	state := restored.Coordinator.Snapshot()
	requeued := state.PendingEffects[effect.ID]
	if requeued.Status != EffectRequested ||
		requeued.Attempt != effect.Attempt {
		t.Fatalf("requeued model effect = %+v", requeued)
	}
	if state.SampleLedger["sample-restore"].Status != SampleRequested ||
		state.ActiveSampleID != "" {
		t.Fatalf("restored sample state = %+v", state)
	}
	restoredAssembly := state.SampleLedger["sample-restore"].Assembly
	if restoredAssembly == nil ||
		restoredAssembly.EventCount() != 2 ||
		len(restoredAssembly.CurrentBlocks()) != 1 ||
		restoredAssembly.CurrentBlocks()[0].Text != "confirmed" ||
		len(restoredAssembly.Segments[0].ToolFragments) != 1 ||
		restoredAssembly.Segments[0].ToolFragments[0].Arguments !=
			`{"path":` {
		t.Fatalf("restored response assembly = %+v", restoredAssembly)
	}
}

func TestModelTransportBoundaryIsDurablyQuiescent(t *testing.T) {
	store := NewMemoryTerminalEnvelopeStore(nil, nil)
	runtime, err := NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Open(
		t.Context(),
		"turn-transport-boundary",
		NewState(protocol.TurnIntentAnswer, "act", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []Command{
		StartTurn{},
		PreparationFinished{},
		ModelSampleRequested{SampleID: "sample-1"},
	} {
		if err := handle.Coordinator.Submit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	effect, err := handle.Dispatcher.Start(
		EffectSampleProvider,
		"sample-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	assembly := providerassembly.NewResponseAssembly("sample-1")
	if err := assembly.BeginTransport(provider.TransportMetadata{
		LogicalRequestID: "sample-1", TransportRequestID: "transport-1",
		Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []provider.StreamEvent{
		{Type: provider.EventTextDelta, Text: "partial"},
		{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens},
	} {
		if _, err := assembly.Apply(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := handle.Coordinator.Submit(
		t.Context(),
		ModelSampleProgressRecorded{
			EffectID: effect.ID, SampleID: "sample-1",
			Attempt: effect.Attempt, Assembly: *assembly,
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := handle.Dispatcher.Requeue(
		EffectSampleProvider,
		"sample-1",
	); err != nil {
		t.Fatal(err)
	}
	state := handle.Coordinator.Snapshot()
	if state.ActiveSampleID != "" ||
		state.SampleLedger["sample-1"].Status != SampleRequested ||
		state.SampleLedger["sample-1"].ProviderRetries != 0 ||
		state.PendingEffects[effect.ID].Status != EffectRequested {
		t.Fatalf("transport boundary state = %+v", state)
	}
	restarted, err := handle.Dispatcher.Start(
		EffectSampleProvider,
		"sample-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Attempt != 2 {
		t.Fatalf("next transport attempt = %d", restarted.Attempt)
	}
}

func TestCompleteAssemblyRecoveryResolvesWithoutProviderRestart(t *testing.T) {
	store := NewMemoryTerminalEnvelopeStore(nil, nil)
	first, err := NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := first.Open(
		t.Context(),
		"turn-complete-assembly",
		NewState(protocol.TurnIntentAnswer, "act", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []Command{
		StartTurn{},
		PreparationFinished{},
		ModelSampleRequested{SampleID: "sample-1"},
	} {
		if err := handle.Coordinator.Submit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	effect, err := handle.Dispatcher.Start(
		EffectSampleProvider,
		"sample-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	assembly := providerassembly.NewResponseAssembly("sample-1")
	if err := assembly.BeginTransport(provider.TransportMetadata{
		LogicalRequestID: "sample-1", TransportRequestID: "transport-1",
		Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []provider.StreamEvent{
		{Type: provider.EventTextDelta, Text: "complete"},
		{Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn},
	} {
		if _, err := assembly.Apply(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := handle.Coordinator.Submit(
		t.Context(),
		ModelSampleProgressRecorded{
			EffectID: effect.ID, SampleID: "sample-1",
			Attempt: effect.Attempt, Assembly: *assembly,
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(t.Context(), "turn-complete-assembly"); err != nil {
		t.Fatal(err)
	}
	second, err := NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := second.Restore(
		t.Context(),
		"turn-complete-assembly",
	)
	if err != nil {
		t.Fatal(err)
	}
	state := restored.Coordinator.Snapshot()
	if state.ActiveSampleID != "" ||
		state.SampleLedger["sample-1"].Status != SampleRequested {
		t.Fatalf("restored complete assembly = %+v", state)
	}
	if err := restored.Dispatcher.Resolve(ModelSampleResultReceived{
		EffectID: effect.ID, SampleID: "sample-1", Text: "complete",
	}); err != nil {
		t.Fatal(err)
	}
	state = restored.Coordinator.Snapshot()
	if state.SampleLedger["sample-1"].Status != SampleCompleted ||
		state.ActiveSampleID != "" ||
		len(state.PendingEffects) != 0 {
		t.Fatalf("resolved complete assembly = %+v", state)
	}
}

func TestC5CoordinatorRestorePreservesDurableEffectPayload(t *testing.T) {
	store := NewMemoryTerminalEnvelopeStore(nil, nil)
	first, err := NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := first.Open(
		t.Context(),
		"turn-c5-effect-payload",
		NewState(protocol.TurnIntentAnswer, "act", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []Command{
		StartTurn{},
		PreparationFinished{},
		ToolCallsProposed{Calls: []ToolCallState{{
			ID:        "call-1",
			Name:      "write",
			Arguments: `{"path":"a.txt","content":"x"}`,
		}}},
	} {
		if err := handle.Coordinator.Submit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	var expected Effect
	for _, effect := range handle.Coordinator.Snapshot().PendingEffects {
		if effect.Kind == EffectExecuteTool {
			expected = effect
		}
	}
	if len(expected.Payload) == 0 {
		t.Fatal("tool effect payload was not persisted")
	}
	if err := first.Release(
		t.Context(),
		"turn-c5-effect-payload",
	); err != nil {
		t.Fatal(err)
	}
	second, err := NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := second.Restore(
		t.Context(),
		"turn-c5-effect-payload",
	)
	if err != nil {
		t.Fatal(err)
	}
	var actual Effect
	for _, effect := range restored.Coordinator.Snapshot().PendingEffects {
		if effect.Kind == EffectExecuteTool {
			actual = effect
		}
	}
	if !reflect.DeepEqual(actual.Payload, expected.Payload) ||
		actual.PayloadDigest != expected.PayloadDigest {
		t.Fatalf("restored effect=%+v want=%+v", actual, expected)
	}
}

func TestProviderRetryScheduleSurvivesCoordinatorRestore(t *testing.T) {
	store := NewMemoryTerminalEnvelopeStore(nil, nil)
	first, err := NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := first.Open(
		t.Context(),
		"turn-provider-retry-restore",
		NewState(protocol.TurnIntentAnswer, "act", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []Command{
		StartTurn{},
		PreparationFinished{},
		ModelSampleRequested{SampleID: "sample-retry"},
	} {
		if err := handle.Coordinator.Submit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	effect, err := handle.Dispatcher.Start(
		EffectSampleProvider,
		"sample-retry",
	)
	if err != nil {
		t.Fatal(err)
	}
	retryAt := time.Now().Add(time.Minute).UTC()
	if err := handle.Dispatcher.ScheduleRetry(
		EffectSampleProvider,
		"sample-retry",
		ProviderRetryRequested{
			EffectID: effect.ID, SampleID: "sample-retry",
			Attempt: effect.Attempt, Retry: 1,
			Failure: provider.Failure{
				Code:         provider.FailureRateLimit,
				Message:      "rate limited",
				RetryAfterMS: 60000,
			},
			EffectiveDelay: 60 * time.Second,
			RetryAt:        retryAt,
			PolicyRevision: "provider-retry/v1",
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(t.Context(), "turn-provider-retry-restore"); err != nil {
		t.Fatal(err)
	}
	second, err := NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := second.Restore(
		t.Context(),
		"turn-provider-retry-restore",
	)
	if err != nil {
		t.Fatal(err)
	}
	sample := restored.Coordinator.Snapshot().SampleLedger["sample-retry"]
	if sample.ProviderRetries != 1 ||
		sample.LastFailure == nil ||
		sample.LastFailure.Code != provider.FailureRateLimit ||
		sample.Retry == nil ||
		!sample.Retry.RetryAt.Equal(retryAt) ||
		sample.Retry.PolicyRevision != "provider-retry/v1" {
		t.Fatalf("restored retry = %+v", sample)
	}
}
