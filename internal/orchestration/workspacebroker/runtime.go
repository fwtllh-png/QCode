// Package workspacebroker composes the trusted workspace mutation brokers.
package workspacebroker

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/filebroker"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/internal/security/vcsbroker"
)

type Runtime struct {
	Files     *filebroker.Runtime
	VCS       *vcsbroker.Broker
	authority *authority.LeaseAuthority
	leaseTTL  time.Duration

	// fileRuntimes caches per-workspace file brokers for CommitFilesAt so a
	// repeated target workspace pays construction once. Broker.Commit is
	// stateless besides its thread-safe authority, so cached runtimes are
	// safe to share.
	fileRuntimesMu sync.Mutex
	fileRuntimes   map[string]*filebroker.Runtime
}

func (r *Runtime) ReadVCS(
	ctx context.Context,
	dir string,
	arguments ...string,
) (string, error) {
	return r.VCS.Read(ctx, dir, arguments...)
}

func (r *Runtime) mutateVCS(
	ctx context.Context,
	kind vcsbroker.MutationKind,
	dir string,
	arguments []string,
) error {
	_, err := r.VCS.Mutate(ctx, vcsbroker.Mutation{
		Kind: kind, Dir: dir, Args: arguments,
	})
	return err
}

func (r *Runtime) AddWorktree(
	ctx context.Context, dir, path, revision string,
) error {
	return r.mutateVCS(ctx, vcsbroker.WorktreeAdd, dir, []string{
		"worktree", "add", "--detach", path, revision,
	})
}

func (r *Runtime) RemoveWorktree(
	ctx context.Context, dir, path string,
) error {
	return r.mutateVCS(ctx, vcsbroker.WorktreeRemove, dir, []string{
		"worktree", "remove", "--force", path,
	})
}

func (r *Runtime) PruneWorktrees(ctx context.Context, dir string) error {
	return r.mutateVCS(
		ctx, vcsbroker.WorktreePrune, dir, []string{"worktree", "prune"},
	)
}

func (r *Runtime) ApplyPatch(
	ctx context.Context, dir, patchPath string,
) error {
	return r.mutateVCS(ctx, vcsbroker.ApplyPatch, dir, []string{
		"apply", "--whitespace=nowarn", patchPath,
	})
}

func (r *Runtime) AddIndex(
	ctx context.Context, dir string, paths []string,
) error {
	arguments := append([]string{"add", "-A", "--"}, paths...)
	return r.mutateVCS(ctx, vcsbroker.IndexAdd, dir, arguments)
}

func (r *Runtime) Commit(
	ctx context.Context, dir, message string,
) error {
	return r.mutateVCS(ctx, vcsbroker.Commit, dir, []string{
		"commit", "--no-gpg-sign", "-m", message,
	})
}

func (r *Runtime) AmendCommit(
	ctx context.Context, dir, message string,
) error {
	return r.mutateVCS(ctx, vcsbroker.Commit, dir, []string{
		"commit", "--amend", "--no-gpg-sign", "-m", message,
	})
}

func (r *Runtime) CommitBaseline(
	ctx context.Context, dir string,
) error {
	return r.mutateVCS(ctx, vcsbroker.Commit, dir, []string{
		"-c", "user.name=QCode",
		"-c", "user.email=qcode@localhost",
		"commit", "--allow-empty", "--no-gpg-sign",
		"-m", "qcode chat baseline",
	})
}

func (r *Runtime) CreateBranch(
	ctx context.Context, dir, branch string,
) error {
	return r.mutateVCS(ctx, vcsbroker.CreateBranch, dir, []string{
		"switch", "-c", branch,
	})
}

func (r *Runtime) SwitchBranch(
	ctx context.Context, dir, branch string,
) error {
	return r.VCS.SwitchBranch(ctx, dir, branch)
}

func (r *Runtime) Fetch(
	ctx context.Context, dir, remote string,
) error {
	return r.mutateVCS(ctx, vcsbroker.Fetch, dir, []string{
		"fetch", "--prune", remote,
	})
}

func (r *Runtime) Pull(
	ctx context.Context, dir, remote, branch string,
) error {
	return r.mutateVCS(ctx, vcsbroker.Pull, dir, []string{
		"pull", "--ff-only", "--", remote, branch,
	})
}

func (r *Runtime) Push(
	ctx context.Context, dir, remote, branch string,
) error {
	return r.mutateVCS(ctx, vcsbroker.Push, dir, []string{
		"push", "--porcelain", "--", remote, "HEAD:refs/heads/" + branch,
	})
}

