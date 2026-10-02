package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (r *ArtifactService) Checkpoints(
	ctx context.Context,
	sessionID string,
	limit int,
) (protocol.CheckpointList, error) {
	if r.runtime.sessionArtifacts == nil {
		return protocol.CheckpointList{}, runtimeProblem(protocol.CodeUnavailable, "Session Checkpoints are unavailable", nil)
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		return protocol.CheckpointList{}, runtimeProblem(protocol.CodeInvalidArgument, "Checkpoint limit exceeds 1000", nil)
	}
	current, err := r.runtime.SessionStatus(ctx, sessionID)
	if err != nil {
		return protocol.CheckpointList{}, err
	}
	values, err := r.runtime.sessionArtifacts.ListCheckpoints(ctx, sessionID, limit)
	if err != nil {
		return protocol.CheckpointList{}, err
	}
	profile, err := r.runtime.SessionProfile(ctx, sessionID)
	if err != nil {
		return protocol.CheckpointList{}, err
	}
	quiescent := ensureSessionQuiescent(current, "restore") == nil
	for index := range values {
		compatible := values[index].ProfileRevision == profile.Profile.Revision
		values[index].CanRestore = compatible && quiescent
		values[index].CanFork = compatible && quiescent
	}
	result := protocol.CheckpointList{
		Version:     protocol.CheckpointProtocolVersion,
		SessionID:   sessionID,
		Checkpoints: values,
	}
	if err := result.Validate(); err != nil {
		return protocol.CheckpointList{}, err
	}
	return result, nil
}

func (r *ArtifactService) Checkpoint(
	ctx context.Context,
	sessionID, checkpointID string,
) (protocol.SessionCheckpoint, error) {
	if r.runtime.sessionArtifacts == nil {
		return protocol.SessionCheckpoint{}, runtimeProblem(protocol.CodeUnavailable, "Session Checkpoints are unavailable", nil)
	}
	current, err := r.runtime.SessionStatus(ctx, sessionID)
	if err != nil {
		return protocol.SessionCheckpoint{}, err
	}
	checkpoint, _, _, err := r.runtime.sessionArtifacts.GetCheckpoint(
		ctx,
		checkpointID,
	)
	if err != nil {
		return protocol.SessionCheckpoint{}, err
	}
	if checkpoint.SessionID != sessionID {
		return protocol.SessionCheckpoint{}, runtimeProblem(protocol.CodeInvalidArgument, "Checkpoint does not belong to the Session", nil)
	}
	if err := r.requireRetainedSource(ctx, checkpoint.ThreadID, checkpoint.TurnID); err != nil {
		checkpoint.CanRestore, checkpoint.CanFork = false, false
		return checkpoint, nil
	}
	profile, err := r.runtime.SessionProfile(ctx, sessionID)
	if err != nil {
		return protocol.SessionCheckpoint{}, err
	}
	compatible := profile.Profile.Revision == checkpoint.ProfileRevision &&
		ensureSessionQuiescent(current, "restore") == nil
	checkpoint.CanRestore = compatible
	checkpoint.CanFork = compatible
	return checkpoint, nil
}

