package engine

import "github.com/fwtllh-png/QCode/internal/runtime/protocol"

// Terminal maintenance changes durable history, not the last model request.
// Copy policy metadata only; all displayed request quantities come from the
// same frozen sample. Without a sample, request usage is explicitly unknown.
func contextBudgetFromSample(policy ContextBudgetSnapshot, sample *protocol.SampleContextData) ContextBudgetSnapshot {
	result := ContextBudgetSnapshot{
		AutoCompactTokens: policy.AutoCompactTokens, PrepareTokens: policy.PrepareTokens,
		EmergencyTokens: policy.EmergencyTokens, RecentTailTurns: policy.RecentTailTurns,
		KeepRecentToolResults: policy.KeepRecentToolResults, HistoryTokenCeiling: policy.HistoryTokenCeiling,
		Digest: policy.Digest, NarrativeMode: policy.NarrativeMode, Compactions: policy.Compactions,
	}
	if sample == nil {
		return result
	}
	result.MeasurementSource = "request_estimate"
	result.ActiveTokens = sample.WindowFullActiveTokens
	if sample.MeasuredInputTokens != 0 {
		result.MeasurementSource = "provider_usage"
		result.ActiveTokens = sample.MeasuredInputTokens
		result.Observed = true
	}
	result.FullActiveTokens = result.ActiveTokens
	result.ContextDigest = sample.ContextDigest
	result.WindowID, result.WindowNumber = sample.WindowID, sample.WindowNumber
	result.PrefillTokens, result.BodyTokens = sample.WindowPrefillTokens, sample.WindowBodyTokens
	result.ToolDefinitionTokens, result.PendingTokens = sample.ToolDefinitionTokens, sample.WindowPendingTokens
	result.OutputReserve, result.HardInputTokens = sample.WindowOutputReserve, sample.WindowHardInputTokens
	result.MaxContextTokens, result.EstimatedTokens = sample.WindowContextTokens, sample.EstimatedTokens
	result.OutputSource = sample.WindowOutputSource
	result.LimitSource = sample.WindowLimitSource
	result.CompactionHeadroomTokens, result.CompactionTargetTokens = sample.CompactionHeadroomTokens, sample.CompactionTargetTokens
	return result
}
