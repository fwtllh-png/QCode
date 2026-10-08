package engine

import (
	"encoding/json"
	"errors"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

type CompactionDelta = agentcontext.Compaction
type SessionStateDelta = agentcontext.SessionState
type SessionDelta = agentcontext.SessionDelta

func (e *Engine) stageSessionDelta(delta SessionDelta) {
	scope := e.runningScope()
	if scope == nil {
		return
	}
	scope.mu.Lock()
	scope.state.delta = &delta
	scope.mu.Unlock()
}

func (e *Engine) discardSessionDelta() {
	scope := e.runningScope()
	if scope == nil {
		return
	}
	scope.mu.Lock()
	scope.state.delta = nil
	scope.mu.Unlock()
}

func (e *Engine) applySessionDelta() error {
	scope := e.runningScope()
	if scope == nil {
		return errors.New("turn scope is not active")
	}
	scope.mu.Lock()
	delta := scope.state.delta
	scope.mu.Unlock()
	if delta == nil {
		return nil
	}
	return e.applyDurableSessionDelta(*delta, false)
}

func (e *Engine) applyDurableSessionDelta(
	delta SessionDelta,
	bootstrap bool,
) error {
	restore, err := agentcontext.PrepareSessionRestore(
		delta,
		e.sessionRevision,
		e.appliedDeltas[delta.ReplayKey()],
		bootstrap,
	)
	if err != nil || restore.Replay {
		return err
	}
	e.history = restore.History
	e.historyTurns = restore.State.HistoryTurns
	e.turn = max(e.turn, restore.State.Turn)
	e.usage.Add(restore.Accounting.Usage)
	e.costUSD += float64(restore.Accounting.CostMicrounits) / 1_000_000
	e.syncSessionTitleState(provider.Usage{})
	e.context.Restore(
		restore.State.WorkingSet,
		restore.State.Evidence,
		restore.State.Failures,
		restore.State.Compaction,
		restore.State.World,
		restore.State.Window,
		restore.History,
	)
	e.context.SetConversation(restore.State.Conversation)
	if restore.State.Plan != nil {
		e.setPlan(restore.State.Plan.Clone())
	}
	e.checkpointMu.Lock()
	e.turnCheckpoints = agentcontext.CloneTurnCheckpoints(
		restore.State.TurnCheckpoints,
	)
	e.checkpointMu.Unlock()
	e.stateEpoch = restore.State.Epoch
	e.sessionRevision = restore.Revision
	e.appliedDeltas[restore.Key] = restore.Digest
	return nil
}

func (e *Engine) PreparedSessionDelta() (SessionDelta, bool) {
	scope := e.runningScope()
	if scope == nil {
		return SessionDelta{}, false
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.state.delta == nil {
		return SessionDelta{}, false
	}
	delta := *scope.state.delta
	delta.History = cloneMessages(delta.History)
	delta.MessageTurns = append([]uint64(nil), delta.MessageTurns...)
	delta.HistoryTurns = agentcontext.CloneHistoryTurns(delta.HistoryTurns)
	delta.World = agentcontext.CloneWorldBaseline(delta.World)
	delta.Window = agentcontext.CloneWindowLedger(delta.Window)
	delta.Compaction = agentcontext.CloneCompaction(delta.Compaction)
	delta.TurnCheckpoints = agentcontext.CloneTurnCheckpoints(delta.TurnCheckpoints)
	return delta, true
}

func (e *Engine) RestoreSessionDelta(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	delta, err := agentcontext.DecodeSessionDelta(raw)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.invalidatePendingNarrative()
	return e.applyDurableSessionDelta(
		delta,
		e.sessionRevision == 0 && len(e.appliedDeltas) == 0,
	)
}

func (e *Engine) SessionRevision() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sessionRevision
}

func (e *Engine) captureWorkspaceBindingFor(
	delta agentcontext.EvidenceDelta,
) (agentcontext.WorkspaceBinding, error) {
	return agentcontext.CaptureWorkspaceBindingForEvidence(
		e.options.Workspace,
		e.options.WorkspaceIdentity,
		e.sessionRevision,
		delta,
	)
}