func (r *ArtifactService) RestoreCheckpoint(
	ctx context.Context,
	sessionID, checkpointID string,
) (protocol.CheckpointRestoreResult, error) {
	release, err := r.beginContextMutation()
	if err != nil {
		return protocol.CheckpointRestoreResult{}, err
	}
	defer release()
	current, checkpoint, history, contextSnapshot, err := r.checkpointState(
		ctx,
		sessionID,
		checkpointID,
		"restore",
	)
	if err != nil {
		return protocol.CheckpointRestoreResult{}, err
	}
	manager, ok := r.runtime.engine.(CheckpointEngine)
	if !ok {
		return protocol.CheckpointRestoreResult{}, resourceProblem(
			protocol.CodeUnavailable,
			"Checkpoint restore is unsupported by this engine",
			false,
			protocol.ProblemReasonUnsupported,
			checkpointID,
		)
	}
	decoded, err := agentcontext.DecodeCompactedHistory(history)
	if err != nil {
		return protocol.CheckpointRestoreResult{}, err
	}
	previous, err := manager.History(current.ThreadID)
	if err != nil {
		return protocol.CheckpointRestoreResult{}, err
	}
	var (
		reconciliation  agentcontext.ReconciliationReceipt
		previousContext *agentcontext.ContextSnapshot
	)
	if contextSnapshot != nil {
		contextManager, supported := manager.(ContextCheckpointEngine)
		if !supported {
			return protocol.CheckpointRestoreResult{}, resourceProblem(
				protocol.CodeUnavailable,
				"Context checkpoint restore is unsupported by this engine",
				false,
				protocol.ProblemReasonUnsupported,
				checkpointID,
			)
		}
		value, exportErr := contextManager.ContextSnapshot(current.ThreadID)
		if exportErr != nil {
			return protocol.CheckpointRestoreResult{}, exportErr
		}
		previousContext = &value
		reconciliation, err = contextManager.RestoreContext(
			current.ThreadID,
			*contextSnapshot,
		)
	} else {
		err = manager.RestoreCheckpoint(current.ThreadID, decoded)
	}
	if err != nil {
		return protocol.CheckpointRestoreResult{}, err
	}
	operationID := protocol.OperationID(stableArtifactID(
		"op",
		sessionID,
		checkpointID,
		"restore",
	))
	currentCommitID := ""
	var committedContext *agentcontext.ContextSnapshot
	if contextSnapshot != nil && r.runtime.durable {
		store, supported := r.runtime.contextRebaseStore.(CurrentContextStore)
		if !supported {
			_, rollbackErr := manager.(ContextCheckpointEngine).RestoreContext(
				current.ThreadID,
				*previousContext,
			)
			return protocol.CheckpointRestoreResult{}, errors.Join(
				errors.New("durable current context store is unavailable"),
				rollbackErr,
			)
		}
		restored, exportErr := manager.(ContextCheckpointEngine).ContextSnapshot(
			current.ThreadID,
		)
		if exportErr != nil {
			_, rollbackErr := manager.(ContextCheckpointEngine).RestoreContext(
				current.ThreadID,
				*previousContext,
			)
			return protocol.CheckpointRestoreResult{}, errors.Join(
				exportErr,
				rollbackErr,
			)
		}
		currentCommitID = stableArtifactID(
			"context",
			string(operationID),
			restored.Digest,
		)
		commitErr := store.CommitCurrentContext(
			ctx,
			agentcontext.CurrentContextCommit{
				ID:       currentCommitID,
				ThreadID: current.ThreadID,
				TurnID:   checkpoint.TurnID,
				Snapshot: restored,
			},
		)
		if commitErr != nil {
			_, rollbackErr := manager.(ContextCheckpointEngine).RestoreContext(
				current.ThreadID,
				*previousContext,
			)
			return protocol.CheckpointRestoreResult{}, errors.Join(
				commitErr,
				rollbackErr,
			)
		}
		committedContext = &restored
	}
	itemID := protocol.ItemID(stableArtifactID(
		"item",
		sessionID,
		checkpointID,
		"restore",
	))
	var contextDigest string
	var contextRevision, stateEpoch uint64
	if committedContext != nil {
		contextDigest = committedContext.Digest
		contextRevision = committedContext.Revision
		stateEpoch = committedContext.Epoch
	}
	publishErr := r.runtime.EventService.publish(
		operationID,
		current.ThreadID,
		checkpoint.TurnID,
		itemID,
		&protocol.CheckpointRestoredData{
			CheckpointID:   checkpoint.ID,
			SourceThreadID: checkpoint.ThreadID,
			SourceTurnID:   checkpoint.TurnID,
			SourceCursor:   checkpoint.Cursor,
			ReplacementHistory: append(
				[]protocol.CompactedMessage(nil),
				history...,
			),
			SideEffectsReplayed: false,
			ExactContext:        contextSnapshot != nil,
			WorkspaceClaimsValid: contextSnapshot != nil &&
				reconciliation.Stale == 0,
			InvalidatedClaims: reconciliation.Invalidated,
			StaleClaims:       reconciliation.Stale,
			ContextCommitID:   currentCommitID,
			ContextDigest:     contextDigest,
			ContextRevision:   contextRevision,
			StateEpoch:        stateEpoch,
		},
	)
	if publishErr != nil {
		var rollbackErr error
		if previousContext != nil {
			contextManager := manager.(ContextCheckpointEngine)
			_, rollbackErr = contextManager.RestoreContext(
				current.ThreadID,
				*previousContext,
			)
			if rollbackErr == nil && currentCommitID != "" {
				rollback, exportErr := contextManager.ContextSnapshot(
					current.ThreadID,
				)
				if exportErr != nil {
					rollbackErr = exportErr
				} else {
					store := r.runtime.contextRebaseStore.(CurrentContextStore)
					rollbackErr = store.CommitCurrentContext(
						ctx,
						agentcontext.CurrentContextCommit{
							ID: stableArtifactID(
								"context",
								string(operationID),
								"rollback",
								rollback.Digest,
							),
							ThreadID: current.ThreadID,
							TurnID:   checkpoint.TurnID,
							Snapshot: rollback,
						},
					)
				}
			}
		} else {
			rollbackErr = manager.RestoreCheckpoint(current.ThreadID, previous)
		}
		return protocol.CheckpointRestoreResult{}, errors.Join(
			publishErr,
			rollbackErr,
		)
	}
	return protocol.CheckpointRestoreResult{
		Version:             protocol.CheckpointProtocolVersion,
		Checkpoint:          checkpoint,
		ThreadID:            current.ThreadID,
		RestoredCursor:      checkpoint.Cursor,
		SideEffectsReplayed: false,
		ExactContext:        contextSnapshot != nil,
		WorkspaceClaimsValid: contextSnapshot != nil &&
			reconciliation.Stale == 0,
		InvalidatedClaims: reconciliation.Invalidated,
		StaleClaims:       reconciliation.Stale,
	}, nil
}

