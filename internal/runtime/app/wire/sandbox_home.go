package wire

import (
	"path/filepath"

	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func newWorkspaceSandbox(
	state *buildState,
	helperPath string,
) (sandbox.Backend, error) {
	privateHome := ""
	if state.config.workspaceStateRoot != "" {
		privateHome = filepath.Join(
			state.config.workspaceStateRoot,
			"sandbox-home",
		)
	}
	return egress.NewManagedBackend(state.platform.processEgress, sandbox.Options{
		WorkspaceRoot: state.config.execution.Workspace,
		HelperPath:    helperPath,
		PrivateTemp:   privateHome,
		HostReadRoots: state.config.diagnosticReadRoots,
		HostReadFiles: state.config.diagnosticReadFiles,
	}, newPlatformBackend)
}
