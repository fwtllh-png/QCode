package app

import (
	"context"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type closingNarrativeRunner struct{ started chan struct{} }

func (r *closingNarrativeRunner) Run(ctx context.Context) (agentengine.NarrativeGenerationResult, error) {
	close(r.started)
	<-ctx.Done()
	return agentengine.NarrativeGenerationResult{Fallback: true, Calls: []agentengine.NarrativeUsage{{ID: "closing-call", Attempt: 1, Provider: "fixture", Model: "summary", Usage: provider.Usage{InputTokens: 9}, ModelMetadata: protocol.ModelMetadataProvenance{CanonicalID: "fixture", WireID: "fixture", Limits: "fixture", Capabilities: "fixture", Pricing: "fixture"}}}}, nil
}

type closingMaintenanceEngine struct {
	NoopEngine
	runner *closingNarrativeRunner
}

func (e *closingMaintenanceEngine) PreparePostTurnNarrative(protocol.ThreadID, protocol.TurnID) (agentengine.PostTurnNarrativeRunner, error) {
	return e.runner, nil
}

func TestRuntimeCloseSettlesNarrativeUsageBeforeClosingEventStore(t *testing.T) {
	events := NewMemoryEventStore(32)
	runner := &closingNarrativeRunner{started: make(chan struct{})}
	runtime := NewRuntime(Options{EventStore: events, Engine: &closingMaintenanceEngine{runner: runner}})
	sink := &runtimeSink{runtime: runtime, terminal: &protocol.TurnCompletedData{Text: "done"}}
	sink.publishPostTurnContextMaintenance("operation", "thread", "turn", "item")
	<-runner.started
	closeRuntime(t, runtime)
	events.mu.Lock()
	defer events.mu.Unlock()
	for _, event := range events.events {
		if usage, ok := event.Data.(*protocol.UsageData); ok && usage.InputTokens == 9 {
			return
		}
	}
	t.Fatal("runtime closed its store before settling observed narrative usage")
}

func TestNarrativeFailedAttemptsPublishSeparateUsageWithoutReceipt(t *testing.T) {
	events := NewMemoryEventStore(32)
	runtime := NewRuntime(Options{EventStore: events})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	sink := &runtimeSink{runtime: runtime}
	result := agentengine.NarrativeGenerationResult{Fallback: true, FailureReason: "invalid JSON", Calls: []agentengine.NarrativeUsage{
		{ID: "source-call", Attempt: 1, Provider: "fixture", Model: "summary", Usage: provider.Usage{InputTokens: 7, OutputTokens: 2}, CostKnown: true, CostUSD: 0.000009},
		{ID: "source-call", Attempt: 2, Provider: "fixture", Model: "summary", Usage: provider.Usage{InputTokens: 8, OutputTokens: 3}, CostKnown: true, CostUSD: 0.000011},
	}}
	for i := range result.Calls {
		result.Calls[i].ModelMetadata = protocol.ModelMetadataProvenance{CanonicalID: "fixture", WireID: "fixture", Limits: "fixture", Capabilities: "fixture", Pricing: "fixture"}
	}
	sink.publishNarrativeMaintenance("operation", "thread", "turn", "item", result, nil)
	stored, err := events.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[uint32]bool{}
	var tokens uint64
	for _, event := range stored {
		if usage, ok := event.Data.(*protocol.UsageData); ok {
			if seen[usage.Sample] {
				t.Fatal("physical attempts share usage identity")
			}
			seen[usage.Sample] = true
			tokens += usage.InputTokens + usage.OutputTokens
		}
	}
	if len(seen) != 2 || tokens != 20 {
		t.Fatalf("usage events=%d tokens=%d", len(seen), tokens)
	}
}

func TestContextCompactionUsageSampleIsStablePerAttempt(t *testing.T) {
	first := contextCompactionSample("compact-1", 1)
	if first == 0 || first&(1<<31) == 0 {
		t.Fatalf("context compaction sample=%d", first)
	}
	if first != contextCompactionSample("compact-1", 1) {
		t.Fatal("context compaction sample changed across replay")
	}
	if first == contextCompactionSample("compact-1", 2) ||
		first == contextCompactionSample("compact-2", 1) {
		t.Fatal("context compaction samples collided in fixture")
	}
}

func TestPostTurnNarrativeRunsOnlyAfterCompletedTurn(t *testing.T) {
	if !postTurnNarrativeAllowed(&protocol.TurnCompletedData{Text: "done"}) {
		t.Fatal("completed turn skipped post-turn narrative")
	}
	if postTurnNarrativeAllowed(&protocol.TurnCanceledData{
		Reason: protocol.CancelReasonUserInterrupted,
	}) {
		t.Fatal("user pause still scheduled post-turn narrative")
	}
	if postTurnNarrativeAllowed(&protocol.TurnFailedData{Message: "provider timeout"}) {
		t.Fatal("failed turn still scheduled post-turn narrative")
	}
}

func TestPostTurnCompactionReceiptProducesValidProtocolEvent(t *testing.T) {
	metadata := &protocol.ModelMetadataProvenance{
		CanonicalID: "bundled", WireID: "bundled", Limits: "bundled",
		Capabilities: "bundled", Pricing: "bundled",
	}
	data := ProtocolCompactionData(&agentengine.CompactionReceipt{
		CompactionID:        "compact-1",
		Status:              "completed",
		Mode:                "post_turn",
		Phase:               agentengine.CompactionPhasePostTurn,
		TruthGeneration:     2,
		TruthEntities:       3,
		CompatibilityHash:   "sha256:compat",
		AuthorityDigest:     "sha256:authority",
		AuthorityEquivalent: true,
		DownshiftPolicy:     agentcontext.DownshiftRuntimeTruthOnly,
		NarrativeIncluded:   true,
		NarrativeProvider:   "provider",
		NarrativeModel:      "summary-model",
		NarrativeMetadata:   metadata,
	})
	if _, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 1, OperationID: "op-1", ThreadID: "thread-1",
		TurnID: "turn-1", ItemID: "item-1",
	}, data); err != nil {
		t.Fatalf("post-turn compaction event = %v", err)
	}
	if data.NarrativeMetadata == nil ||
		data.NarrativeMetadata.Limits != "bundled" {
		t.Fatalf("post-turn narrative metadata = %+v", data)
	}
}
