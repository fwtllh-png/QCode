package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/orchestration/execsettle"
	"github.com/fwtllh-png/QCode/internal/orchestration/workspacebroker"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
)

func TestExistingWriteTreesIgnoresFilesAndWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "generated"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	trees := existingWriteTrees(workspace, []string{"generated", "file.txt", ".", "missing"})
	if len(trees) != 1 || trees[0] != "generated" {
		t.Fatalf("trees = %#v", trees)
	}
}

func TestRequiredWriteIsolationDoesNotFallBackAfterTreeDisappears(t *testing.T) {
	workspace, err := sandbox.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &commandProtocol{workspace: workspace}
	ctx := tool.RequireWriteIsolation(t.Context())
	if _, degraded, err := p.beginIsolatedCommand(ctx, []string{"missing"}, false); err == nil || degraded {
		t.Fatalf("required isolation fell back: degraded=%v error=%v", degraded, err)
	}
}

type failingShellIsolator struct{ err error }

func (f failingShellIsolator) Begin(context.Context, string, []string) (tool.IsolatedWorkspace, error) {
	return nil, f.err
}

func (f failingShellIsolator) BeginShadow(ctx context.Context, id string, paths []string) (tool.IsolatedWorkspace, error) {
	return f.Begin(ctx, id, paths)
}

