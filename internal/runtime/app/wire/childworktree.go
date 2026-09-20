package wire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/skill"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agenttool "github.com/fwtllh-png/QCode/internal/adapter/tool/agent"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/builtin"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	interacttool "github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	webtool "github.com/fwtllh-png/QCode/internal/adapter/tool/web"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/observability/diagnostics"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/orchestration/chatmerge"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/persist/joblog"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// gitCommandTimeout bounds one caller-owned provisioning attempt.
const gitCommandTimeout = 2 * time.Minute

// childWorktrees gives read-only agents scratch roots and writing agents
// isolated Git worktrees that cannot mutate the parent workspace.
type childWorktrees struct {
	// repository is the host workspace, which must be inside a git work tree for
	// isolation to be possible at all.
	repository string
	root       string
	strategy   string
	backend    sandbox.Backend
	scratch    subagent.WorktreeProvider
	brokers    chatmerge.WorkspaceBroker
}

func newChildWorktrees(
	workspace, root, strategy string,
	backend sandbox.Backend,
	leases *toolguard.LeaseAuthority,
	leaseTTL time.Duration,
) (*childWorktrees, error) {
	brokers, err := builtin.NewWorkspaceBroker(workspace, leases, leaseTTL)
	if err != nil {
		return nil, err
	}
	trees := &childWorktrees{
		repository: workspace, root: root, strategy: strategy, backend: backend,
		scratch: subagent.NewScratchWorktrees(root), brokers: brokers,
	}
	if strategy != config.SubagentWorkspaceWorktree {
		return trees, nil
	}
	// Fail at startup when explicitly requested isolation is unavailable.
	if err := trees.checkRepository(context.Background()); err != nil {
		return nil, err
	}
	return trees, nil
}

func (c *childWorktrees) isolates(stance subagent.Stance) bool {
	switch c.strategy {
	case config.SubagentWorkspaceReadOnly:
		return false
	case config.SubagentWorkspaceWorktree:
		return stance != subagent.StanceReadOnly
	default:
		return stance != subagent.StanceReadOnly
	}
}

func (c *childWorktrees) Provision(
	agentID string, stance subagent.Stance,
) (subagent.Worktree, error) {
	if c.strategy == config.SubagentWorkspaceSerialized {
		return subagent.Worktree{
			ID: agentID, Path: c.repository, Serialized: true,
		}, nil
	}
	if !c.isolates(stance) {
		return c.scratch.Provision(agentID, stance)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitCommandTimeout)
	defer cancel()
	// Under the auto strategy this is the first moment a workspace's inability to
	// isolate matters, and refusing here means the agent is never created rather
	// than created and then unusable.
	if err := c.checkRepository(ctx); err != nil {
		return subagent.Worktree{}, protocol.NewProblem(
			protocol.CodeUnavailable,
			fmt.Sprintf(
				"child agents with stance %q need an isolated git worktree: %s. "+
					"Spawn an explore or review agent, or set execution.subagent.workspace = %q "+
					"to run children read-only.",
				stance, err, config.SubagentWorkspaceReadOnly,
			),
			false, nil,
		)
	}
	path := filepath.Join(c.root, "worktrees", agentID)
	// git refuses to add a worktree at an existing non-empty path, and a stale
	// directory from a previous run is exactly that.
	if err := os.RemoveAll(path); err != nil {
		return subagent.Worktree{}, err
	}
	baseRev, err := c.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return subagent.Worktree{}, protocol.NewProblem(
			protocol.CodeUnavailable,
			fmt.Sprintf("cannot record base revision for child agent %s: %s", agentID, err),
			false, nil,
		)
	}
	baseRev = strings.TrimSpace(baseRev)
	if err := c.addDetachedWorktree(ctx, path, baseRev); err != nil {
		return subagent.Worktree{}, protocol.NewProblem(
			protocol.CodeUnavailable,
			fmt.Sprintf("cannot create a git worktree for child agent %s: %s", agentID, err),
			false, nil,
		)
	}
	return subagent.Worktree{
		ID: agentID, Path: path, Isolated: true,
		BaseRev: strings.TrimSpace(baseRev),
	}, nil
}

