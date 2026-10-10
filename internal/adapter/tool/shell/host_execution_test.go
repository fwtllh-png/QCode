package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestHostExecutionSessionLifecycleAndEvidence(t *testing.T) {
	root := t.TempDir()
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: root, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, root, manager, backend); err != nil {
		t.Fatal(err)
	}
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
	guarded, err := toolguard.New(toolguard.Options{Registry: registry, Workspace: root, Policy: runtime,
		Approvals: func(context.Context, toolguard.ApprovalRequest) error { return fmt.Errorf("unexpected approval") },
	})
	if err != nil {
		t.Fatal(err)
	}
	sequence := 0
	run := func(thread, name string, args map[string]any) (tool.Result, error) {
		t.Helper()
		sequence++
		id := fmt.Sprintf("host-%d", sequence)
		ctx := tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{ThreadID: thread, TurnID: "turn", CallID: id})
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		return guarded.Execute(ctx, id, name, raw)
	}
	command := map[string]any{"command": "printf host-ok > output; cat output", "execution_target": "host"}
	result, err := run("owner", "exec_command", command)
	if err != nil || result.IsError || result.Content != "host-ok" {
		t.Fatalf("authorized host: %+v %v", result, err)
	}
	if result.Execution == nil || len(result.Execution.Attempts) != 1 {
		t.Fatalf("missing execution receipt: %+v", result)
	}
	attempt := result.Execution.Attempts[0]
	if attempt.ExecutionTarget != "host" || attempt.FullAccess || attempt.Enforcement != "none" || attempt.EffectiveControls.FilesystemWrite != "unrestricted" || attempt.EffectiveControls.Network != "direct" || attempt.EffectiveControls.IPC != "unrestricted" {
		t.Fatalf("dishonest host controls: %+v", attempt)
	}
	for key, value := range map[string]any{"write_paths": []string{"output"}, "network_targets": []any{map[string]any{"host": "example.com", "port": 443}}, "allow_loopback": true, "settle": "discard"} {
		result, err := run("owner", "exec_command", map[string]any{"command": "true", "execution_target": "host", key: value})
		if err == nil && !result.IsError {
			t.Fatalf("host accepted scoped %s", key)
		}
	}
	result, err = run("owner", "exec_command", map[string]any{"command": "read answer; printf 'received:%s' \"$answer\"", "execution_target": "host", "yield_time_ms": 1})
	if err != nil || result.IsError || result.Metadata["session_id"] == nil {
		t.Fatalf("start interactive host: %+v %v", result, err)
	}
	id := result.Metadata["session_id"]
	result, err = run("intruder", "write_stdin", map[string]any{"session_id": id, "chars": "bad\n"})
	if err == nil && !result.IsError {
		t.Fatal("another thread controlled host process")
	}
	result, err = run("owner", "write_stdin", map[string]any{"session_id": id, "chars": "ok\n", "yield_time_ms": 1000})
	if err != nil || result.IsError || !strings.Contains(result.Content, "received:ok") || result.Metadata["execution_target"] != "host" {
		t.Fatalf("host interaction: %+v %v", result, err)
	}
	result, err = run("owner", "exec_command", map[string]any{"command": "printf changed > output", "execution_target": "host"})
	if err != nil || result.IsError || result.Metadata["verification_evidence"] != nil {
		t.Fatalf("host verified its own mutation: %+v %v", result, err)
	}
	runtime.Repository = []policy.Rule{{Tool: "*", Resource: "/protected/location", Action: policy.ActionDeny}}
	_, err = run("owner", "exec_command", command)
	if err == nil || !strings.Contains(err.Error(), "repository_rule_denied") {
		t.Fatalf("host lost explicit denial: %v", err)
	}
	runtime.Repository = nil
	runtime.SetPermission(policy.PermissionNever)
	_, err = run("owner", "exec_command", command)
	if err == nil || !strings.Contains(err.Error(), "host_execution_forbidden") {
		t.Fatalf("Read only authorized host: %v", err)
	}
}

