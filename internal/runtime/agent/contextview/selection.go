package contextview

import (
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

type SelectionPolicy struct {
	RecentTurns      int
	FoldStart        int
	Folds            []HistoryFold
	MaxTokens        uint64
	Limited          bool
	TokenLimitReason agentcontext.OmissionReason
	Estimate         func([]provider.Message) uint64
}

// HistoryFold records the cause for a source interval, so a later throughput
// fold does not relabel messages already omitted for a different constraint.
type HistoryFold struct {
	Start, End int
	Reason     agentcontext.OmissionReason
}

// SelectHistory is the single raw-history selection decision. A positive turn
// limit bounds candidates; zero leaves them to capacity. Budget trimming and
// explicit folds retain the current request and only cross safe tool boundaries.
func SelectHistory(history []provider.Message, policy SelectionPolicy) agentcontext.ProjectionResult {
	turnStart := agentcontext.SafeTailStart(history, policy.RecentTurns)
	foldStart := VisibleTailStart(history, policy.RecentTurns, policy.FoldStart)
	start := FillVisibleTailStart(history, policy.RecentTurns, foldStart,
		policy.MaxTokens, policy.Limited, policy.Estimate)
	result := agentcontext.ProjectionResult{
		SourceHistoryDigest: agentcontext.HistoryDigest(history),
		TailStart:           start, RawTokenLimit: policy.MaxTokens,
		RawTokenLimited: policy.Limited,
		Messages:        ProjectContextViewFrom(history, start),
	}
	if policy.Estimate != nil {
		result.RawTokens = policy.Estimate(RawTailMessages(history, start))
	}
	for index, message := range history {
		if agentcontext.IsWorldStateMessage(message) {
			continue
		}
		source := agentcontext.ProjectionSource{
			Index: index, Turn: message.Turn, Digest: provider.MessageContentDigest(message),
		}
		if index >= start {
			result.Selected = append(result.Selected, source)
			continue
		}
		reason := policy.TokenLimitReason
		if index < turnStart {
			reason = agentcontext.OmittedTurnLimit
		} else {
			for _, fold := range policy.Folds {
				if index >= fold.Start && index < fold.End {
					reason = fold.Reason
					break
				}
			}
		}
		if reason == "" {
			reason = agentcontext.OmittedCapacity
		}
		omission := agentcontext.ProjectionOmission{Source: source, Reason: reason}
		if message.Turn != 0 {
			omission.Retrieval = &agentcontext.TurnRetrieval{Turn: message.Turn}
		}
		result.Omissions = append(result.Omissions, omission)
		result.LimitingConstraint = reason
	}
	result.Seal()
	return result
}
