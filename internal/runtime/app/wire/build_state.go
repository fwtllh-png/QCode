package wire

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/adapter/skill"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	handletool "github.com/fwtllh-png/QCode/internal/adapter/tool/handle"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/observability/diagnostics"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// buildState exists only while NewExec assembles a Session. Runtime components
// receive explicit dependencies and must never retain this construction state.
type buildState struct {
	options ExecOptions
	session *Session

	config        configBuildState
	provider      providerBuildState
	platform      platformBuildState
	persistence   persistenceBuildState
	tools         toolBuildState
	capabilities  capabilityBuildState
	security      securityBuildState
	orchestration orchestrationBuildState
	agent         agentBuildState
	runtime       runtimeBuildState
}

type configBuildState struct {
	snapshot                                               config.Snapshot
	execution                                              config.Execution
	skillPaths                                             SkillPaths
	runtimeSessionID, workspaceStateID, workspaceStateRoot string
	diagnosticCommands                                     map[string]diagnostics.Command
	diagnosticReadRoots                                    []string
	diagnosticReadFiles                                    []string
}

type toolBuildState struct {
	registry     *tool.Registry
	handleStore  *handletool.Store
	skillCatalog *skill.Catalog
}

type agentBuildState struct {
	defaultProfile      protocol.SessionProfile
	profileCapabilities protocol.SessionProfileCapabilities
	profileModels       map[string]protocol.ModelCapabilities
	threads             *app.ThreadManager
}

type runtimeBuildState struct {
	application *app.Runtime
}

type buildModule interface {
	Name() string
	// Contract declares the buildState domains this module writes and reads.
	// validateModuleContracts enforces the declarations before any Build runs.
	Contract() ModuleContract
	Build(context.Context, *buildState) error
}

func buildModules(
	ctx context.Context,
	state *buildState,
	modules ...buildModule,
) error {
	if err := validateModuleContracts(modules); err != nil {
		return err
	}
	for _, module := range modules {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := module.Build(ctx, state); err != nil {
			return &moduleBuildError{name: module.Name(), err: err}
		}
	}
	return nil
}

type moduleBuildError struct {
	name string
	err  error
}

func (e *moduleBuildError) Error() string {
	return "build wire module " + e.name + ": " + e.err.Error()
}

func (e *moduleBuildError) Unwrap() error { return e.err }
