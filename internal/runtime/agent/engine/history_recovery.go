package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const compactScopeBodyAfterPrefix = "body_after_prefix"

func (e *Engine) compact() *CompactionReceipt {
	e.resetViewFold()
	receipt := e.compactHistory(&e.history, false)
	e.reconcileWorldBaseline(e.history)
	return receipt
}

func (e *Engine) projectGateHistory(
	history []provider.Message,
	projectHistory agentcontext.HistoryProjector,
) []provider.Message {
	if projectHistory != nil {
		return agentcontext.ProjectHistory(history, projectHistory)
	}
	return agentcontext.ProjectContextViewFrom(
		history, e.visibleTailStart(history),
	)
}

func (e *Engine) runCompactGate(
	ctx context.Context,
	history *[]provider.Message,
	input agentcontext.MessageSnapshot,
	outputReserve uint64,
	phase string,
	allowCurrentTurn bool,
	send func(State, Event) error,
	economicInput uint64,
	projectHistory agentcontext.HistoryProjector,
) (tokenWindow, error) {
	baseInput := input
	projected := e.projectGateHistory(*history, projectHistory)
	input = baseInput.WithHistory(projected)
	window, err := e.measureTokenWindow(input, outputReserve, economicInput)
	if err != nil {
		return tokenWindow{}, err
	}
	recentTailMaxTokens := e.recentTailMaxTokens()
	forceTailBudget := phase == CompactionPhasePostTurn &&
		allowCurrentTurn &&
		recentTailMaxTokens != 0 &&
		agentcontext.EstimateMessageTokens(*history) > recentTailMaxTokens
	overHard := window.hardLimit != 0 && window.total > window.hardLimit
	operatorCeiling := window.compactLimit != 0 &&
		window.compactLimit < window.hardLimit &&
		window.active >= window.compactLimit
	if phase == CompactionPhasePostTurn {
		e.resetViewFold()
		projected = e.projectGateHistory(*history, projectHistory)
		input = baseInput.WithHistory(projected)
		window, err = e.measureTokenWindow(input, outputReserve, economicInput)
		if err != nil {
			return tokenWindow{}, err
		}
		overHard = window.hardLimit != 0 && window.total > window.hardLimit
		operatorCeiling = window.compactLimit != 0 &&
			window.compactLimit < window.hardLimit &&
			window.active >= window.compactLimit
		var receipt *CompactionReceipt
		if overHard || operatorCeiling || forceTailBudget ||
			window.compactLimit != 0 && window.active >= window.compactLimit {
			receipt = e.compactHistoryWithPolicy(
				history, overHard || forceTailBudget, allowCurrentTurn, input,
				outputReserve, economicInput, projectHistory,
			)
		}
		if receipt != nil {
			receipt.Phase = phase
			if err := send(Compacting, Event{Compaction: receipt}); err != nil {
				return tokenWindow{}, err
			}
			projected = e.projectGateHistory(*history, projectHistory)
			input = baseInput.WithHistory(projected)
			window, err = e.measureTokenWindow(
				input, outputReserve, economicInput,
			)
		}
		if err == nil &&
			window.hardLimit != 0 &&
			window.total > window.hardLimit &&
			len(baseInput.Partition(agentcontext.KindContinuation)) != 0 {
			err = protocol.NewProblem(
				protocol.CodeResourceExhausted,
				"partial provider output cannot be compacted within the model context window",
				false,
				nil,
			)
		}
		return window, err
	}
	if (overHard || operatorCeiling) && e.foldOldestVisibleTail(*history, allowCurrentTurn) {
		before := projected
		beforeWindow := window
		projected = e.projectGateHistory(*history, projectHistory)
		input = baseInput.WithHistory(projected)
		window, err = e.measureTokenWindow(input, outputReserve, economicInput)
		if err != nil {
			return tokenWindow{}, err
		}
		receipt := viewFoldReceipt(phase, before, projected, beforeWindow, window)
		if err := send(Compacting, Event{Compaction: receipt}); err != nil {
			return tokenWindow{}, err
		}
		overHard = window.hardLimit != 0 && window.total > window.hardLimit
	}
	if err == nil && overHard {
		return e.relieveCurrentTurnPressure(
			history, baseInput, outputReserve, economicInput, phase,
			send, projectHistory, window,
		)
	}
	return window, err
}