func (c *childWorktrees) addDetachedWorktree(
	ctx context.Context,
	path string,
	revision string,
) error {
	firstErr := c.brokers.AddWorktree(ctx, c.repository, path, revision)
	if firstErr == nil {
		return nil
	}
	// Reconcile an orphaned exact-path registration, then retry once.
	if cleanupErr := c.brokers.RemoveWorktree(
		ctx, c.repository, path,
	); cleanupErr != nil {
		return firstErr
	}
	retryErr := c.brokers.AddWorktree(ctx, c.repository, path, revision)
	if retryErr != nil {
		return errors.Join(firstErr, fmt.Errorf("retry worktree add: %w", retryErr))
	}
	return nil
}

func (c *childWorktrees) Discard(worktree subagent.Worktree) error {
	if worktree.Serialized {
		return nil
	}
	if !worktree.Isolated {
		return c.scratch.Discard(worktree)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitCommandTimeout)
	defer cancel()
	// Discard intentionally removes the child's uncommitted changes.
	if err := c.brokers.RemoveWorktree(
		ctx, c.repository, worktree.Path,
	); err != nil {
		// Prune reconciles a partially removed registration and directory.
		if pruneErr := c.brokers.PruneWorktrees(
			ctx, c.repository,
		); pruneErr != nil {
			return errors.Join(err, pruneErr)
		}
		return os.RemoveAll(worktree.Path)
	}
	return nil
}

func (c *childWorktrees) checkRepository(ctx context.Context) error {
	out, err := c.git(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return fmt.Errorf(
			"execution.subagent.workspace = %q needs a git work tree at %s: %w",
			config.SubagentWorkspaceWorktree, c.repository, err,
		)
	}
	if strings.TrimSpace(out) != "true" {
		return fmt.Errorf(
			"execution.subagent.workspace = %q needs a git work tree at %s",
			config.SubagentWorkspaceWorktree, c.repository,
		)
	}
	if _, err := c.git(ctx, "rev-parse", "--verify", "HEAD"); err != nil {
		return fmt.Errorf(
			"execution.subagent.workspace = %q needs at least one commit at %s: %w",
			config.SubagentWorkspaceWorktree, c.repository, err,
		)
	}
	return nil
}

func (c *childWorktrees) git(ctx context.Context, arguments ...string) (string, error) {
	return c.brokers.ReadVCS(ctx, c.repository, arguments...)
}

func (c *childWorktrees) commonGitDir(ctx context.Context) (string, error) {
	value, err := c.git(ctx, "rev-parse", "--git-common-dir")
	if err != nil {
		if _, markerErr := os.Lstat(filepath.Join(c.repository, ".git")); errors.Is(markerErr, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	path := strings.TrimSpace(value)
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.repository, path)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("Git common directory is not a directory")
	}
	return filepath.Clean(canonical), nil
}

func worktreeGitReadRoots(root, expectedCommonDir string) ([]string, error) {
	if expectedCommonDir == "" {
		return nil, nil
	}
	common, err := filepath.EvalSymlinks(expectedCommonDir)
	if err != nil {
		return nil, err
	}
	gitFile, err := os.ReadFile(filepath.Join(root, ".git"))
	if err != nil {
		return nil, err
	}
	const prefix = "gitdir: "
	value := strings.TrimSpace(string(gitFile))
	if !strings.HasPrefix(value, prefix) {
		return nil, errors.New("worktree .git file has no gitdir")
	}
	gitDirPath := strings.TrimSpace(strings.TrimPrefix(value, prefix))
	if !filepath.IsAbs(gitDirPath) {
		gitDirPath = filepath.Join(root, gitDirPath)
	}
	gitDir, err := filepath.EvalSymlinks(gitDirPath)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(common, gitDir)
	if err != nil || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errors.New("worktree gitdir escapes the repository Git directory")
	}
	commonRef, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return nil, err
	}
	resolvedCommon := strings.TrimSpace(string(commonRef))
	if !filepath.IsAbs(resolvedCommon) {
		resolvedCommon = filepath.Join(gitDir, resolvedCommon)
	}
	resolvedCommon, err = filepath.EvalSymlinks(resolvedCommon)
	if err != nil {
		return nil, err
	}
	if filepath.Clean(resolvedCommon) != filepath.Clean(common) {
		return nil, errors.New("worktree commondir does not match the repository")
	}
	candidates := []string{
		gitDir,
		filepath.Join(common, "objects"),
		filepath.Join(common, "refs"), filepath.Join(common, "info"),
		filepath.Join(common, "packed-refs"),
		filepath.Join(common, "HEAD"),
		filepath.Join(common, "shallow"),
		filepath.Join(common, "config"),
		filepath.Join(common, "config.worktree"),
	}
	roots := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if _, err := os.Lstat(candidate); err == nil {
			roots = append(roots, candidate)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return roots, nil
}

