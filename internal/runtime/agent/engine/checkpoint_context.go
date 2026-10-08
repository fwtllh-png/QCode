package engine

import (
	"errors"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func (e *Engine) ExportContextSnapshot() (agentcontext.ContextSnapshot, error) {
	if e == nil {
		return agentcontext.ContextSnapshot{}, errors.New("engine is nil")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.buildContextSnapshot(
		e.history,
		e.context.Compaction(),
		max(uint64(1), e.sessionRevision),
		max(uint64(1), e.stateEpoch),
	)
}

func (e *Engine) buildContextSnapshot(
	history []provider.Message,
	compaction agentcontext.Compaction,
	revision uint64,
	epoch uint64,
) (agentcontext.ContextSnapshot, error) {
	e.planMu.Lock()
	plan := e.plan.Clone()
	e.planMu.Unlock()
	e.checkpointMu.Lock()
	checkpoints := agentcontext.CloneTurnCheckpoints(e.turnCheckpoints)
	e.checkpointMu.Unlock()
	scope := e.runningScope()
	authority := e.context.Clone()
	if scope != nil {
		scope.mu.Lock()
		authority = scope.state.context.Clone()
		scope.mu.Unlock()
	}
	authority.SetCompaction(compaction)
	authority.ReconcileWorld(history)
	if !agentcontext.WorldBaselineValid(history, authority.World()) &&
		agentcontext.WorldBaselineValid(history, e.context.World()) {
		authority.SetWorld(e.context.World())
	}
	return authority.Snapshot(agentcontext.SnapshotRequest{
		History: history, HistoryTurns: e.historyTurns, Plan: plan,
		Turn: e.turn, Revision: revision, Epoch: epoch,
		WorkingSetLimit:     e.options.WorkingSetLimit,
		TruthMaxEntities:    e.options.Context.TruthRetention.TruthMaxEntities,
		FactMaxEntities:     e.options.Context.TruthRetention.FactMaxEntities,
		VerifiedChangeTurns: e.options.Context.TruthRetention.VerifiedChangeRetentionTurns,
		HandleMaxEntities:   e.options.Context.TruthRetention.HandleMaxEntities,
		WorkspaceRoot:       e.options.Workspace,
		WorkspaceIdentity:   e.options.WorkspaceIdentity,
		WorkspaceRevision:   e.sessionRevision,
		TurnCheckpoints:     checkpoints,
	})
}

func (e *Engine) RestoreContextSnapshot(
	snapshot agentcontext.ContextSnapshot,
) (agentcontext.ReconciliationReceipt, error) {
	if err := snapshot.Validate(); err != nil {
		return agentcontext.ReconciliationReceipt{}, err
	}
	current, err := e.currentWorkspaceBinding(snapshot.Workspace)
	if err != nil {
		return agentcontext.ReconciliationReceipt{}, err
	}
	reconciled, receipt, err := agentcontext.ReconcileWorkspace(
		snapshot,
		current,
	)
	if err != nil {
		return agentcontext.ReconciliationReceipt{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	reconciled.Revision = max(e.sessionRevision, snapshot.Revision) + 1
	reconciled.Epoch = max(e.stateEpoch, snapshot.Epoch) + 1
	reconciled.Window, err = agentcontext.NewWindowLedger(
		fmt.Sprintf("checkpoint:%d:%d", reconciled.Epoch, reconciled.Revision),
		e.context.Window().Number+1,
	)
	if err != nil {
		return agentcontext.ReconciliationReceipt{}, err
	}
	if err := reconciled.Seal(); err != nil {
		return agentcontext.ReconciliationReceipt{}, err
	}
	e.applyContextSnapshot(reconciled)
	return receipt, nil
}

func (e *Engine) ForkFromContextSnapshot(
	snapshot agentcontext.ContextSnapshot,
) (*Engine, agentcontext.ReconciliationReceipt, error) {
	if err := snapshot.Validate(); err != nil {
		return nil, agentcontext.ReconciliationReceipt{}, err
	}
	current, err := e.currentWorkspaceBinding(snapshot.Workspace)
	if err != nil {
		return nil, agentcontext.ReconciliationReceipt{}, err
	}
	reconciled, receipt, err := agentcontext.ReconcileWorkspace(snapshot, current)
	if err != nil {
		return nil, agentcontext.ReconciliationReceipt{}, err
	}
	options := e.options
	options.Guard = nil
	forked, err := New(options)
	if err != nil {
		return nil, agentcontext.ReconciliationReceipt{}, err
	}
	reconciled.Epoch = 1
	reconciled.Revision = 1
	// A fork has a new Thread identity. An in-flight or completed compaction
	// fence belongs to the parent and must not be resumed in the child.
	reconciled.Compaction.State = nil
	reconciled.Window, err = agentcontext.NewWindowLedger(
		fmt.Sprintf("checkpoint-fork:%s", snapshot.Digest),
		1,
	)
	if err != nil {
		return nil, agentcontext.ReconciliationReceipt{}, err
	}
	if err := reconciled.Seal(); err != nil {
		return nil, agentcontext.ReconciliationReceipt{}, err
	}
	forked.mu.Lock()
	forked.applyContextSnapshot(reconciled)
	// Inherited turn identities remain valid archive pointers in the child.
	for turnID, number := range reconciled.HistoryTurns {
		forked.turnIDs[turnID] = number
	}
	if reconciled.Conversation != nil {
		for _, source := range reconciled.Conversation.Sources {
			forked.turnIDs[source.TurnID] = source.Turn
		}
	}
	forked.mu.Unlock()
	return forked, receipt, nil
}

func (e *Engine) currentWorkspaceBinding(
	checkpoint agentcontext.WorkspaceBinding,
) (agentcontext.WorkspaceBinding, error) {
	paths := make([]string, len(checkpoint.BoundPaths))
	for index, path := range checkpoint.BoundPaths {
		paths[index] = path.Path
	}
	return agentcontext.CaptureWorkspaceBinding(
		e.options.Workspace,
		e.options.WorkspaceIdentity,
		e.sessionRevision,
		paths,
	)
}

// applyContextSnapshot changes only live Context state. Usage and cost remain
// monotonic accounting owned by the receiving Engine.
func (e *Engine) applyContextSnapshot(snapshot agentcontext.ContextSnapshot) {
	e.invalidatePendingNarrative()
	e.history = cloneMessages(snapshot.History)
	for index, turn := range snapshot.MessageTurns {
		if index < len(e.history) {
			e.history[index].Turn = turn
		}
	}
	e.historyTurns = agentcontext.CloneHistoryTurns(snapshot.HistoryTurns)
	e.turn = max(e.turn, snapshot.Turn)
	e.context.Restore(
		snapshot.WorkingSet,
		snapshot.Evidence,
		snapshot.Failures,
		snapshot.Compaction,
		snapshot.World,
		snapshot.Window,
		snapshot.History,
	)
	e.context.SetConversation(snapshot.Conversation)
	if snapshot.Plan != nil {
		e.setPlan(snapshot.Plan.Clone())
	} else {
		e.setPlan(agentcontext.Plan{})
	}
	e.checkpointMu.Lock()
	e.turnCheckpoints = agentcontext.CloneTurnCheckpoints(snapshot.TurnCheckpoints)
	e.checkpointMu.Unlock()
	e.stateEpoch = snapshot.Epoch
	e.sessionRevision = snapshot.Revision
	e.appliedDeltas = make(map[string]string)
	e.scopeMu.Lock()
	e.lastScope = &Scope{engine: e, state: newScopeState(e)}
	e.scopeMu.Unlock()
}