func TestHostExecutionApprovalModes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		posture     policy.Permission
		rule        policy.Action
		answer      string
		disableHost bool
		revoke      bool
		approvals   int
		wantError   string
	}{
		{name: "auto approves each command", posture: policy.PermissionAuto, approvals: 2},
		{name: "full access", posture: policy.PermissionBypass},
		{name: "auto user allow still asks", posture: policy.PermissionAuto, rule: policy.ActionAllow, approvals: 2},
		{name: "full access explicit ask", posture: policy.PermissionBypass, rule: policy.ActionAsk, approvals: 2},
		{name: "full access explicit deny", posture: policy.PermissionBypass, rule: policy.ActionDeny, wantError: "user_rule_denied"},
		{name: "read only", posture: policy.PermissionNever, wantError: "host_execution_forbidden"},
		{name: "delegated", posture: policy.PermissionBypass, disableHost: true, wantError: "host_execution_forbidden"},
		{name: "denied approval", posture: policy.PermissionAuto, answer: "deny", approvals: 2, wantError: "approval_denied"},
		{name: "canceled approval", posture: policy.PermissionAuto, answer: "cancel", approvals: 2, wantError: "approval_canceled"},
		{name: "permission revoked while waiting", posture: policy.PermissionAuto, revoke: true, approvals: 1, wantError: "host_execution_forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			backend, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: root, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
			manager := process.NewSessionManager(4096)
			t.Cleanup(manager.CloseAll)
			registry := tool.NewRegistry(nil, nil)
			if err := RegisterWithManagerAndBackend(registry, root, manager, backend); err != nil {
				t.Fatal(err)
			}
			runtime := policy.DefaultRuntime(policy.ModeAct, tc.posture)
			runtime.DisableHostExecution = tc.disableHost
			runtime.ConfigurePlanning(policy.PlanningRequired)
			if tc.rule != "" {
				runtime.User = []policy.Rule{{Tool: "exec_command", Action: tc.rule}}
			}
			guarded, err := toolguard.New(toolguard.Options{Registry: registry, Workspace: root, Policy: runtime})
			if err != nil {
				t.Fatal(err)
			}
			approvals := 0
			guarded.SetApprovalHandler(func(_ context.Context, request toolguard.ApprovalRequest) error {
				approvals++
				if approvals > tc.approvals {
					return fmt.Errorf("unexpected approval: %+v", request)
				}
				if len(request.AllowedScopes) != 1 || request.AllowedScopes[0] != policy.ApprovalOnce || request.ReplacementAllowed {
					return fmt.Errorf("host approval must be fresh and once: %+v", request)
				}
				if !strings.Contains(string(request.Arguments), `"execution_target":"host"`) {
					return fmt.Errorf("approval lost host target: %+v", request)
				}
				// Each invocation appends one byte. Nothing may run before this approval.
				data, _ := os.ReadFile(filepath.Join(root, "output"))
				wantBytes := approvals - 1
				if tc.wantError != "" {
					wantBytes = 0
				}
				if len(data) != wantBytes {
					return fmt.Errorf("command ran before approval: %q", data)
				}
				if tc.revoke {
					runtime.SetPermission(policy.PermissionNever)
				}
				return guarded.Decide(toolguard.ApprovalDecision{RequestID: request.RequestID, Scope: policy.ApprovalOnce, Approved: tc.answer == "", Canceled: tc.answer == "cancel"})
			})
			for i := 0; i < 2; i++ {
				id := fmt.Sprintf("host-%d", i)
				ctx := tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{ThreadID: "owner", TurnID: "turn", CallID: id})
				result, err := guarded.Execute(ctx, id, "exec_command", json.RawMessage(`{"command":"printf x >> output","execution_target":"host"}`))
				if tc.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantError) {
						t.Fatalf("result=%+v err=%v, want %s", result, err, tc.wantError)
					}
				} else if err != nil || result.IsError {
					t.Fatalf("host failed: %+v %v", result, err)
				}
			}
			if approvals != tc.approvals {
				t.Fatalf("approvals=%d, want %d", approvals, tc.approvals)
			}
			data, err := os.ReadFile(filepath.Join(root, "output"))
			if tc.wantError != "" {
				if !os.IsNotExist(err) {
					t.Fatalf("rejected command wrote %q: %v", data, err)
				}
			} else if err != nil || string(data) != "xx" {
				t.Fatalf("host output=%q err=%v", data, err)
			}
		})
	}
}
