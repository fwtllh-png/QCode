package engine

import (
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (e *Engine) relieveCurrentTurnPressure(
	history *[]provider.Message,
	baseInput agentcontext.MessageSnapshot,
	outputReserve uint64,
	economicInput uint64,
	phase string,
	send func(State, Event) error,
	projectHistory agentcontext.HistoryProjector,
	window tokenWindow,
) (tokenWindow, error) {
	for window.hardLimit != 0 && window.total > window.hardLimit {
		progressed := false
		next, receipt, err := e.collapseCurrentTurnWorkingSet(
			history, baseInput, outputReserve, economicInput, projectHistory, window,
		)
		if err != nil {
			return next, err
		}
		if receipt != nil {
			receipt.Phase = phase
			if err := send(Compacting, Event{Compaction: receipt}); err != nil {
				return next, err
			}
			window = next
			progressed = true
			if window.total <= window.hardLimit {
				break
			}
		}
		if changed, next, err := e.rebaseCurrentTurnWorld(
			history, baseInput, outputReserve, economicInput, projectHistory,
		); err != nil {
			return next, err
		} else if changed {
			window = next
			progressed = true
			if window.total <= window.hardLimit {
				break
			}
		}
		if changed, next := e.summarizeCurrentTurnAssistantText(
			history, baseInput, outputReserve, economicInput, projectHistory, false,
		); changed {
			window = next
			progressed = true
			if window.total <= window.hardLimit {
				break
			}
		}
		if next, receipt, err := e.pruneCurrentTurnSurfaces(
			history, baseInput, outputReserve, economicInput, projectHistory,
			false, window,
		); err != nil {
			return next, err
		} else if receipt != nil {
			receipt.Phase = phase
			if err := send(Compacting, Event{Compaction: receipt}); err != nil {
				return next, err
			}
			window = next
			progressed = true
			if window.total <= window.hardLimit {
				break
			}
		}
		if next, receipt, err := e.pruneCurrentTurnSurfaces(
			history, baseInput, outputReserve, economicInput, projectHistory,
			true, window,
		); err != nil {
			return next, err
		} else if receipt != nil {
			receipt.Phase = phase
			if err := send(Compacting, Event{Compaction: receipt}); err != nil {
				return next, err
			}
			window = next
			progressed = true
			if window.total <= window.hardLimit {
				break
			}
		}
		if changed, next := e.boundCurrentTurnArguments(
			history, baseInput, outputReserve, economicInput, projectHistory, false,
		); changed {
			window = next
			progressed = true
			if window.total <= window.hardLimit {
				break
			}
		}
		if changed, next := e.boundCurrentTurnArguments(
			history, baseInput, outputReserve, economicInput, projectHistory, true,
		); changed {
			window = next
			progressed = true
			if window.total <= window.hardLimit {
				break
			}
		}
		if changed, next := e.summarizeCurrentTurnAssistantText(
			history, baseInput, outputReserve, economicInput, projectHistory, true,
		); changed {
			window = next
			progressed = true
			if window.total <= window.hardLimit {
				break
			}
		}
		if changed, next := e.stripCurrentTurnReasoning(
			history, baseInput, outputReserve, economicInput, projectHistory, true,
		); changed {
			window = next
			progressed = true
			if window.total <= window.hardLimit {
				break
			}
		}
		if changed, next := e.stripCurrentTurnReasoning(
			history, baseInput, outputReserve, economicInput, projectHistory, false,
		); changed {
			window = next
			progressed = true
			if window.total <= window.hardLimit {
				break
			}
		}
		if !progressed {
			break
		}
	}
	if window.hardLimit != 0 &&
		window.total > window.hardLimit &&
		len(baseInput.Partition(agentcontext.KindContinuation)) != 0 {
		return window, protocol.NewProblem(
			protocol.CodeResourceExhausted,
			"partial provider output cannot be compacted within the model context window",
			false,
			nil,
		)
	}
	if window.hardLimit != 0 && window.total > window.hardLimit {
		return window, compactionBudgetError(window)
	}
	return window, nil
}

func (e *Engine) measureProjectedWindow(
	history []provider.Message,
	baseInput agentcontext.MessageSnapshot,
	outputReserve uint64,
	economicInput uint64,
	projectHistory agentcontext.HistoryProjector,
) (tokenWindow, error) {
	return e.measureTokenWindow(
		baseInput.WithHistory(e.projectGateHistory(history, projectHistory)),
		outputReserve,
		economicInput,
	)
}

func (e *Engine) collapseCurrentTurnWorkingSet(
	history *[]provider.Message,
	baseInput agentcontext.MessageSnapshot,
	outputReserve uint64,
	economicInput uint64,
	projectHistory agentcontext.HistoryProjector,
	window tokenWindow,
) (tokenWindow, *CompactionReceipt, error) {
	cuts := agentcontext.CurrentTurnWorkingSetCuts(*history)
	if len(cuts) == 0 {
		return window, nil, nil
	}
	userIndex := agentcontext.CurrentTurnUserIndex(*history)
	if userIndex < 0 {
		return window, nil, nil
	}
	for index := len(cuts) - 1; index >= 0; index-- {
		candidate, err := e.buildCurrentTurnCompactionCandidate(
			*history, userIndex, cuts[index],
		)
		if err != nil {
			continue
		}
		if !agentcontext.ToolPairsClosed(candidate.History) ||
			candidate.Capsule.ContainsAuthority(candidate.Authority) != nil {
			continue
		}
		measured, err := e.measureProjectedWindow(
			candidate.History, baseInput, outputReserve, economicInput, projectHistory,
		)
		if err != nil {
			return window, nil, err
		}
		if measured.total >= window.total && measured.active >= window.active {
			continue
		}
		before := cloneMessages(*history)
		*history = candidate.History
		e.noteCompaction()
		e.advanceTokenWindow()
		e.reconcileWorldBaseline(*history)
		return measured, currentTurnWorkingSetReceipt(
			before, *history, window, measured,
		), nil
	}
	return window, nil, nil
}

func (e *Engine) buildCurrentTurnCompactionCandidate(
	history []provider.Message,
	userIndex, cut int,
) (compactionCandidate, error) {
	pinned := history[userIndex]
	removed := cloneMessages(history[userIndex+1 : cut])
	toSummarize := agentcontext.StripWorldState(
		promptcontext.StripContextualFragments(cloneMessages(removed)),
	)
	summary := e.buildCompactSummary(toSummarize)
	if summary.Goal == "" {
		summary.Goal = agentcontext.ActiveTurnGoal(history)
	}
	tail := agentcontext.StripWorldState(
		promptcontext.StripContextualFragments(cloneMessages(history[cut:])),
	)
	candidate, err := agentcontext.BuildCompactionCandidate(
		agentcontext.CompactionCandidateInput{
			Cut: cut, Removed: removed, ToSummarize: toSummarize,
			Tail: tail, OriginalHistory: history,
			Summary: summary, CurrentTruth: e.buildTruthCapsule(summary, nil),
			RetentionPolicy:  e.options.Context.TruthRetention,
			Turn:             e.turn,
			SummaryMaxBytes:  e.summaryBudget(),
			IncludeNarrative: false,
			PinnedUser:       &pinned,
		},
	)
	if err != nil {
		return compactionCandidate{}, err
	}
	world := agentcontext.LatestCurrentTurnWorld(removed)
	assembled := append(cloneMessages(history[:userIndex]), candidate.History[0])
	assembled = append(assembled, world...)
	assembled = append(assembled, candidate.History[1:]...)
	candidate.History = assembled
	return candidate, nil
}

func currentTurnWorkingSetReceipt(
	before, after []provider.Message,
	beforeWindow, afterWindow tokenWindow,
) *CompactionReceipt {
	return &promptcontext.CompactionReceipt{
		Status:           "compacted",
		Mode:             "current_turn",
		OriginalMessages: len(before),
		RemovedMessages:  max(0, len(before)-len(after)),
		OriginalBytes:    agentcontext.HistoryBytes(before),
		RetainedBytes:    agentcontext.HistoryBytes(after),
		OriginalTokens:   beforeWindow.total,
		RetainedTokens:   afterWindow.total,
		TruncationReason: "current_turn_working_set",
	}
}

func (e *Engine) rebaseCurrentTurnWorld(
	history *[]provider.Message,
	baseInput agentcontext.MessageSnapshot,
	outputReserve uint64,
	economicInput uint64,
	projectHistory agentcontext.HistoryProjector,
) (bool, tokenWindow, error) {
	collapsed, changed := agentcontext.CollapseCurrentTurnWorld(*history)
	if !changed {
		window, err := e.measureProjectedWindow(
			*history, baseInput, outputReserve, economicInput, projectHistory,
		)
		return false, window, err
	}
	*history = collapsed
	e.advanceTokenWindow()
	e.reconcileWorldBaseline(*history)
	window, err := e.measureProjectedWindow(
		*history, baseInput, outputReserve, economicInput, projectHistory,
	)
	return true, window, err
}

func (e *Engine) pruneCurrentTurnSurfaces(
	history *[]provider.Message,
	baseInput agentcontext.MessageSnapshot,
	outputReserve uint64,
	economicInput uint64,
	projectHistory agentcontext.HistoryProjector,
	includeLatest bool,
	window tokenWindow,
) (tokenWindow, *CompactionReceipt, error) {
	input := baseInput.WithHistory(e.projectGateHistory(*history, projectHistory))
	stats, pruned, err := e.pruneToolResultSurfaces(
		history, input, outputReserve, true, includeLatest,
		economicInput, projectHistory,
	)
	if err != nil || stats.results == 0 {
		return window, nil, err
	}
	e.advanceTokenWindow()
	return pruned, &promptcontext.CompactionReceipt{
		Status:            "pruned",
		Mode:              "surface",
		OriginalTokens:    window.total,
		RetainedTokens:    pruned.total,
		PrunedToolResults: stats.results,
		PrunedBytes:       stats.bytes,
	}, nil
}

func (e *Engine) summarizeCurrentTurnAssistantText(
	history *[]provider.Message,
	baseInput agentcontext.MessageSnapshot,
	outputReserve uint64,
	economicInput uint64,
	projectHistory agentcontext.HistoryProjector,
	includeLatest bool,
) (bool, tokenWindow) {
	if agentcontext.SummarizeClosedAssistantText(
		*history, includeLatest, summaryLineBytes,
	) == 0 {
		window, _ := e.measureProjectedWindow(
			*history, baseInput, outputReserve, economicInput, projectHistory,
		)
		return false, window
	}
	e.advanceTokenWindow()
	window, _ := e.measureProjectedWindow(
		*history, baseInput, outputReserve, economicInput, projectHistory,
	)
	return true, window
}

func (e *Engine) boundCurrentTurnArguments(
	history *[]provider.Message,
	baseInput agentcontext.MessageSnapshot,
	outputReserve uint64,
	economicInput uint64,
	projectHistory agentcontext.HistoryProjector,
	includeLatest bool,
) (bool, tokenWindow) {
	if agentcontext.BoundToolCallArguments(*history, includeLatest) == 0 {
		window, _ := e.measureProjectedWindow(
			*history, baseInput, outputReserve, economicInput, projectHistory,
		)
		return false, window
	}
	e.advanceTokenWindow()
	window, _ := e.measureProjectedWindow(
		*history, baseInput, outputReserve, economicInput, projectHistory,
	)
	return true, window
}

func (e *Engine) stripCurrentTurnReasoning(
	history *[]provider.Message,
	baseInput agentcontext.MessageSnapshot,
	outputReserve uint64,
	economicInput uint64,
	projectHistory agentcontext.HistoryProjector,
	keepLatest bool,
) (bool, tokenWindow) {
	if agentcontext.StripConsumedReasoning(*history, keepLatest) == 0 {
		window, _ := e.measureProjectedWindow(
			*history, baseInput, outputReserve, economicInput, projectHistory,
		)
		return false, window
	}
	e.advanceTokenWindow()
	window, _ := e.measureProjectedWindow(
		*history, baseInput, outputReserve, economicInput, projectHistory,
	)
	return true, window
}