func (r *ArtifactService) ForkCheckpoint(
	ctx context.Context,
	sessionID, checkpointID, title string,
) (protocol.CheckpointForkResult, error) {
	release, err := r.beginContextMutation()
	if err != nil {
		return protocol.CheckpointForkResult{}, err
	}
	defer release()
	_, checkpoint, history, contextSnapshot, err := r.checkpointState(
		ctx,
		sessionID,
		checkpointID,
		"fork",
	)
	if err != nil {
		return protocol.CheckpointForkResult{}, err
	}
	manager, ok := r.runtime.engine.(CheckpointEngine)
	if !ok {
		return protocol.CheckpointForkResult{}, resourceProblem(
			protocol.CodeUnavailable,
			"Checkpoint Fork is unsupported by this engine",
			false,
			protocol.ProblemReasonUnsupported,
			checkpointID,
		)
	}
	decoded, err := agentcontext.DecodeCompactedHistory(history)
	if err != nil {
		return protocol.CheckpointForkResult{}, err
	}
	newThreadID, err := protocol.NewThreadID()
	if err != nil {
		return protocol.CheckpointForkResult{}, err
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Checkpoint Fork"
	}
	title = boundedArtifactText(title, 256)
	operationID := protocol.OperationID(stableArtifactID(
		"op",
		sessionID,
		checkpointID,
		string(newThreadID),
	))
	itemID := protocol.ItemID(stableArtifactID(
		"item",
		sessionID,
		checkpointID,
		string(newThreadID),
	))
	var reconciliation agentcontext.ReconciliationReceipt
	currentCommitID := ""
	var committedContext *agentcontext.ContextSnapshot
	if contextSnapshot != nil {
		contextManager, supported := manager.(ContextCheckpointEngine)
		if !supported {
			return protocol.CheckpointForkResult{}, resourceProblem(
				protocol.CodeUnavailable,
				"Context checkpoint Fork is unsupported by this engine",
				false,
				protocol.ProblemReasonUnsupported,
				checkpointID,
			)
		}
		reconciliation, err = contextManager.ForkContext(
			checkpoint.ThreadID,
			newThreadID,
			*contextSnapshot,
		)
		if err == nil && r.runtime.durable {
			store, durable := r.runtime.contextRebaseStore.(CurrentContextStore)
			if !durable {
				manager.Release(newThreadID)
				return protocol.CheckpointForkResult{},
					errors.New("durable current context store is unavailable")
			}
			forked, exportErr := contextManager.ContextSnapshot(newThreadID)
			if exportErr != nil {
				manager.Release(newThreadID)
				return protocol.CheckpointForkResult{}, exportErr
			}
			currentCommitID = stableArtifactID(
				"context",
				string(operationID),
				forked.Digest,
			)
			err = store.CommitCurrentContext(
				ctx,
				agentcontext.CurrentContextCommit{
					ID:             currentCommitID,
					ThreadID:       newThreadID,
					TurnID:         checkpoint.TurnID,
					SessionID:      sessionID,
					ParentThreadID: checkpoint.ThreadID,
					Title:          title,
					SourceCursor:   checkpoint.Cursor,
					Snapshot:       forked,
				},
			)
			if err != nil {
				manager.Release(newThreadID)
				return protocol.CheckpointForkResult{}, err
			}
			committedContext = &forked
		}
	} else {
		err = manager.ForkCheckpoint(
			checkpoint.ThreadID,
			newThreadID,
			decoded,
		)
	}
	if err != nil {
		return protocol.CheckpointForkResult{}, err
	}
	var contextDigest string
	var contextRevision, stateEpoch uint64
	if committedContext != nil {
		contextDigest = committedContext.Digest
		contextRevision = committedContext.Revision
		stateEpoch = committedContext.Epoch
	}
	if err := r.runtime.EventService.publish(
		operationID,
		checkpoint.ThreadID,
		checkpoint.TurnID,
		itemID,
		&protocol.CheckpointForkedData{
			CheckpointID: checkpoint.ID,
			NewThreadID:  newThreadID,
			Title:        title,
			SourceCursor: checkpoint.Cursor,
			ReplacementHistory: append(
				[]protocol.CompactedMessage(nil),
				history...,
			),
			ExactContext: contextSnapshot != nil,
			WorkspaceClaimsValid: contextSnapshot != nil &&
				reconciliation.Stale == 0,
			InvalidatedClaims: reconciliation.Invalidated,
			StaleClaims:       reconciliation.Stale,
			ContextCommitID:   currentCommitID,
			ContextDigest:     contextDigest,
			ContextRevision:   contextRevision,
			StateEpoch:        stateEpoch,
		},
	); err != nil {
		if currentCommitID != "" {
			store := r.runtime.contextRebaseStore.(CurrentContextStore)
			err = errors.Join(
				err,
				store.DeleteCurrentContext(
					ctx,
					newThreadID,
					currentCommitID,
					true,
				),
			)
		}
		manager.Release(newThreadID)
		return protocol.CheckpointForkResult{}, err
	}
	if _, err := r.runtime.sessionLifecycle.ActivateThread(
		ctx,
		sessionID,
		newThreadID,
	); err != nil {
		return protocol.CheckpointForkResult{}, err
	}
	return protocol.CheckpointForkResult{
		Version:      protocol.CheckpointProtocolVersion,
		Checkpoint:   checkpoint,
		SessionID:    sessionID,
		ThreadID:     newThreadID,
		ParentID:     checkpoint.ThreadID,
		ExactContext: contextSnapshot != nil,
		WorkspaceClaimsValid: contextSnapshot != nil &&
			reconciliation.Stale == 0,
		InvalidatedClaims: reconciliation.Invalidated,
		StaleClaims:       reconciliation.Stale,
	}, nil
}

