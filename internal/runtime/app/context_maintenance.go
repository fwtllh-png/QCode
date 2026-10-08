package app

import (
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
	_, observed := narrative.(interface {
		Observe(func(agentengine.NarrativeGenerationResult))
	})
	if source, ok := narrative.(interface {
		Observe(func(agentengine.NarrativeGenerationResult))
	}); ok {
		source.Observe(func(result agentengine.NarrativeGenerationResult) {
			s.publishNarrativeMaintenance(operationID, threadID, turnID, itemID, result, nil)
		})
	}
	// Generation is optional; the engine installs candidates at a safe boundary.
	s.runtime.narrativeWorkers.Add(1)
	go func() {
		defer s.runtime.narrativeWorkers.Done()
		result, runErr := narrative.Run(s.runtime.ctx)
		if observed {
			result.Receipt = nil
		}
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
	for _, call := range result.Calls {
		if call.Usage.Total() == 0 {
			continue
		}
		_ = s.runtime.publish(operationID, threadID, turnID, itemID, narrativeUsageData(call))
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

func narrativeUsageData(call agentengine.NarrativeUsage) *protocol.UsageData {
	return &protocol.UsageData{
		Sample: contextCompactionSample(call.ID, call.Attempt), Provider: call.Provider, Model: call.Model,
		ModelMetadata: &call.ModelMetadata, InputTokens: call.Usage.InputTokens, OutputTokens: call.Usage.OutputTokens,
		ReasoningTokens: call.Usage.ReasoningTokens, CachedTokens: call.Usage.CachedTokens,
		CostMicrounits: CostMicrounits(call.CostUSD), CostKnown: call.CostKnown,
	}
}

func emitNarrativeUsage(sink EngineSink, result agentengine.NarrativeGenerationResult) error {
	for _, call := range result.Calls {
		if call.Usage.Total() != 0 {
			if err := sink.Emit(narrativeUsageData(call)); err != nil {
				return err
			}
		}
	}
	return nil
}

func contextCompactionSample(compactionID string, attempt uint32) uint32 {
	sum := sha256.Sum256([]byte(
		"context_compaction\x00" + compactionID + "\x00" +
			strconv.FormatUint(uint64(attempt), 10),
	))
	return binary.BigEndian.Uint32(sum[:4]) | 1<<31
}
