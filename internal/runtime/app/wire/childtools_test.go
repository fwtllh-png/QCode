package wire

import (
	"context"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
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

func TestRuntimeCoreBuilderFailsClosedWithoutChildToolsets(t *testing.T) {
	_, err := (runtimeCoreBuilder{}).BuildChild(app.ChildSpec{
		Workspace:     t.TempDir(),
		HostWorkspace: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "child toolsets") {
		t.Fatalf("BuildChild error = %v", err)
	}
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

func openWritingChildSession(t *testing.T, workspace string) *Session {
	t.Helper()
	tools := true
	session, err := NewExec(context.Background(), withNonDurableTestJournal(t, ExecOptions{
		FixturePath: subagentFixture(t, "subagent-write"), Permission: "bypass",
		ConfigOverrides: config.Overrides{Tools: &tools, Workspace: &workspace},
	}))
	if err != nil {
		t.Fatalf("NewExec: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = session.Close(ctx)
	})
	return session
}

// runWritingChild spawns an implementer child, runs one turn, and returns the
// settled result together with the worktree it was given.
func runWritingChild(t *testing.T, session *Session) (subagent.Result, string) {
	t.Helper()
	manager := session.subagents
	child, err := manager.Spawn("", subagent.RoleImplementer, "write the note")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, err := manager.Takeover(context.Background(), child.ID, "write the note"); err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	waited, err := manager.Wait(ctx, []string{child.ID}, 25*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if waited.TimedOut {
		t.Fatal("writing child never reached a terminal status")
	}
	result, ok := manager.Result(child.ID)
	if !ok {
		t.Fatal("terminal writing child has no structured result")
	}
	return result, child.Worktree
}

func TestWritingChildWritesOnlyInsideItsWorktree(t *testing.T) {
	workspace := newGitWorkspace(t)
	session := openWritingChildSession(t, workspace)
	result, worktree := runWritingChild(t, session)

	if result.Status != subagent.StatusCompleted {
		t.Fatalf("result = %+v", result)
	}
	// The child's own receipt is what proves the write happened, and the path in
	// it is relative to the child's root rather than the host workspace.
	if paths := result.WritePaths(); len(paths) != 1 || paths[0] != "child-note.txt" {
		t.Fatalf("write paths = %v", paths)
	}
	if _, err := os.Stat(filepath.Join(worktree, "child-note.txt")); err != nil {
		t.Fatalf("child wrote nothing in its worktree: %v", err)
	}
	// This is the whole point of isolation: the host workspace is untouched.
	if _, err := os.Stat(filepath.Join(workspace, "child-note.txt")); !os.IsNotExist(err) {
		t.Fatalf("child reached the host workspace: err = %v", err)
	}
	// A git worktree, not a scratch directory that merely looks like one.
	if info, err := os.Stat(filepath.Join(worktree, ".git")); err != nil || info.IsDir() {
		t.Fatalf("worktree is not a git worktree: info = %v err = %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "README.md")); err != nil {
		t.Fatalf("worktree has no checkout of HEAD: %v", err)
	}
	agent, ok := session.subagents.Agent(result.AgentID)
	if !ok || agent.BaseRev == "" {
		t.Fatalf("writing child missing BaseRev: ok=%v agent=%+v", ok, agent)
	}
}

func TestTwoWritingChildrenOnTheSamePathConflict(t *testing.T) {
	workspace := newGitWorkspace(t)
	session := openWritingChildSession(t, workspace)

	first, _ := runWritingChild(t, session)
	if len(first.Unresolved) != 0 {
		t.Fatalf("first child reported unresolved issues: %v", first.Unresolved)
	}
	second, _ := runWritingChild(t, session)
	if second.Status != subagent.StatusCompleted {
		t.Fatalf("second result = %+v", second)
	}
	// Both children wrote child-note.txt in their own worktrees. Merging either
	// one is fine; merging both silently is not, so the second child's result has
	// to name the conflict rather than let the later merge win.
	if !unresolvedContains(second, "child-note.txt") {
		t.Fatalf("second child did not report the conflict: %v", second.Unresolved)
	}
	if owner, ok := session.subagents.WriteOwner("child-note.txt"); !ok || owner != "agent-1" {
		t.Fatalf("write owner = %q ok = %v", owner, ok)
	}
}

func TestWorktreeStrategyRequiresGitWorkspace(t *testing.T) {
	workspace := t.TempDir()
	tools := true
	strategy := config.SubagentWorkspaceWorktree
	_, err := NewExec(context.Background(), withNonDurableTestJournal(t, ExecOptions{
		FixturePath: subagentFixture(t, "subagent"), Permission: "bypass",
		ConfigOverrides: config.Overrides{
			Tools: &tools, Workspace: &workspace, SubagentWorkspace: &strategy,
		},
	}))
	// Asking for worktree isolation in a workspace that cannot provide it is a
	// startup error: discovering it at the first spawn would be a session that
	// looked healthy and was not.
	if err == nil {
		t.Fatal("worktree isolation must not be accepted without a git work tree")
	}
	if !strings.Contains(err.Error(), "execution.subagent.workspace") {
		t.Fatalf("error does not name the setting: %v", err)
	}
}

func TestWritingChildRejectedWithoutGitWorkspace(t *testing.T) {
	// The default auto strategy cannot isolate a writing child here, so the child
	// is refused before it exists rather than pointed at the host workspace.
	session := openChildSession(t, "subagent", nil)
	_, err := session.subagents.Spawn("", subagent.RoleImplementer, "write the note")
	if err == nil {
		t.Fatal("a writing child must not be spawned without isolation")
	}
	if !protocol.IsCode(err, protocol.CodeUnavailable) {
		t.Fatalf("Spawn error = %v (want unavailable)", err)
	}
	if !strings.Contains(err.Error(), config.SubagentWorkspaceReadOnly) {
		t.Fatalf("error does not offer the read-only alternative: %v", err)
	}
}

func TestSerializedWritingChildUsesHostWorkspaceWithoutGit(t *testing.T) {
	workspace := t.TempDir()
	tools := true
	strategy := config.SubagentWorkspaceSerialized
	session, err := NewExec(context.Background(), withNonDurableTestJournal(t, ExecOptions{
		FixturePath: subagentFixture(t, "subagent-write"), Permission: "bypass",
		ConfigOverrides: config.Overrides{
			Tools: &tools, Workspace: &workspace, SubagentWorkspace: &strategy,
		},
	}))
	if err != nil {
		t.Fatalf("NewExec: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = session.Close(ctx)
	})
	result, childWorkspace := runWritingChild(t, session)
	if result.Status != subagent.StatusCompleted || childWorkspace != workspace {
		t.Fatalf("result=%+v child workspace=%q", result, childWorkspace)
	}
	if _, err := os.Stat(filepath.Join(workspace, "child-note.txt")); err != nil {
		t.Fatalf("serialized child did not write host workspace: %v", err)
	}
	agent, ok := session.subagents.Agent(result.AgentID)
	if !ok || !agent.Serialized || agent.Isolated {
		t.Fatalf("serialized agent = %+v ok=%v", agent, ok)
	}
	if err := session.subagents.Close(result.AgentID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("closing serialized child removed host workspace: %v", err)
	}
}

func TestChildEngineOptionsOnlySharesHostJournalUnderSerializedStrategy(t *testing.T) {
	gate := agentengine.NewWorkspaceTurnGate()
	journal := new(workspacejournal.Manager)
	seed := agentengine.Options{SecurityConfig: agentengine.SecurityConfig{Journal: journal, WorkspaceTurnGate: gate}}

	serialized := childEngineOptions(seed, app.ChildSpec{Serialized: true})
	if serialized.Journal != journal || serialized.WorkspaceTurnGate != gate ||
		serialized.ReadTracker == nil {
		t.Fatalf("serialized child options = %+v", serialized)
	}

	isolated := childEngineOptions(seed, app.ChildSpec{Workspace: t.TempDir()})
	if isolated.Journal != nil || isolated.WorkspaceTurnGate != nil {
		t.Fatalf("isolated child inherited host serialization: %+v", isolated)
	}

	readOnly := childEngineOptions(
		seed, app.ChildSpec{Serialized: true, ReadOnly: true},
	)
	if readOnly.Journal != nil || readOnly.WorkspaceTurnGate != gate {
		t.Fatalf("serialized read-only child options = %+v", readOnly)
	}
}

func TestChildrenNeverInheritHostExecution(t *testing.T) {
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
	seed := agentengine.Options{SecurityConfig: agentengine.SecurityConfig{Security: runtime}}
	for _, spec := range []app.ChildSpec{{Serialized: true}, {Workspace: t.TempDir()}, {ReadOnly: true}} {
		child := childEngineOptions(seed, spec)
		if !child.Security.DisableHostExecution {
			t.Fatal("child inherited host execution authority")
		}
	}
	if runtime.DisableHostExecution {
		t.Fatal("child construction restricted parent host execution")
	}
}

func TestChildEngineOptionsSharesProviderLimiter(t *testing.T) {
	shared := &agentengine.SharedRateLimit{}
	seed := agentengine.Options{ProviderConfig: agentengine.ProviderConfig{
		SharedRateLimit: shared, RateLimitMaxRetries: 2,
	}}
	child := childEngineOptions(seed, app.ChildSpec{ReadOnly: true})
	if child.SharedRateLimit != shared {
		t.Fatal("child engine must share the session provider limiter")
	}
}

func TestChildEngineOptionsUsesOwningSession(t *testing.T) {
	seed := agentengine.Options{LifecycleConfig: agentengine.LifecycleConfig{
		SessionID: "process-internal",
	}}
	options := childEngineOptions(seed, app.ChildSpec{SessionID: "session-user"})
	if options.SessionID != "session-user" {
		t.Fatalf("child session = %q, want owning session", options.SessionID)
	}
}