func (r *ArtifactService) checkpointState(
	ctx context.Context,
	sessionID, checkpointID, action string,
) (
	protocol.SessionSummary,
	protocol.SessionCheckpoint,
	[]protocol.CompactedMessage,
	*agentcontext.ContextSnapshot,
	error,
) {
	if r.runtime.sessionArtifacts == nil {
		return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil,
			runtimeProblem(protocol.CodeUnavailable, "Session Checkpoints are unavailable", nil)
	}
	current, err := r.runtime.SessionStatus(ctx, sessionID)
	if err != nil {
		return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil, err
	}
	if err := ensureSessionQuiescent(current, action); err != nil {
		return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil, err
	}
	checkpoint, history, checkpointProfile, err :=
		r.runtime.sessionArtifacts.GetCheckpoint(ctx, checkpointID)
	if err != nil {
		return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil, err
	}
	if checkpoint.SessionID != sessionID {
		return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil,
			resourceProblem(
				protocol.CodeInvalidArgument,
				"Checkpoint does not belong to the Session",
				false,
				protocol.ProblemReasonWrongSession,
				checkpointID,
			)
	}
	if err := r.requireRetainedSource(ctx, checkpoint.ThreadID, checkpoint.TurnID); err != nil {
		return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil, err
	}
	currentProfile, err := r.runtime.SessionProfile(ctx, sessionID)
	if err != nil {
		return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil, err
	}
	if currentProfile.Profile.Revision != checkpoint.ProfileRevision ||
		checkpointProfile.Revision != checkpoint.ProfileRevision {
		return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil,
			revisionProblem(
				"Checkpoint Profile Revision is stale",
				checkpointID,
				checkpoint.ProfileRevision,
				currentProfile.Profile.Revision,
			)
	}
	var contextSnapshot *agentcontext.ContextSnapshot
	if checkpoint.ContextDigest != "" {
		store, ok := r.runtime.sessionArtifacts.(ContextSessionArtifactStore)
		if !ok {
			return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil,
				errors.New("context checkpoint store is unavailable")
		}
		contextCheckpoint, snapshot, storedProfile, contextErr :=
			store.GetContextCheckpoint(ctx, checkpointID)
		if contextErr != nil {
			return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil,
				contextErr
		}
		if contextCheckpoint.ID != checkpoint.ID ||
			storedProfile.Revision != checkpointProfile.Revision {
			return protocol.SessionSummary{}, protocol.SessionCheckpoint{}, nil, nil,
				errors.New("context checkpoint lookup is inconsistent")
		}
		contextSnapshot = &snapshot
	}
	return current, checkpoint, history, contextSnapshot, nil
}