func (e *Engine) runTerminalCompactGate(
	history *[]provider.Message,
	allowCurrentTurn bool,
	send func(State, Event) error,
) error {
	input := agentcontext.NewMessageLedger(agentcontext.LedgerInput{Stable: e.promptMessages()}).Snapshot()
	window, err := e.runCompactGate(
		context.Background(),
		history, input, 0,
		CompactionPhasePostTurn, allowCurrentTurn, send,
		0, nil,
	)
	if err == nil && window.total > window.hardLimit {
		err = compactionBudgetError(window)
	}
	return err
}

type tokenWindow struct {
	estimated, total, active, hardLimit, compactLimit uint64
	accounting                                        agentcontext.WindowProjection
}

func (e *Engine) measureTokenWindow(
	input agentcontext.MessageSnapshot,
	outputReserve uint64,
	economicInput uint64,
) (tokenWindow, error) {
	measured, err := input.Measure("", "", e.options.TokenEstimator)
	if err != nil {
		return tokenWindow{}, err
	}
	projected := e.projectTokenWindow(&measured, outputReserve)
	active := projected.FullActiveTokens
	if economicInput == 0 &&
		e.options.Context.Window.Scope == compactScopeBodyAfterPrefix {
		active = projected.BodyTokens
		if !projected.Observed {
			active = projected.PendingTokens
		}
	}
	compactLimit := projected.AutoCompactLimit
	if economicInput != 0 {
		compactLimit = min(compactLimit, economicInput)
	}
	return tokenWindow{
		estimated: measured.EstimatedTokens,
		total:     projected.FullActiveTokens + outputReserve,
		active:    active, hardLimit: projected.HardLimit,
		compactLimit: compactLimit,
		accounting:   projected,
	}, nil
}

// EstimateFirstWindow projects the first provider sample for prompt using the
// same token window as a live child turn.
func (e *Engine) EstimateFirstWindow(prompt string) (uint64, uint64) {
	if e == nil {
		return 0, 0
	}
	history := []provider.Message{provider.TextMessage(provider.RoleUser, prompt)}
	snapshot := e.contextBudgetSnapshot(history)
	projected := snapshot.FullActiveTokens + snapshot.OutputReserve
	limit := e.options.Budget.MaxTurnTokens
	if limit == 0 {
		limit = e.options.Budget.MaxTokens
	}
	return projected, limit
}

func (e *Engine) contextBudgetSnapshot(history []provider.Message) ContextBudgetSnapshot {
	value := agentcontext.LedgerInput{
		Stable: e.promptMessages(), History: history,
	}
	if scope := e.runningScope(); scope != nil {
		scope.mu.Lock()
		if scope.state.contextLedger != nil {
			snapshot := scope.state.contextLedger.Snapshot()
			value.Stable = snapshot.Partition(agentcontext.KindStable)
			value.Definitions = snapshot.Definitions()
		}
		scope.mu.Unlock()
	}
	input := agentcontext.NewMessageLedger(value).Snapshot()
	window, _ := e.measureTokenWindow(input, e.maxOutputFor(e.activeRoute()), 0)
	capacity := e.contextCapacity()
	snapshot := ContextBudgetSnapshot{
		ActiveTokens: window.active, AutoCompactTokens: window.compactLimit,
		RecentTailTurns:       e.recentTailTurns(),
		KeepRecentToolResults: e.options.Context.KeepRecentToolResults,
		HistoryTokenCeiling:   e.recentTailMaxTokens(),
		Digest:                e.options.Context.Digest,
		NarrativeMode:         e.options.Context.SemanticNarrative,
		EstimatedTokens:       window.estimated, MaxContextTokens: window.hardLimit,
		HardInputTokens: capacity.HardInputTokens,
		LimitSource:     string(capacity.LimitSource),
		OutputSource:    capacity.OutputSource,
		WindowID:        window.accounting.ID, WindowNumber: window.accounting.Number,
		Observed:             window.accounting.Observed,
		FullActiveTokens:     window.accounting.FullActiveTokens,
		PrefillTokens:        window.accounting.PrefillTokens,
		BodyTokens:           window.accounting.BodyTokens,
		ToolDefinitionTokens: window.accounting.ToolDefinitionTokens,
		PendingTokens:        window.accounting.PendingTokens,
		OutputReserve:        window.accounting.OutputReserve,
		Compactions:          e.compactionTotal(),
	}
	if e.options.Context.Window.PrepareTokens != 0 {
		snapshot.PrepareTokens = e.prepareCompactLimit()
	}
	if e.options.Context.Window.EmergencyTokens != 0 {
		snapshot.EmergencyTokens = e.emergencyCompactLimit()
	}
	return snapshot
}

