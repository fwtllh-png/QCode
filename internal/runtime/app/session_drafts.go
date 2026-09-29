package app

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type workspaceDraftReclaimer interface {
	DraftTurnIDs() []string
	RevertWorkspaceDraft(context.Context, string) error
}

func (r *SessionService) reclaimSessionWorkspaceDrafts(
	ctx context.Context,
	threadIDs []protocol.ThreadID,
) error {
	reclaimer, ok := r.runtime.engine.(workspaceDraftReclaimer)
	if !ok || reclaimer == nil {
		return nil
	}
	draftIDs := reclaimer.DraftTurnIDs()
	if len(draftIDs) == 0 {
		return nil
	}
	owned, err := r.turnIDsOwnedByThreads(ctx, threadIDs)
	if err != nil {
		return err
	}
	for _, turnID := range draftIDs {
		if _, ok := owned[turnID]; !ok {
			continue
		}
		if err := reclaimer.RevertWorkspaceDraft(ctx, turnID); err != nil {
			return runtimeProblem(
				protocol.CodeConflict,
				"cannot delete session while its workspace draft cannot be reverted",
				err,
			)
		}
	}
	return nil
}

func (r *SessionService) turnIDsOwnedByThreads(
	ctx context.Context,
	threadIDs []protocol.ThreadID,
) (map[string]struct{}, error) {
	owned := make(map[string]struct{})
	if r.runtime.events == nil || len(threadIDs) == 0 {
		return owned, nil
	}
	threads := make(map[protocol.ThreadID]struct{}, len(threadIDs))
	for _, threadID := range threadIDs {
		threads[threadID] = struct{}{}
	}
	events, err := r.runtime.events.Replay(ctx, 0)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if event.TurnID == "" {
			continue
		}
		if _, ok := threads[event.ThreadID]; ok {
			owned[string(event.TurnID)] = struct{}{}
		}
	}
	return owned, nil
}