func (r *Runtime) Merge(
	ctx context.Context, dir, revision string,
) error {
	return r.mutateVCS(ctx, vcsbroker.Merge, dir, []string{
		"merge", "--no-edit", "--", revision,
	})
}

func (r *Runtime) Rebase(
	ctx context.Context, dir, revision string,
) error {
	return r.mutateVCS(ctx, vcsbroker.Rebase, dir, []string{
		"rebase", "--", revision,
	})
}

func (r *Runtime) CherryPick(
	ctx context.Context, dir, revision string,
) error {
	return r.mutateVCS(ctx, vcsbroker.CherryPick, dir, []string{
		"cherry-pick", "--", revision,
	})
}

func (r *Runtime) Restore(
	ctx context.Context, dir string, paths []string, staged bool,
) error {
	arguments := []string{"restore"}
	if staged {
		arguments = append(arguments, "--staged")
	}
	arguments = append(arguments, "--")
	arguments = append(arguments, paths...)
	return r.mutateVCS(ctx, vcsbroker.Restore, dir, arguments)
}

func (r *Runtime) StashPush(
	ctx context.Context, dir, message string,
) error {
	arguments := []string{"stash", "push"}
	if message != "" {
		arguments = append(arguments, "-m", message)
	}
	return r.mutateVCS(ctx, vcsbroker.StashPush, dir, arguments)
}

func (r *Runtime) StashPop(ctx context.Context, dir string) error {
	return r.mutateVCS(ctx, vcsbroker.StashPop, dir, []string{"stash", "pop"})
}

func (r *Runtime) Tag(
	ctx context.Context, dir, tag, message string,
) error {
	arguments := []string{"tag", "--", tag}
	if message != "" {
		arguments = []string{"tag", "-a", tag, "-m", message}
	}
	return r.mutateVCS(ctx, vcsbroker.Tag, dir, arguments)
}

func (r *Runtime) ResolveConflict(
	ctx context.Context, dir, action string,
) error {
	arguments := map[string][]string{
		"merge_abort":          {"merge", "--abort"},
		"rebase_abort":         {"rebase", "--abort"},
		"rebase_continue":      {"-c", "core.editor=true", "rebase", "--continue"},
		"cherry_pick_abort":    {"cherry-pick", "--abort"},
		"cherry_pick_continue": {"-c", "core.editor=true", "cherry-pick", "--continue"},
	}[action]
	if len(arguments) == 0 {
		return errors.New("unsupported Git conflict action")
	}
	return r.mutateVCS(ctx, vcsbroker.Conflict, dir, arguments)
}

func (r *Runtime) CommitFiles(
	ctx context.Context,
	toolName string,
	plan filebroker.Plan,
	journal *workspacejournal.Manager,
) (filebroker.Result, error) {
	var transactionJournal filebroker.Journal
	if journal != nil {
		transactionJournal = journal
	}
	return r.Files.Commit(ctx, toolName, plan, transactionJournal)
}

func New(
	workspace string,
	manager *authority.LeaseAuthority,
	leaseTTL time.Duration,
) (*Runtime, error) {
	root, err := sandbox.NewWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	files, err := filebroker.NewRuntime(root, manager, leaseTTL)
	if err != nil {
		return nil, err
	}
	vcs, err := vcsbroker.New(workspace, manager, leaseTTL)
	if err != nil {
		return nil, err
	}
	return &Runtime{
		Files: files, VCS: vcs, authority: manager, leaseTTL: leaseTTL,
	}, nil
}

func (r *Runtime) CommitFilesAt(
	ctx context.Context,
	workspace string,
	toolName string,
	plan filebroker.Plan,
) (filebroker.Result, error) {
	root, err := sandbox.NewWorkspace(workspace)
	if err != nil {
		return filebroker.Result{}, err
	}
	r.fileRuntimesMu.Lock()
	runtime, cached := r.fileRuntimes[root.Root()]
	if !cached {
		runtime, err = filebroker.NewRuntime(root, r.authority, r.leaseTTL)
		if err != nil {
			r.fileRuntimesMu.Unlock()
			return filebroker.Result{}, err
		}
		if r.fileRuntimes == nil {
			r.fileRuntimes = make(map[string]*filebroker.Runtime)
		}
		r.fileRuntimes[root.Root()] = runtime
	}
	r.fileRuntimesMu.Unlock()
	return runtime.Commit(ctx, toolName, plan, nil)
}
