package wire

import (
	"os"
	"path/filepath"

	"github.com/fwtllh-png/QCode/internal/orchestration/execsettle"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func buildExecIsolator(
	state *buildState,
	gate *agentengine.WorkspaceTurnGate,
) execsettle.Isolator {
	if state == nil || gate == nil ||
		state.security.journal == nil ||
		state.orchestration.parentFiles == nil ||
		state.orchestration.chatTrees == nil {
		return nil
	}
	scratch := filepath.Join(childStateRoot(state), "exec-isolates")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil
	}
	allowApply := state.security.runtime != nil &&
		state.security.runtime.PermissionValue() != policy.PermissionNever
	return execsettle.New(execsettle.Options{
		Repository: state.config.execution.Workspace,
		Scratch:    scratch,
		Parent:     state.orchestration.parentFiles,
		Journal:    state.security.journal,
		Gate:       gate,
		Brokers:    state.orchestration.chatTrees.brokers,
		AllowApply: allowApply,
		NewBackend: newPlatformBackend,
	})
}
