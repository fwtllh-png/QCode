package subagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// gitCommandTimeout bounds one caller-owned provisioning attempt.
const gitCommandTimeout = 2 * time.Minute

// Worktrees gives read-only agents scratch roots and writing agents
// isolated Git worktrees that cannot mutate the parent workspace.
type Worktrees struct {
	// repository is the host workspace, which must be inside a git work tree for
	// isolation to be possible at all.
	repository string
	root       string
	strategy   string
	scratch    WorktreeProvider
	brokers    WorktreeBroker
}

// WorktreeBroker provides the guarded Git operations used during provisioning.
type WorktreeBroker interface {
	ReadVCS(context.Context, string, ...string) (string, error)
	AddWorktree(context.Context, string, string, string) error
	RemoveWorktree(context.Context, string, string) error
	PruneWorktrees(context.Context, string) error
}

type WorktreeOptions struct {
	Workspace string
	Root      string
	Strategy  string
	Broker    WorktreeBroker
}

func NewWorktrees(options WorktreeOptions) (*Worktrees, error) {
	if options.Broker == nil {
		return nil, errors.New("worktree broker is required")
	}
	trees := &Worktrees{
		repository: options.Workspace, root: options.Root, strategy: options.Strategy,
		scratch: NewScratchWorktrees(options.Root), brokers: options.Broker,
	}
	if options.Strategy != config.SubagentWorkspaceWorktree {
		return trees, nil
	}
	// Fail at startup when explicitly requested isolation is unavailable.
	if err := trees.checkRepository(context.Background()); err != nil {
		return nil, err
	}
	return trees, nil
}

func (c *Worktrees) isolates(stance Stance) bool {
	switch c.strategy {
	case config.SubagentWorkspaceReadOnly:
		return false
	case config.SubagentWorkspaceWorktree:
		return stance != StanceReadOnly
	default:
		return stance != StanceReadOnly
	}
}

func (c *Worktrees) Provision(
	agentID string, stance Stance,
) (Worktree, error) {
	if c.strategy == config.SubagentWorkspaceSerialized {
		return Worktree{
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
		return Worktree{}, protocol.NewProblem(
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
		return Worktree{}, err
	}
	baseRev, err := c.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return Worktree{}, protocol.NewProblem(
			protocol.CodeUnavailable,
			fmt.Sprintf("cannot record base revision for child agent %s: %s", agentID, err),
			false, nil,
		)
	}
	baseRev = strings.TrimSpace(baseRev)
	if err := c.addDetachedWorktree(ctx, path, baseRev); err != nil {
		return Worktree{}, protocol.NewProblem(
			protocol.CodeUnavailable,
			fmt.Sprintf("cannot create a git worktree for child agent %s: %s", agentID, err),
			false, nil,
		)
	}
	return Worktree{
		ID: agentID, Path: path, Isolated: true,
		BaseRev: strings.TrimSpace(baseRev),
	}, nil
}

func (c *Worktrees) addDetachedWorktree(
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

func (c *Worktrees) Discard(worktree Worktree) error {
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

func (c *Worktrees) checkRepository(ctx context.Context) error {
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

func (c *Worktrees) git(ctx context.Context, arguments ...string) (string, error) {
	return c.brokers.ReadVCS(ctx, c.repository, arguments...)
}

// CommonGitDir resolves the repository metadata directory used to scope child sandbox reads.
func (c *Worktrees) CommonGitDir(ctx context.Context) (string, error) {
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

// WorktreeGitReadRoots validates a worktree's Git metadata links before exposing read roots.
func WorktreeGitReadRoots(root, expectedCommonDir string) ([]string, error) {
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