// childToolset roots one child's registry, sandbox and journal at its worktree;
// reusing the parent's registry would redirect child writes into the parent.
type childToolset struct {
	registry     *tool.Registry
	backend      sandbox.Backend
	processes    *process.SessionManager
	journal      *workspacejournal.Manager
	jobLogs      *joblog.Store
	inputHost    *interacttool.Host
	diagnostics  diagnostics.Runner
	verify       verify.Runner
	files        *filetool.Tools
	skillCatalog *skill.Catalog
}

func (t *childToolset) close() {
	if t == nil {
		return
	}
	if t.processes != nil {
		t.processes.CloseAll()
	}
	if t.jobLogs != nil {
		_ = t.jobLogs.Close()
	}
	if t.journal != nil {
		_ = t.journal.Close(context.Background())
	}
	_ = errors.Join(t.registry.Close(), sandbox.CloseBackend(t.backend))
}

// childToolsets builds and owns one toolset per isolated child root.
type childToolsets struct {
	helperPath          string
	content             contentstore.Store
	web                 webtool.Options
	verify              config.Verify
	journals            config.Journal
	diagnosticCommands  map[string]diagnostics.Command
	diagnosticReadRoots []string
	diagnosticReadFiles []string
	gitCommonDir        string
	managedProxyPort    uint16
	workspaceStateRoot  string
	skillPaths          SkillPaths
	agents              *subagent.AgentControl
	agentSession        string
	agentRelease        func(string)
	interactionsBound   bool
	interactionVision   interacttool.VisionClient
	interactionPlan     func(interacttool.Plan) error

	mu    sync.Mutex
	built map[string]*childToolset
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
	helperPath string, content contentstore.Store, web webtool.Options,
	verifyConfig config.Verify, journals config.Journal,
	diagnosticCommands map[string]diagnostics.Command,
	diagnosticReadRoots []string,
	diagnosticReadFiles []string,
	gitCommonDir string, managedProxyPort uint16,
	workspaceStateRoot string,
	skillPaths SkillPaths,
) *childToolsets {
	return &childToolsets{
		helperPath: helperPath, content: content, web: web, verify: verifyConfig,
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
) (*childToolset, error) {
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
	c.mu.Unlock()
	hostReadRoots := append([]string(nil), c.diagnosticReadRoots...)
	gitRoots, err := worktreeGitReadRoots(root, c.gitCommonDir)
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
	backend, err := newPlatformBackend(sandbox.Options{
		WorkspaceRoot: root, HelperPath: c.helperPath,
		PrivateTemp:      stateLayout.SandboxHome,
		ManagedProxyPort: c.managedProxyPort, HostReadRoots: hostReadRoots,
		HostReadFiles: c.diagnosticReadFiles,
	})
	if err != nil {
		return nil, fmt.Errorf("child sandbox: %w", err)
	}
	// Child process journals stay isolated from the parent and sibling roots.
	processes := process.NewSessionManager(0)
	var jobs *joblog.Store
	if stateLayout.Root != "" {
		processes.SetJournalPath(filepath.Join(stateLayout.Control, "jobs", "journal.jsonl"))
		if err := processes.LoadStaleJournal(); err != nil {
			processes.CloseAll()
			_ = sandbox.CloseBackend(backend)
			return nil, fmt.Errorf("load child process journal: %w", err)
		}
		if archive, archiveErr := joblog.New(
			filepath.Join(stateLayout.Control, "jobs", "logs"),
		); archiveErr == nil {
			jobs = archive
			processes.SetArchive(archive)
		}
	}
	registry, handles, err := builtin.NewWithDependencies(
		root, backend, c.content, processes, c.web,
	)
	if err != nil {
		processes.CloseAll()
		_ = sandbox.CloseBackend(backend)
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
			if jobs != nil {
				_ = jobs.Close()
			}
			processes.CloseAll()
			_ = sandbox.CloseBackend(backend)
			return nil, fmt.Errorf("child interact tools: %w", registerErr)
		}
	}
	journal, err := c.openJournal(root, stateLayout)
	if err != nil {
		processes.CloseAll()
		_ = sandbox.CloseBackend(backend)
		return nil, err
	}
	runner := &verify.ReceiptRunner{Root: root, Command: c.verify.Command}
	files, err := filetool.NewWithBackend(root, backend)
	if err != nil {
		_ = journal.Close(context.Background())
		processes.CloseAll()
		_ = sandbox.CloseBackend(backend)
		return nil, fmt.Errorf("child integration files: %w", err)
	}
	if agents != nil {
		if err := agenttool.Register(registry, agenttool.Options{
			Control: agents, Handles: handles,
			Files: files, OnRelease: agentRelease,
			Sandbox: backend, Verify: runner, Workspace: root, SessionID: agentSession,
		}); err != nil {
			_ = journal.Close(context.Background())
			processes.CloseAll()
			_ = sandbox.CloseBackend(backend)
			return nil, fmt.Errorf("child agent tools: %w", err)
		}
	}
	toolset := &childToolset{
		registry: registry, backend: backend, processes: processes, journal: journal,
		jobLogs: jobs, inputHost: inputHost,
		diagnostics: diagnostics.NewCommandRunner(root, backend, c.diagnosticCommands),
		verify:      runner, files: files,
	}
	// Keep the owner's enablement and lock policy, but discover only this
	// child's workspace and execution HOME alongside configured/user skills.
	policy, _ := sandbox.BackendPolicy(backend)
	var capabilities capabilityBuildState
	if err := (skillContributor{
		paths: c.skillPaths, workspace: root, sandboxHome: policy.PrivateTemp,
		output: &capabilities,
	}).Contribute(context.Background(), registry); err != nil {
		toolset.close()
		return nil, fmt.Errorf("child skills: %w", err)
	}
	toolset.skillCatalog = capabilities.skillCatalog
	c.mu.Lock()
	if existing := c.built[root]; existing != nil {
		c.mu.Unlock()
		toolset.close()
		if (existing.inputHost != nil) != interactive {
			return nil, errors.New("child toolset interaction mode changed")
		}
		return existing, nil
	}
	c.built[root] = toolset
	c.mu.Unlock()
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
	if c.journals.RecoverOnStart {
		if _, err := journal.Recover(context.Background()); err != nil {
			_ = journal.Close(context.Background())
			return nil, fmt.Errorf("recover interrupted child turns: %w", err)
		}
	}
	return journal, nil
}

// release drops the toolset for a root once its child is closed.
func (c *childToolsets) release(root string) {
	c.mu.Lock()
	toolset := c.built[root]
	delete(c.built, root)
	c.mu.Unlock()
	toolset.close()
}

func (c *childToolsets) closeAll() {
	c.mu.Lock()
	toolsets := make([]*childToolset, 0, len(c.built))
	for _, toolset := range c.built {
		toolsets = append(toolsets, toolset)
	}
	c.built = make(map[string]*childToolset)
	c.mu.Unlock()
	for _, toolset := range toolsets {
		toolset.close()
	}
}
