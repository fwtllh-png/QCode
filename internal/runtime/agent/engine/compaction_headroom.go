package engine

import (
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

// compactionHeadroom leaves room for one observed work cycle after a prefix
// has to change. The newest completed turn and the active turn are semantic
// boundaries, not an arbitrary sample-count or context-window percentage.
// This is a soft target: only closed historical groups may be removed for it.
func (e *Engine) compactionHeadroom(history []provider.Message, window tokenWindow, outputReserve, economicInput uint64) (uint64, uint64) {
	limit := window.hardLimit - min(window.hardLimit, outputReserve)
	if economicInput != 0 {
		limit = min(limit, economicInput)
	}
	// A body-only operator ceiling leaves the observed prefix outside that
	// ceiling, while hard admission always accounts for the complete input.
	if window.compactLimit != 0 {
		fixed := window.accounting.FullActiveTokens - min(window.accounting.FullActiveTokens, window.active)
		if fixed < limit {
			limit = fixed + min(limit-fixed, window.compactLimit)
		}
	}
	if ceiling := e.recentTailMaxTokens(); ceiling != 0 {
		raw := e.contextProjection(history).RawTokens
		fixed := window.accounting.FullActiveTokens - min(window.accounting.FullActiveTokens, raw)
		if fixed < limit {
			limit = fixed + min(limit-fixed, ceiling)
		}
	}
	current := agentcontext.CurrentTurnUserIndex(history)
	var active, previous []provider.Message
	var previousTurn uint64
	for i := current - 1; i >= 0; i-- {
		if !agentcontext.IsWorldStateMessage(history[i]) && history[i].Turn != 0 {
			previousTurn = history[i].Turn
			break
		}
	}
	for i, message := range history {
		if agentcontext.IsWorldStateMessage(message) {
			continue
		}
		if current < 0 || i >= current {
			active = append(active, message)
		} else if previousTurn != 0 && message.Turn == previousTurn {
			previous = append(previous, message)
		}
	}
	growth := max(e.estimateMessageTokens(active), e.estimateMessageTokens(previous))
	if window.accounting.Observed {
		growth = max(growth, window.accounting.PendingTokens)
	}
	margin := min(limit, growth)
	margin += min(limit-margin, e.currentWindowLedger().LastUnderestimateTokens)
	return margin, limit - margin
}
