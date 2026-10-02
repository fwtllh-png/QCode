package engine

import (
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/contextview"
)

type ContextPolicy struct {
	Window                CompactWindowPolicy
	TruthRetention        agentcontext.RetentionPolicy
	SemanticNarrative     string
	NarrativeLimits       agentcontext.NarrativeLimits
	NarrativeTimeout      time.Duration
	NarrativeRetryLimit   int
	OwnerDeltaMaxSegments int
	OwnerDeltaMaxBytes    int
	RecentTailTurns       int
	RecentTailMaxTokens   uint64
	KeepRecentToolResults int
	CheckpointMaxBytes    int
	Digest                string
}

func (e *Engine) contextCapacity() agentcontext.Capacity {
	if scope := e.runningScope(); scope != nil &&
		scope.spec.Limits.Context.ContextTokens != 0 {
		return scope.spec.Limits.Context
	}
	return agentcontext.ResolveCapacity(
		e.activeRoute(), e.options.MaxOutputTokens,
		e.options.Budget.MaxTurnTokens, e.options.Budget.MaxTokens,
	)
}

func (e *Engine) prepareCompactLimit() uint64 {
	prepare, _, _ := agentcontext.WindowThresholds(
		e.options.Context.Window, e.contextCapacity().HardInputTokens,
	)
	return prepare
}

func (e *Engine) emergencyCompactLimit() uint64 {
	_, _, emergency := agentcontext.WindowThresholds(
		e.options.Context.Window, e.contextCapacity().HardInputTokens,
	)
	return emergency
}

func (e *Engine) recentTailMaxTokens() uint64 {
	return e.options.Context.RecentTailMaxTokens
}

func (e *Engine) estimateTokens(messages []provider.Message) uint64 {
	if len(messages) == 0 {
		return 0
	}
	if e.options.TokenEstimator != nil {
		tokens, err := e.options.TokenEstimator.Estimate(messages)
		if err == nil {
			return tokens
		}
	}
	return agentcontext.EstimateMessageTokens(messages)
}

func (e *Engine) estimateNonTailTokens(history []provider.Message) uint64 {
	var mandatory []provider.Message
	mandatory = append(mandatory, e.promptMessages()...)
	for _, message := range contextview.ProjectContextViewFrom(history, len(history)) {
		if agentcontext.IsWorldStateMessage(message) {
			mandatory = append(mandatory, message)
		}
	}
	mandatory = append(mandatory, e.closedTurnCheckpointMessages()...)
	return e.estimateTokens(mandatory)
}

// rawTailTokenBudget is leftover hard input after mandatory partitions, then
// the optional operator history_token_ceiling. Zero operator does
// not invent a percent of the window.
func (e *Engine) rawTailTokenBudget(
	history []provider.Message,
) (uint64, bool) {
	hard := e.contextCapacity().HardInputTokens
	operator := e.recentTailMaxTokens()
	if hard == 0 && operator == 0 {
		return 0, false
	}
	residual := operator
	if hard != 0 {
		leftover := uint64(0)
		if nonTail := e.estimateNonTailTokens(history); nonTail < hard {
			leftover = hard - nonTail
		}
		if operator == 0 {
			return leftover, true
		}
		if leftover < operator {
			return leftover, true
		}
		residual = operator
	}
	return residual, true
}
