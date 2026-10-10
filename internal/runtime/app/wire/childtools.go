package wire

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/fwtllh-png/QCode/internal/adapter/skill"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agenttool "github.com/fwtllh-png/QCode/internal/adapter/tool/agent"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/builtin"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
	interacttool "github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	webtool "github.com/fwtllh-png/QCode/internal/adapter/tool/web"
	"github.com/fwtllh-png/QCode/internal/common/environment"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/persist/joblog"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// childToolset roots one child's registry, sandbox and journal at its worktree;
// reusing the parent's registry would redirect child writes into the parent.
type childToolset struct {
	resources        *ResourceStack
	preparationFacts []environment.Fact
	registry         *tool.Registry
	backend          sandbox.Backend
	processes        *process.SessionManager
	journal          *workspacejournal.Manager
	jobLogs          *joblog.Store
	inputHost        *interacttool.Host
	diagnostics      verify.DiagnosticRunner
	files            *filetool.Tools
	skillCatalog     *skill.Catalog
}

func (t *childToolset) close(ctx context.Context) error {
	if t == nil {
		return nil
	}
	return t.resources.Close(ctx)
}

func (t *childToolset) registerResourceClosers() error {
	// Read fields at close time so partial construction and normal shutdown
	// share this dependency order. The parent's content store is borrowed.
	resources := []closeResource{
		{name: "sandbox", close: func(context.Context) error {
			return sandbox.CloseBackend(t.backend)
		}},
		{name: "registry", close: func(context.Context) error {
			return t.registry.Close()
		}},
		{name: "workspace-journal", close: func(ctx context.Context) error {
			if t.journal == nil {
				return nil
			}
			return t.journal.Close(ctx)
		}},
		{name: "job-logs", close: func(context.Context) error {
			if t.jobLogs == nil {
				return nil
			}
			return t.jobLogs.Close()
		}},
		{name: "processes", close: func(context.Context) error {
			if t.processes == nil {
				return nil
			}
			return errors.Join(t.processes.CloseAllWithError(), t.processes.JournalError())
		}},
	}
	for _, resource := range resources {
		if err := t.resources.Add(resource.name, resource.close); err != nil {
			return err
		}
	}
	return nil
}

// childToolsets builds and owns one toolset per isolated child root.
type childToolsets struct {
	content             contentstore.Store
	web                 webtool.Options
	journals            config.Journal
	diagnosticCommands  map[string]verify.DiagnosticCommand
	diagnosticReadRoots []string
	diagnosticReadFiles []string
	gitCommonDir        string
	managedProxyPort    uint16
	// managedProxyCredential authenticates to managedProxyPort.
	managedProxyCredential string
	parentSandbox          sandbox.Backend
	workspaceStateRoot     string
	environment            config.ExecutionEnvironment
	skillPaths             SkillPaths
	agents                 *subagent.AgentControl
	agentSession           string
	agentRelease           func(string)
	interactionsBound      bool
	interactionVision      interacttool.VisionClient
	interactionPlan        func(interacttool.Plan) error

	mu    sync.Mutex
	built map[string]*childToolset
}

func (c *childToolsets) bindParentSandbox(backend sandbox.Backend) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.parentSandbox = backend
	c.mu.Unlock()
}

func (c *childToolsets) bindAgents(
	control *subagent.AgentControl,
	sessionID string,
	onRelease func(string),
) {
	c.mu.Lock()
	c.agents, c.agentSession, c.agentRelease = control, sessionID, onRelease
	c.mu.Unlock()
}

func (c *childToolsets) bindInteractions(
	vision interacttool.VisionClient,
	onPlan func(interacttool.Plan) error,
) {
	c.mu.Lock()
	c.interactionsBound, c.interactionVision = true, vision
	c.interactionPlan = onPlan
	c.mu.Unlock()
}

func newChildToolsets(
	content contentstore.Store, web webtool.Options,
	journals config.Journal,
	diagnosticCommands map[string]verify.DiagnosticCommand,
	diagnosticReadRoots []string,
	diagnosticReadFiles []string,
	gitCommonDir string, managedProxyPort uint16,
	workspaceStateRoot string,
	skillPaths SkillPaths,
) *childToolsets {
	return &childToolsets{
		content: content, web: web,
		journals: journals, diagnosticCommands: diagnosticCommands,
		diagnosticReadRoots: append([]string(nil), diagnosticReadRoots...),
		diagnosticReadFiles: append([]string(nil), diagnosticReadFiles...),
		gitCommonDir:        gitCommonDir,
		managedProxyPort:    managedProxyPort,
		workspaceStateRoot:  workspaceStateRoot,
		skillPaths:          skillPaths,
		built:               make(map[string]*childToolset),
	}
}

