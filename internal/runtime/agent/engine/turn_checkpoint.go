package engine

import (
	"context"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	turnhistory "github.com/fwtllh-png/QCode/internal/adapter/tool/turnhistory"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (e *Engine) registerTurnHistoryTool() error {
	if e.options.Tools == nil {
		return nil
	}
	return turnhistory.Register(e.options.Tools, func(
		ctx context.Context, turn uint64,
	) ([]provider.Message, error) {
		return e.lookupTurnHistory(ctx, turn)
	})
}

func (e *Engine) lookupTurnHistory(
	ctx context.Context, turn uint64,
) ([]provider.Message, error) {
	if messages := agentcontext.MessagesForTurn(
		e.cloneHistoryForLookup(), turn,
	); len(messages) > 0 {
		return messages, nil
	}
	return e.lookupArchivedTurn(ctx, turn)
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
		thread := tool.InvocationIdentityFrom(ctx).ThreadID
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

func (e *Engine) omittedTurnTruthEntities(
	history []provider.Message,
) []agentcontext.TruthEntity {
	if len(history) == 0 {
		history = e.cloneHistoryForLookup()
	}
	turns := agentcontext.OmittedTurnIDs(history, e.recentTailTurns())
	entity, ok := agentcontext.OmittedTurnRetrievalEntity(turns)
	if !ok {
		return nil
	}
	return []agentcontext.TruthEntity{entity}
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

func (e *Engine) closedTurnCheckpointMessages() []provider.Message {
	e.checkpointMu.Lock()
	defer e.checkpointMu.Unlock()
	return agentcontext.CheckpointMessages(e.turnCheckpoints)
}

func (e *Engine) checkpointBudget() int {
	return agentcontext.ResolveCheckpointBudget(
		e.options.Context.CheckpointMaxBytes,
		e.options.SummaryMaxBytes,
		e.options.Context.NarrativeLimits.ItemMaxBytes,
	)
}

func (e *Engine) promoteOpenWork(artifact agentcontext.NarrativeArtifact) {
	e.planMu.Lock()
	current := e.plan.Clone()
	e.planMu.Unlock()
	promoted := agentcontext.PromoteNarrativeOpenWork(current, artifact)
	if planStepsEqual(current, promoted) {
		return
	}
	e.setPlan(promoted)
}

func planStepsEqual(left, right interact.Plan) bool {
	if len(left.Steps) != len(right.Steps) {
		return false
	}
	for index := range left.Steps {
		if left.Steps[index] != right.Steps[index] {
			return false
		}
	}
	return true
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
	if artifact != nil && status == agentcontext.CheckpointCompleted {
		e.promoteOpenWork(*artifact)
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
