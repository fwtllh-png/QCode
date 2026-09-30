package wire

import (
	"context"
	"os"
	"path/filepath"

	"github.com/fwtllh-png/QCode/internal/adapter/envprep"
	gittool "github.com/fwtllh-png/QCode/internal/adapter/tool/git"
	"github.com/fwtllh-png/QCode/internal/common/environment"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func newWorkspaceSandbox(
	state *buildState,
) (sandbox.Backend, []environment.Fact, error) {
	privateHome := ""
	if state.config.workspaceStateRoot != "" {
		privateHome = filepath.Join(
			state.config.workspaceStateRoot,
			"sandbox-home",
		)
	} else {
		// Ephemeral runtimes still need a home before compiling cache requests.
		// Keep it outside the Darwin shared user temp, even when that is enabled.
		root, err := os.MkdirTemp("/tmp", "qcode-environment-")
		if err != nil {
			return nil, nil, err
		}
		state.session.environmentStateDir = root
		root, err = sandbox.CanonicalStateDirectory(root)
		if err != nil {
			return nil, nil, err
		}
		state.session.environmentStateDir = root
		privateHome = filepath.Join(root, "sandbox-home")
	}
	environmentConfig := state.config.execution.Environment
	options, prepareFacts, err := bindEnvironmentSandbox(
		sandbox.Options{
			WorkspaceRoot:       state.config.execution.Workspace,
			PrivateTemp:         privateHome,
			HostReadRoots:       append([]string(nil), state.config.diagnosticReadRoots...),
			HostReadFiles:       append([]string(nil), state.config.diagnosticReadFiles...),
			EnvironmentContract: environmentConfig.Contract,
			EnvironmentProfile:  environmentConfig.Profile,
			SharedUserTemp:      environmentConfig.SharedUserTemp,
		},
		environmentConfig,
		state.config.workspaceStateID,
		privateHome,
		nil,
	)
	if err != nil {
		return nil, nil, err
	}
	backend, err := egress.NewManagedBackend(
		state.platform.processEgress, options, newPlatformBackend,
	)
	if err != nil {
		return nil, nil, err
	}
	return backend, prepareFacts, nil
}

func bindEnvironmentSandbox(
	options sandbox.Options,
	environmentConfig config.ExecutionEnvironment,
	workspaceID, sandboxHome string,
	sourceEnv []string,
) (sandbox.Options, []environment.Fact, error) {
	options.PrivateTemp = sandboxHome
	options.EnvironmentContract = environmentConfig.Contract
	if sourceEnv == nil {
		sourceEnv = os.Environ()
	}
	declarations := environmentConfig.DeclaredRequests()
	if options.EnvironmentProfile == environment.ProfileNative {
		declarations = append(declarations, gittool.EnvironmentRequests(sourceEnv)...)
	}
	prepared, err := envprep.Prepare(context.Background(), envprep.Options{
		Sandbox:      options,
		Source:       environmentConfig.Source,
		WorkspaceID:  workspaceID,
		Declarations: declarations,
		SourceEnv:    sourceEnv,
	})
	if err != nil {
		return sandbox.Options{}, nil, err
	}
	return prepared.Sandbox, prepared.Facts, nil
}
