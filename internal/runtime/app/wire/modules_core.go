package wire

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/fwtllh-png/QCode/internal/adapter/lsp"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/builtin"
	webtool "github.com/fwtllh-png/QCode/internal/adapter/tool/web"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/platform/symbols"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/goproxy"
)

type configModule struct{}

func (configModule) Name() string { return "config" }

func (configModule) Build(_ context.Context, state *buildState) error {
	snapshot, err := config.Load(config.LoadOptions{
		Path: state.options.ConfigPath, Overrides: state.options.ConfigOverrides,
	})
	if err != nil {
		return fmt.Errorf("load execution config: %w", err)
	}
	execution := snapshot.Config.Execution
	if !execution.Tools &&
		(state.options.RepositoryRulesPath != "" ||
			state.options.MCPConfigPath != "") {
		return errors.New(
			"repository policy and MCP require tools to be enabled",
		)
	}
	skillPaths, err := resolveRuntimeSkillPaths(state, execution.Workspace)
	if err != nil {
		return err
	}
	workspaceStateRoot := ""
	workspaceStateID := ""
	if state.options.PersistentStore != nil {
		workspaceStateRoot, workspaceStateID, err = externalWorkspaceStateRoot(
			state.options.PersistentStore.Root(),
			execution.Workspace,
			state.options.WorkspaceIdentity,
		)
		if err != nil {
			return err
		}
	}
	commands := configuredDiagnosticCommands(snapshot.Config.Diagnostics.Commands)
	state.config = configBuildState{
		snapshot:            snapshot,
		execution:           execution,
		skillPaths:          skillPaths,
		runtimeSessionID:    fmt.Sprintf("process-%d-%p", os.Getpid(), state.session),
		workspaceStateID:    workspaceStateID,
		workspaceStateRoot:  workspaceStateRoot,
		diagnosticCommands:  commands,
		diagnosticReadRoots: diagnosticCommandReadRoots(commands),
		diagnosticReadFiles: diagnosticCommandReadFiles(commands),
	}
	state.session.configuration.snapshot = config.CloneSnapshot(snapshot)
	return nil
}

type platformModule struct{}

func (platformModule) Name() string { return "platform" }

func (platformModule) Build(_ context.Context, state *buildState) error {
	session := state.session
	execution := state.config.execution
	processes := process.NewSessionManager(0)
	if err := configureProcessState(state, processes); err != nil {
		return err
	}
	if state.persistence.jobLogs != nil {
		processes.SetArchive(state.persistence.jobLogs)
	}
	session.processes = processes
	state.platform.processes = processes
	state.platform.processEgress = &egress.Gate{Enforce: true}
	state.platform.leaseAuthority = newLeaseAuthority()
	backend, prepareFacts, err := newWorkspaceSandbox(state)
	if err != nil {
		return fmt.Errorf("create sandbox: %w", err)
	}
	session.sandbox = backend
	state.platform.backend = backend
	moduleProxy, authBindReport, err := bindAuthServices(state)
	if err != nil {
		return fmt.Errorf("bind auth services: %w", err)
	}
	state.platform.moduleProxy = moduleProxy
	// Preparer facts (toolchain requirements, missing go, direct fallback)
	// are binding-time facts: they surface on failed process results through
	// the same report channel the auth service uses, instead of being
	// dropped at binding time.
	if authBindReport == nil {
		authBindReport = &goproxy.BindReport{}
	}
	for _, fact := range prepareFacts {
		authBindReport.Record(fact)
	}
	state.platform.authBindReport = authBindReport
	session.workspaceQuery, err = builtin.NewWorkspaceQuery(execution.Workspace, backend, state.platform.leaseAuthority, execution.LeaseTimeout)
	if err != nil {
		return fmt.Errorf("create workspace query: %w", err)
	}
	index, status := openRepositoryIndex(
		execution.Workspace,
		backend,
		state.persistence.runtimeStore,
		state.config.snapshot.Config.Context.Index,
	)
	session.repositoryIndex = index
	session.metrics.SetRepositoryIndexState(status)
	state.platform.backend = backend
	state.platform.repositoryIndex = index
	if !execution.Tools {
		return nil
	}
	webOptions := webtool.OptionsFromEnv()
	if search := state.config.snapshot.Config.Web.SearchBackend; search != "" {
		webOptions.SearchBackend = search
	}
	state.platform.webEgress = &egress.Gate{Enforce: true, UseCallScope: true}
	grantWebBackendHosts(state.platform.webEgress, webOptions)
	webOptions.HTTP = egress.WrapClient(&http.Client{}, state.platform.webEgress)
	state.platform.web = webOptions
	return nil
}

type persistenceModule struct{}

func (persistenceModule) Name() string { return "persistence" }

func (persistenceModule) Build(
	ctx context.Context,
	state *buildState,
) error {
	content := contentstore.NewMemory(contentstore.Options{})
	state.session.content = content
	state.persistence.content = content
	openJobLog(state)
	store, ephemeral, cleanupDir, err := openRuntimeStateStore(
		ctx,
		state.options.PersistentStore,
		state.config.execution.Workspace,
		state.config.execution.Tools,
	)
	if err != nil {
		return fmt.Errorf("runtime state store: %w", err)
	}
	state.persistence.runtimeStore = store
	state.persistence.ephemeralState = ephemeral
	state.session.ephemeralState = ephemeral
	state.session.ephemeralStateDir = cleanupDir
	return nil
}

type builtinToolsModule struct{}

func (builtinToolsModule) Name() string { return "builtin-tools" }

func (builtinToolsModule) Build(
	_ context.Context,
	state *buildState,
) error {
	if !state.config.execution.Tools {
		state.tools.registry = tool.NewRegistry(
			nil,
			tool.NewResultStoreWithStore(32<<10, state.persistence.content),
		)
		return nil
	}
	// A resident language-server pool answers semantic queries without
	// restarting a server each time. It is a host process, so it exists only
	// where the configuration explicitly enables it; the session otherwise
	// keeps the one-shot checker, byte for byte.
	var semantic symbols.Provider
	if lspSettings := state.config.snapshot.Config.Context.LSP; lspSettings.ResidentEnabled {
		pool := lsp.NewResident(lsp.Checker{
			Root:    state.config.execution.Workspace,
			Sandbox: state.platform.backend,
		}, lsp.ResidentOptions{
			IdleTimeout:   lspSettings.IdleTimeout,
			MaxServers:    lspSettings.MaxServers,
			CacheCapacity: lspSettings.CacheCapacity,
		})
		if err := state.session.RegisterResource(
			"lsp-resident-sessions",
			func(context.Context) error { return pool.Close() },
		); err != nil {
			return fmt.Errorf("register language-server sessions: %w", err)
		}
		semantic = pool
	}
	registry, handles, err := builtin.NewWithAuthority(
		state.config.execution.Workspace,
		state.platform.backend,
		state.session.content,
		state.session.processes,
		state.platform.repositoryIndex,
		semantic,
		state.platform.leaseAuthority, state.config.execution.LeaseTimeout,
		state.platform.web,
	)
	if err != nil {
		return fmt.Errorf("create tools: %w", err)
	}
	state.tools.registry = registry
	state.tools.handleStore = handles
	return nil
}
