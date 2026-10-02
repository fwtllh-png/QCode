package engine

import (
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/contextview"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
)

func (e *Engine) recentTailTurns() int {
	return agentcontext.ResolveRecentTailTurns(e.options.Context.RecentTailTurns)
}

type viewFoldState struct {
	start  int
	folded bool
}

func (e *Engine) visibleTailStart(history []provider.Message) int {
	turns := e.recentTailTurns()
	start := contextview.VisibleTailStart(
		history, turns, e.viewFold.start,
	)
	budget, limited := e.rawTailTokenBudget(history)
	return contextview.FillVisibleTailStart(
		history, turns, start, budget, limited, e.estimateTokens,
	)
}

func (e *Engine) contextViewProject(
	next agentcontext.HistoryProjector,
) agentcontext.HistoryProjector {
	return func(history []provider.Message) []provider.Message {
		return agentcontext.ProjectHistory(
			contextview.ProjectContextViewFrom(
				history, e.visibleTailStart(history),
			),
			next,
		)
	}
}

func (e *Engine) foldOldestVisibleTail(
	history []provider.Message,
	allowCurrentTurn bool,
) bool {
	if e.viewFold.folded {
		return false
	}
	start, ok := contextview.OldestVisibleTailFold(
		history, e.recentTailTurns(), e.viewFold.start, allowCurrentTurn,
	)
	if !ok {
		return false
	}
	e.viewFold = viewFoldState{start: start, folded: true}
	return true
}

func (e *Engine) resetViewFold() {
	e.viewFold = viewFoldState{}
}

func viewFoldReceipt(
	phase string,
	before, after []provider.Message,
	beforeWindow, afterWindow tokenWindow,
) *CompactionReceipt {
	afterTurns := make(map[uint64]struct{})
	for _, message := range after {
		if !agentcontext.IsWorldStateMessage(message) && message.Turn != 0 {
			afterTurns[message.Turn] = struct{}{}
		}
	}
	var removedTurns []uint64
	seen := make(map[uint64]struct{})
	for _, message := range before {
		if agentcontext.IsWorldStateMessage(message) || message.Turn == 0 {
			continue
		}
		if _, keep := afterTurns[message.Turn]; keep {
			continue
		}
		if _, exists := seen[message.Turn]; exists {
			continue
		}
		seen[message.Turn] = struct{}{}
		removedTurns = append(removedTurns, message.Turn)
	}
	return &promptcontext.CompactionReceipt{
		Status:           "folded",
		Mode:             "view",
		Phase:            phase,
		OriginalMessages: len(before),
		RemovedMessages:  max(0, len(before)-len(after)),
		OriginalBytes:    agentcontext.HistoryBytes(before),
		RetainedBytes:    agentcontext.HistoryBytes(after),
		OriginalTokens:   beforeWindow.active,
		RetainedTokens:   afterWindow.active,
		TruncationReason: "visible_tail_fold",
		RemovedTurns:     removedTurns,
	}
}
