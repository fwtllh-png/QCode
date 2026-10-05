package app

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/persist/snapshot"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type sessionThreadBatchReader interface {
	ThreadIDsForSessions(context.Context, []string) (map[string][]protocol.ThreadID, error)
}

type turnWithdrawalBatchReader interface {
	TurnsWithdrawn(context.Context, map[protocol.ThreadID]protocol.TurnID) (map[protocol.ThreadID]bool, error)
}

func (r *SessionService) projectSessionActivities(
	ctx context.Context, summaries []protocol.SessionSummary,
) ([]protocol.SessionSummary, map[protocol.ThreadID]string, error) {
	ids := make([]string, len(summaries))
	turns := make(map[protocol.ThreadID]protocol.TurnID, len(summaries))
	for i, summary := range summaries {
		ids[i] = summary.SessionID
		if summary.LatestTurnID != "" {
			turns[summary.ThreadID] = summary.LatestTurnID
		}
	}
	threads, err := r.sessionThreads(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	checkpoints, err := r.sessionCheckpointSummaries(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	withdrawn, err := r.sessionTurnWithdrawals(ctx, turns)
	if err != nil {
		return nil, nil, err
	}
	byThread := make(map[protocol.ThreadID]string, len(summaries))
	for _, summary := range summaries {
		for _, thread := range threads[summary.SessionID] {
			byThread[thread] = summary.SessionID
		}
	}
	live := r.sessionLiveActivities(ids, byThread)
	result := make([]protocol.SessionSummary, 0, len(summaries))
	for _, summary := range summaries {
		summary.LatestTurnWithdrawn = withdrawn[summary.ThreadID]
		if summary.LatestTurnWithdrawn {
			summary.Status = protocol.SessionStatusIdle
		}
		if r.runtime.sessionArtifacts != nil {
			checkpoint := checkpoints[summary.SessionID]
			summary.CheckpointCount = checkpoint.Count
			summary.ChangedFiles = checkpoint.ChangedFiles
		}
		result = append(result, live[summary.SessionID].apply(summary))
	}
	return result, byThread, nil
}

// sessionState adds only execution preconditions to an ownership-checked read.
// Checkpoint counts and sidebar presentation are not needed for this path.
func (r *SessionService) sessionState(ctx context.Context, sessionID string) (protocol.SessionSummary, error) {
	summary, err := r.session(ctx, sessionID)
	if err != nil {
		return protocol.SessionSummary{}, err
	}
	threads, err := r.sessionThreads(ctx, []string{sessionID})
	if err != nil {
		return protocol.SessionSummary{}, err
	}
	if summary.LatestTurnID != "" {
		withdrawn, err := r.sessionTurnWithdrawals(ctx, map[protocol.ThreadID]protocol.TurnID{summary.ThreadID: summary.LatestTurnID})
		if err != nil {
			return protocol.SessionSummary{}, err
		}
		summary.LatestTurnWithdrawn = withdrawn[summary.ThreadID]
		if summary.LatestTurnWithdrawn {
			summary.Status = protocol.SessionStatusIdle
		}
	}
	byThread := make(map[protocol.ThreadID]string, len(threads[sessionID]))
	for _, thread := range threads[sessionID] {
		byThread[thread] = sessionID
	}
	return r.sessionLiveActivities([]string{sessionID}, byThread)[sessionID].apply(summary), nil
}

type sessionLiveActivity struct {
	active bool
	pendingInteractionCounts
}

func (a sessionLiveActivity) apply(summary protocol.SessionSummary) protocol.SessionSummary {
	summary.PendingApprovals = a.approvals
	summary.PendingInputs = a.inputs
	switch {
	case a.approvals > 0:
		summary.Status = protocol.SessionStatusAwaitingApproval
	case a.inputs > 0:
		summary.Status = protocol.SessionStatusAwaitingInput
	case a.active:
		summary.Status = protocol.SessionStatusRunning
	}
	return summary
}

// Each owner is read once with its own lock. These IDs/counts are a local
// presentation snapshot, not a global transaction or an admission lock.
func (r *SessionService) sessionLiveActivities(sessionIDs []string, byThread map[protocol.ThreadID]string) map[string]sessionLiveActivity {
	sessions := make(map[string]struct{}, len(sessionIDs))
	for _, session := range sessionIDs {
		sessions[session] = struct{}{}
	}
	threads := make(map[protocol.ThreadID]struct{}, len(byThread))
	for thread := range byThread {
		threads[thread] = struct{}{}
	}
	result := make(map[string]sessionLiveActivity)
	for session := range r.runtime.OperationService.pendingSessions(sessions) {
		result[session] = sessionLiveActivity{active: true}
	}
	for _, thread := range r.runtime.active.activeThreads(threads) {
		if session, ok := byThread[thread]; ok {
			value := result[session]
			value.active = true
			result[session] = value
		}
	}
	for thread, counts := range r.runtime.EventService.pendingCountsByThread(threads) {
		if session, ok := byThread[thread]; ok {
			value := result[session]
			value.approvals += counts.approvals
			value.inputs += counts.inputs
			result[session] = value
		}
	}
	return result
}

func (r *SessionService) sessionThreads(ctx context.Context, ids []string) (map[string][]protocol.ThreadID, error) {
	if reader, ok := r.runtime.sessionLifecycle.(sessionThreadBatchReader); ok {
		return reader.ThreadIDsForSessions(ctx, ids)
	}
	result := make(map[string][]protocol.ThreadID, len(ids))
	for _, id := range ids {
		threads, err := r.runtime.sessionLifecycle.ThreadIDs(ctx, id)
		if err != nil {
			return nil, err
		}
		result[id] = threads
	}
	return result, nil
}

func (r *SessionService) sessionCheckpointSummaries(ctx context.Context, ids []string) (map[string]snapshot.CheckpointSummary, error) {
	if reader, ok := r.runtime.sessionArtifacts.(SessionCheckpointSummaryStore); ok {
		return reader.CheckpointSummaries(ctx, ids)
	}
	result := make(map[string]snapshot.CheckpointSummary, len(ids))
	if r.runtime.sessionArtifacts == nil {
		return result, nil
	}
	for _, id := range ids {
		count, err := r.runtime.sessionArtifacts.CountCheckpoints(ctx, id)
		if err != nil {
			return nil, err
		}
		summary := snapshot.CheckpointSummary{Count: count}
		if count > 0 {
			checkpoints, err := r.runtime.sessionArtifacts.ListCheckpoints(ctx, id, 1)
			if err != nil {
				return nil, err
			}
			if len(checkpoints) == 1 {
				summary.ChangedFiles = checkpoints[0].ChangedFiles
			}
		}
		result[id] = summary
	}
	return result, nil
}

func (r *SessionService) sessionTurnWithdrawals(ctx context.Context, turns map[protocol.ThreadID]protocol.TurnID) (map[protocol.ThreadID]bool, error) {
	if reader, ok := r.runtime.contextRebaseStore.(turnWithdrawalBatchReader); ok {
		return reader.TurnsWithdrawn(ctx, turns)
	}
	result := make(map[protocol.ThreadID]bool, len(turns))
	for thread, turn := range turns {
		withdrawn, err := r.runtime.TurnWithdrawn(ctx, thread, turn)
		if err != nil {
			return nil, err
		}
		result[thread] = withdrawn
	}
	return result, nil
}