func TestIsolationPreparationFailureNeverRunsInParent(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "generated"), 0o700); err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: root, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	registry.SetSandboxBackend(backend)
	if err := RegisterWithManagerAndBackend(registry, root, manager, backend); err != nil {
		t.Fatal(err)
	}
	g, err := toolguard.New(toolguard.Options{Workspace: root, Registry: registry,
		Policy:   policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		Isolator: failingShellIsolator{err: errors.New("snapshot unavailable")},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Execute(t.Context(), "failed-isolation", "exec_command", json.RawMessage(`{"command":"echo changed > generated/out.txt","write_paths":["generated"]}`))
	if !errors.Is(err, tool.ErrPrecondition) {
		t.Fatalf("preparation error=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "generated", "out.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed preparation changed parent: %v", err)
	}
}

func TestExecCommandIsolatedTreeWriteDoesNotTakeUserEdit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "generated"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "user.txt"), []byte("parent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: workspace, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, workspace, manager, backend); err != nil {
		t.Fatal(err)
	}
	isolator := newShellIsolator(t, workspace, backend)
	ctx := tool.WithIsolator(
		tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{
			SessionID: "session-isolate",
			ThreadID:  processTestThread,
			TurnID:    "turn-isolate",
			CallID:    "call-isolate",
		}),
		isolator,
	)
	if err := os.WriteFile(filepath.Join(workspace, "user.txt"), []byte("user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"command":       "printf 'generated\\n' > generated/out.txt",
		"write_paths":   []string{"generated"},
		"yield_time_ms": 5000,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(ctx, registry, tool.Call{
		Name: "exec_command", Arguments: raw,
	})
	if err != nil || result.IsError {
		t.Fatalf("isolated exec_command: result=%+v err=%v", result, err)
	}
	cwd, _ := result.Metadata["isolated_cwd"].(string)
	if cwd == "" || cwd == workspace {
		t.Fatalf("isolated_cwd = %q workspace = %q", cwd, workspace)
	}
	if result.Metadata["workspace_settlement"] != "isolated_three_way" {
		t.Fatalf("settlement = %#v", result.Metadata["workspace_settlement"])
	}
	if body, err := os.ReadFile(filepath.Join(workspace, "user.txt")); err != nil ||
		string(body) != "user\n" {
		t.Fatalf("user.txt = %q err=%v", body, err)
	}
	if body, err := os.ReadFile(filepath.Join(workspace, "generated", "out.txt")); err != nil ||
		string(body) != "generated\n" {
		t.Fatalf("generated/out.txt = %q err=%v", body, err)
	}
}

func newShellIsolator(t *testing.T, workspace string, backend sandbox.Backend) execsettle.Isolator {
	t.Helper()
	leases := authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{})
	brokers, err := workspacebroker.New(workspace, leases, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := filetool.NewWithBackend(workspace, backend)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := workspacejournal.New(workspace, contentstore.NewMemory(contentstore.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close(t.Context()) })
	if err := journal.Begin("shell-isolate"); err != nil {
		t.Fatal(err)
	}
	service := execsettle.New(execsettle.Options{
		Repository: workspace,
		Scratch:    t.TempDir(),
		Parent:     parent,
		Journal:    journal,
		Gate:       agentengine.NewWorkspaceTurnGate(),
		Brokers:    brokers,
		AllowApply: true,
		NewBackend: sandbox.NewPlatformBackend,
	})
	if service == nil {
		t.Fatal("isolator is nil")
	}
	return service
}

func TestExecCommandShadowDiscardKeepsWorkspaceUntouched(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	workspace := t.TempDir()
	module := filepath.Join(workspace, "eds_metaserver")
	if err := os.Mkdir(module, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(module, "go.mod"), []byte("module example.com/eds\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(module, "fence_test.go"), []byte("package meta\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: workspace, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, workspace, manager, backend); err != nil {
		t.Fatal(err)
	}
	isolator := newShellIsolator(t, workspace, backend)
	ctx := tool.WithIsolator(
		tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{
			SessionID: "session-shadow",
			ThreadID:  processTestThread,
			TurnID:    "turn-shadow",
			CallID:    "call-shadow",
		}),
		isolator,
	)
	// The kitc_stub shape: rewrite go.mod and drop a stub inside the module,
	// verify an untouched file, and exit zero — all inside a discarded copy.
	raw, err := json.Marshal(map[string]any{
		"command": "printf '\\nreplace example.com/x => ./stub\\n' >> go.mod; " +
			"mkdir -p stub; printf 'package stub\\n' > stub/stub.go; " +
			"test -f fence_test.go",
		"write_paths":   []string{"eds_metaserver"},
		"settle":        "discard",
		"cwd":           "eds_metaserver",
		"yield_time_ms": 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(ctx, registry, tool.Call{
		Name: "exec_command", Arguments: raw,
	})
	if err != nil || result.IsError {
		t.Fatalf("shadow exec_command: result=%+v err=%v", result, err)
	}
	if result.Metadata["workspace_settlement"] != "shadow_discarded" {
		t.Fatalf("settlement = %#v", result.Metadata["workspace_settlement"])
	}
	if result.Metadata["discarded_changes"] == 0 {
		t.Fatalf("discard summary missing changes: %#v", result.Metadata)
	}
	// Attack: discarded writes must not enter workspace settlement facts —
	// the turn must not count mutations that never landed.
	if facts := result.Outcome.Facts; facts != nil &&
		len(facts.WorkspaceChanges) != 0 {
		t.Fatalf("discarded writes leaked into facts: %+v", facts.WorkspaceChanges)
	}
	if result.Outcome.Facts.Verification != nil {
		t.Fatal("ordinary command produced coverage evidence")
	}
	// The workspace stays byte-identical.
	if body, err := os.ReadFile(filepath.Join(module, "go.mod")); err != nil ||
		string(body) != "module example.com/eds\n" {
		t.Fatalf("go.mod was polluted: %q err=%v", body, err)
	}
	if _, err := os.Stat(filepath.Join(module, "stub")); !os.IsNotExist(err) {
		t.Fatalf("stub leaked into the workspace: %v", err)
	}
}

func TestValidateSettleMode(t *testing.T) {
	if err := validateSettleMode(execCommandInput{Settle: ""}); err != nil {
		t.Fatalf("default settle rejected: %v", err)
	}
	if err := validateSettleMode(execCommandInput{Settle: "apply"}); err != nil {
		t.Fatalf("apply rejected: %v", err)
	}
	if err := validateSettleMode(execCommandInput{Settle: "purge"}); err == nil {
		t.Fatal("unknown settle mode accepted")
	}
	if err := validateSettleMode(execCommandInput{Settle: "discard"}); err == nil {
		t.Fatal("discard without write_paths accepted")
	}
	if err := validateSettleMode(execCommandInput{
		Settle: "discard", WritePaths: []string{"tree"},
	}); err != nil {
		t.Fatalf("discard with write_paths rejected: %v", err)
	}
}

func TestAttachShadowDiscardCapsSummaryPaths(t *testing.T) {
	changes := make([]tool.WorkspaceChange, shadowSummaryMaxPaths+15)
	for index := range changes {
		changes[index] = tool.WorkspaceChange{
			Path: "tree/file" + string(rune('a'+index%26)) + string(rune('0'+index%10)),
			Kind: tool.WorkspaceModified,
		}
	}
	result := tool.Result{}
	attachShadowDiscard(&result, changes)
	if result.Metadata["discarded_changes"] != len(changes) {
		t.Fatalf("count = %v want %d", result.Metadata["discarded_changes"], len(changes))
	}
	paths, _ := result.Metadata["discarded_change_paths"].([]string)
	if len(paths) != shadowSummaryMaxPaths {
		t.Fatalf("summary paths = %d, cap = %d", len(paths), shadowSummaryMaxPaths)
	}
}

func TestExecCommandDiscardWithoutTreeFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: workspace, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, workspace, manager, backend); err != nil {
		t.Fatal(err)
	}
	isolator := newShellIsolator(t, workspace, backend)
	ctx := tool.WithIsolator(
		tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{
			SessionID: "session-discard",
			ThreadID:  processTestThread,
			TurnID:    "turn-discard",
			CallID:    "call-discard",
		}),
		isolator,
	)
	raw, err := json.Marshal(map[string]any{
		"command":     "printf leaked > discard-target.txt",
		"write_paths": []string{"discard-target.txt"},
		"settle":      "discard",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(ctx, registry, tool.Call{
		Name: "exec_command", Arguments: raw,
	})
	if err == nil || !errors.Is(err, tool.ErrPrecondition) {
		t.Fatalf("discard without a tree must fail closed: result=%+v err=%v", result, err)
	}
	if _, statErr := os.Lstat(filepath.Join(workspace, "discard-target.txt")); statErr == nil {
		t.Fatal("failed discard still created a workspace file")
	}
}

func TestExecCommandDiscardWithoutIsolatorFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "generated"), 0o700); err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: workspace, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, workspace, manager, backend); err != nil {
		t.Fatal(err)
	}
	ctx := tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{
		SessionID: "session-discard-noiso",
		ThreadID:  processTestThread,
		TurnID:    "turn-discard-noiso",
		CallID:    "call-discard-noiso",
	})
	raw, err := json.Marshal(map[string]any{
		"command":     "printf leaked > generated/out.txt",
		"write_paths": []string{"generated"},
		"settle":      "discard",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(ctx, registry, tool.Call{
		Name: "exec_command", Arguments: raw,
	})
	if err == nil || !errors.Is(err, tool.ErrPrecondition) {
		t.Fatalf("discard without an isolator must fail closed: result=%+v err=%v", result, err)
	}
	if _, statErr := os.Lstat(filepath.Join(workspace, "generated", "out.txt")); statErr == nil {
		t.Fatal("failed discard still wrote into the real workspace tree")
	}
}

func TestExecCommandApplyDegradedWithoutIsolatorIsReported(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "generated"), 0o700); err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: workspace, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, workspace, manager, backend); err != nil {
		t.Fatal(err)
	}
	ctx := tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{
		SessionID: "session-apply-degraded",
		ThreadID:  processTestThread,
		TurnID:    "turn-apply-degraded",
		CallID:    "call-apply-degraded",
	})
	raw, err := json.Marshal(map[string]any{
		"command":       "printf 'in place\\n' > generated/out.txt",
		"write_paths":   []string{"generated"},
		"yield_time_ms": 5000,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(ctx, registry, tool.Call{
		Name: "exec_command", Arguments: raw,
	})
	if err != nil || result.IsError {
		t.Fatalf("degraded apply exec: result=%+v err=%v", result, err)
	}
	if result.Metadata["workspace_settlement"] != "in_place_degraded" {
		t.Fatalf("settlement metadata = %#v", result.Metadata["workspace_settlement"])
	}
	if result.Metadata["degradation_reason"] != "workspace_isolator_unavailable" {
		t.Fatalf("degradation reason = %#v", result.Metadata["degradation_reason"])
	}
	if body, readErr := os.ReadFile(filepath.Join(workspace, "generated", "out.txt")); readErr != nil ||
		string(body) != "in place\n" {
		t.Fatalf("in-place write missing: %q err=%v", body, readErr)
	}
}

func TestAbandonedIsolatedSessionIsReclaimedOnThreadClose(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "generated"), 0o700); err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: workspace, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, workspace, manager, backend); err != nil {
		t.Fatal(err)
	}
	isolator := newShellIsolator(t, workspace, backend)
	ctx := tool.WithIsolator(
		tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{
			SessionID: "session-abandon",
			ThreadID:  processTestThread,
			TurnID:    "turn-abandon",
			CallID:    "call-abandon",
		}),
		isolator,
	)
	raw, err := json.Marshal(map[string]any{
		"command":       "sleep 30",
		"write_paths":   []string{"generated"},
		"yield_time_ms": 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(ctx, registry, tool.Call{
		Name: "exec_command", Arguments: raw,
	})
	if err != nil || result.IsError {
		t.Fatalf("exec: result=%+v err=%v", result, err)
	}
	sessionID, _ := result.Metadata["session_id"].(string)
	cwd, _ := result.Metadata["isolated_cwd"].(string)
	if sessionID == "" || cwd == "" {
		t.Fatalf("session=%q isolated_cwd=%q", sessionID, cwd)
	}
	if _, err := os.Stat(cwd); err != nil {
		t.Fatalf("isolate root missing while running: %v", err)
	}
	// The turn ends without a final poll: the session's OnClose hook must
	// reclaim the isolated workspace instead of leaking the worktree.
	if _, err := manager.CloseByThread(processTestThread); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(cwd); !os.IsNotExist(err) {
		t.Fatalf("abandoned isolate was not reclaimed: %v", err)
	}
}

func TestGuardedExecCommandBuildsWithLargeIgnoredDependencyTree(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(workspace, "deps")
	if err := os.MkdirAll(filepath.Join(tree, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte("deps/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range sandbox.MaxExactWorkspaceWritePaths + 1 {
		if err := os.WriteFile(filepath.Join(tree, fmt.Sprintf("%04d.txt", i)), []byte("dependency\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: workspace, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	registry.SetSandboxBackend(backend)
	if err := RegisterWithManagerAndBackend(registry, workspace, manager, backend); err != nil {
		t.Fatal(err)
	}
	g, err := toolguard.New(toolguard.Options{Workspace: workspace, Registry: registry,
		Policy:   policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		Isolator: newShellIsolator(t, workspace, backend),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{SessionID: "large-tree", ThreadID: processTestThread, TurnID: "large-tree"})
	raw, err := json.Marshal(map[string]any{
		"command": "cat deps/0000.txt > deps/cache/result.txt && printf 'exact\\n' > exact.txt",
		"cwd":     workspace, "write_paths": []string{tree, filepath.Join(workspace, "exact.txt")}, "yield_time_ms": 30000,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := g.Execute(ctx, "large-tree", "exec_command", raw)
	if err != nil || result.IsError {
		t.Fatalf("guarded command: result=%+v error=%v", result, err)
	}
	if result.Metadata["workspace_settlement"] != "isolated_three_way" {
		t.Fatalf("settlement=%+v", result.Metadata)
	}
	for name, want := range map[string]string{"deps/cache/result.txt": "dependency\n", "exact.txt": "exact\n"} {
		body, err := os.ReadFile(filepath.Join(workspace, name))
		if err != nil || string(body) != want {
			t.Fatalf("%s=%q error=%v", name, body, err)
		}
	}
}

func TestExecCommandDeclaredPathLimitRejectsBeforeApproval(t *testing.T) {
	root := t.TempDir()
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: root, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := RegisterWithManagerAndBackend(registry, root, manager, backend); err != nil {
		t.Fatal(err)
	}
	asked := false
	g, err := toolguard.New(toolguard.Options{Workspace: root, Registry: registry,
		Policy: policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Approvals: func(context.Context, toolguard.ApprovalRequest) error {
			asked = true
			return errors.New("unexpected approval")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, sandbox.MaxExactWorkspaceWritePaths+1)
	for i := range paths {
		paths[i] = fmt.Sprintf("%d.txt", i)
	}
	raw, err := json.Marshal(map[string]any{"command": "true", "write_paths": paths})
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Execute(t.Context(), "too-many-paths", "exec_command", raw)
	if !errors.Is(err, tool.ErrInvalidArguments) || asked {
		t.Fatalf("error=%v approved=%v", err, asked)
	}
}
