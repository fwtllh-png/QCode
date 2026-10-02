package app

import (
	"testing"

	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

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
