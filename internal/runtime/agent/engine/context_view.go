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
	start            int
	folded           bool
	folds            []contextview.HistoryFold
	headroom, target uint64
}

func (e *Engine) visibleTailStart(history []provider.Message) int {
	return e.contextProjection(history).TailStart
}

func (e *Engine) contextProjection(history []provider.Message) agentcontext.ProjectionResult {
	budget, limited := e.rawTailTokenBudget(history)
	reason := agentcontext.OmittedCapacity
	if operator := e.recentTailMaxTokens(); operator != 0 && budget == operator {
		reason = agentcontext.OmittedTokenLimit
	}
	start := e.viewFold.start
	if floor := e.currentWindowLedger().HistoryFloorTurn; floor != 0 {
		// A recovered older request must still retain its own user message.
		floor = min(floor, agentcontext.LastNonWorldTurn(history))
		for index, message := range history {
			if !agentcontext.IsWorldStateMessage(message) && message.Turn >= floor {
				start = max(start, index)
				break
			}
		}
	}
	return contextview.SelectHistory(history, contextview.SelectionPolicy{
		RecentTurns: e.recentTailTurns(), FoldStart: start,
		Folds: e.viewFold.folds, MaxTokens: budget, Limited: limited,
		TokenLimitReason: reason, Estimate: e.estimateTokens,
	})
}

func (e *Engine) retainHistoryFloor(history []provider.Message) {
	selection := e.contextProjection(history)
	for _, message := range history[selection.TailStart:] {
		if agentcontext.IsWorldStateMessage(message) {
			continue
		}
		if scope := e.runningScope(); scope != nil {
			scope.mu.Lock()
			window := scope.state.context.Window()
			window.RetainHistoryFrom(message.Turn)
			scope.state.context.SetWindow(window)
			scope.mu.Unlock()
		} else {
			window := e.context.Window()
			window.RetainHistoryFrom(message.Turn)
			e.context.SetWindow(window)
		}
		return
	}
}

func (e *Engine) foldOldestVisibleTail(
	history []provider.Message,
	reason agentcontext.OmissionReason,
) bool {
	previousStart := e.visibleTailStart(history)
	start, ok := contextview.OldestVisibleTailFold(
		history, e.recentTailTurns(), previousStart,
	)
	if !ok {
		return false
	}
	folds := append([]contextview.HistoryFold(nil), e.viewFold.folds...)
	folds = append(folds, contextview.HistoryFold{Start: previousStart, End: start, Reason: reason})
	e.viewFold.start, e.viewFold.folded, e.viewFold.folds = start, true, folds
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
