package engine

import (
	"context"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	turnhistory "github.com/fwtllh-png/QCode/internal/adapter/tool/turnhistory"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (e *Engine) registerTurnHistoryTool() error {
	if e.options.Tools == nil {
		return nil
	}
	return turnhistory.Register(e.options.Tools, e.lookupTurnHistoryEntry, e.lookupConversationEntry)
}

func (e *Engine) lookupTurnHistoryEntry(ctx context.Context, turn uint64) (*turnhistory.Entry, error) {
	messages, complete, err := e.lookupTurnHistoryData(ctx, turn)
	if err != nil {
		return nil, err
	}
	conversation := e.contextAuthority().Conversation()
	sources := conversation.SourcesForTurn(turn)
	// A source body is another representation of the same turn; it must not
	// bypass withdrawal when the transcript lookup deliberately returned none.
	for _, source := range sources {
		available, err := e.conversationSourceAvailable(ctx, source)
		if err != nil || !available {
			return nil, err
		}
	}
	if len(messages) == 0 && len(sources) == 0 {
		return nil, nil
	}
	transcript := agentcontext.RenderTurnTranscript(messages)
	if len(messages) != 0 && !complete {
		transcript = "[In-memory turn projection; full transcript completeness is not established.]\n" + transcript
	}
	if len(messages) == 0 {
		transcript = "[Only completed answer sources are available; the full turn transcript is unavailable.]\n"
		for _, source := range sources {
			transcript += source.Text + "\n"
		}
	}
	findings, _ := e.lookupTurnFindings(ctx, turn)
	return &turnhistory.Entry{
		Transcript:    transcript,
		FindingsIndex: agentcontext.RenderTurnFindings(turn, findings) + promptcontext.ConversationSourceIndex(conversation, sources),
	}, nil
}

func (e *Engine) lookupTurnFindings(
	_ context.Context, turn uint64,
) (agentcontext.TurnFindings, bool) {
	if e == nil || turn == 0 {
		return agentcontext.TurnFindings{}, false
	}
	e.checkpointMu.Lock()
	defer e.checkpointMu.Unlock()
	for _, checkpoint := range e.turnCheckpoints {
		if checkpoint.Turn != turn || checkpoint.Findings.Empty() {
			continue
		}
		return agentcontext.CloneTurnFindings(checkpoint.Findings), true
	}
	return agentcontext.TurnFindings{}, false
}

func (e *Engine) lookupTurnHistoryData(ctx context.Context, turn uint64) ([]provider.Message, bool, error) {
	// A surviving in-memory message does not prove the turn is complete.
	// Prefer the authoritative archive whenever this turn has one.
	if messages, err := e.lookupArchivedTurn(ctx, turn); err != nil || len(messages) != 0 {
		return messages, err == nil, err
	}
	_, turnID := e.turnArchiveSource(turn)
	if store := e.options.TurnContexts; store != nil && turnID != "" {
		thread := e.historySourceThread(ctx, turn)
		withdrawn, err := store.TurnWithdrawn(ctx, protocol.ThreadID(thread), protocol.TurnID(turnID))
		if err != nil || withdrawn {
			return nil, false, err
		}
	}
	if messages := agentcontext.MessagesForTurn(
		e.cloneHistoryForLookup(), turn,
	); len(messages) > 0 {
		return messages, false, nil
	}
	return nil, false, nil
}

// lookupArchivedTurn recovers a closed turn from the durable transcript
// archive when compaction or replacement removed it from the in-memory
// history. A missing archive, an unknown turn number, or an absent durable
// transcript yields a nil slice so the tool reports the honest miss.
func (e *Engine) lookupArchivedTurn(
	ctx context.Context, turn uint64,
) ([]provider.Message, error) {
	archive, turnID := e.turnArchiveSource(turn)
	if archive == nil || turnID == "" {
		return nil, nil
	}
	if store := e.options.TurnContexts; store != nil {
		thread := e.historySourceThread(ctx, turn)
		withdrawn, err := store.TurnWithdrawn(ctx, protocol.ThreadID(thread), protocol.TurnID(turnID))
		if err != nil || withdrawn {
			return nil, err
		}
	}
	history, err := archive.LookupTurn(ctx, turnID)
	if err != nil {
		return nil, err
	}
	return agentcontext.MessagesForTurn(history, turn), nil
}

// Forked history retains its origin thread for withdrawal checks.
func (e *Engine) historySourceThread(ctx context.Context, turn uint64) string {
	for _, source := range e.contextAuthority().Conversation().SourcesForTurn(turn) {
		return source.ThreadID
	}
	return tool.InvocationIdentityFrom(ctx).ThreadID
}

func (e *Engine) turnArchiveSource(
	turn uint64,
) (TurnTranscriptArchive, string) {
	if scope := e.runningScope(); scope != nil {
		return scope.state.turnArchive.source(turn)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return (turnArchiveSnapshot{
		archive: e.options.TurnTranscriptArchive,
		turnIDs: e.turnIDs,
	}).source(turn)
}

func (e *Engine) cloneHistoryForLookup() []provider.Message {
	if e.mu.TryLock() {
		defer e.mu.Unlock()
		return cloneMessages(e.history)
	}
	return cloneMessages(e.history)
}

func (e *Engine) currentTurn() uint64 {
	if e.mu.TryLock() {
		defer e.mu.Unlock()
		return e.turn
	}
	return e.turn
}

func (e *Engine) currentPlan() agentcontext.Plan {
	e.planMu.Lock()
	defer e.planMu.Unlock()
	return e.plan.Clone()
}

func (e *Engine) resumeReadPaths() []string {
	return e.workingLedger().PathsWithSource(agentcontext.SourceRead)
}

func (e *Engine) resumeTruthEntities() []agentcontext.TruthEntity {
	entity, ok := agentcontext.ResumeRetrievalEntityBudgeted(
		e.currentPlan(),
		e.resumeReadPaths(),
		e.sessionStateBudget(),
		e.locatedSites(),
	)
	if !ok {
		return nil
	}
	return []agentcontext.TruthEntity{entity}
}

func (e *Engine) continuityTruthEntities() []agentcontext.TruthEntity {
	entity, ok := agentcontext.ContinuityRetrievalEntityBudgeted(
		e.continuityInput(),
		e.sessionStateBudget(),
	)
	if !ok {
		return nil
	}
	return []agentcontext.TruthEntity{entity}
}

func (e *Engine) continuityInput() agentcontext.ContinuityInput {
	e.checkpointMu.Lock()
	checkpoints := agentcontext.CloneTurnCheckpoints(e.turnCheckpoints)
	e.checkpointMu.Unlock()
	conversation := e.contextAuthority().Conversation()
	input := agentcontext.ContinuityInput{
		Next: agentcontext.FirstOutstandingPlanTitle(e.currentPlan()),
	}
	for index := len(checkpoints) - 1; index >= 0; index-- {
		checkpoint := checkpoints[index]
		if checkpoint.Status != agentcontext.CheckpointCompleted ||
			checkpoint.Findings.Empty() {
			continue
		}
		input.Conclusion = strings.TrimSpace(checkpoint.Findings.Conclusion)
		if conversation != nil {
			for _, source := range conversation.Sources {
				if source.Turn == checkpoint.Turn {
					// Indexed answers use the current reference projection. A frozen
					// continuity capsule must not duplicate or revive a cleared focus.
					input.Conclusion = ""
					break
				}
			}
		}
		input.SourceTurns = append(input.SourceTurns, checkpoint.Turn)
		input.SourceTurns = append(input.SourceTurns, checkpoint.Findings.SourceTurns...)
		break
	}
	facts := e.EvidenceSnapshot().Facts
	input.Sites = agentcontext.ContinuitySites(facts)
	for _, fact := range facts {
		if fact.Line > 0 && fact.Turn > 0 {
			input.SourceTurns = append(input.SourceTurns, fact.Turn)
		}
	}
	input.SourceTurns = agentcontext.UniqueTurns(input.SourceTurns)
	return input
}

func (e *Engine) ensureClosedTurnCheckpoints() {
	history := e.cloneHistoryForLookup()
	current := e.currentTurn()
	e.checkpointMu.Lock()
	have := make(map[uint64]struct{}, len(e.turnCheckpoints))
	for _, checkpoint := range e.turnCheckpoints {
		have[checkpoint.Turn] = struct{}{}
	}
	e.checkpointMu.Unlock()
	budget := e.checkpointBudget()
	for _, turn := range agentcontext.UniqueMessageTurns(history) {
		if turn == 0 || turn >= current {
			continue
		}
		if _, exists := have[turn]; exists {
			continue
		}
		checkpoint, err := agentcontext.RenderTurnCheckpoint(
			agentcontext.CheckpointRenderInput{
				Turn:   turn,
				Status: agentcontext.CheckpointUnknown,
				Budget: budget,
			},
		)
		if err != nil {
			continue
		}
		e.checkpointMu.Lock()
		duplicate := false
		for _, existing := range e.turnCheckpoints {
			if existing.Turn == turn {
				duplicate = true
				break
			}
		}
		if !duplicate {
			e.turnCheckpoints = append(e.turnCheckpoints, checkpoint)
			have[turn] = struct{}{}
		}
		e.checkpointMu.Unlock()
	}
}

func (e *Engine) sealTurnFindings(turn uint64, status string) agentcontext.TurnFindings {
	var facts []agentcontext.EvidenceFact
	var sourceTurns []uint64
	for _, fact := range e.EvidenceSnapshot().Facts {
		if fact.Turn != turn || fact.Line <= 0 {
			continue
		}
		facts = append(facts, fact)
		sourceTurns = append(sourceTurns, fact.Turn)
	}
	findings := agentcontext.TurnFindings{
		Sites:       agentcontext.ContinuitySites(facts),
		SourceTurns: agentcontext.UniqueTurns(sourceTurns),
	}
	if status == agentcontext.CheckpointCompleted {
		findings.Conclusion = agentcontext.LastAssistantConclusion(
			agentcontext.MessagesForTurn(e.cloneHistoryForLookup(), turn),
		)
		// Terminal capture precedes history replacement and durable sealing.
		if conversation := e.contextAuthority().Conversation(); conversation != nil {
			for _, source := range conversation.Sources {
				if source.Turn == turn {
					findings.Conclusion = source.Text
					break
				}
			}
		}
		if findings.Conclusion != "" {
			findings.SourceTurns = agentcontext.UniqueTurns(
				append(findings.SourceTurns, turn),
			)
		}
	}
	return findings
}

func (e *Engine) closedTurnSealStatus() (string, bool) {
	turn := e.currentTurn()
	if turn == 0 {
		return "", false
	}
	e.checkpointMu.Lock()
	defer e.checkpointMu.Unlock()
	for _, checkpoint := range e.turnCheckpoints {
		if checkpoint.Turn == turn {
			return checkpoint.Status, true
		}
	}
	return "", false
}

func (e *Engine) checkpointBudget() int {
	return agentcontext.ResolveCheckpointBudget(
		e.options.Context.CheckpointMaxBytes,
		e.options.SummaryMaxBytes,
		e.options.Context.NarrativeLimits.ItemMaxBytes,
	)
}

func (e *Engine) sealClosedTurnMemory(
	status string,
	artifact *agentcontext.NarrativeArtifact,
	failure string,
) {
	if e == nil {
		return
	}
	turn := e.currentTurn()
	if turn == 0 {
		return
	}
	if status == "" {
		status = agentcontext.CheckpointCompleted
	}
	e.checkpointMu.Lock()
	for _, existing := range e.turnCheckpoints {
		if existing.Turn == turn {
			e.checkpointMu.Unlock()
			return
		}
	}
	e.checkpointMu.Unlock()
	e.planMu.Lock()
	plan := e.plan.Clone()
	e.planMu.Unlock()
	var items []agentcontext.NarrativeItem
	if artifact != nil {
		items = artifact.Body.Items
	}
	var readPaths []string
	if status == agentcontext.CheckpointCanceled || status == agentcontext.CheckpointFailed {
		readPaths = e.resumeReadPaths()
	}
	checkpoint, err := agentcontext.RenderTurnCheckpoint(
		agentcontext.CheckpointRenderInput{
			Turn:      turn,
			Status:    status,
			Plan:      plan,
			Items:     items,
			Failure:   strings.TrimSpace(failure),
			ReadPaths: readPaths,
			Findings:  e.sealTurnFindings(turn, status),
			Budget:    e.checkpointBudget(),
		},
	)
	if err != nil {
		return
	}
	e.checkpointMu.Lock()
	defer e.checkpointMu.Unlock()
	for _, existing := range e.turnCheckpoints {
		if existing.Turn == turn {
			return
		}
	}
	e.turnCheckpoints = append(e.turnCheckpoints, checkpoint)
}
