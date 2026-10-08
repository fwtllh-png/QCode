package engine

import (
	"context"
	"fmt"

	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// WithdrawTurn runs only after Runtime admission has excluded concurrent work.
// Commit the replacement before touching memory so a failed write is harmless.
func (e *Engine) WithdrawTurn(ctx context.Context, thread protocol.ThreadID, turn protocol.TurnID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	store := e.options.TurnContexts
	if store == nil {
		return protocol.NewProblem(protocol.CodeUnavailable, "Turn context storage is unavailable", false, nil)
	}
	withdrawn, err := store.TurnWithdrawn(ctx, thread, turn)
	if err != nil {
		return err
	}
	if withdrawn {
		return e.keepWithdrawnDraft(string(turn))
	}
	baseline, found, err := store.TurnBaseline(ctx, thread, turn)
	if err != nil {
		return err
	}
	if !found {
		return protocol.NewProblem(protocol.CodeConflict, "This Turn has no saved pre-Turn context; it cannot be withdrawn", false, nil)
	}
	current, err := e.currentWorkspaceBinding(baseline.Workspace)
	if err != nil {
		return err
	}
	restored, _, err := agentcontext.ReconcileWorkspace(baseline, current)
	if err != nil {
		return err
	}
	// Keep actual changed paths as unverified facts, never the rejected goal,
	// plan, read handles, assistant output, or generated narrative.
	changes := e.context.Evidence().Delta().Changes
	if e.journal != nil {
		for _, change := range e.journal.DraftChanges(string(turn)) {
			changes = append(changes, agentcontext.EvidenceChange{Path: change.Path, Turn: e.turn})
		}
	}
	seen := make(map[string]int, len(restored.Evidence.Changes))
	for index, change := range restored.Evidence.Changes {
		seen[change.Path] = index
	}
	for _, change := range changes {
		if change.Turn <= baseline.Turn {
			continue
		}
		risk := agentcontext.EvidenceChange{Path: change.Path, Turn: change.Turn, Stale: true}
		if index, exists := seen[change.Path]; exists {
			restored.Evidence.Changes[index] = risk
		} else {
			seen[change.Path] = len(restored.Evidence.Changes)
			restored.Evidence.Changes = append(restored.Evidence.Changes, risk)
		}
	}
	restored.Turn = max(e.turn, baseline.Turn)
	restored.Revision = max(e.sessionRevision, baseline.Revision) + 1
	restored.Epoch = max(e.stateEpoch, baseline.Epoch) + 1
	restored.Window, err = agentcontext.NewWindowLedger(
		fmt.Sprintf("withdraw:%s:%d", turn, restored.Revision), e.context.Window().Number+1)
	if err != nil {
		return err
	}
	if err := restored.Seal(); err != nil {
		return err
	}
	if err := store.CommitTurnWithdrawal(ctx, thread, turn, restored); err != nil {
		return err
	}
	e.applyContextSnapshot(restored)
	e.resetViewFold()
	delete(e.turnIDs, string(turn))
	e.scopeMu.Lock()
	e.mailboxHold = nil
	e.scopeMu.Unlock()
	e.readResultMu.Lock()
	e.readResults = make(map[string]readResultEntry)
	e.readInvalidations = nil
	e.readResultMu.Unlock()
	return e.keepWithdrawnDraft(string(turn))
}

func (e *Engine) keepWithdrawnDraft(turnID string) error {
	if e.journal == nil {
		return nil
	}
	return e.journal.KeepDraft(turnID)
}