// Compact applies the auto budget policy under the engine lock.
func (e *Engine) Compact() *CompactionReceipt {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.compact()
}

// CompactForced summarizes older turns even below the automatic token limit.
// Used by explicit thread.compact operations.
func (e *Engine) CompactForced() *CompactionReceipt {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resetViewFold()
	receipt := e.compactHistory(&e.history, true)
	e.reconcileWorldBaseline(e.history)
	return receipt
}

// CompactForcedDurable applies a forced history replacement. Semantic
// narrative is post-turn only and does not block this operation.
func (e *Engine) CompactForcedDurable(
	ctx context.Context,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
	focus string,
) (NarrativeGenerationResult, error) {
	// Settle any pending post-turn narrative first so its digest cannot
	// overwrite the compaction's own replacement window.
	e.joinPendingNarrative()
	e.mu.Lock()
	e.resetViewFold()
	source := cloneMessages(e.history)
	receipt := e.compactHistory(
		&e.history,
		true,
		TurnIdentity{
			ThreadID: string(threadID),
			TurnID:   string(turnID),
		},
	)
	e.reconcileWorldBaseline(e.history)
	e.mu.Unlock()
	result, err := e.generatePostTurnDigest(ctx, threadID, turnID, focus, source)
	if receipt == nil {
		return result, err
	}
	if result.Receipt != nil {
		receipt.Status = result.Receipt.Status
		receipt.Mode = "post_turn"
		receipt.SourceWindowID = result.Receipt.SourceWindowID
		receipt.TargetWindowID = result.Receipt.TargetWindowID
		receipt.AuthorityDigest = result.Receipt.AuthorityDigest
		receipt.AuthorityEquivalent = result.Receipt.AuthorityEquivalent
		receipt.NarrativeIncluded = result.Receipt.NarrativeIncluded
		receipt.NarrativeBytes = result.Receipt.NarrativeBytes
		receipt.NarrativeInputTokens = result.Receipt.NarrativeInputTokens
		receipt.NarrativeOutputTokens = result.Receipt.NarrativeOutputTokens
		receipt.NarrativeProvider = result.Receipt.NarrativeProvider
		receipt.NarrativeModel = result.Receipt.NarrativeModel
		receipt.NarrativeMetadata = result.Receipt.NarrativeMetadata
		receipt.FallbackReason = result.Receipt.FallbackReason
	}
	result.Receipt = receipt
	return result, err
}

func (e *Engine) reconcileWorldBaseline(history []provider.Message) {
	e.context.ReconcileWorld(history)
}

func (e *Engine) compactHistory(
	history *[]provider.Message,
	force bool,
	identities ...TurnIdentity,
) *CompactionReceipt {
	input := agentcontext.NewMessageLedger(agentcontext.LedgerInput{Stable: e.promptMessages()}).Snapshot()
	return e.compactHistoryWithPolicy(
		history, force, false, input, 0, 0, nil, identities...,
	)
}

