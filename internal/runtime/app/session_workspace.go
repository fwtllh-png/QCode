package app

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/orchestration/chatmerge"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const SessionIsolationWorktree = "worktree"

var ErrSessionWorkspaceClean = chatmerge.ErrWorkspaceClean

// SessionWorkspace binds one durable host session to an isolated execution
// root. Root is returned for diagnostics only; callers persist Mode and derive
// the trusted path again when loading the session.
type SessionWorkspace struct {
	Mode string
	Root string
}

// SessionWorkspaceManager owns the lifecycle and merge boundary of isolated
// host sessions. Implementations must register the thread engine before
// Provision or Restore returns.
type SessionWorkspaceManager interface {
	Provision(
		ctx context.Context,
		sessionID string,
		threadID protocol.ThreadID,
	) (SessionWorkspace, error)
	Restore(
		ctx context.Context,
		sessionID string,
		threadID protocol.ThreadID,
	) (SessionWorkspace, error)
	Discard(
		ctx context.Context,
		sessionID string,
		threadID protocol.ThreadID,
	) error
	PlanMerge(
		ctx context.Context,
		sessionID string,
		threadID protocol.ThreadID,
	) (tool.EditPlan, error)
	ApplyMerge(
		ctx context.Context,
		sessionID string,
		threadID protocol.ThreadID,
		planID string,
	) (tool.EditPlan, error)
}

func (r *SessionService) sessionProfileForRestore(
	ctx context.Context,
	sessionID string,
	threadID protocol.ThreadID,
) (protocol.SessionProfileSnapshot, error) {
	if r.runtime.sessionLifecycle == nil {
		return r.SessionProfile(ctx, sessionID)
	}
	current, err := r.SessionStatus(ctx, sessionID)
	if err != nil {
		return protocol.SessionProfileSnapshot{}, err
	}
	if current.ThreadID != threadID ||
		current.Isolation != SessionIsolationWorktree {
		return r.SessionProfile(ctx, sessionID)
	}
	if r.runtime.sessionWorkspaces == nil {
		return protocol.SessionProfileSnapshot{}, runtimeProblem(protocol.CodeUnavailable,
			"isolated Chat workspaces are unavailable", nil)
	}
	if _, err := r.runtime.sessionWorkspaces.Restore(ctx,
		current.SessionID, current.ThreadID); err != nil {
		return protocol.SessionProfileSnapshot{}, err
	}
	return r.SessionProfile(ctx, sessionID)
}
