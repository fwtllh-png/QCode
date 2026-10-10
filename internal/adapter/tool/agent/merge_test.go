package agent_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agenttool "github.com/fwtllh-png/QCode/internal/adapter/tool/agent"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/handle"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type fixedWorktrees struct {
	path       string
	baseRev    string
	serialized bool
}

func (f fixedWorktrees) Provision(agentID string, _ subagent.Stance) (subagent.Worktree, error) {
	return subagent.Worktree{
		ID: agentID, Path: f.path, Isolated: !f.serialized,
		BaseRev: f.baseRev, Serialized: f.serialized,
	}, nil
}

func (f fixedWorktrees) Discard(subagent.Worktree) error { return nil }

func newMergeFixture(t *testing.T) (workspace, worktree, baseRev string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	workspace = t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = resolved
	}
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspace, "init", "--quiet")
	runGit(t, workspace, "config", "user.email", "fixture@example.com")
	runGit(t, workspace, "config", "user.name", "Fixture")
	runGit(t, workspace, "config", "commit.gpgsign", "false")
	runGit(t, workspace, "add", "README.md")
	runGit(t, workspace, "commit", "--quiet", "-m", "seed")
	baseRev = strings.TrimSpace(runGitOutput(t, workspace, "rev-parse", "HEAD"))
	worktree = filepath.Join(workspace, ".qcode", "child")
	runGit(t, workspace, "worktree", "add", "--detach", worktree, "HEAD")
	if err := os.WriteFile(filepath.Join(worktree, "child-note.txt"), []byte("from-child\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return workspace, worktree, baseRev
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out := runGitOutput(t, dir, args...); false {
		_ = out
	}
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=")
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func openMergeHarness(t *testing.T) (
	*tool.Registry, *toolguard.Guard, *subagent.Manager, string, string,
) {
	return openMergeHarnessWithVerifier(t)
}

func openMergeHarnessWithVerifier(
	t *testing.T,
) (*tool.Registry, *toolguard.Guard, *subagent.Manager, string, string) {
	t.Helper()
	workspace, worktree, baseRev := newMergeFixture(t)
	sandboxPolicy, err := sandbox.BuildPolicy(sandbox.Options{
		WorkspaceRoot: workspace,
		HostReadRoots: []string{worktree},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sandboxPolicy.PrivateTemp) })
	backend := mergeTestBackend{policy: sandboxPolicy}
	files, err := filetool.NewWithBackend(workspace, backend)
	if err != nil {
		t.Fatal(err)
	}
	handles := handle.NewStore()
	registry := tool.NewRegistry(nil, nil)
	registry.SetSandboxBackend(backend)
	gate := &recordingGate{}
	manager, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: gate, Runtime: &dualRuntime{},

		Worktrees: fixedWorktrees{path: worktree, baseRev: baseRev}, Budget: subagent.Budget{MaxDepth: 3, MaxParallel: 4}, SessionID: "merge-session",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := agenttool.Register(registry, agenttool.Options{
		Manager: manager, Handles: handles,
		Root: t.TempDir(), Gate: gate, Files: files,

		Sandbox: backend, Budget: subagent.Budget{MaxDepth: 3, MaxParallel: 4}, Workspace: workspace, SessionID: "merge-session",
	}); err != nil {
		t.Fatal(err)
	}
	journal, err := workspacejournal.New(workspace, contentstore.NewMemory(contentstore.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Begin("turn-merge"); err != nil {
		t.Fatal(err)
	}
	security := policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
	guard, err := toolguard.New(toolguard.Options{
		Registry: registry, Policy: security, Workspace: workspace, Journal: journal,
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry, guard, manager, workspace, worktree
}

type mergeTestBackend struct {
	policy sandbox.Policy
}

func (mergeTestBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "test", Backend: "passthrough",
		Available: true,
		Effective: securitymodel.Controls{FilesystemRead: securitymodel.FilesystemReadDeclaredRoots,

			FilesystemWrite: securitymodel.FilesystemWriteExactPaths,

			Network: securitymodel.NetworkDenied, ProcessTree: securitymodel.ProcessTreeGroupKill, CrossProcess: securitymodel.CrossProcessUnrestricted,
			Syscall: securitymodel.SyscallDenyDangerous, IPC: securitymodel.IPCUnrestricted, PathIdentity: securitymodel.PathIdentityDescriptorRelative,
			ArtifactOrigin:  securitymodel.ArtifactOriginUnverifiedPath,
			DurableRecovery: securitymodel.DurableRecoveryMemoryOnly,
		},
	}
}

func (b mergeTestBackend) Policy() sandbox.Policy { return b.policy }

func (b mergeTestBackend) Prepare(
	_ context.Context,
	command sandbox.Command,
) (sandbox.Command, error) {
	command.PreparedPolicyID = b.policy.ID
	command.PreparedAuthorityDigest = command.AuthorityDigest
	var err error
	command.PreparedControls, err = sandbox.CommandControls(
		b.Capability(), b.policy, command,
	)
	if err != nil {
		return sandbox.Command{}, err
	}
	command.PreparedReadOnly = command.WorkspaceReadOnly
	command.PreparedReadPaths = append(
		[]string(nil),
		command.AdditionalReadPaths...,
	)
	command.PreparedWritePaths = append(
		[]string(nil),
		command.WorkspaceWritePaths...,
	)
	command.PreparedNetworkDenied = command.DenyNetwork
	command.PreparedLoopbackAllowed = command.AllowLoopback
	return command, nil
}

func settleWritingChild(t *testing.T, manager *subagent.Manager) string {
	t.Helper()
	child, err := manager.Spawn("", subagent.RoleImplementer, "write")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Settle(subagent.Result{
		AgentID: child.ID, ThreadID: subagent.ThreadIDFor(child.ID),
		TurnID: "turn-1", Status: subagent.StatusCompleted, Summary: "wrote note",
		Diff: []protocol.ReceiptChange{{
			Path: "child-note.txt", Tool: "file_write", Kind: "created", Added: 1,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	return child.ID
}

func TestAgentMergePreviewAndApply(t *testing.T) {
	_, guard, manager, workspace, _ := openMergeHarness(t)
	agentID := settleWritingChild(t, manager)

	result, err := guard.Execute(context.Background(), "merge-1", "integrate_agent", mustJSON(map[string]any{
		"agent_id": agentID, "op": "preview",
	}))
	if err != nil {
		t.Fatal(err)
	}
	digest, ok := result.Metadata["preview_digest"].(string)
	if !ok || len(digest) != 64 {
		t.Fatalf("preview digest = %#v", result.Metadata["preview_digest"])
	}
	if _, err := os.Stat(filepath.Join(workspace, "child-note.txt")); !os.IsNotExist(err) {
		t.Fatal("preview must not write the parent workspace")
	}

	result, err = guard.Execute(context.Background(), "merge-2", "integrate_agent", mustJSON(map[string]any{
		"agent_id": agentID, "op": "apply", "preview_digest": digest,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Metadata["integration_status"] != string(subagent.IntegrationApplied) {
		t.Fatalf("apply metadata = %#v", result.Metadata)
	}
	body, err := os.ReadFile(filepath.Join(workspace, "child-note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "from-child\n" {
		t.Fatalf("parent content = %q", body)
	}
	changes := result.Outcome.Facts.WorkspaceChanges
	if len(changes) == 0 {
		t.Fatalf("expected typed changes for turnDiff, got %#v", result.Outcome)
	}
}

func TestAgentMergeUsesLastSuccessfulResultAfterFailedFollowUp(t *testing.T) {
	_, guard, manager, workspace, _ := openMergeHarness(t)
	agentID := settleWritingChild(t, manager)
	if _, err := manager.FollowUp(t.Context(), agentID, "recheck"); err != nil {
		t.Fatal(err)
	}
	current, ok := manager.Agent(agentID)
	if !ok {
		t.Fatal("writing Agent disappeared")
	}
	if err := manager.Settle(subagent.Result{
		AgentID: agentID, ThreadID: current.ThreadID,
		TurnID: "turn-follow-up", Status: subagent.StatusFailed,
		Summary: "follow-up failed",
	}); err != nil {
		t.Fatal(err)
	}
	if result, ok := manager.Result(agentID); !ok ||
		result.Status != subagent.StatusFailed {
		t.Fatalf("current Result = %+v, ok=%v", result, ok)
	}
	baseline, ok := manager.IntegrationResult(agentID)
	if !ok || baseline.Status != subagent.StatusCompleted ||
		baseline.TurnID != "turn-1" {
		t.Fatalf("Integration Result = %+v, ok=%v", baseline, ok)
	}
	preview, err := guard.Execute(
		t.Context(), "merge-after-failed-follow-up", "integrate_agent",
		mustJSON(map[string]any{"agent_id": agentID, "op": "preview"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := preview.Metadata["preview_digest"].(string)
	if digest == "" {
		t.Fatalf("Preview = %#v", preview.Metadata)
	}
	if _, err := guard.Execute(
		t.Context(), "apply-after-failed-follow-up", "integrate_agent",
		mustJSON(map[string]any{
			"agent_id": agentID, "op": "apply", "preview_digest": digest,
		}),
	); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(workspace, "child-note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "from-child\n" {
		t.Fatalf("integrated content = %q", body)
	}
}

func TestIntegrateAgentUsesGuardExpansion(t *testing.T) {
	_, guard, manager, workspace, _ := openMergeHarness(t)
	agentID := settleWritingChild(t, manager)
	guard.Policy().User = []policy.Rule{{
		Tool: "integrate_agent", Resource: "*", Action: policy.ActionAllow,
	}}

	result, err := guard.Execute(context.Background(), "merge-guarded", "integrate_agent", mustJSON(map[string]any{
		"agent_id": agentID, "op": "preview",
	}))
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := result.Metadata["preview_digest"].(string)
	result, err = guard.Execute(context.Background(), "merge-guarded-apply", "integrate_agent", mustJSON(map[string]any{
		"agent_id": agentID, "op": "apply", "preview_digest": digest,
	}))
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(workspace, "child-note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "from-child\n" {
		t.Fatalf("parent content = %q", body)
	}
	changes := result.Outcome.Facts.WorkspaceChanges
	if len(changes) == 0 {
		t.Fatalf("integration bypassed guarded file changes: %#v", result.Metadata)
	}
}

func TestAgentMergeDoesNotFabricateVerification(t *testing.T) {
	_, guard, manager, _, _ := openMergeHarnessWithVerifier(
		t,
	)
	agentID := settleWritingChild(t, manager)
	preview, err := guard.Execute(
		context.Background(), "merge-verify-preview", "integrate_agent",
		mustJSON(map[string]any{"agent_id": agentID, "op": "preview"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := preview.Metadata["preview_digest"].(string)
	applied, err := guard.Execute(
		context.Background(), "merge-verify-apply", "integrate_agent",
		mustJSON(map[string]any{
			"agent_id": agentID, "op": "apply", "preview_digest": digest,
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	receipt, ok := applied.Metadata["integration_receipt"].(subagent.IntegrationReceipt)
	if !ok || receipt.Verification != (protocol.ReceiptVerification{}) {
		t.Fatalf("integration receipt = %#v", applied.Metadata["integration_receipt"])
	}
}

func TestAgentMergeClaimConflict(t *testing.T) {
	_, guard, manager, _, _ := openMergeHarness(t)
	first := settleWritingChild(t, manager)
	second, err := manager.Spawn("", subagent.RoleImplementer, "write")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Settle(subagent.Result{
		AgentID: second.ID, ThreadID: subagent.ThreadIDFor(second.ID),
		TurnID: "turn-2", Status: subagent.StatusCompleted,
		Diff: []protocol.ReceiptChange{{
			Path: "child-note.txt", Tool: "file_write", Kind: "created",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := guard.Execute(context.Background(), "merge-conflict", "integrate_agent", mustJSON(map[string]any{
		"agent_id": second.ID, "op": "preview",
	}))
	if err != nil {
		t.Fatal(err)
	}
	conflicts, _ := result.Metadata["conflicts"].([]string)
	if len(conflicts) == 0 || !strings.Contains(conflicts[0], "child-note.txt") {
		t.Fatalf("conflicts = %#v", result.Metadata["conflicts"])
	}
	digest, _ := result.Metadata["preview_digest"].(string)
	_, err = guard.Execute(context.Background(), "merge-conflict-apply", "integrate_agent", mustJSON(map[string]any{
		"agent_id": second.ID, "op": "apply", "preview_digest": digest,
	}))
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("second apply conflict error = %v", err)
	}
	// First child still owns the claim and can merge.
	if _, err := guard.Execute(context.Background(), "merge-ok", "integrate_agent", mustJSON(map[string]any{
		"agent_id": first, "op": "preview",
	})); err != nil {
		t.Fatal(err)
	}
}

func TestAgentMergeParentDriftInvalidatesPreview(t *testing.T) {
	_, guard, manager, workspace, _ := openMergeHarness(t)
	agentID := settleWritingChild(t, manager)
	preview, err := guard.Execute(context.Background(), "merge-preview", "integrate_agent", mustJSON(map[string]any{
		"agent_id": agentID, "op": "preview",
	}))
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := preview.Metadata["preview_digest"].(string)
	if err := os.WriteFile(filepath.Join(workspace, "child-note.txt"), []byte("parent-drift\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = guard.Execute(context.Background(), "merge-drift", "integrate_agent", mustJSON(map[string]any{
		"agent_id": agentID, "op": "apply", "preview_digest": digest,
	}))
	if err == nil {
		t.Fatal("apply must fail when parent drifted after preview")
	}
	if !strings.Contains(err.Error(), "drift") && !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("drift error = %v", err)
	}
}

func TestAgentMergeChildDriftInvalidatesPreview(t *testing.T) {
	_, guard, manager, _, worktree := openMergeHarness(t)
	agentID := settleWritingChild(t, manager)
	preview, err := guard.Execute(context.Background(), "merge-preview", "integrate_agent", mustJSON(map[string]any{
		"agent_id": agentID, "op": "preview",
	}))
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := preview.Metadata["preview_digest"].(string)
	if err := os.WriteFile(
		filepath.Join(worktree, "child-note.txt"), []byte("changed-after-preview\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	_, err = guard.Execute(context.Background(), "merge-stale-child", "integrate_agent", mustJSON(map[string]any{
		"agent_id": agentID, "op": "apply", "preview_digest": digest,
	}))
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale child error = %v", err)
	}
}

func TestAgentMergeAfterClose(t *testing.T) {
	_, guard, manager, _, _ := openMergeHarness(t)
	agentID := settleWritingChild(t, manager)
	if err := manager.Close(agentID); err != nil {
		t.Fatal(err)
	}
	_, err := guard.Execute(context.Background(), "merge-closed", "integrate_agent", mustJSON(map[string]any{
		"agent_id": agentID, "op": "preview",
	}))
	if err == nil {
		t.Fatal("merge after close must fail")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("close error = %v", err)
	}
}

func TestAgentMergeDiscardClosesChild(t *testing.T) {
	_, guard, manager, _, _ := openMergeHarness(t)
	agentID := settleWritingChild(t, manager)
	preview, err := guard.Execute(context.Background(), "merge-preview", "integrate_agent", mustJSON(map[string]any{
		"agent_id": agentID, "op": "preview",
	}))
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := preview.Metadata["preview_digest"].(string)
	result, err := guard.Execute(context.Background(), "merge-discard", "integrate_agent", mustJSON(map[string]any{
		"agent_id": agentID, "op": "discard", "preview_digest": digest,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Metadata["integration_status"] != string(subagent.IntegrationDiscarded) {
		t.Fatalf("discard metadata = %#v", result.Metadata)
	}
	if _, ok := manager.Agent(agentID); ok {
		t.Fatal("discarded agent must no longer be available")
	}
}

func TestAgentMergeRetryCreatesNewDigest(t *testing.T) {
	_, guard, manager, _, _ := openMergeHarness(t)
	agentID := settleWritingChild(t, manager)
	preview, err := guard.Execute(context.Background(), "merge-preview", "integrate_agent", mustJSON(map[string]any{
		"agent_id": agentID, "op": "preview",
	}))
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, _ := preview.Metadata["preview_digest"].(string)
	candidate, ok, err := manager.Integration(agentID, firstDigest)
	if err != nil || !ok {
		t.Fatalf("load candidate: ok=%v err=%v", ok, err)
	}
	candidate.Status = subagent.IntegrationApplying
	if err := manager.SaveIntegration(candidate); err != nil {
		t.Fatal(err)
	}
	candidate.Status = subagent.IntegrationFailed
	if err := manager.SaveIntegration(candidate); err != nil {
		t.Fatal(err)
	}
	retry, err := guard.Execute(context.Background(), "merge-retry", "integrate_agent", mustJSON(map[string]any{
		"agent_id": agentID, "op": "retry", "preview_digest": firstDigest,
	}))
	if err != nil {
		t.Fatal(err)
	}
	retryDigest, _ := retry.Metadata["preview_digest"].(string)
	if retryDigest == "" || retryDigest == firstDigest {
		t.Fatalf("retry digest = %q, first = %q", retryDigest, firstDigest)
	}
}

func TestGitBlobHashMatchesWorkspaceJournal(t *testing.T) {
	// Sanity: baseline hashing agrees with journal fingerprints for README.
	workspace, worktree, baseRev := newMergeFixture(t)
	body, err := os.ReadFile(filepath.Join(worktree, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])
	fp, _, _, err := workspacejournal.Snapshot(filepath.Join(workspace, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !fp.Exists || fp.SHA256 != want {
		t.Fatalf("parent fingerprint = %+v want %s", fp, want)
	}
	_ = baseRev
}
