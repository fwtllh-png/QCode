package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	gittool "github.com/fwtllh-png/QCode/internal/adapter/tool/git"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/orchestration/workspacebroker"
	"github.com/fwtllh-png/QCode/internal/runtime/app/workspacequery"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func gitFixture(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, output, err)
	}
	return strings.TrimSpace(string(output))
}

func gitFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newGitRuntime(t *testing.T, initial bool) (*Runtime, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	root := t.TempDir()
	gitFixture(t, root, "init", "-b", "main")
	gitFixture(t, root, "config", "user.name", "Fixture")
	gitFixture(t, root, "config", "user.email", "fixture@example.test")
	gitFile(t, root, "a.txt", "before\n")
	gitFile(t, root, "b.txt", "untouched\n")
	if initial {
		gitFixture(t, root, "add", ".")
		gitFixture(t, root, "commit", "-m", "initial")
	}
	manager := authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{})
	brokers, err := workspacebroker.New(root, manager, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: root, PrivateTemp: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	query, err := workspacequery.New(root, backend)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := gittool.RegisterMutations(registry, root, brokers); err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntime(Options{
		Engine: &testEngine{}, WorkspaceRoot: root,
		GitControl: &GitControl{Workspace: query, NewGuard: func(context.Context, protocol.SessionProfile) (*guard.Guard, error) {
			return guard.New(guard.Options{
				Registry: registry, Workspace: root, LeaseAuthority: manager, LeaseTTL: time.Minute,
				Policy: policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
			})
		}},
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	return runtime, root
}

func gitRequest(t *testing.T, r *Runtime, action string) GitRequest {
	t.Helper()
	state, err := r.opts.GitControl.Workspace.GitOverview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return GitRequest{
		Action: action, Branch: state.Branch, Revision: state.Revision,
		Message: "explicit commit", Paths: []string{"a.txt"}, Remote: "origin",
	}
}

func TestDirectGitCommitPushDoesNotCreateTurn(t *testing.T) {
	r, root := newGitRuntime(t, true)
	remote := t.TempDir()
	gitFixture(t, remote, "init", "--bare")
	gitFixture(t, root, "remote", "add", "origin", remote)
	gitFile(t, root, "a.txt", "after\n")
	gitFile(t, root, "b.txt", "keep unstaged\n")
	gitFixture(t, root, "add", "a.txt")
	request := gitRequest(t, r, "commit_push")
	before := r.Snapshot(t.Context())
	result, err := r.ExecuteGit(t.Context(), request)
	if err != nil || result.Problem != nil || result.CommitHash == "" ||
		!slices.Equal(result.Completed, []string{"git_commit", "git_push"}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if got := gitFixture(t, remote, "rev-parse", "refs/heads/main"); got != result.CommitHash {
		t.Fatalf("remote=%s commit=%s", got, result.CommitHash)
	}
	if got := gitFixture(t, root, "show", "HEAD:b.txt"); got != "untouched" {
		t.Fatalf("included unstaged file: %s", got)
	}
	after := r.Snapshot(t.Context())
	if after.LastSequence != before.LastSequence || after.ActiveTurns != 0 ||
		after.OperationsProcessed != before.OperationsProcessed || after.PendingOperations != 0 {
		t.Fatalf("direct Git created Runtime conversation work: %+v", after)
	}
	if _, err := r.ExecuteGit(t.Context(), request); !protocol.IsCode(err, protocol.CodeConflict) {
		t.Fatalf("replayed stale commit was not rejected: %v", err)
	}
}

func TestDirectGitFirstCommitAndLiteralPaths(t *testing.T) {
	r, root := newGitRuntime(t, false)
	gitFile(t, root, ":(glob)*", "literal path\n")
	request := gitRequest(t, r, "commit")
	request.Paths = []string{":(glob)*"}
	request.IncludeUnstaged = true
	result, err := r.ExecuteGit(t.Context(), request)
	if err != nil || result.Problem != nil || result.CommitHash == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if got := gitFixture(t, root, "ls-tree", "--name-only", "HEAD"); got != ":(glob)*" {
		t.Fatalf("unexpected committed paths: %q", got)
	}
}

func TestDirectGitRetainsCommitWhenPushFails(t *testing.T) {
	r, root := newGitRuntime(t, true)
	gitFixture(t, root, "remote", "add", "origin", filepath.Join(t.TempDir(), "missing.git"))
	gitFile(t, root, "a.txt", "after\n")
	gitFixture(t, root, "add", "a.txt")
	result, err := r.ExecuteGit(t.Context(), gitRequest(t, r, "commit_push"))
	if err != nil || result.Problem == nil || result.CommitHash == "" ||
		!slices.Equal(result.Completed, []string{"git_commit"}) {
		t.Fatalf("partial result=%+v err=%v", result, err)
	}
	if gitFixture(t, root, "rev-parse", "HEAD") != result.CommitHash {
		t.Fatal("partial commit was not preserved")
	}
	if got := gitFixture(t, root, "rev-list", "--count", "HEAD"); got != "2" {
		t.Fatalf("unexpected commit count: %s", got)
	}
}

func TestDirectGitRejectsUnexpectedIndexAndStateChanges(t *testing.T) {
	r, root := newGitRuntime(t, true)
	gitFile(t, root, "a.txt", "after\n")
	gitFixture(t, root, "add", "a.txt")
	request := gitRequest(t, r, "commit")
	gitFile(t, root, "b.txt", "other\n")
	gitFixture(t, root, "add", "b.txt")
	if _, err := r.ExecuteGit(t.Context(), request); !protocol.IsCode(err, protocol.CodeConflict) {
		t.Fatalf("stale index accepted: %v", err)
	}
	request = gitRequest(t, r, "commit")
	if _, err := r.ExecuteGit(t.Context(), request); !protocol.IsCode(err, protocol.CodeInvalidArgument) {
		t.Fatalf("additional staged paths accepted: %v", err)
	}
	gitFixture(t, root, "reset", "--", "b.txt")
	gitFile(t, root, ".gitignore", "hidden.txt\n")
	gitFile(t, root, "hidden.txt", "hidden\n")
	gitFixture(t, root, "add", "-f", "hidden.txt")
	if _, err := r.ExecuteGit(t.Context(), gitRequest(t, r, "commit")); !protocol.IsCode(err, protocol.CodeInvalidArgument) {
		t.Fatalf("force-added ignored file accepted: %v", err)
	}
	if got := gitFixture(t, root, "rev-list", "--count", "HEAD"); got != "1" {
		t.Fatalf("rejected request changed HEAD: %s", got)
	}
}

func TestDirectGitHonorsPolicyAndWorkspaceAdmission(t *testing.T) {
	r, root := newGitRuntime(t, true)
	gitFile(t, root, "a.txt", "after\n")
	gitFixture(t, root, "add", "a.txt")
	request := gitRequest(t, r, "commit")
	factory := r.opts.GitControl.NewGuard
	r.opts.GitControl.NewGuard = func(ctx context.Context, profile protocol.SessionProfile) (*guard.Guard, error) {
		g, err := factory(ctx, profile)
		if err == nil {
			g.Policy().SetPermission(policy.PermissionNever)
		}
		return g, err
	}
	result, err := r.ExecuteGit(t.Context(), request)
	if err != nil || result.Problem == nil || len(result.Completed) != 0 {
		t.Fatalf("read-only policy bypassed: %+v %v", result, err)
	}
	r.sessionLifecycle = &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session", ThreadID: "thread", Title: "Git",
		Status: protocol.SessionStatusIdle, Isolation: "shared", WorkspaceRoot: root,
		ExecutionTarget: "local", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	release, err := r.beginWorkspaceOperation()
	if err != nil {
		t.Fatal(err)
	}
	summary, err := r.SessionStatus(t.Context(), "session")
	if err != nil || summary.Status != protocol.SessionStatusIdle {
		t.Errorf("direct Git changed Session activity: %+v %v", summary, err)
	}
	archived := true
	if _, err := r.UpdateSessionLifecycle(t.Context(), "session", 1,
		protocol.SessionLifecyclePatch{Archived: &archived}); !protocol.IsCode(err, protocol.CodeConflict) {
		t.Errorf("Session archived during direct Git: %v", err)
	}
	if _, err := r.DeleteSession(t.Context(), "session", 1); !protocol.IsCode(err, protocol.CodeConflict) {
		t.Errorf("Session deleted during direct Git: %v", err)
	}
	if _, err := r.DiscardSession(t.Context(), "session", 1); !protocol.IsCode(err, protocol.CodeConflict) {
		t.Errorf("Session discarded during direct Git: %v", err)
	}
	if err := r.Submit(t.Context(), startOperation(t, 1)); !protocol.IsCode(err, protocol.CodeConflict) {
		t.Fatalf("Turn admitted during direct Git: %v", err)
	}
	if _, err := r.active.Reserve("child", "turn", "op", "item"); !protocol.IsCode(err, protocol.CodeConflict) {
		t.Fatalf("child admitted during direct Git: %v", err)
	}
	if _, err := r.ExecuteGit(t.Context(), request); !protocol.IsCode(err, protocol.CodeConflict) {
		t.Fatalf("concurrent direct Git admitted: %v", err)
	}
	release()
	lease, err := r.active.Reserve("active", "turn", "op", "item")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ExecuteGit(t.Context(), request); !protocol.IsCode(err, protocol.CodeConflict) {
		t.Fatalf("direct Git admitted during a Turn: %v", err)
	}
	_ = r.active.Release(lease)
	r.TurnQueueService.mu.Lock()
	r.TurnQueueService.items["queued"] = protocol.QueuedTurn{
		QueueID: "queued", ThreadID: "waiting", Prompt: "next task",
	}
	r.TurnQueueService.mu.Unlock()
	if _, err := r.ExecuteGit(t.Context(), request); !protocol.IsCode(err, protocol.CodeConflict) {
		t.Fatalf("direct Git admitted ahead of queued work: %v", err)
	}
	if len(r.TurnQueueService.snapshotMap()) != 1 {
		t.Error("direct Git consumed queued work")
	}
}

func TestDirectGitShutdownCancelsWithoutStartingAnEffect(t *testing.T) {
	r, root := newGitRuntime(t, true)
	gitFile(t, root, "a.txt", "after\n")
	gitFixture(t, root, "add", "a.txt")
	request := gitRequest(t, r, "commit")
	started := make(chan struct{})
	done := make(chan error, 1)
	r.opts.GitControl.NewGuard = func(ctx context.Context, _ protocol.SessionProfile) (*guard.Guard, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	go func() {
		_, err := r.ExecuteGit(context.Background(), request)
		done <- err
	}()
	<-started
	if err := r.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != context.Canceled {
		t.Fatalf("operation was not canceled: %v", err)
	}
	if got := gitFixture(t, root, "rev-list", "--count", "HEAD"); got != "1" {
		t.Fatalf("shutdown created a commit: %s", got)
	}
}
