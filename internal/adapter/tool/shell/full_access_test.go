package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestFullAccessCommandsUseSessionAuthority(t *testing.T) {
	root := t.TempDir()
	host := t.TempDir()
	stateRoot := t.TempDir()
	privateHome := filepath.Join(stateRoot, "sandbox-home")
	if err := os.Mkdir(privateHome, 0700); err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: root, PrivateTemp: privateHome,
		RuntimeStateRoots: []string{stateRoot}, SkipPATHReadRoots: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skip(err)
	}
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, root, manager, backend); err != nil {
		t.Fatal(err)
	}
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionAuto)
	runtime.ConfigurePlanning(policy.PlanningRequired)
	guarded, err := toolguard.New(toolguard.Options{Registry: registry, Policy: runtime, Workspace: root,
		Approvals: func(context.Context, toolguard.ApprovalRequest) error { return fmt.Errorf("unexpected approval") },
	})
	if err != nil {
		t.Fatal(err)
	}
	sequence := 0
	run := func(name string, args map[string]any) (tool.Result, error) {
		t.Helper()
		sequence++
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		return guarded.Execute(ctx, fmt.Sprintf("full-access-%d", sequence), name, raw)
	}
	write := map[string]any{"command": "mkdir -p generated/cache && printf ok > generated/cache/result"}
	result, err := run("exec_command", write)
	if err != nil || !result.IsError {
		t.Fatalf("Auto must preserve readonly execution: %+v %v", result, err)
	}
	runtime.SetPermission(policy.PermissionBypass)
	result, err = run("exec_command", write)
	if err != nil || result.IsError {
		t.Fatalf("Full Access write: %+v %v", result, err)
	}
	if result.Execution == nil || len(result.Execution.Attempts) != 1 || !result.Execution.Attempts[0].FullAccess || result.Execution.Attempts[0].NetworkMode != "direct" {
		t.Fatalf("missing effective Full Access receipt: %+v", result.Execution)
	}
	if content, err := os.ReadFile(filepath.Join(root, "generated/cache/result")); err != nil || string(content) != "ok" {
		t.Fatalf("write = %q, %v", content, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "network-ok") }))
	defer server.Close()
	result, err = run("exec_command", map[string]any{"command": "/usr/bin/curl --fail --silent --max-time 5 " + server.URL})
	if err != nil || result.IsError || result.Content != "network-ok" {
		t.Fatalf("Full Access direct network: %+v %v", result, err)
	}
	result, err = run("exec_command", map[string]any{
		"command":        "mkdir -p e2e/results && /usr/bin/curl --fail --silent --max-time 5 " + server.URL + " > e2e/results/output",
		"allow_loopback": true,
	})
	if err != nil || result.IsError || !result.Execution.Attempts[0].FullAccess || result.Execution.Attempts[0].NetworkMode != "loopback_any" {
		t.Fatalf("loopback must not remove Full Access file grants: %+v %v", result, err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "e2e/results/output")); err != nil || string(content) != "network-ok" {
		t.Fatalf("loopback result = %q, %v", content, err)
	}
	outside := filepath.Join(host, "output")
	result, err = run("exec_command", map[string]any{"command": fmt.Sprintf("printf host > %q", outside)})
	if err != nil || result.IsError {
		t.Fatalf("Full Access host write: %+v %v", result, err)
	}
	for _, name := range []string{".git", ".qcode", ".agents", "nested/.GiT"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
		result, err = run("exec_command", map[string]any{"command": "printf forbidden > " + name + "/probe"})
		if err != nil || !result.IsError {
			t.Fatalf("protected %s: %+v %v", name, result, err)
		}
		if _, err := os.Stat(filepath.Join(root, name, "probe")); !os.IsNotExist(err) {
			t.Fatalf("protected file created: %s", name)
		}
	}
	for _, target := range []string{filepath.Join(stateRoot, "state-v1.db"), filepath.Join(host, ".ssh", "fixture"), filepath.Join(host, ".netrc")} {
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		for _, command := range []string{fmt.Sprintf("cat %q", target), fmt.Sprintf("printf forbidden > %q", target)} {
			result, err = run("exec_command", map[string]any{"command": command})
			if err != nil || !result.IsError {
				t.Fatalf("protected state or credential: %+v %v", result, err)
			}
		}
	}
	result, err = run("exec_command", map[string]any{"command": fmt.Sprintf("printf cache > %q", filepath.Join(privateHome, "cache"))})
	if err != nil || result.IsError {
		t.Fatalf("runtime protection lost private environment grant: %+v %v", result, err)
	}
	result, err = run("exec_command", map[string]any{"command": fmt.Sprintf("cd %q && /bin/pwd -P && cat cache", privateHome)})
	if err != nil || result.IsError {
		t.Fatalf("private environment path resolution: %+v %v", result, err)
	}
	runtime.SubmitPlan()
	result, err = run("exec_command", map[string]any{"command": "printf ok > scoped-output", "write_paths": []string{"scoped-output"}})
	if err != nil || result.IsError || !result.Execution.Attempts[0].FullAccess || result.Execution.Attempts[0].NetworkMode != "direct" || result.Execution.Attempts[0].EffectiveControls.FilesystemWrite != "exact_paths" {
		t.Fatalf("explicit write scope: %+v %v", result, err)
	}
	result, err = run("exec_command", map[string]any{"command": "printf forbidden > outside-scope", "write_paths": []string{"scoped-output"}})
	if err != nil || !result.IsError {
		t.Fatalf("explicit scope was widened: %+v %v", result, err)
	}
	result, err = run("exec_command", map[string]any{"command": "printf changed > generated/cache/result"})
	if err != nil || result.IsError || result.Metadata["verification_evidence"] != nil {
		t.Fatalf("Full Access verified its own input mutation: %+v %v", result, err)
	}
	result, err = run("shell_read", map[string]any{"command": "printf forbidden > readonly-output"})
	if err == nil && !result.IsError {
		t.Fatal("Full Access changed shell_read contract")
	}
	result, err = run("exec_command", map[string]any{"command": "true", "full_access": true})
	if err == nil && !result.IsError {
		t.Fatal("model supplied execution authority")
	}
	runtime.SetPermission(policy.PermissionNever)
	result, err = run("exec_command", map[string]any{"command": "printf forbidden > never-output"})
	if err != nil || !result.IsError {
		t.Fatalf("Read only must tighten: %+v %v", result, err)
	}
	runtime.SetPermission(policy.PermissionAuto)
	result, err = run("exec_command", map[string]any{"command": "printf forbidden > auto-output"})
	if err != nil || !result.IsError {
		t.Fatalf("Auto must tighten: %+v %v", result, err)
	}
	runtime.SetPermission(policy.PermissionBypass)
	runtime.Repository = []policy.Rule{{Tool: "*", Resource: "/protected/location", Action: policy.ActionDeny}}
	_, err = run("exec_command", map[string]any{"command": "true"})
	if err == nil || !strings.Contains(err.Error(), "repository_rule_denied") {
		t.Fatalf("explicit restriction lost: %v", err)
	}
}