func (e *Engine) compactHistoryWithPolicy(
	history *[]provider.Message,
	force bool,
	allowCurrentTurn bool,
	input agentcontext.MessageSnapshot,
	outputReserve uint64,
	economicInput uint64,
	projectHistory agentcontext.HistoryProjector,
	identities ...TurnIdentity,
) *CompactionReceipt {
	if len(*history) <= 1 {
		return nil
	}
	finish := func(receipt *CompactionReceipt) *CompactionReceipt {
		e.noteCompaction()
		e.advanceTokenWindow()
		e.options.Metrics.Compaction(
			receipt.OriginalBytes - receipt.RetainedBytes,
		)
		return receipt
	}
	authority := e.buildTruthCapsule(e.buildCompactSummary(nil), nil)
	authorityDigest, err := authority.AuthorityDigest()
	if err != nil {
		return nil
	}
	selection, err := agentcontext.SelectCompaction(
		agentcontext.CompactionSelectionRequest{
			History: *history, Force: force,
			AllowCurrentTurn: allowCurrentTurn,
			Input:            input, OutputReserve: outputReserve,
			RecentTailTurns:     e.options.Context.RecentTailTurns,
			RecentTailMaxTokens: e.recentTailMaxTokens(),
			WindowScope:         e.options.Context.Window.Scope,
			AuthorityDigest:     authorityDigest,
			EstimateMessages:    agentcontext.EstimateMessageTokens,
			ProjectHistory:      projectHistory,
			PruneBeforePressure: true,
			Measure: func(
				snapshot agentcontext.MessageSnapshot,
				reserve uint64,
			) (agentcontext.WindowMeasurement, error) {
				window, err := e.measureTokenWindow(
					snapshot, reserve, economicInput,
				)
				return agentcontext.WindowMeasurement{
					Estimated: window.estimated, Total: window.total,
					Active: window.active, HardLimit: window.hardLimit,
					CompactLimit: window.compactLimit,
					Projection:   window.accounting,
				}, err
			},
			Prune: func(
				history *[]provider.Message,
				snapshot agentcontext.MessageSnapshot,
				reserve uint64,
				forced bool,
			) (
				agentcontext.SurfacePruning,
				agentcontext.WindowMeasurement,
				error,
			) {
				stats, window, err := e.pruneToolResultSurfaces(
					history,
					snapshot,
					reserve,
					forced,
					false,
					economicInput,
					projectHistory,
				)
				return agentcontext.SurfacePruning{
						Results: stats.results,
						Bytes:   stats.bytes,
					}, agentcontext.WindowMeasurement{
						Estimated: window.estimated, Total: window.total,
						Active: window.active, HardLimit: window.hardLimit,
						CompactLimit: window.compactLimit,
						Projection:   window.accounting,
					}, err
			},
			Build: e.buildCompactionCandidate,
		},
	)
	if err != nil {
		return nil
	}
	if selection.Candidate == nil {
		if selection.Pruning.Results == 0 {
			return nil
		}
		*history = selection.History
		return finish(promptcontext.NewPruningReceipt(
			selection,
			authorityDigest,
			e.contextReceipts(),
		))
	}
	selected := selection.Candidate
	workingSet, criticalPaths := e.compactionPaths()
	receipt := promptcontext.NewCompactionReceipt(
		selection,
		summaryLineBytes,
		e.contextReceipts(),
		workingSet,
		criticalPaths,
	)
	*history = selected.History
	return finish(receipt)
}

type compactionCandidate = agentcontext.CompactionCandidate

func (e *Engine) buildCompactionCandidate(
	history []provider.Message,
	cut int,
	includeNarrative bool,
) (compactionCandidate, error) {
	removed := cloneMessages(history[:cut])
	toSummarize := agentcontext.StripWorldState(
		promptcontext.StripContextualFragments(cloneMessages(removed)),
	)
	summary := e.buildCompactSummary(toSummarize)
	if summary.Goal == "" {
		goal := agentcontext.ActiveTurnGoal(history)
		summary.Digest = agentcontext.RemoveGoalDigest(summary.Digest, goal)
		goal = strings.Join(strings.Fields(goal), " ")
		goalLimit := max(32, min(summaryLineBytes, e.summaryBudget()/2))
		if len(goal) > goalLimit {
			goal = agentcontext.TruncateUTF8(goal, goalLimit) + "..."
		}
		summary.Goal = goal
	}
	current := e.buildTruthCapsule(summary, nil)
	tail := agentcontext.StripWorldState(
		promptcontext.StripContextualFragments(cloneMessages(history[cut:])),
	)
	return agentcontext.BuildCompactionCandidate(
		agentcontext.CompactionCandidateInput{
			Cut: cut, Removed: removed, ToSummarize: toSummarize,
			Tail: tail, OriginalHistory: history,
			Summary: summary, CurrentTruth: current,
			RetentionPolicy:  e.options.Context.TruthRetention,
			Turn:             e.turn,
			SummaryMaxBytes:  e.summaryBudget(),
			IncludeNarrative: includeNarrative,
		},
	)
}