// open builds once per child root so follow-up turns retain the same journal.
func (c *childToolsets) open(
	root string,
	interactive bool,
) (result *childToolset, resultErr error) {
	c.mu.Lock()
	if existing, ok := c.built[root]; ok {
		if (existing.inputHost != nil) != interactive {
			c.mu.Unlock()
			return nil, errors.New("child toolset interaction mode changed")
		}
		c.mu.Unlock()
		return existing, nil
	}
	if interactive && !c.interactionsBound {
		c.mu.Unlock()
		return nil, errors.New("child interaction tools are not configured")
	}
	agents, agentSession, agentRelease := c.agents, c.agentSession, c.agentRelease
	vision, onPlan := c.interactionVision, c.interactionPlan
	parentSandbox := c.parentSandbox
	c.mu.Unlock()
	hostReadRoots := append([]string(nil), c.diagnosticReadRoots...)
	gitRoots, err := subagent.WorktreeGitReadRoots(root, c.gitCommonDir)
	if err != nil {
		return nil, fmt.Errorf("child Git metadata: %w", err)
	}
	hostReadRoots = append(hostReadRoots, gitRoots...)
	stateLayout, err := sandbox.PrepareChildStateLayout(
		c.workspaceStateRoot,
		root,
	)
	if err != nil {
		return nil, fmt.Errorf("child state layout: %w", err)
	}
	// Inherit only the parent's selected execution values and platform binding.
	// Recompile configured resource paths against this child's isolated home.
	sourceEnv := []string{}
	var inherited *sandbox.ToolchainExposure
	var stateRoots []string
	if policy, ok := sandbox.BackendPolicy(parentSandbox); ok {
		stateRoots = append(stateRoots, policy.RuntimeStateRoots...)
		sourceEnv = append(sourceEnv, policy.EnvironmentValues...)
		exposure := policy.Toolchains
		inherited = &exposure
	}
	options, preparationFacts, err := bindEnvironmentSandbox(sandbox.Options{
		WorkspaceRoot:          root,
		RuntimeStateRoots:      stateRoots,
		PrivateTemp:            stateLayout.SandboxHome,
		ManagedProxyPort:       c.managedProxyPort,
		ManagedProxyCredential: c.managedProxyCredential,
		HostReadRoots:          hostReadRoots,
		HostReadFiles:          c.diagnosticReadFiles,
		EnvironmentContract:    c.environment.Contract,
		EnvironmentProfile:     environment.ChildProfile(c.environment.Profile),
		SharedUserTemp:         false,
		Toolchains:             inherited,
		SkipPATHReadRoots:      inherited != nil,
	}, c.environment, "", stateLayout.SandboxHome, sourceEnv)
	if err != nil {
		return nil, fmt.Errorf("child environment: %w", err)
	}
	toolset := &childToolset{resources: NewResourceStack()}
	if err := toolset.registerResourceClosers(); err != nil {
		return nil, err
	}
	retained := false
	defer func() {
		if !retained {
			resultErr = errors.Join(resultErr, toolset.close(context.Background()))
			if resultErr != nil {
				result = nil
			}
		}
	}()
	backend, err := newPlatformBackend(options)
	toolset.backend = backend
	if err != nil {
		return nil, fmt.Errorf("child sandbox: %w", err)
	}
	if opener, ok := egress.LookupProcessSessionOpener(parentSandbox); ok {
		composed, composeErr := egress.NewSessionBackend(backend, opener)
		if composeErr != nil {
			return nil, fmt.Errorf("child sandbox: %w", composeErr)
		}
		backend = composed
		toolset.backend = composed
	}
	// Child process journals stay isolated from the parent and sibling roots.
	processes := process.NewSessionManager(0)
	toolset.processes = processes
	if stateLayout.Root != "" {
		processes.SetJournalPath(filepath.Join(stateLayout.Control, "jobs", "journal.jsonl"))
		if err := processes.LoadStaleJournal(); err != nil {
			return nil, fmt.Errorf("load child process journal: %w", err)
		}
		if archive, archiveErr := joblog.New(
			filepath.Join(stateLayout.Control, "jobs", "logs"),
		); archiveErr == nil {
			toolset.jobLogs = archive
			processes.SetArchive(archive)
		}
	}
	registry, handles, err := builtin.NewWithDependencies(
		root, backend, c.content, processes, c.web,
	)
	toolset.registry = registry
	if err != nil {
		return nil, fmt.Errorf("child tools: %w", err)
	}
	var inputHost *interacttool.Host
	if interactive {
		inputHost = interacttool.NewHost(0)
		registerErr := interacttool.Register(registry, interacttool.Options{
			Host: inputHost, Backend: backend,
			Vision: vision, OnPlan: onPlan, Workspace: root,
		})
		if registerErr != nil {
			return nil, fmt.Errorf("child interact tools: %w", registerErr)
		}
	}
	journal, err := c.openJournal(root, stateLayout)
	toolset.journal = journal
	if err != nil {
		return nil, err
	}
	if c.journals.Durable && c.journals.RecoverOnStart {
		if _, err := journal.Recover(context.Background()); err != nil {
			return nil, fmt.Errorf("recover interrupted child turns: %w", err)
		}
	}
	files, err := filetool.NewWithBackend(root, backend)
	if err != nil {
		return nil, fmt.Errorf("child integration files: %w", err)
	}
	if agents != nil {
		if err := agenttool.Register(registry, agenttool.Options{
			Control: agents, Handles: handles,
			Files: files, OnRelease: agentRelease,
			Sandbox: backend, Workspace: root, SessionID: agentSession,
		}); err != nil {
			return nil, fmt.Errorf("child agent tools: %w", err)
		}
	}
	toolset.preparationFacts, toolset.inputHost = preparationFacts, inputHost
	toolset.diagnostics = verify.NewDiagnosticCommandRunner(root, backend, c.diagnosticCommands)
	toolset.files = files
	// Keep the owner's enablement and lock policy, but discover only this
	// child's workspace and execution HOME alongside configured/user skills.
	policy, _ := sandbox.BackendPolicy(backend)
	var capabilities capabilityBuildState
	if err := (skillContributor{
		paths: c.skillPaths, workspace: root, sandboxHome: policy.PrivateTemp,
		output: &capabilities,
	}).Contribute(context.Background(), registry); err != nil {
		return nil, fmt.Errorf("child skills: %w", err)
	}
	toolset.skillCatalog = capabilities.skillCatalog
	c.mu.Lock()
	if existing := c.built[root]; existing != nil {
		c.mu.Unlock()
		if (existing.inputHost != nil) != interactive {
			return nil, errors.New("child toolset interaction mode changed")
		}
		return existing, nil
	}
	c.built[root] = toolset
	c.mu.Unlock()
	retained = true
	return toolset, nil
}