func (r *ArtifactService) persistTerminalCheckpoint(
	ctx context.Context,
	event protocol.Event,
	status protocol.CheckpointStatus,
	summary string,
) {
	manager, ok := r.runtime.engine.(CheckpointEngine)
	if !ok {
		return
	}
	history, err := manager.History(event.ThreadID)
	if err != nil || len(history) == 0 {
		if err != nil {
			r.LogArtifactError("read Checkpoint history", event, err)
		}
		return
	}
	encoded, err := agentcontext.EncodeCompactedHistory(history)
	if err != nil {
		r.LogArtifactError("encode Checkpoint history", event, err)
		return
	}
	sessionID, err := r.runtime.sessionLifecycle.SessionForThread(ctx, event.ThreadID)
	if err != nil {
		r.LogArtifactError("resolve Checkpoint Session", event, err)
		return
	}
	profile, err := r.runtime.profiles.Profile(ctx, sessionID, r.runtime.defaultProfile)
	if err != nil {
		r.LogArtifactError("read Checkpoint Profile", event, err)
		return
	}
	changed, external, note, parentCheckpointID, receipt := r.checkpointEffects(
		ctx,
		event.ThreadID,
		event.TurnID,
	)
	summary = boundedArtifactText(strings.TrimSpace(summary), 2048)
	if summary == "" {
		summary = fmt.Sprintf("%s Turn %s", status, event.TurnID)
	}
	checkpoint := protocol.SessionCheckpoint{
		Version: protocol.CheckpointProtocolVersion,
		ID: stableArtifactID(
			"checkpoint",
			sessionID,
			string(event.ThreadID),
			string(event.TurnID),
			fmt.Sprint(event.Sequence),
		),
		SessionID:           sessionID,
		ThreadID:            event.ThreadID,
		TurnID:              event.TurnID,
		Cursor:              event.Sequence,
		Status:              status,
		Summary:             summary,
		ProfileRevision:     profile.Revision,
		ParentCheckpointID:  parentCheckpointID,
		ChangeReceipt:       receipt,
		ChangedFiles:        changed,
		ExternalSideEffects: external,
		SideEffectNote:      note,
		CanRestore:          true,
		CanFork:             true,
		CreatedAt:           event.CreatedAt,
	}
	var saved protocol.SessionCheckpoint
	if contextManager, ok := manager.(ContextCheckpointEngine); ok {
		contextSnapshot, snapshotErr := contextManager.ContextSnapshot(
			event.ThreadID,
		)
		if snapshotErr != nil {
			r.LogArtifactError("snapshot Checkpoint context", event, snapshotErr)
			return
		}
		store, supported := r.runtime.sessionArtifacts.(ContextSessionArtifactStore)
		if !supported {
			r.LogArtifactError(
				"save Checkpoint context",
				event,
				errors.New("context checkpoint store is unavailable"),
			)
			return
		}
		checkpoint.StateEpoch = contextSnapshot.Epoch
		checkpoint.ContextDigest = contextSnapshot.Digest
		checkpoint.WorkspaceDigest = contextSnapshot.Workspace.SparseDigest
		saved, err = store.SaveContextCheckpoint(
			ctx,
			checkpoint,
			encoded,
			contextSnapshot,
			profile,
		)
	} else {
		saved, err = r.runtime.sessionArtifacts.SaveCheckpoint(
			ctx,
			checkpoint,
			encoded,
			profile,
		)
	}
	if err != nil {
		r.LogArtifactError("save Session Checkpoint", event, err)
		return
	}
	if err := r.runtime.EventService.publish(
		event.OperationID,
		event.ThreadID,
		event.TurnID,
		protocol.ItemID(stableArtifactID(
			"item",
			saved.ID,
			"created",
		)),
		&protocol.CheckpointCreatedData{Checkpoint: saved},
	); err != nil {
		r.LogArtifactError("publish Session Checkpoint", event, err)
	}
}

