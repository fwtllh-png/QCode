package subagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/orchestration/workspacebroker"
	"github.com/fwtllh-png/QCode/internal/security/authority"
)

// newGitWorkspace makes a workspace a writing child can be isolated inside: a
// git work tree with one commit, which is what `git worktree add HEAD` needs.
func newGitWorkspace(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	workspace, err := os.MkdirTemp("", "qcode-git-workspace-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeGitWorkspace(t, workspace) })
	if err := os.WriteFile(
		filepath.Join(workspace, "README.md"), []byte("fixture\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "fixture@example.com"},
		{"config", "user.name", "Fixture"},
		{"config", "commit.gpgsign", "false"},
		{"config", "maintenance.auto", "false"},
		{"config", "gc.auto", "0"},
		{"add", "README.md"},
		{"commit", "--quiet", "-m", "seed"},
	} {
		command := exec.Command("git", arguments...)
		command.Dir = workspace
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=")
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, out)
		}
	}
	return workspace
}

func removeGitWorkspace(t *testing.T, workspace string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		removeErr := os.RemoveAll(workspace)
		_, statErr := os.Lstat(workspace)
		if os.IsNotExist(statErr) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf(
				"remove Git workspace %s: remove=%v stat=%v",
				workspace, removeErr, statErr,
			)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWorktreeGitReadRootsRejectsEscapingGitDir(t *testing.T) {
	base := t.TempDir()
	common := filepath.Join(base, "repository.git")
	root := filepath.Join(base, "worktree")
	outside := filepath.Join(base, "outside.git")
	for _, directory := range []string{common, root, outside} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(root, ".git"), []byte("gitdir: "+outside+"\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := WorktreeGitReadRoots(root, common); err == nil ||
		!strings.Contains(err.Error(), "escapes") {
		t.Fatalf("WorktreeGitReadRoots error = %v", err)
	}
}

func TestWorktreeGitReadRootsRejectsMismatchedCommonDir(t *testing.T) {
	base := t.TempDir()
	common := filepath.Join(base, "repository.git")
	gitDir := filepath.Join(common, "worktrees", "chat")
	root := filepath.Join(base, "worktree")
	other := filepath.Join(base, "other.git")
	for _, directory := range []string{gitDir, root, other} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(root, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(gitDir, "commondir"), []byte(other+"\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := WorktreeGitReadRoots(root, common); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("WorktreeGitReadRoots error = %v", err)
	}
}

func TestWorktreeGitReadRootsIncludeCommonInfoDirectory(t *testing.T) {
	base := t.TempDir()
	common := filepath.Join(base, "repository.git")
	gitDir := filepath.Join(common, "worktrees", "chat")
	root := filepath.Join(base, "worktree")
	for _, directory := range []string{
		filepath.Join(common, "objects"),
		filepath.Join(common, "refs"),
		filepath.Join(common, "info"),
		gitDir,
		root,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(common, "info", "exclude"), []byte("fixture\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	roots, err := WorktreeGitReadRoots(root, common)
	if err != nil {
		t.Fatal(err)
	}
	resolvedCommon, err := filepath.EvalSymlinks(common)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(resolvedCommon, "info")
	for _, candidate := range roots {
		if candidate == want {
			return
		}
	}
	t.Fatalf("common info directory %q missing from %v", want, roots)
}

func TestChildWorktreeProvisionRecoversMissingRegisteredPath(t *testing.T) {
	workspace := newGitWorkspace(t)
	root := t.TempDir()
	broker, err := workspacebroker.New(workspace,
		authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}), time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	trees, err := NewWorktrees(WorktreeOptions{
		Workspace: workspace, Root: root,
		Strategy: config.SubagentWorkspaceWorktree, Broker: broker,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "worktrees", "agent-stale")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(
		"git", "worktree", "add", "--detach", path, "HEAD",
	)
	command.Dir = workspace
	command.Env = append(
		os.Environ(), "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=",
	)
	if output, commandErr := command.CombinedOutput(); commandErr != nil {
		t.Fatalf("create stale worktree: %v: %s", commandErr, output)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}

	worktree, err := trees.Provision("agent-stale", StanceWrite)
	if err != nil {
		t.Fatal(err)
	}
	if !worktree.Isolated || worktree.Path != path || worktree.BaseRev == "" {
		t.Fatalf("recovered worktree = %+v", worktree)
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		t.Fatalf("recovered worktree .git: %v", err)
	}
	if err := trees.Discard(worktree); err != nil {
		t.Fatal(err)
	}
}