// openJournal preserves rollback evidence when a child dies mid-turn.
func (c *childToolsets) openJournal(
	root string,
	stateLayout sandbox.StateLayout,
) (*workspacejournal.Manager, error) {
	if !c.journals.Durable {
		journal, err := workspacejournal.New(root, c.content)
		if err != nil {
			return nil, fmt.Errorf("child journal: %w", err)
		}
		return journal, nil
	}
	if stateLayout.Root == "" {
		return nil, errors.New(
			"durable child journal requires an external Runtime state store",
		)
	}
	journal, err := workspacejournal.Open(
		root,
		filepath.Join(stateLayout.Control, "journal"),
		stateLayout.WorkspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("child journal: %w", err)
	}
	return journal, nil
}

// release drops the toolset for a root once its child is closed.
func (c *childToolsets) Release(root string) {
	c.mu.Lock()
	toolset := c.built[root]
	delete(c.built, root)
	c.mu.Unlock()
	// ToolPlanes.Release has no error result; construction and Session.Close
	// report cleanup failures without changing an already-settled child turn.
	_ = toolset.close(context.Background())
}

func (c *childToolsets) closeAll(ctx context.Context) error {
	c.mu.Lock()
	toolsets := c.built
	c.built = make(map[string]*childToolset)
	c.mu.Unlock()
	var closeErrors []error
	for root, toolset := range toolsets {
		if err := toolset.close(ctx); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close child toolset %q: %w", root, err))
		}
	}
	return errors.Join(closeErrors...)
}
