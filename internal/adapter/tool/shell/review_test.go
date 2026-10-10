package shell

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/guardian"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func guardianShellFixture(t *testing.T) (string, *toolguard.Guard, context.Context, json.RawMessage) {
	root, guard, _, ctx, raw := guardianShellRegistryFixture(t)
	return root, guard, ctx, raw
}

func guardianShellRegistryFixture(t *testing.T) (string, *toolguard.Guard, *tool.Registry, context.Context, json.RawMessage) {
	return guardianShellRegistryAt(t, t.TempDir())
}

func guardianShellRegistryAt(t *testing.T, directory string) (string, *toolguard.Guard, *tool.Registry, context.Context, json.RawMessage) {
	t.Helper()
	root, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "generated"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "build.sh"), []byte("#!/bin/sh\nprintf 'reviewed\\n' > generated/out.txt\n"), 0700); err != nil {
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
	files, err := filetool.NewWithBackend(root, backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Register(registry); err != nil {
		t.Fatal(err)
	}
	var g *toolguard.Guard
	g, err = toolguard.New(toolguard.Options{Workspace: root, Registry: registry,
		Policy: policy.DefaultRuntime(policy.ModeAct, policy.PermissionAuto), Isolator: newShellIsolator(t, root, backend),
		Approvals: func(_ context.Context, request toolguard.ApprovalRequest) error {
			// Fixture still traverses real pending approval and one-time consent.
			return g.Decide(toolguard.ApprovalDecision{RequestID: request.RequestID, Approved: true, Scope: policy.ApprovalOnce})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{SessionID: "session", ThreadID: "thread", TurnID: "turn", CallID: "call"})
	raw, err := json.Marshal(map[string]any{"command": "./build.sh", "write_paths": []string{"generated"}, "yield_time_ms": 30000})
	if err != nil {
		t.Fatal(err)
	}
	return root, g, registry, ctx, raw
}

func prepareGuardianShell(t *testing.T, g *toolguard.Guard, ctx context.Context, raw json.RawMessage, complete bool) *toolguard.ReviewExecution {
	t.Helper()
	request := toolguard.ReviewContentRequest{AttemptID: "attempt", MaxBytes: 1024}
	if !complete {
		request.Missing = []string{"fixture dependency gap"}
	}
	r, err := g.PrepareGuardianExecution(ctx, "call", "exec_command", raw, tool.CatalogBinding{}, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestGuardianExecutesReviewedCopyAfterSourceScriptChanges(t *testing.T) {
	root, g, ctx, raw := guardianShellFixture(t)
	reviewed := prepareGuardianShell(t, g, ctx, raw, true)
	snapshot := reviewed.Snapshot()
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "build.sh"), []byte("#!/bin/sh\nprintf 'changed\\n' > generated/out.txt\n"), 0700); err != nil {
		t.Fatal(err)
	}
	result, err := g.Execute(tool.WithReviewedExecution(ctx, reviewed), "call", "exec_command", raw)
	if err != nil || result.IsError {
		t.Fatalf("reviewed execution: %+v err=%v", result, err)
	}
	if result.Metadata["isolated_cwd"] != snapshot.Root {
		t.Fatalf("execution created a different copy: %+v", result.Metadata)
	}
	body, err := os.ReadFile(filepath.Join(root, "generated", "out.txt"))
	if err != nil || string(body) != "reviewed\n" {
		t.Fatalf("unreviewed script executed: %q %v", body, err)
	}
	if _, err := os.Stat(snapshot.Root); !os.IsNotExist(err) {
		t.Fatalf("snapshot not reclaimed: %v", err)
	}
}

func TestGuardianSnapshotRejectsDriftBeforeExecution(t *testing.T) {
	for _, kind := range []string{"script", "command", "thread", "coverage", "environment", "scopes"} {
		t.Run(kind, func(t *testing.T) {
			root, g, ctx, raw := guardianShellFixture(t)
			reviewed := prepareGuardianShell(t, g, ctx, raw, kind != "coverage")
			switch kind {
			case "script":
				if err := os.WriteFile(filepath.Join(reviewed.Snapshot().Root, "build.sh"), []byte("changed"), 0700); err != nil {
					t.Fatal(err)
				}
			case "command":
				raw = json.RawMessage(`{"command":"printf changed > generated/out.txt","write_paths":["generated"]}`)
			case "thread":
				ctx = tool.WithInvocationIdentity(ctx, tool.InvocationIdentity{SessionID: "session", ThreadID: "other", TurnID: "turn", CallID: "call"})
			case "environment":
				raw = json.RawMessage(`{"command":"./build.sh","env":{"BUILD_MODE":"changed"},"write_paths":["generated"]}`)
			case "scopes":
				raw = json.RawMessage(`{"command":"./build.sh","write_paths":["generated/out.txt"]}`)
			}
			result, err := g.Execute(tool.WithReviewedExecution(ctx, reviewed), "call", "exec_command", raw)
			if err == nil && !result.IsError {
				t.Fatal("changed or incomplete review executed")
			}
			if _, err := os.Stat(filepath.Join(root, "generated", "out.txt")); !os.IsNotExist(err) {
				t.Fatalf("process executed before rejection: %v", err)
			}
		})
	}
}

func TestGuardianSnapshotSingleConsumerAndCancellation(t *testing.T) {
	_, g, ctx, raw := guardianShellFixture(t)
	reviewed := prepareGuardianShell(t, g, ctx, raw, true)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := reviewed.Begin(canceled, "call", []string{"generated"}); err == nil {
		t.Fatal("canceled copy consumed")
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			workspace, err := reviewed.Begin(ctx, "call", []string{"generated"})
			if err == nil {
				t.Cleanup(func() { _ = workspace.Close() })
			}
			results <- err
		})
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("snapshot consumers=%d", successes)
	}
	second := prepareGuardianShell(t, g, ctx, raw, true)
	if reviewed.Snapshot().ID == second.Snapshot().ID || reviewed.Snapshot().Root == second.Snapshot().Root {
		t.Fatal("different review preparations reused a private copy")
	}
}

func TestGuardianCandidateBindsActualPreparedExecution(t *testing.T) {
	root, g, ctx, raw := guardianShellFixture(t)
	reviewed := prepareGuardianShell(t, g, ctx, raw, true)
	digest := func(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }
	authorization := guardian.AuthorizationSnapshot{
		WorkspaceID: digest(root), SessionID: "session", ThreadID: "thread", Revision: digest("revision"), Complete: true,
		Sources: []guardian.AuthorizationSource{{ID: "source", ThreadID: "thread", TurnID: "turn", Role: "user", Version: 1, Digest: digest("run tests")}},
	}
	versions := guardian.ReviewVersions{ConfigurationDigest: digest("configuration"), RouteDigest: digest("route"), PromptVersion: "v2", SchemaVersion: "v1"}
	candidate, err := reviewed.Candidate(ctx, "review", authorization, versions)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidate.Execution.WritePaths) != 1 || candidate.Execution.WritePaths[0] != "generated" {
		t.Fatal("candidate omitted the actual write scope")
	}
	snapshot := reviewed.Snapshot()
	snapshot.WritePaths[0] = "mutated"
	if reviewed.Snapshot().WritePaths[0] != "generated" {
		t.Fatal("snapshot write scopes alias caller data")
	}
	evidence, err := guardian.BindAssessment(candidate, []byte(`{"risk_level":"low","authorization":"supported","authorization_source_ids":["source"],"recommendation":"allow","rationale":"fixture"}`))
	if err != nil {
		t.Fatal(err)
	}
	current := guardian.PolicyContext{Candidate: &candidate, GuardianEnabled: true, PermissionAuto: true, CurrentAction: "ask"}
	if got := guardian.Evaluate(evidence, guardian.EvidenceInvalidation{}, current); got != guardian.OutcomeAllow {
		t.Fatalf("valid evidence: %s", got)
	}
	authorization.Sources[0].Revoked = true
	changed, err := reviewed.Candidate(ctx, "review", authorization, versions)
	if err != nil {
		t.Fatal(err)
	}
	current.Candidate = &changed
	if got := guardian.Evaluate(evidence, guardian.EvidenceInvalidation{}, current); got != guardian.OutcomeDiscard {
		t.Fatalf("revoked source retained allow: %s", got)
	}
	if err := os.WriteFile(filepath.Join(reviewed.Snapshot().Root, "build.sh"), []byte("changed"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewed.Candidate(ctx, "review", authorization, versions); err == nil {
		t.Fatal("changed content produced a valid candidate")
	}
}

func TestGuardianAutomaticallyCapturesNestedScripts(t *testing.T) {
	root, g, ctx, raw := guardianShellFixture(t)
	if err := os.WriteFile(filepath.Join(root, "build.sh"), []byte("#!/bin/sh\n./generate.sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	child := []byte("#!/bin/sh\nprintf 'nested\\n' > generated/out.txt\n")
	if err := os.WriteFile(filepath.Join(root, "generate.sh"), child, 0700); err != nil {
		t.Fatal(err)
	}
	reviewed := prepareGuardianShell(t, g, ctx, raw, true)
	var paths []string
	for _, entry := range reviewed.Snapshot().Content {
		paths = append(paths, entry.Path)
	}
	if !slices.Equal(paths, []string{"build.sh", "generate.sh"}) || string(reviewed.Content("generate.sh")) != string(child) {
		t.Fatalf("nested content was not automatically captured: %v", paths)
	}
	if err := os.WriteFile(filepath.Join(reviewed.Snapshot().Root, "generate.sh"), []byte("changed"), 0700); err != nil {
		t.Fatal(err)
	}
	result, err := g.Execute(tool.WithReviewedExecution(ctx, reviewed), "call", "exec_command", raw)
	if err == nil && !result.IsError {
		t.Fatal("changed nested dependency executed")
	}
	if _, err := os.Stat(filepath.Join(root, "generated", "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("changed nested script produced output: %v", err)
	}
}

func TestGuardianDoesNotAcceptCallerDeclaredCoverage(t *testing.T) {
	for _, command := range []string{"go test ./...", "npm test", "python main.py", "printf changed > build.sh; ./build.sh"} {
		t.Run(command, func(t *testing.T) {
			_, g, ctx, _ := guardianShellFixture(t)
			raw, err := json.Marshal(map[string]any{"command": command, "write_paths": []string{"generated"}})
			if err != nil {
				t.Fatal(err)
			}
			reviewed := prepareGuardianShell(t, g, ctx, raw, true)
			if snapshot := reviewed.Snapshot(); snapshot.Validate() == nil || len(snapshot.Missing) == 0 {
				t.Fatalf("unsupported execution declared complete: %+v", snapshot)
			}
		})
	}
}

func TestGuardianDiscoveredContentHonorsTotalBudget(t *testing.T) {
	_, g, ctx, raw := guardianShellFixture(t)
	if reviewed, err := g.PrepareGuardianExecution(ctx, "call", "exec_command", raw, tool.CatalogBinding{}, toolguard.ReviewContentRequest{AttemptID: "attempt", MaxBytes: 1}); err == nil {
		_ = reviewed.Close()
		t.Fatal("automatically discovered content bypassed total byte budget")
	}
}

func TestGuardianRejectsExecutionContentInsideWriteScope(t *testing.T) {
	root, g, ctx, _ := guardianShellFixture(t)
	script := []byte("#!/bin/sh\nprintf done > generated/out.txt\n")
	if err := os.WriteFile(filepath.Join(root, "generated", "build.sh"), script, 0700); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"command":"./generated/build.sh","write_paths":["generated"]}`)
	reviewed := prepareGuardianShell(t, g, ctx, raw, true)
	if snapshot := reviewed.Snapshot(); snapshot.Validate() == nil || len(snapshot.Missing) == 0 {
		t.Fatal("writable execution content admitted as fixed evidence")
	}
}

func TestGuardianContentReadsHonorFileReadPolicy(t *testing.T) {
	for _, action := range []policy.Action{policy.ActionAsk, policy.ActionDeny} {
		t.Run(string(action), func(t *testing.T) {
			_, g, ctx, raw := guardianShellFixture(t)
			runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionAuto)
			runtime.User = []policy.Rule{{Tool: "file_read", Action: action}}
			g.SwapPolicy(runtime)
			if reviewed, err := g.PrepareGuardianExecution(ctx, "call", "exec_command", raw, tool.CatalogBinding{}, toolguard.ReviewContentRequest{AttemptID: "attempt", MaxBytes: 1024}); err == nil {
				_ = reviewed.Close()
				t.Fatal("script capture bypassed file_read policy")
			}
		})
	}
}

func TestGuardianRejectsProtectedWriteScope(t *testing.T) {
	root, g, ctx, _ := guardianShellFixture(t)
	if err := os.Mkdir(filepath.Join(root, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"command":"printf done > generated/out.txt","write_paths":["generated",".ssh"]}`)
	if reviewed, err := g.PrepareGuardianExecution(ctx, "call", "exec_command", raw, tool.CatalogBinding{}, toolguard.ReviewContentRequest{AttemptID: "attempt", MaxBytes: 1024}); err == nil {
		_ = reviewed.Close()
		t.Fatal("credential scope entered Guardian preparation")
	}
}
