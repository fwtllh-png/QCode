package app

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/persist/artifact"
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
	result := make([]protocol.SessionSummary, 0, len(summaries))
	for _, summary := range summaries {
		for _, thread := range threads[summary.SessionID] {
			byThread[thread] = summary.SessionID
		}
		summary.LatestTurnWithdrawn = withdrawn[summary.ThreadID]
		if summary.LatestTurnWithdrawn {
			summary.Status = protocol.SessionStatusIdle
		}
		if r.sessionArtifacts != nil {
			checkpoint := checkpoints[summary.SessionID]
			summary.CheckpointCount = checkpoint.Count
			summary.ChangedFiles = checkpoint.ChangedFiles
		}
		result = append(result, r.projectSessionLiveActivity(summary, threads[summary.SessionID]))
	}
	return result, byThread, nil
}

func (r *SessionService) sessionThreads(ctx context.Context, ids []string) (map[string][]protocol.ThreadID, error) {
	if reader, ok := r.sessionLifecycle.(sessionThreadBatchReader); ok {
		return reader.ThreadIDsForSessions(ctx, ids)
	}
	result := make(map[string][]protocol.ThreadID, len(ids))
	for _, id := range ids {
		threads, err := r.sessionLifecycle.ThreadIDs(ctx, id)
		if err != nil {
			return nil, err
		}
		result[id] = threads
	}
	return result, nil
}

func (r *SessionService) sessionCheckpointSummaries(ctx context.Context, ids []string) (map[string]artifact.SessionCheckpointSummary, error) {
	if reader, ok := r.sessionArtifacts.(artifact.SessionCheckpointSummaryStore); ok {
		return reader.CheckpointSummaries(ctx, ids)
	}
	result := make(map[string]artifact.SessionCheckpointSummary, len(ids))
	if r.sessionArtifacts == nil {
		return result, nil
	}
	for _, id := range ids {
		count, err := r.sessionArtifacts.CountCheckpoints(ctx, id)
		if err != nil {
			return nil, err
		}
		summary := artifact.SessionCheckpointSummary{Count: count}
		if count > 0 {
			checkpoints, err := r.sessionArtifacts.ListCheckpoints(ctx, id, 1)
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
	if reader, ok := r.contextRebaseStore.(turnWithdrawalBatchReader); ok {
		return reader.TurnsWithdrawn(ctx, turns)
	}
	result := make(map[protocol.ThreadID]bool, len(turns))
	for thread, turn := range turns {
		withdrawn, err := r.TurnWithdrawn(ctx, thread, turn)
		if err != nil {
			return nil, err
		}
		result[thread] = withdrawn
	}
	return result, nil
}