func (r *ArtifactService) checkpointEffects(
	ctx context.Context,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
) (int, bool, string, string, *protocol.ReceiptReference) {
	events, err := r.replayArtifactTurn(ctx, turnID)
	if err != nil {
		return 0, true, "Side-effect receipt could not be read", "", nil
	}
	forks, err := r.replayArtifactKind(ctx, protocol.EventCheckpointForked)
	if err != nil {
		return 0, true, "Side-effect receipt could not be read", "", nil
	}
	changed := make(map[string]struct{})
	external := false
	parentCheckpointID := ""
	var reference *protocol.ReceiptReference
	for _, event := range forks {
		if fork, ok := event.Data.(*protocol.CheckpointForkedData); ok &&
			fork.NewThreadID == threadID {
			parentCheckpointID = fork.CheckpointID
		}
	}
	for _, event := range events {
		if event.TurnID != turnID {
			continue
		}
		receipt, ok := event.Data.(*protocol.ExecutionReceiptData)
		if !ok || receipt == nil {
			continue
		}
		reference = &protocol.ReceiptReference{
			EventID: event.ID, TurnID: event.TurnID, Cursor: event.Sequence,
		}
		for _, change := range receipt.Changes {
			changed[change.Path] = struct{}{}
		}
		external = external || len(receipt.ToolsSucceeded) > 0
	}
	note := ""
	if external {
		note = "Completed Tool effects remain applied and are never replayed by Restore"
	}
	return len(changed), external, note, parentCheckpointID, reference
}

func isRetainedDraftMessage(message string) bool {
	return strings.Contains(message, workspacejournal.ErrRetainedDraft.Error())
}
