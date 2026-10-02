package app

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"strconv"

	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func postTurnNarrativeAllowed(terminal protocol.EventData) bool {
	_, ok := terminal.(*protocol.TurnCompletedData)
	return ok
}

func (s *runtimeSink) publishPostTurnContextMaintenance(
	operationID protocol.OperationID,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
	itemID protocol.ItemID,
) {
	if !postTurnNarrativeAllowed(s.terminal) {
		return
	}
	maintenance, ok := s.runtime.engine.(ContextMaintenanceEngine)
	if !ok {
		return
	}
	narrative, err := maintenance.PreparePostTurnNarrative(threadID, turnID)
	if err != nil {
		s.publishNarrativeMaintenance(
			operationID, threadID, turnID, itemID,
			agentengine.NarrativeGenerationResult{}, err,
		)
		return
	}
	if narrative == nil {
		return
	}
	// The business terminal is already durable. Settle the non-authoritative
	// narrative off the queue's critical path so queued turns drain now; the
	// engine joins the pending narrative before its next turn starts, and a
	// missing narrative falls back to deterministic truth plus the raw tail.
	go func() {
		result, runErr := narrative.Run(context.Background())
		s.publishNarrativeMaintenance(
			operationID, threadID, turnID, itemID, result, runErr,
		)
	}()
}

func (s *runtimeSink) publishNarrativeMaintenance(
	operationID protocol.OperationID,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
	itemID protocol.ItemID,
	result agentengine.NarrativeGenerationResult,
	err error,
) {
	var data *protocol.TurnCompactionData
	switch {
	case err != nil:
		data = &protocol.TurnCompactionData{
			Phase:  agentengine.CompactionPhasePostTurn,
			Status: "fallback", Mode: "post_turn",
			Summary: "semantic narrative unavailable; retained deterministic " +
				"truth and raw tail",
			FallbackReason: err.Error(),
		}
	case result.Receipt != nil:
		data = ProtocolCompactionData(result.Receipt)
	}
	if result.Receipt != nil && result.Usage.Total() != 0 {
		_ = s.runtime.publish(
			operationID,
			threadID,
			turnID,
			itemID,
			&protocol.UsageData{
				Sample: contextCompactionSample(
					result.Receipt.CompactionID,
					result.Attempt,
				),
				Provider:        result.Provider,
				Model:           result.Model,
				ModelMetadata:   &result.ModelMetadata,
				InputTokens:     result.Usage.InputTokens,
				OutputTokens:    result.Usage.OutputTokens,
				ReasoningTokens: result.Usage.ReasoningTokens,
				CachedTokens:    result.Usage.CachedTokens,
				CostMicrounits:  CostMicrounits(result.CostUSD),
				CostKnown:       result.CostKnown,
			},
		)
	}
	if data == nil {
		return
	}
	// Maintenance is optional and runs after the business terminal. Its
	// projection cannot rewrite that outcome.
	_ = s.runtime.publish(
		operationID,
		threadID,
		turnID,
		itemID,
		data,
	)
}

func contextCompactionSample(compactionID string, attempt uint32) uint32 {
	sum := sha256.Sum256([]byte(
		"context_compaction\x00" + compactionID + "\x00" +
			strconv.FormatUint(uint64(attempt), 10),
	))
	return binary.BigEndian.Uint32(sum[:4]) | 1<<31
}
