package wire

import (
	"github.com/fwtllh-png/QCode/internal/orchestration/workspacemerge"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func buildChatWorkspaces(
	state *buildState,
	threads *app.ThreadManager,
	gate *agentengine.WorkspaceTurnGate,
) app.SessionWorkspaceManager {
	if gate == nil || state.orchestration.chatTrees == nil {
		return nil
	}
	allowApply := state.security.runtime != nil &&
		state.security.runtime.PermissionValue() != policy.PermissionNever
	merger := workspacemerge.New(
		state.config.execution.Workspace, state.orchestration.chatRoot,
		state.orchestration.parentFiles, state.security.journal,
		gate, state.orchestration.workspaceBroker, allowApply,
	)
	if merger == nil {
		return nil
	}
	return app.NewIsolatedSessionWorkspaces(app.IsolatedSessionWorkspaceOptions{
		Root:  state.orchestration.chatRoot,
		Trees: state.orchestration.chatTrees, Tools: state.orchestration.childToolsets,
		Threads: threads, Merger: merger, AllowApply: allowApply,
	})
}