func compactionBudgetError(window tokenWindow) error {
	return protocol.NewProblem(
		protocol.CodeResourceExhausted,
		fmt.Sprintf(
			"context compaction could not reduce %d tokens below the %d-token hard limit",
			window.total, window.hardLimit,
		),
		false,
		nil,
	)
}

func (e *Engine) History() []provider.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneMessages(e.history)
}

// ReplaceHistory installs a compacted replacement window as the model-visible history.
func (e *Engine) ReplaceHistory(messages []provider.Message) {
	// Settle any pending post-turn narrative first so its digest lands
	// before the replacement window rather than racing it.
	e.joinPendingNarrative()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resetViewFold()
	e.history = cloneMessages(messages)
	agentcontext.ReconcileHistoryTurns(&e.historyTurns, e.history, "", 0)
	e.advanceTokenWindow()
	e.context.ReconcileWorld(e.history)
	var maxTurn uint64
	for _, message := range e.history {
		if message.Turn > maxTurn {
			maxTurn = message.Turn
		}
	}
	if maxTurn > e.turn {
		e.turn = maxTurn
	}
}

func (e *Engine) Fork() (*Engine, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.scopeMu.Lock()
	defer e.scopeMu.Unlock()
	e.planMu.Lock()
	defer e.planMu.Unlock()
	options := e.options
	options.Guard = nil
	forked, err := New(options)
	if err != nil {
		return nil, fmt.Errorf("construct forked engine: %w", err)
	}
	forkWindow, err := agentcontext.CreateWindowLedger(1)
	if err != nil {
		forkWindow = agentcontext.FallbackWindowLedger(
			agentcontext.WindowLedger{},
			fmt.Sprintf("%s:fork:%d", e.options.SessionID, e.turn),
		)
	}
	forked.history = cloneMessages(e.history)
	forked.viewFold = e.viewFold
	forked.mailboxHold = append([]PendingInput(nil), e.mailboxHold...)
	forked.turn = e.turn
	e.readResultMu.Lock()
	forked.readResults = make(
		map[string]readResultEntry,
		len(e.readResults),
	)
	for path, entry := range e.readResults {
		forked.readResults[path] = entry
	}
	e.readResultMu.Unlock()
	forked.context = e.context.Clone()
	forked.context.SetWindow(forkWindow)
	forked.historyTurns = agentcontext.CloneHistoryTurns(e.historyTurns)
	forked.planText = e.planText
	forked.plan = e.plan.Clone()
	forked.lastScope = &Scope{
		engine: forked,
		state:  newScopeState(forked),
	}
	if e.planReceipt != nil {
		receipt := *e.planReceipt
		forked.planReceipt = &receipt
	}
	return forked, nil
}

// LastTurnID returns the workspace journal turn id with the highest turn number.
func (e *Engine) LastTurnID() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.turnIDs) == 0 {
		return "", errors.New("no turn to revert")
	}
	var bestID string
	var bestTurn uint64
	for id, turn := range e.turnIDs {
		if bestID == "" || turn >= bestTurn {
			bestID = id
			bestTurn = turn
		}
	}
	return bestID, nil
}

func (e *Engine) RevertWorkspace(
	ctx context.Context, targetTurnID string,
) (workspacejournal.Receipt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.journal == nil {
		return workspacejournal.Receipt{}, errors.New("workspace journal is not configured")
	}
	turn, exists := e.turnIDs[targetTurnID]
	if !exists {
		return workspacejournal.Receipt{}, errors.New("target turn is not present in workspace history")
	}
	receipt, err := e.journal.Revert(ctx, targetTurnID)
	if err != nil {
		return receipt, err
	}
	history := e.history[:0]
	for _, message := range e.history {
		if message.Turn != turn {
			history = append(history, message)
		}
	}
	e.history = history
	e.advanceTokenWindow()
	e.reconcileWorldBaseline(e.history)
	delete(e.turnIDs, targetTurnID)
	delete(e.historyTurns, targetTurnID)
	return receipt, nil
}

func cloneMessages(messages []provider.Message) []provider.Message {
	return agentcontext.CloneMessages(messages)
}

func cloneBlocks(blocks []provider.ContentBlock) []provider.ContentBlock {
	return agentcontext.CloneBlocks(blocks)
}
