package vcsbroker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/authority"
)

func TestBrokerAddsAndRemovesDetachedWorktree(t *testing.T) {
	repository := gitRepository(t)
	manager := authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{})
	broker, err := New(repository, manager, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	head, err := broker.Read(t.Context(), repository, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "worktree")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if _, err := broker.Mutate(ctx, Mutation{
		Kind: WorktreeAdd, Dir: repository,
		Args: []string{
			"worktree", "add", "--detach", target, strings.TrimSpace(head),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(target, ".git")); err != nil || info.IsDir() {
		t.Fatalf("detached worktree marker = %+v, err = %v", info, err)
	}
	if _, err := broker.Mutate(ctx, Mutation{
		Kind: WorktreeRemove, Dir: repository,
		Args: []string{"worktree", "remove", "--force", target},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("removed worktree still exists: %v", err)
	}
}

func TestBrokerRejectsUnallowlistedGitMutation(t *testing.T) {
	repository := gitRepository(t)
	broker, err := New(
		repository,
		authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}),
		time.Minute, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := broker.Mutate(ctx, Mutation{
		Kind: IndexAdd, Dir: repository,
		Args: []string{"reset", "--hard"},
	}); err == nil || !strings.Contains(err.Error(), "invalid index") {
		t.Fatalf("unallowlisted mutation error = %v", err)
	}
}

func TestBrokerRejectsUnsafeModelGitArguments(t *testing.T) {
	repository := gitRepository(t)
	broker, err := New(
		repository,
		authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}),
		time.Minute, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	tests := []Mutation{
		{Kind: RepositoryInit, Dir: repository, Args: []string{"init", "--bare"}},
		{Kind: SnapshotIndex, Dir: repository, Args: []string{"add", "-A", "--force", "--", "../outside"}},
		{
			Kind: IndexAdd, Dir: repository,
			Args: []string{"add", "-A", "--", "../outside"},
		},
		{
			Kind: Commit, Dir: repository,
			Args: []string{"commit", "--no-gpg-sign", "-m", ""},
		},
		{
			Kind: Push, Dir: repository,
			Args: []string{"push", "--porcelain", "--", "--force", "HEAD:refs/heads/main"},
		},
		{
			Kind: Push, Dir: repository,
			Args: []string{"push", "--porcelain", "--", ".", "HEAD:refs/heads/main"},
		},
		{
			Kind: Push, Dir: repository,
			Args: []string{"push", "--porcelain", "--", "origin", "+HEAD:refs/heads/main"},
		},
		{
			Kind: Pull, Dir: repository,
			Args: []string{"pull", "--rebase", "--", "origin", "main"},
		},
	}
	for _, mutation := range tests {
		if _, err := broker.Mutate(t.Context(), mutation); err == nil {
			t.Fatalf("unsafe mutation was accepted: %+v", mutation)
		}
	}
}

func TestExtendedMutationAllowlist(t *testing.T) {
	repository := gitRepository(t)
	valid := []Mutation{
		{Kind: Merge, Dir: repository, Args: []string{"merge", "--no-edit", "--", "feature"}},
		{Kind: Rebase, Dir: repository, Args: []string{"rebase", "--", "main"}},
		{Kind: CherryPick, Dir: repository, Args: []string{"cherry-pick", "--", "HEAD~1"}},
		{Kind: Restore, Dir: repository, Args: []string{"restore", "--", "README.md"}},
		{Kind: Restore, Dir: repository, Args: []string{"restore", "--staged", "--", "README.md"}},
		{Kind: StashPush, Dir: repository, Args: []string{"stash", "push", "-m", "work"}},
		{Kind: StashPop, Dir: repository, Args: []string{"stash", "pop"}},
		{Kind: Tag, Dir: repository, Args: []string{"tag", "--", "v1.0.0"}},
		{Kind: Tag, Dir: repository, Args: []string{"tag", "-a", "v1.0.0", "-m", "release"}},
		{Kind: Commit, Dir: repository, Args: []string{
			"commit", "--amend", "--no-gpg-sign", "-m", "updated",
		}},
		{Kind: Conflict, Dir: repository, Args: []string{"merge", "--abort"}},
		{Kind: Conflict, Dir: repository, Args: []string{"rebase", "--abort"}},
		{Kind: Conflict, Dir: repository, Args: []string{
			"-c", "core.editor=true", "rebase", "--continue",
		}},
		{Kind: Conflict, Dir: repository, Args: []string{"cherry-pick", "--abort"}},
		{Kind: Conflict, Dir: repository, Args: []string{
			"-c", "core.editor=true", "cherry-pick", "--continue",
		}},
	}
	for _, mutation := range valid {
		if err := validateMutation(repository, mutation); err != nil {
			t.Fatalf("valid mutation rejected: %+v: %v", mutation, err)
		}
	}
	invalid := []Mutation{
		{Kind: Merge, Dir: repository, Args: []string{"merge", "--abort"}},
		{Kind: Rebase, Dir: repository, Args: []string{"rebase", "--exec", "touch owned", "main"}},
		{Kind: CherryPick, Dir: repository, Args: []string{"cherry-pick", "--no-commit", "HEAD"}},
		{Kind: Restore, Dir: repository, Args: []string{"restore", "--", "../outside"}},
		{Kind: StashPop, Dir: repository, Args: []string{"stash", "pop", "stash@{2}"}},
		{Kind: Tag, Dir: repository, Args: []string{"tag", "-f", "v1"}},
		{Kind: Conflict, Dir: repository, Args: []string{"reset", "--hard"}},
	}
	for _, mutation := range invalid {
		if err := validateMutation(repository, mutation); err == nil {
			t.Fatalf("unsafe mutation accepted: %+v", mutation)
		}
	}
}

func TestBrokerExecutesExtendedMutations(t *testing.T) {
	repository := gitRepository(t)
	baseBranch := strings.TrimSpace(runRepositoryGit(
		t, repository, "symbolic-ref", "--short", "HEAD",
	))
	runRepositoryGit(t, repository, "switch", "-c", "feature")
	if err := os.WriteFile(filepath.Join(repository, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRepositoryGit(t, repository, "add", "feature.txt")
	runRepositoryGit(t, repository, "commit", "-qm", "feature")
	featureCommit := strings.TrimSpace(runRepositoryGit(t, repository, "rev-parse", "HEAD"))
	runRepositoryGit(t, repository, "switch", baseBranch)

	broker, err := New(
		repository,
		authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}),
		time.Minute, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(kind MutationKind, arguments ...string) {
		t.Helper()
		if _, mutationErr := broker.Mutate(t.Context(), Mutation{
			Kind: kind, Dir: repository, Args: arguments,
		}); mutationErr != nil {
			t.Fatalf("%s %v: %v", kind, arguments, mutationErr)
		}
	}

	mutate(Merge, "merge", "--no-edit", "--", "feature")
	mutate(Tag, "tag", "--", "v1.0.0")
	if got := strings.TrimSpace(runRepositoryGit(t, repository, "rev-parse", "v1.0.0")); got != featureCommit {
		t.Fatalf("tag points to %q, want %q", got, featureCommit)
	}

	if err := os.WriteFile(filepath.Join(repository, "feature.txt"), []byte("stashed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mutate(StashPush, "stash", "push", "-m", "fixture")
	if got := runRepositoryGit(t, repository, "status", "--porcelain"); strings.TrimSpace(got) != "" {
		t.Fatalf("status after stash = %q", got)
	}
	mutate(StashPop, "stash", "pop")
	mutate(Restore, "restore", "--", "feature.txt")

	if err := os.WriteFile(filepath.Join(repository, "amended.txt"), []byte("amended\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRepositoryGit(t, repository, "add", "amended.txt")
	mutate(Commit, "commit", "--amend", "--no-gpg-sign", "-m", "amended")

	runRepositoryGit(t, repository, "switch", "-c", "source")
	if err := os.WriteFile(filepath.Join(repository, "picked.txt"), []byte("picked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRepositoryGit(t, repository, "add", "picked.txt")
	runRepositoryGit(t, repository, "commit", "-qm", "picked")
	picked := strings.TrimSpace(runRepositoryGit(t, repository, "rev-parse", "HEAD"))
	runRepositoryGit(t, repository, "switch", baseBranch)
	mutate(CherryPick, "cherry-pick", "--", picked)

	runRepositoryGit(t, repository, "switch", "-c", "topic")
	if err := os.WriteFile(filepath.Join(repository, "topic.txt"), []byte("topic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRepositoryGit(t, repository, "add", "topic.txt")
	runRepositoryGit(t, repository, "commit", "-qm", "topic")
	runRepositoryGit(t, repository, "switch", baseBranch)
	if err := os.WriteFile(filepath.Join(repository, "main.txt"), []byte("main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRepositoryGit(t, repository, "add", "main.txt")
	runRepositoryGit(t, repository, "commit", "-qm", "main")
	runRepositoryGit(t, repository, "switch", "topic")
	mutate(Rebase, "rebase", "--", baseBranch)
	if _, err := os.Stat(filepath.Join(repository, "main.txt")); err != nil {
		t.Fatalf("rebased branch is missing main.txt: %v", err)
	}
}

func TestBrokerRejectsUnknownRemoteBeforeMutation(t *testing.T) {
	repository := gitRepository(t)
	broker, err := New(
		repository,
		authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}),
		time.Minute, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = broker.Mutate(t.Context(), Mutation{
		Kind: Push, Dir: repository,
		Args: []string{
			"push", "--porcelain", "--", "missing", "HEAD:refs/heads/main",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "resolve configured Git remote") {
		t.Fatalf("unknown remote error = %v", err)
	}
}

func TestBrokerSwitchesAllowlistedLocalBranch(t *testing.T) {
	repository := gitRepository(t)
	command := exec.Command("git", "branch", "feature")
	command.Dir = repository
	command.Env = append(
		os.Environ(), "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create branch: %v: %s", err, output)
	}
	broker, err := New(
		repository,
		authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}),
		time.Minute, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.SwitchBranch(t.Context(), repository, "feature"); err != nil {
		t.Fatal(err)
	}
	branch, err := broker.Read(
		t.Context(), repository, "symbolic-ref", "--short", "HEAD",
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(branch) != "feature" {
		t.Fatalf("branch = %q, want feature", branch)
	}
}

func TestBrokerRejectsInvalidBranchSwitch(t *testing.T) {
	repository := gitRepository(t)
	broker, err := New(
		repository,
		authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}),
		time.Minute, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Mutate(t.Context(), Mutation{
		Kind: SwitchBranch,
		Dir:  repository,
		Args: []string{"switch", "--no-guess", "--", "--detach"},
	}); err == nil || !strings.Contains(err.Error(), "invalid branch switch") {
		t.Fatalf("invalid branch switch error = %v", err)
	}
}

func TestBrokerRequiresExplicitLeaseTTL(t *testing.T) {
	repository := gitRepository(t)
	if _, err := New(
		repository,
		authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}),
		0, nil,
	); err == nil || !strings.Contains(err.Error(), "Lease TTL") {
		t.Fatalf("missing TTL error = %v", err)
	}
}

func TestBrokerRejectsIndexDriftBeforeMutation(t *testing.T) {
	repository := gitRepository(t)
	broker, err := New(
		repository,
		authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}),
		time.Minute, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	broker.beforeRevalidate = func() error {
		if err := os.WriteFile(
			filepath.Join(repository, "drift.txt"), []byte("drift\n"), 0o600,
		); err != nil {
			return err
		}
		command := exec.Command("git", "add", "drift.txt")
		command.Dir = repository
		command.Env = append(
			os.Environ(), "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=",
		)
		return command.Run()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := broker.Mutate(ctx, Mutation{
		Kind: WorktreePrune, Dir: repository,
		Args: []string{"worktree", "prune"},
	}); err == nil || !strings.Contains(err.Error(), "repository changed") {
		t.Fatalf("index drift error = %v", err)
	}
}

func TestBrokerRejectsConfigDriftBeforeMutation(t *testing.T) {
	repository := gitRepository(t)
	broker, err := New(
		repository,
		authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}),
		time.Minute, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	broker.beforeRevalidate = func() error {
		command := exec.Command("git", "config", "fixture.changed", "true")
		command.Dir = repository
		command.Env = append(
			os.Environ(), "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=",
		)
		return command.Run()
	}
	if _, err := broker.Mutate(t.Context(), Mutation{
		Kind: WorktreePrune, Dir: repository,
		Args: []string{"worktree", "prune"},
	}); err == nil || !strings.Contains(err.Error(), "repository changed") {
		t.Fatalf("config drift error = %v", err)
	}
}

func TestBrokerBindsMutationToTargetWorktreeIndex(t *testing.T) {
	repository := gitRepository(t)
	broker, err := New(
		repository,
		authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}),
		time.Minute, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "worktree")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	head, err := broker.Read(ctx, repository, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Mutate(ctx, Mutation{
		Kind: WorktreeAdd, Dir: repository,
		Args: []string{
			"worktree", "add", "--detach", target, strings.TrimSpace(head),
		},
	}); err != nil {
		t.Fatal(err)
	}
	broker.beforeRevalidate = func() error {
		if err := os.WriteFile(
			filepath.Join(target, "target-drift.txt"), []byte("drift\n"), 0o600,
		); err != nil {
			return err
		}
		command := exec.Command("git", "add", "target-drift.txt")
		command.Dir = target
		command.Env = append(
			os.Environ(), "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=",
		)
		return command.Run()
	}
	if _, err := broker.Mutate(ctx, Mutation{
		Kind: WorktreePrune, Dir: target,
		Args: []string{"worktree", "prune"},
	}); err == nil || !strings.Contains(err.Error(), "repository changed") {
		t.Fatalf("target worktree index drift error = %v", err)
	}
}

func gitRepository(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "fixture@example.invalid"},
		{"config", "user.name", "Fixture"},
		{"config", "commit.gpgsign", "false"},
		{"add", "README.md"},
		{"commit", "--quiet", "-m", "seed"},
	} {
		command := exec.Command("git", arguments...)
		command.Dir = root
		command.Env = append(
			os.Environ(), "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=",
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
		}
	}
	return root
}

func runRepositoryGit(t *testing.T, root string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = root
	command.Env = append(
		os.Environ(), "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}
