package app

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/persist/history"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (r *Runtime) HistoryEventReader() history.BackwardReader {
	reader, _ := r.events.(history.BackwardReader)
	return reader
}

func (r *Runtime) HistoryWorkspaceRoot() string {
	if r == nil {
		return ""
	}
	return r.workspaceRoot
}

func (r *Runtime) HistoryThreadIDs(
	ctx context.Context,
	sessionID string,
) ([]protocol.ThreadID, error) {
	return r.sessionLifecycle.ThreadIDs(ctx, sessionID)
}

func (r *Runtime) HistoryReadFence(
	ctx context.Context,
	sessionID string,
) (protocol.SessionReadFence, error) {
	store, ok := r.sessionLifecycle.(sessionPresentationStore)
	if !ok {
		return protocol.SessionReadFence{}, runtimeProblem(
			protocol.CodeUnavailable,
			"transactional session presentation is unavailable",
			nil,
		)
	}
	fence, err := store.PresentationReadFence(ctx, sessionID)
	if err != nil {
		return fence, err
	}
	if fence.Session.LatestTurnID != "" {
		fence.Session.LatestTurnWithdrawn, err = r.TurnWithdrawn(ctx, fence.Session.ThreadID, fence.Session.LatestTurnID)
		if fence.Session.LatestTurnWithdrawn {
			fence.Session.Status = protocol.SessionStatusIdle
		}
	}
	return fence, err
}
