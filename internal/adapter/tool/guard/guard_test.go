package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestMalformedArgumentsFailBeforePolicy(t *testing.T) {
	executor := testExecutor{descriptor: writeDescriptor()}
	registry := newTestRegistry(t, nil, &executor)
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
	runtime.Repository = []policy.Rule{{Tool: "write", Action: policy.ActionHold, Code: "hold"}}
	guard := newTestGuard(t, registry, runtime, nil)
	_, err := guard.Execute(t.Context(), "call", "write", json.RawMessage(`{"path":`))
	if err == nil || !contains(err.Error(), "arguments") || contains(err.Error(), "hold") {
		t.Fatalf("error = %v, want schema error before policy", err)
	}
}

func TestExecCommandWritePathPreflightRunsBeforeApproval(t *testing.T) {
	for name, path := range map[string]string{
		"directory":      "directory",
		"missing_parent": filepath.Join("missing", "new.txt"),
	} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.Mkdir(filepath.Join(workspace, "directory"), 0o700); err != nil {
				t.Fatal(err)
			}
			descriptor := writeDescriptor()
			descriptor.Name = "exec_command"
			descriptor.Capability = tool.CapabilityProcess
			descriptor.ResourceResolver = tool.ResourceResolver{
				PathsField: "write_paths",
			}
			descriptor.InputSchema = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"write_paths": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "string"},
					},
				},
				"required": []string{"write_paths"}, "additionalProperties": false,
			}
			registry := tool.NewRegistry(nil, nil)
			binding := tool.TrustedBindingFromDescriptor(descriptor)
			binding.ValidateMissingWriteParent = true
			executor := &testExecutor{
				descriptor: descriptor,
				binding:    binding,
			}
			if err := registry.Register(executor); err != nil {
				t.Fatal(err)
			}
			requested := atomic.Bool{}
			guard, err := New(Options{
				Registry: registry,
				Policy: policy.DefaultRuntime(
					policy.ModeAct,
					policy.PermissionSuggest,
				),
				Workspace: workspace,
				Approvals: func(context.Context, ApprovalRequest) error {
					requested.Store(true)
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(map[string]any{
				"write_paths": []string{path},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := guard.Execute(
				t.Context(), "call-preflight-"+name, "exec_command", raw,
			); err == nil {
				t.Fatalf("Execute(%q) error = nil", path)
			}
			if requested.Load() {
				t.Fatalf("Execute(%q) requested approval before preflight", path)
			}
			if executor.calls.Load() != 0 {
				t.Fatalf("Execute(%q) calls = %d", path, executor.calls.Load())
			}
		})
	}
}

func TestNetworkTargetsResolveHostPortMethodAndPrivateScope(t *testing.T) {
	resources, err := networkTargets([]any{map[string]any{
		"host": "API.Example.com.", "protocol": "https", "port": float64(8443),
		"methods": []any{"post", "GET", "GET"}, "allow_private": true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 || resources[0].ID != "api.example.com" ||
		resources[0].Protocol != "https" || resources[0].Port != 8443 ||
		!reflect.DeepEqual(resources[0].Methods, []string{"GET", "POST"}) ||
		!resources[0].AllowPrivate {
		t.Fatalf("network resources = %+v", resources)
	}
}

func TestTrustedHostPathResourceUsesWorkspaceSandboxPolicy(t *testing.T) {
	workspace := t.TempDir()
	privateHome := t.TempDir()
	privateExecutable := filepath.Join(privateHome, "target", "app")
	if err := os.MkdirAll(filepath.Dir(privateExecutable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privateExecutable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	privateExecutable, err := filepath.EvalSymlinks(privateExecutable)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.BindPolicy(strongBackend{}, sandbox.Options{
		WorkspaceRoot: workspace,
		PrivateTemp:   privateHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	registry.SetSandboxBackend(backend)
	guard, err := New(Options{
		Registry: registry,
		Policy: policy.DefaultRuntime(
			policy.ModeAct,
			policy.PermissionBypass,
		),
		Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := readDescriptor("host_tool")
	descriptor.ResourceResolver = tool.ResourceResolver{
		TrustedHostPathField: "path",
	}
	descriptor.InputSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "minLength": 1},
		},
		"required": []string{"path"}, "additionalProperties": false,
	}
	if err := registry.Register(&testExecutor{descriptor: descriptor}); err != nil {
		t.Fatal(err)
	}
	resources, err := guard.resolveResources(
		"host_tool",
		descriptor,
		json.RawMessage(`{"path":"target/app"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 || resources[0].Path != privateExecutable ||
		resources[0].Access != tool.AccessRead {
		t.Fatalf("resources = %+v", resources)
	}
	outside := filepath.Join(t.TempDir(), "outside-app")
	raw, err := json.Marshal(map[string]string{"path": outside})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.resolveResources("host_tool", descriptor, raw); err == nil {
		t.Fatal("trusted host resource accepted a path outside policy-owned roots")
	}
}

func TestArgumentExpansionFailureIsRecoverableInvalidArguments(t *testing.T) {
	executor := &failingExpanderExecutor{testExecutor: testExecutor{
		descriptor: readDescriptor("expand"),
	}}
	registry := newTestRegistry(t, nil, executor)
	guard := newTestGuard(
		t,
		registry,
		policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		nil,
	)
	_, err := guard.Execute(t.Context(), "call", "expand", json.RawMessage(`{}`))
	if !errors.Is(err, tool.ErrInvalidArguments) {
		t.Fatalf("error = %v, want ErrInvalidArguments", err)
	}
}

func TestDefaultsNormalizeBeforeCanonicalResources(t *testing.T) {
	executor := testExecutor{descriptor: writeDescriptor()}
	registry := newTestRegistry(t, nil, &executor)
	guard := newTestGuard(
		t, registry, policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass), nil,
	)
	invocation, _, err := guard.prepare(
		t.Context(), "write", "call",
		json.RawMessage(`{"value":"x"}`), tool.CatalogBinding{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(invocation.Arguments) != `{"path":"default.txt","value":"x"}` {
		t.Fatalf("normalized arguments = %s", invocation.Arguments)
	}
	if len(invocation.Resources) != 1 ||
		invocation.Resources[0].Path == "" ||
		invocation.Resources[0].Access != tool.AccessWrite {
		t.Fatalf("resources = %+v", invocation.Resources)
	}
}

func TestRepositoryAskPausesAndApproveDenyResume(t *testing.T) {
	for _, permission := range []policy.Permission{policy.PermissionAuto, policy.PermissionBypass} {
		t.Run(string(permission), func(t *testing.T) {
			executor := testExecutor{descriptor: writeDescriptor()}
			registry := newTestRegistry(t, nil, &executor)
			runtime := policy.DefaultRuntime(policy.ModeAct, permission)
			runtime.Repository = []policy.Rule{{
				Tool: "write", Resource: "default.txt", Action: policy.ActionAsk,
			}}
			requests := make(chan ApprovalRequest, 1)
			guard := newTestGuard(t, registry, runtime, func(_ context.Context, request ApprovalRequest) error {
				requests <- request
				return nil
			})
			result := make(chan error, 1)
			go func() {
				_, err := guard.Execute(
					context.Background(), "call", "write",
					json.RawMessage(`{"path":"default.txt","value":"x"}`),
				)
				result <- err
			}()
			request := <-requests
			select {
			case err := <-result:
				t.Fatalf("call did not pause: %v", err)
			default:
			}
			if err := guard.Decide(ApprovalDecision{
				RequestID: request.RequestID, Approved: true, Scope: policy.ApprovalOnce,
				ExpiresAt: request.ExpiresAt,
			}); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}

			go func() {
				_, err := guard.Execute(
					context.Background(), "call-deny", "write",
					json.RawMessage(`{"path":"default.txt","value":"y"}`),
				)
				result <- err
			}()
			denied := <-requests
			if err := guard.Decide(ApprovalDecision{RequestID: denied.RequestID}); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err == nil || !contains(err.Error(), "approval_denied") {
				t.Fatalf("denial error = %v", err)
			}

			go func() {
				_, err := guard.Execute(
					context.Background(), "call-cancel", "write",
					json.RawMessage(`{"path":"default.txt","value":"z"}`),
				)
				result <- err
			}()
			canceled := <-requests
			if err := guard.Decide(ApprovalDecision{
				RequestID: canceled.RequestID, Canceled: true,
			}); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err == nil || !contains(err.Error(), "approval_canceled") {
				t.Fatalf("cancel error = %v", err)
			}
		})
	}
}

func TestActAutoProcessPausesForApprovalThenResumes(t *testing.T) {
	descriptor := readDescriptor("exec_command")
	descriptor.Capability = tool.CapabilityProcess
	descriptor.SandboxRequirement = tool.SandboxNone
	registry := newTestRegistry(t, nil, &testExecutor{descriptor: descriptor})
	requests := make(chan ApprovalRequest, 1)
	guard := newTestGuard(
		t,
		registry,
		policy.DefaultRuntime(policy.ModeAct, policy.PermissionAuto),
		func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	)
	result := make(chan error, 1)
	go func() {
		_, err := guard.Execute(
			context.Background(), "process-call", "exec_command", json.RawMessage(`{}`),
		)
		result <- err
	}()
	request := <-requests
	if request.Tool != "exec_command" {
		t.Fatalf("approval tool = %q, want exec_command", request.Tool)
	}
	if request.Effect != policy.EffectProcessMutating ||
		request.Risk != policy.RiskHigh || request.ReasonCode == "" {
		t.Fatalf("approval presentation facts = %+v", request)
	}
	select {
	case err := <-result:
		t.Fatalf("process call did not pause for approval: %v", err)
	default:
	}
	mustDecide(t, guard, request, policy.ApprovalOnce, nil)
	if err := <-result; err != nil {
		t.Fatalf("approved process call failed: %v", err)
	}
}

func TestActAutoReadOnlyShellDoesNotAsk(t *testing.T) {
	descriptor := readDescriptor("shell_read")
	descriptor.Capability = tool.CapabilityRead
	descriptor.SandboxRequirement = tool.SandboxStrong
	registry := newTestRegistry(
		t, strongBackend{}, &testExecutor{descriptor: descriptor},
	)
	var approvals atomic.Int64
	guard := newTestGuard(
		t,
		registry,
		policy.DefaultRuntime(policy.ModeAct, policy.PermissionAuto),
		func(context.Context, ApprovalRequest) error {
			approvals.Add(1)
			return nil
		},
	)
	if _, err := guard.Execute(
		t.Context(), "read-process-call", "shell_read", json.RawMessage(`{}`),
	); err != nil {
		t.Fatal(err)
	}
	if approvals.Load() != 0 {
		t.Fatalf("read-only shell requested %d approvals", approvals.Load())
	}
	if !guard.canEscalate(Invocation{
		Descriptor: descriptor,
		Binding:    tool.TrustedBindingFromDescriptor(descriptor),
	}) {
		t.Fatal("read-only shell must support bounded path-read amendments")
	}
}

func TestApprovalAlwaysPersistsAllow(t *testing.T) {
	now := time.Unix(10_000, 0)
	descriptor := networkFetchDescriptor()
	descriptor.AccessMode = tool.AccessWrite
	executor := testExecutor{descriptor: descriptor}
	registry := newTestRegistry(t, nil, &executor)
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
	runtime.DisableAutoReview = true
	requests := make(chan ApprovalRequest, 2)
	var persisted atomic.Int32
	guard, err := New(Options{
		Registry: registry, Policy: runtime, Workspace: t.TempDir(),
		Now: func() time.Time { return now },
		Approvals: func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
		PersistAllow: func(invocation policy.Invocation) error {
			persisted.Add(1)
			_, err := runtime.AppendUserRule(policy.Rule{
				Tool: invocation.Tool, Resource: "*", Action: policy.ActionAllow,
			})
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, execErr := guard.Execute(context.Background(), "always-1", "web_fetch",
			json.RawMessage(`{"url":"https://example.com/a"}`))
		result <- execErr
	}()
	request := <-requests
	foundAlways := false
	for _, scope := range request.AllowedScopes {
		if scope == policy.ApprovalAlways {
			foundAlways = true
		}
	}
	if !foundAlways {
		t.Fatalf("allowed scopes = %+v", request.AllowedScopes)
	}
	mustDecide(t, guard, request, policy.ApprovalAlways, nil)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if persisted.Load() != 1 {
		t.Fatalf("persisted = %d", persisted.Load())
	}
	if _, err := guard.Execute(context.Background(), "always-2", "web_fetch",
		json.RawMessage(`{"url":"https://example.com/b"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case unexpected := <-requests:
		t.Fatalf("repository allow should skip ask: %+v", unexpected)
	default:
	}
}

func TestUnscopedApprovalOffersOnceOnly(t *testing.T) {
	registry := newTestRegistry(
		t, nil, &testExecutor{descriptor: writeDescriptor()},
	)
	requests := make(chan ApprovalRequest, 1)
	guard, err := New(Options{
		Registry:  registry,
		Policy:    policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Workspace: t.TempDir(),
		Approvals: func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
		PersistAllow: func(policy.Invocation) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := guard.Execute(context.Background(), "once-only", "write",
			json.RawMessage(`{"path":"a","value":"x"}`))
		done <- err
	}()
	request := <-requests
	if request.Grant != nil ||
		!reflect.DeepEqual(request.AllowedScopes, []policy.ApprovalScope{policy.ApprovalOnce}) {
		t.Fatalf("request = %+v", request)
	}
	if err := guard.Decide(ApprovalDecision{
		RequestID: request.RequestID, Approved: true, Scope: policy.ApprovalAlways,
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !contains(err.Error(), "approval_scope_denied") {
		t.Fatalf("forged always decision = %v", err)
	}
}

func TestApprovalOnceSessionExpiryAndModifiedArguments(t *testing.T) {
	now := time.Unix(10_000, 0)
	descriptor := writeDescriptor()
	descriptor.Name = "followup_task"
	descriptor.ResourceResolver.Templates[0].Kind = "agent"
	executor := testExecutor{descriptor: descriptor}
	registry := newTestRegistry(t, nil, &executor)
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
	runtime.DisableAutoReview = true
	runtime.Repository = []policy.Rule{{
		Tool: "followup_task", Resource: "blocked", Action: policy.ActionDeny,
	}}
	requests := make(chan ApprovalRequest, 4)
	guard, err := New(Options{
		Registry: registry, Policy: runtime, Workspace: t.TempDir(),
		Now: func() time.Time { return now }, ApprovalTTL: 5 * time.Minute,
		Approvals: func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(id string, arguments string) <-chan error {
		result := make(chan error, 1)
		go func() {
			_, err := guard.Execute(context.Background(), id, "followup_task", json.RawMessage(arguments))
			result <- err
		}()
		return result
	}

	first := call("once-1", `{"path":"a","value":"x"}`)
	request := <-requests
	mustDecide(t, guard, request, policy.ApprovalOnce, nil)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	second := call("once-2", `{"path":"a","value":"x"}`)
	request = <-requests
	mustDecide(t, guard, request, policy.ApprovalSession, nil)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if err := <-call("session", `{"path":"a","value":"x"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case unexpected := <-requests:
		t.Fatalf("session approval did not cache: %+v", unexpected)
	default:
	}

	now = now.Add(6 * time.Minute)
	expired := call("expired", `{"path":"a","value":"x"}`)
	request = <-requests
	mustDecide(t, guard, request, policy.ApprovalOnce, json.RawMessage(`{"path":7}`))
	if err := <-expired; err == nil || !contains(err.Error(), "replacement arguments") {
		t.Fatalf("replacement error = %v", err)
	}

}

func TestApprovalCancelDuplicateLateAndWrongRequest(t *testing.T) {
	executor := testExecutor{descriptor: writeDescriptor()}
	registry := newTestRegistry(t, nil, &executor)
	requests := make(chan ApprovalRequest, 1)
	guard := newTestGuard(
		t, registry, policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := guard.Execute(ctx, "call", "write", json.RawMessage(`{"path":"a","value":"x"}`))
		result <- err
	}()
	request := <-requests
	if err := guard.Decide(ApprovalDecision{RequestID: "wrong"}); err == nil {
		t.Fatal("wrong request was accepted")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	if guard.Pending() != 0 {
		t.Fatal("canceled request remained pending")
	}
	if err := guard.Decide(ApprovalDecision{RequestID: request.RequestID}); err == nil {
		t.Fatal("late decision was accepted")
	}

	ctx = context.Background()
	go func() {
		_, err := guard.Execute(ctx, "call-2", "write", json.RawMessage(`{"path":"b","value":"x"}`))
		result <- err
	}()
	request = <-requests
	decision := ApprovalDecision{
		RequestID: request.RequestID, Approved: true,
		Scope: policy.ApprovalOnce, ExpiresAt: request.ExpiresAt,
	}
	if err := guard.Decide(decision); err != nil {
		t.Fatal(err)
	}
	if err := guard.Decide(decision); err == nil {
		t.Fatal("duplicate decision was accepted")
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestPendingApprovalExpiresFailClosed(t *testing.T) {
	executor := testExecutor{descriptor: writeDescriptor()}
	registry := newTestRegistry(t, nil, &executor)
	guard, err := New(Options{
		Registry:  registry,
		Policy:    policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Workspace: t.TempDir(), ApprovalTTL: 20 * time.Millisecond,
		Approvals: func(context.Context, ApprovalRequest) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	var expired ApprovalWait
	guard.SetApprovalExpiryHandler(func(wait ApprovalWait) error {
		expired = wait
		return nil
	})
	_, err = guard.Execute(
		t.Context(), "expires", "write",
		json.RawMessage(`{"path":"a","value":"x"}`),
	)
	if err == nil || !contains(err.Error(), "approval_expired") {
		t.Fatalf("expiry error = %v", err)
	}
	if executor.calls.Load() != 0 || guard.Pending() != 0 {
		t.Fatalf("expired call executed or remained pending: calls=%d pending=%d", executor.calls.Load(), guard.Pending())
	}
	if expired.CallID != "expires" ||
		expired.Outcome != ApprovalWaitExpired {
		t.Fatalf("expiry handler wait = %+v", expired)
	}
}

func TestAliasDeferredUnknownAvailabilityAndSandboxFailClosed(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	descriptor := writeDescriptor()
	descriptor.Aliases = []tool.Alias{{Name: "legacy_write", Hidden: true}}
	executor := testExecutor{descriptor: descriptor}
	if err := registry.Register(&executor); err != nil {
		t.Fatal(err)
	}
	guard := newTestGuard(
		t, registry, policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass), nil,
	)
	if _, err := guard.Execute(
		t.Context(), "alias", "legacy_write",
		json.RawMessage(`{"path":"a","value":"x"}`),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Execute(t.Context(), "unknown", "missing", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unknown tool was accepted")
	}

	deferredDescriptor := readDescriptor("deferred")
	deferredDescriptor.Availability = tool.AvailabilityDeferred
	deferredDescriptor.DeferredLoading.Enabled = true
	var loads atomic.Int32
	if err := registry.RegisterTrusted(
		"test:deferred",
		tool.NewExternalDeferredRegistration(
			tool.ExternalFromDescriptor(deferredDescriptor),
			tool.TrustedBindingFromDescriptor(deferredDescriptor),
			func() (tool.Executor, error) {
				loads.Add(1)
				loaded := readDescriptor("deferred")
				return &testExecutor{descriptor: loaded}, nil
			},
		),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Execute(t.Context(), "deferred", "deferred", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if loads.Load() != 1 {
		t.Fatalf("deferred loads = %d", loads.Load())
	}

	unavailable := readDescriptor("unavailable")
	unavailable.Availability = tool.AvailabilityUnavailable
	unavailable.UnavailableReason = "dependency missing"
	if err := registry.Register(&testExecutor{descriptor: unavailable}); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Execute(t.Context(), "unavailable", "unavailable", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unavailable tool was accepted")
	}

	required := readDescriptor("sandboxed")
	required.Capability = tool.CapabilityProcess
	required.SandboxRequirement = tool.SandboxStrong
	if err := registry.Register(&testExecutor{descriptor: required}); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Execute(t.Context(), "sandboxed", "sandboxed", json.RawMessage(`{}`)); err == nil ||
		!sandbox.IsUnavailable(err) {
		t.Fatalf("sandbox error = %v", err)
	}
}

func TestStrongSandboxDescriptorUsesInjectedBackend(t *testing.T) {
	descriptor := readDescriptor("sandboxed")
	descriptor.Capability = tool.CapabilityProcess
	descriptor.SandboxRequirement = tool.SandboxStrong
	registry := newTestRegistry(
		t, strongBackend{}, &testExecutor{descriptor: descriptor},
	)
	guard := newTestGuard(
		t, registry, policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass), nil,
	)
	if _, err := guard.Execute(t.Context(), "sandboxed", "sandboxed", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
}

func TestPatchRenameAndSymlinkTargetsCanonicalizeIdentically(t *testing.T) {
	workspace := t.TempDir()
	target := filepath.Join(workspace, "target.txt")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	canonicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(workspace, "alias.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	write := testExecutor{descriptor: writeDescriptor()}
	patchDescriptor := readDescriptor("patch")
	patchDescriptor.Capability = tool.CapabilityWrite
	patchDescriptor.AccessMode = tool.AccessTree
	patchDescriptor.ResourceResolver = tool.ResourceResolver{PatchField: "patch"}
	patchDescriptor.InputSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"patch": map[string]any{"type": "string", "minLength": float64(1)},
		},
		"required": []string{"patch"}, "additionalProperties": false,
	}
	registry := newTestRegistry(
		t, nil, &write, &testExecutor{descriptor: patchDescriptor},
	)
	guard, err := New(Options{
		Registry:  registry,
		Policy:    policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeInvocation, _, err := guard.prepare(
		t.Context(), "write", "write",
		json.RawMessage(`{"path":"alias.txt","value":"x"}`),
		tool.CatalogBinding{},
	)
	if err != nil {
		t.Fatal(err)
	}
	writePath := writeInvocation.Resources[0].Path
	patchInvocation, _, err := guard.prepare(
		t.Context(), "patch", "patch",
		json.RawMessage(`{"patch":"rename from old.txt\nrename to alias.txt\n"}`),
		tool.CatalogBinding{},
	)
	if err != nil {
		t.Fatal(err)
	}
	var patchPath string
	for _, resource := range patchInvocation.Resources {
		if resource.Path == writePath {
			patchPath = resource.Path
		}
	}
	if writePath != canonicalTarget || patchPath != canonicalTarget {
		t.Fatalf("write path = %q, patch target = %q, want %q", writePath, patchPath, canonicalTarget)
	}
}

type testExecutor struct {
	descriptor tool.Descriptor
	binding    tool.TrustedBinding
	calls      atomic.Int32
	mu         sync.Mutex
	profiles   []sandbox.ExecutionAuthority
}

type approvalMetricSink struct {
	mu     sync.Mutex
	values []string
}

func (s *approvalMetricSink) Approval(
	outcome, effect, risk, reasonCode string,
	_ time.Duration,
) {
	if effect == "" || risk == "" || reasonCode == "" {
		panic("approval metric has an empty dimension")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = append(s.values, outcome)
}

func (s *approvalMetricSink) outcomes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.values...)
}

type failingExpanderExecutor struct {
	testExecutor
}

func (*failingExpanderExecutor) ExpandArguments(
	context.Context,
	json.RawMessage,
) (json.RawMessage, error) {
	return nil, errors.New("expanded beyond the bounded set")
}

func (e *testExecutor) Descriptor() tool.Descriptor { return e.descriptor }
func (e *testExecutor) TrustedBinding() tool.TrustedBinding {
	if e.binding.Capability != "" {
		return e.binding
	}
	return tool.TrustedBindingFromDescriptor(e.descriptor)
}
func (e *testExecutor) Execute(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	e.calls.Add(1)
	if profile, ok := sandbox.ExecutionAuthorityFromContext(ctx); ok {
		e.mu.Lock()
		e.profiles = append(e.profiles, profile)
		e.mu.Unlock()
	}
	return tool.Result{Content: string(raw)}, nil
}

func (e *testExecutor) profilesSnapshot() []sandbox.ExecutionAuthority {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sandbox.ExecutionAuthority(nil), e.profiles...)
}

func TestEgressDeniedAsksThenRetries(t *testing.T) {
	executor := &egressRetryExecutor{descriptor: networkFetchDescriptor()}
	registry := newTestRegistry(t, nil, executor)
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
	runtime.DisableAutoReview = true
	requests := make(chan ApprovalRequest, 2)
	guard, err := New(Options{
		Registry: registry, Policy: runtime, Workspace: t.TempDir(),
		Approvals: func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
		OnNetworkAllow: func(capability tool.Capability, target egress.Target) {
			t.Errorf("Web grant escaped call scope: %q %+v", capability, target)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		result tool.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, execErr := guard.Execute(
			context.Background(), "call-egress", "web_fetch",
			json.RawMessage(`{"url":"https://example.com/page"}`),
		)
		done <- outcome{result: result, err: execErr}
	}()

	// Pre-flight Suggest asks for the tool URL host first.
	first := <-requests
	if first.ReasonCode != ApprovalReasonNetworkHost ||
		first.Network == nil || first.Network.Host != "example.com" {
		t.Fatalf("preflight approval = %+v", first)
	}
	mustDecide(t, guard, first, policy.ApprovalSession, nil)

	request := <-requests
	if request.ReasonCode != ApprovalReasonNetworkHost ||
		request.Network == nil || request.Network.Host != "cdn.example" {
		t.Fatalf("mid-flight approval = %+v", request)
	}
	mustDecide(t, guard, request, policy.ApprovalSession, nil)
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.result.IsError || out.result.Content != `{"ok":true}` {
		t.Fatalf("result = %+v", out.result)
	}
	if executor.calls.Load() != 2 {
		t.Fatalf("calls = %d, want retry after grant", executor.calls.Load())
	}
}

func TestEgressDeniedBypassAutoGrantsWithoutAsk(t *testing.T) {
	executor := &egressRetryExecutor{descriptor: networkFetchDescriptor()}
	registry := newTestRegistry(t, nil, executor)
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
	guard, err := New(Options{
		Registry: registry, Policy: runtime, Workspace: t.TempDir(),
		Approvals: func(context.Context, ApprovalRequest) error {
			t.Fatal("bypass must not ask for mid-flight egress hosts")
			return nil
		},
		OnNetworkAllow: func(capability tool.Capability, target egress.Target) {
			t.Errorf("Web grant escaped call scope: %q %+v", capability, target)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := guard.Execute(
		context.Background(), "call-bypass-egress", "web_fetch",
		json.RawMessage(`{"url":"https://example.com/page"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || result.Content != `{"ok":true}` {
		t.Fatalf("result = %+v", result)
	}
	if executor.calls.Load() != 2 {
		t.Fatalf("calls = %d, want retry after auto-grant", executor.calls.Load())
	}
}

func TestEgressDeniedApprovalDenyKeepsFailure(t *testing.T) {
	executor := &egressRetryExecutor{descriptor: networkFetchDescriptor()}
	registry := newTestRegistry(t, nil, executor)
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
	runtime.DisableAutoReview = true
	requests := make(chan ApprovalRequest, 2)
	guard, err := New(Options{
		Registry: registry, Policy: runtime, Workspace: t.TempDir(),
		Approvals: func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		result tool.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, execErr := guard.Execute(
			context.Background(), "call-deny-egress", "web_fetch",
			json.RawMessage(`{"url":"https://example.com/page"}`),
		)
		done <- outcome{result: result, err: execErr}
	}()
	preflight := <-requests
	mustDecide(t, guard, preflight, policy.ApprovalSession, nil)
	request := <-requests
	if err := guard.Decide(ApprovalDecision{RequestID: request.RequestID}); err != nil {
		t.Fatal(err)
	}
	out := <-done
	if out.err != nil {
		t.Fatalf("deny should soft-return tool failure, got err %v", out.err)
	}
	if !out.result.IsError || out.result.Metadata["error_category"] != "egress_denied" {
		t.Fatalf("result = %+v", out.result)
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("calls = %d, want no retry after deny", executor.calls.Load())
	}
}

type egressRetryExecutor struct {
	calls      atomic.Int32
	descriptor tool.Descriptor
}

func (e *egressRetryExecutor) Descriptor() tool.Descriptor { return e.descriptor }

func (e *egressRetryExecutor) Execute(ctx context.Context, _ json.RawMessage) (tool.Result, error) {
	if e.calls.Add(1) == 1 {
		return tool.Result{
			Content: "egress denied · host=cdn.example", IsError: true,
			Metadata: map[string]any{
				"error_category": "egress_denied", "host": "cdn.example",
				"protocol": "https", "status_code": 0,
			},
			Outcome: &tool.Outcome{
				Status: tool.OutcomeFailed,
				Security: &tool.SecuritySignal{
					EgressDenied: &tool.NetworkTarget{
						Host: "cdn.example", Protocol: "https",
					},
				},
			},
		}, nil
	}
	gate := &egress.Gate{
		Enforce: true, UseCallScope: true,
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		},
	}
	if _, err := gate.Authorize(ctx, egress.Target{Host: "cdn.example", Protocol: "https"}, "test"); err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Content: `{"ok":true}`}, nil
}

func TestNetworkHostApprovalSessionReuseAndCancel(t *testing.T) {
	executor := testExecutor{descriptor: networkFetchDescriptor()}
	registry := newTestRegistry(t, nil, &executor)
	runtime := policy.DefaultRuntime(policy.ModeOperate, policy.PermissionAuto)
	runtime.DisableAutoReview = true
	requests := make(chan ApprovalRequest, 4)
	guard := newTestGuard(t, registry, runtime, func(_ context.Context, request ApprovalRequest) error {
		requests <- request
		return nil
	})

	result := make(chan error, 1)
	go func() {
		_, err := guard.Execute(
			context.Background(), "call-a", "web_fetch",
			json.RawMessage(`{"url":"https://example.com/a"}`),
		)
		result <- err
	}()
	first := <-requests
	if first.ReasonCode != ApprovalReasonNetworkHost ||
		first.Network == nil || first.Network.Host != "example.com" ||
		first.Network.Mode != string(policy.NetworkImmediate) {
		t.Fatalf("first approval = %+v", first)
	}
	mustDecide(t, guard, first, policy.ApprovalSession, nil)
	if err := <-result; err != nil {
		t.Fatal(err)
	}

	if _, err := guard.Execute(
		context.Background(), "call-b", "web_fetch",
		json.RawMessage(`{"url":"https://example.com/b"}`),
	); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-requests:
		t.Fatalf("same host re-asked: %+v", request)
	default:
	}

	go func() {
		_, err := guard.Execute(
			context.Background(), "call-c", "web_fetch",
			json.RawMessage(`{"url":"https://other.com/"}`),
		)
		result <- err
	}()
	second := <-requests
	if second.Network == nil || second.Network.Host != "other.com" {
		t.Fatalf("second approval = %+v", second)
	}
	mustDecide(t, guard, second, policy.ApprovalOnce, nil)
	if err := <-result; err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, err := guard.Execute(
			ctx, "call-cancel", "web_fetch",
			json.RawMessage(`{"url":"https://cancel.example/"}`),
		)
		result <- err
	}()
	canceled := <-requests
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v, want context.Canceled (not approval_denied)", err)
	}
	_ = canceled
}

func TestProcessNetworkTargetRequiresApprovalUnderSuggest(t *testing.T) {
	executor := &testExecutor{descriptor: processNetworkDescriptor()}
	registry := newTestRegistry(t, strongBackend{}, executor)
	requests := make(chan ApprovalRequest, 1)
	grants := make(chan egress.Target, 1)
	guard, err := New(Options{
		Registry:  registry,
		Policy:    policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Workspace: t.TempDir(),
		Approvals: func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
		OnNetworkAllow: func(capability tool.Capability, target egress.Target) {
			if capability != tool.CapabilityProcess {
				t.Errorf("grant capability = %q, want process", capability)
			}
			grants <- target
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := guard.Execute(
			context.Background(),
			"call-process-network",
			"process_network",
			json.RawMessage(`{"network_targets":[{
				"host":"api.example.com",
				"protocol":"https",
				"port":443,
				"methods":["CONNECT"],
				"allow_private":false
			}]}`),
		)
		done <- err
	}()
	request := <-requests
	if request.ReasonCode != ApprovalReasonNetworkHost ||
		request.Network == nil ||
		request.Network.Host != "api.example.com" ||
		request.Network.Protocol != "https" ||
		request.Network.Port != 443 ||
		!reflect.DeepEqual(request.Network.Methods, []string{"CONNECT"}) {
		t.Fatalf("process network approval = %+v", request)
	}
	if executor.calls.Load() != 0 {
		t.Fatalf("process executed before approval: calls=%d", executor.calls.Load())
	}
	mustDecide(t, guard, request, policy.ApprovalOnce, nil)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	grant := <-grants
	if grant.Host != "api.example.com" ||
		grant.Protocol != "https" ||
		grant.Port != 443 ||
		!reflect.DeepEqual(grant.Methods, []string{"CONNECT"}) {
		t.Fatalf("network grant = %+v", grant)
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("process calls = %d, want 1", executor.calls.Load())
	}
}

func TestProcessLoopbackRequiresApprovalAndBindsAuthority(t *testing.T) {
	descriptor := processNetworkDescriptor()
	descriptor.ResourceResolver.NetworkTargetsField = ""
	descriptor.ResourceResolver.LoopbackField = "allow_loopback"
	descriptor.InputSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"allow_loopback": map[string]any{"type": "boolean"},
		},
		"additionalProperties": false,
	}
	executor := &testExecutor{descriptor: descriptor}
	registry := newTestRegistry(t, strongBackend{}, executor)
	requests := make(chan ApprovalRequest, 1)
	guard, err := New(Options{
		Registry:  registry,
		Policy:    policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Workspace: t.TempDir(),
		Approvals: func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan tool.Result, 1)
	errs := make(chan error, 1)
	go func() {
		result, executeErr := guard.Execute(
			context.Background(),
			"call-process-loopback",
			descriptor.Name,
			json.RawMessage(`{"allow_loopback":true}`),
		)
		done <- result
		errs <- executeErr
	}()
	request := <-requests
	if request.ReasonCode != ApprovalReasonNetworkHost ||
		request.Network == nil ||
		request.Network.Host != "localhost" ||
		request.Network.Protocol != "loopback" ||
		!request.Network.AllowPrivate ||
		!reflect.DeepEqual(request.Network.Methods, []string{"BIND", "CONNECT"}) {
		t.Fatalf("loopback approval = %+v", request)
	}
	mustDecide(t, guard, request, policy.ApprovalOnce, nil)
	result := <-done
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	profiles := executor.profilesSnapshot()
	if len(profiles) != 1 || !profiles[0].AllowLoopback {
		t.Fatalf("loopback profiles = %+v", profiles)
	}
	if result.Execution == nil ||
		len(result.Execution.Attempts) != 1 ||
		!result.Execution.Attempts[0].LoopbackAllowed ||
		result.Execution.Attempts[0].EffectiveControls.Network !=
			controlmatrix.NetworkLoopbackExact {
		t.Fatalf("loopback execution receipt = %+v", result.Execution)
	}
}

func TestNetworkAutoReviewsUnderActAuto(t *testing.T) {
	executor := &testExecutor{descriptor: networkFetchDescriptor()}
	registry := newTestRegistry(t, nil, executor)
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionAuto)
	metrics := &approvalMetricSink{}
	guard := newTestGuard(t, registry, runtime, func(
		context.Context, ApprovalRequest,
	) error {
		return errors.New("auto-reviewed network read requested human approval")
	})
	guard.SetApprovalObserver(metrics.Approval)
	if _, err := guard.Execute(
		context.Background(), "call", "web_fetch",
		json.RawMessage(`{"url":"https://example.com/"}`),
	); err != nil {
		t.Fatal(err)
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("calls = %d", executor.calls.Load())
	}
	if !reflect.DeepEqual(metrics.outcomes(), []string{"evaluated", "auto_allowed"}) {
		t.Fatalf("approval metrics = %v", metrics.outcomes())
	}
}

func networkFetchDescriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "web_fetch", Description: "test fetch", Visibility: tool.VisibleModel,
		Capability: tool.CapabilityNetwork, AccessMode: tool.AccessRead,
		ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
			Kind: "url", Field: "url", Access: tool.AccessRead,
		}}},
		ParallelPolicy: tool.ParallelConcurrent, SandboxRequirement: tool.SandboxNone,
		Availability: tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{"type": "string", "minLength": 1},
			},
			"required": []string{"url"}, "additionalProperties": false,
		},
	}
}

func processNetworkDescriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "process_network", Description: "test process network",
		Visibility: tool.VisibleModel, Capability: tool.CapabilityProcess,
		AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelConcurrent,
		SandboxRequirement: tool.SandboxStrong,
		ResourceResolver: tool.ResourceResolver{
			Templates: []tool.ResourceTemplate{{
				Kind: "process", ID: "workspace", Access: tool.AccessRead, Tree: true,
			}},
			NetworkTargetsField: "network_targets",
		},
		Availability: tool.AvailabilityAvailable,
		RepeatPolicy: tool.RepeatExecute,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"network_targets": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"host": map[string]any{"type": "string"},
							"protocol": map[string]any{
								"type": "string", "enum": []string{"http", "https"},
							},
							"port": map[string]any{
								"type": "integer", "minimum": 1, "maximum": 65535,
							},
							"methods": map[string]any{
								"type": "array", "items": map[string]any{"type": "string"},
							},
							"allow_private": map[string]any{"type": "boolean"},
						},
						"required": []string{
							"host", "protocol", "port", "methods", "allow_private",
						},
						"additionalProperties": false,
					},
				},
			},
			"required": []string{"network_targets"}, "additionalProperties": false,
		},
	}
}

func writeDescriptor() tool.Descriptor {
	descriptor := readDescriptor("write")
	descriptor.Capability = tool.CapabilityWrite
	descriptor.AccessMode = tool.AccessWrite
	descriptor.ResourceResolver = tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
		Kind: "file", Field: "path", Access: tool.AccessWrite,
	}}}
	descriptor.InputSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":  map[string]any{"type": "string", "default": "default.txt"},
			"value": map[string]any{"type": "string"},
		},
		"required": []string{"path", "value"}, "additionalProperties": false,
	}
	return descriptor
}

func TestControlPlaneWriteCannotBeApprovedOrBypassed(t *testing.T) {
	executor := &testExecutor{descriptor: writeDescriptor()}
	registry := newTestRegistry(t, nil, executor)
	requested := atomic.Bool{}
	guard := newTestGuard(
		t,
		registry,
		policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		func(context.Context, ApprovalRequest) error {
			requested.Store(true)
			return nil
		},
	)
	_, err := guard.Execute(
		t.Context(),
		"control-plane-write",
		"write",
		json.RawMessage(`{"path":".qcode/permissions.toml","value":"allow"}`),
	)
	var denied *policy.DecisionError
	if !errors.As(err, &denied) ||
		denied.Code != "control_plane_protected" {
		t.Fatalf("Execute() error = %v", err)
	}
	if requested.Load() || executor.calls.Load() != 0 {
		t.Fatalf(
			"approval requested=%v executor calls=%d",
			requested.Load(),
			executor.calls.Load(),
		)
	}
}

func TestGitControlPlaneWriteSuggestsDedicatedTool(t *testing.T) {
	executor := &testExecutor{descriptor: writeDescriptor()}
	registry := newTestRegistry(t, nil, executor)
	guard := newTestGuard(
		t,
		registry,
		policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		nil,
	)
	_, err := guard.Execute(
		t.Context(),
		"git-control-plane-write",
		"write",
		json.RawMessage(`{"path":".git/index","value":"unsafe"}`),
	)
	hint, ok := tool.RecoveryHintFromError(err)
	if !ok ||
		hint.ErrorCategory != "control_plane_protected" ||
		hint.RequiredAction != "use_git_tool" ||
		hint.RetryOriginal {
		t.Fatalf("Git control-plane recovery hint = %+v, ok=%t, err=%v", hint, ok, err)
	}
	if executor.calls.Load() != 0 {
		t.Fatalf("executor calls = %d", executor.calls.Load())
	}
}

func readDescriptor(name string) tool.Descriptor {
	return tool.Descriptor{
		Name: name, Description: "test tool", Visibility: tool.VisibleModel,
		Capability: tool.CapabilityRead, AccessMode: tool.AccessRead,
		ParallelPolicy: tool.ParallelConcurrent, SandboxRequirement: tool.SandboxNone,
		Availability: tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{},
			"additionalProperties": false,
		},
	}
}

type strongBackend struct{}

func (strongBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "test", Backend: "test", Available: true,
		Effective: controlmatrix.
			Matrix{FilesystemRead: controlmatrix.
			FilesystemReadDeclaredRoots,

			FilesystemWrite: controlmatrix.
				FilesystemWriteExactPaths,

			Network: controlmatrix.
				NetworkDenied, ProcessTree: controlmatrix.ProcessTreeGroupKill,
			CrossProcess: controlmatrix.CrossProcessUnrestricted,
			Syscall:      controlmatrix.SyscallDenyDangerous, IPC: controlmatrix.IPCUnrestricted,
			PathIdentity:    controlmatrix.PathIdentityDescriptorRelative,
			ArtifactOrigin:  controlmatrix.ArtifactOriginUnverifiedPath,
			DurableRecovery: controlmatrix.DurableRecoveryMemoryOnly,
		},
	}
}
func (strongBackend) Prepare(_ context.Context, command sandbox.Command) (sandbox.Command, error) {
	return command, nil
}

func TestAdditionalPermissionRequiresReapproval(t *testing.T) {
	executor := &escalateExecutor{descriptor: sandboxedDescriptor("escalate")}
	registry := newTestRegistry(t, strongBackend{}, executor)
	requests := make(chan ApprovalRequest, 2)
	guard := newTestGuard(
		t, registry, policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	)

	result := make(chan error, 1)
	go func() {
		_, err := guard.Execute(
			context.Background(), "esc-1", "escalate", json.RawMessage(`{}`),
		)
		result <- err
	}()
	request := <-requests
	if request.ReasonCode != ApprovalReasonAdditionalPermission {
		t.Fatalf("reason = %q, want %s", request.ReasonCode, ApprovalReasonAdditionalPermission)
	}
	hasPath := false
	for _, resource := range request.Resources {
		if resource.Kind == "file" && resource.Access == tool.AccessWrite {
			hasPath = true
		}
	}
	if !hasPath || request.AdditionalPermission == nil ||
		request.AdditionalPermission.Permission.Kind != authority.AdditionalPathWrite {
		t.Fatalf("additional permission request = %+v", request)
	}
	if len(request.AllowedScopes) != 1 ||
		request.AllowedScopes[0] != policy.ApprovalOnce {
		t.Fatalf("additional permission scopes = %v", request.AllowedScopes)
	}
	if request.Effect != policy.EffectExternalMutation ||
		request.Risk != policy.RiskCritical {
		t.Fatalf("additional permission risk = %s/%s", request.Effect, request.Risk)
	}
	if err := guard.Decide(ApprovalDecision{RequestID: request.RequestID}); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil || !contains(err.Error(), "approval_denied") {
		t.Fatalf("denial error = %v", err)
	}
	if modes := executor.modesSnapshot(); len(modes) != 1 || modes[0] != SandboxModeStrong {
		t.Fatalf("modes = %v, want only strong (no silent unsandbox)", modes)
	}
}

func TestAdditionalPermissionRequiresApprovalForEveryInvocation(t *testing.T) {
	executor := &escalateExecutor{descriptor: sandboxedDescriptor("escalate")}
	registry := newTestRegistry(t, strongBackend{}, executor)
	requests := make(chan ApprovalRequest, 4)
	guard := newTestGuard(
		t, registry, policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	)

	first := make(chan error, 1)
	go func() {
		_, err := guard.Execute(
			context.Background(), "esc-cache-1", "escalate", json.RawMessage(`{}`),
		)
		first <- err
	}()
	request := <-requests
	mustDecide(t, guard, request, policy.ApprovalOnce, nil)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if modes := executor.modesSnapshot(); len(modes) != 2 ||
		modes[0] != SandboxModeStrong || modes[1] != SandboxModeStrong {
		t.Fatalf("first modes = %v", modes)
	}

	second := make(chan error, 1)
	go func() {
		_, err := guard.Execute(
			context.Background(), "esc-cache-2", "escalate", json.RawMessage(`{}`),
		)
		second <- err
	}()
	request = <-requests
	if request.ReasonCode != ApprovalReasonAdditionalPermission {
		t.Fatalf("second reason = %q", request.ReasonCode)
	}
	mustDecide(t, guard, request, policy.ApprovalOnce, nil)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if modes := executor.modesSnapshot(); len(modes) != 4 ||
		modes[2] != SandboxModeStrong || modes[3] != SandboxModeStrong {
		t.Fatalf("second modes = %v", modes)
	}
}

func TestAdditionalPermissionRetriesAtMostOnce(t *testing.T) {
	executor := &repeatedEscalateExecutor{
		descriptor: sandboxedDescriptor("repeated_escalate"),
	}
	registry := newTestRegistry(t, strongBackend{}, executor)
	requests := make(chan ApprovalRequest, 2)
	guard := newTestGuard(
		t, registry, policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	)
	done := make(chan error, 1)
	go func() {
		_, err := guard.Execute(
			context.Background(),
			"esc-once",
			"repeated_escalate",
			json.RawMessage(`{}`),
		)
		done <- err
	}()
	request := <-requests
	mustDecide(t, guard, request, policy.ApprovalOnce, nil)
	select {
	case unexpected := <-requests:
		_ = guard.Decide(ApprovalDecision{RequestID: unexpected.RequestID})
		t.Fatalf("second amendment approval requested: %+v", unexpected)
	case err := <-done:
		denial, ok := sandbox.DenialFromError(err)
		if !ok || denial.ReasonCode != sandbox.ReasonPathWriteNotAuthorized {
			t.Fatalf("second denial = %+v error=%v", denial, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for bounded amendment retry")
	}
	if executor.calls.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", executor.calls.Load())
	}
}

func TestSandboxStrongApprovalDoesNotCoverAdditionalPermission(t *testing.T) {
	executor := &escalateExecutor{descriptor: sandboxedDescriptor("escalate")}
	registry := newTestRegistry(t, strongBackend{}, executor)
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
	requests := make(chan ApprovalRequest, 4)
	guard := newTestGuard(t, registry, runtime, func(_ context.Context, request ApprovalRequest) error {
		requests <- request
		return nil
	})

	first := make(chan error, 1)
	go func() {
		_, err := guard.Execute(
			context.Background(), "ask-then-esc", "escalate", json.RawMessage(`{}`),
		)
		first <- err
	}()
	escalateAsk := <-requests
	if escalateAsk.ReasonCode != ApprovalReasonAdditionalPermission {
		t.Fatalf("ask reason = %q", escalateAsk.ReasonCode)
	}
	if len(escalateAsk.AllowedScopes) != 1 ||
		escalateAsk.AllowedScopes[0] != policy.ApprovalOnce {
		t.Fatalf("additional permission scopes = %v", escalateAsk.AllowedScopes)
	}
	mustDecide(t, guard, escalateAsk, policy.ApprovalOnce, nil)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestAdditionalPermissionDisabled(t *testing.T) {
	executor := &escalateExecutor{descriptor: sandboxedDescriptor("escalate")}
	registry := newTestRegistry(t, strongBackend{}, executor)
	disabled := EscalationPolicy{EscalateOnFailure: false}
	guard, err := New(Options{
		Registry: registry, Policy: policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		Workspace: t.TempDir(), Escalation: &disabled,
		Approvals: func(context.Context, ApprovalRequest) error {
			t.Fatal("approval must not be requested when escalate disabled")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = guard.Execute(t.Context(), "no-esc", "escalate", json.RawMessage(`{}`))
	denial, ok := sandbox.DenialFromError(err)
	if !ok || denial.ReasonCode != sandbox.ReasonPathWriteNotAuthorized {
		t.Fatalf("error = %v, denial = %+v", err, denial)
	}
	if modes := executor.modesSnapshot(); len(modes) != 1 || modes[0] != SandboxModeStrong {
		t.Fatalf("modes = %v", modes)
	}
}

func TestIsSandboxDenial(t *testing.T) {
	typed := sandbox.Denied(sandbox.Denial{
		Operation: sandbox.DenialWrite, Resource: "/tmp/result",
		ReasonCode: sandbox.ReasonPathWriteNotAuthorized,
	}, nil)
	if !IsSandboxDenial(typed, tool.Outcome{}) {
		t.Fatal("typed denial should match")
	}
	if IsSandboxDenial(errors.New("sandbox denied by policy"), tool.Outcome{}) {
		t.Fatal("untyped sandbox text must not authorize escalation")
	}
	if IsSandboxDenial(errors.New("Operation not permitted"), tool.Outcome{}) {
		t.Fatal("OS error text must not authorize escalation")
	}
	if !IsSandboxDenial(nil, tool.Outcome{
		Security: &tool.SecuritySignal{SandboxDenied: &sandbox.Denial{
			Operation: sandbox.DenialRead, Resource: "/tmp/input",
			ReasonCode: sandbox.ReasonPathReadNotAuthorized,
		}},
	}) {
		t.Fatal("typed sandbox signal should match")
	}
	if IsSandboxDenial(errors.New("command failed"), tool.Outcome{}) {
		t.Fatal("generic error must not match")
	}
}

func TestUntypedSandboxFailureFailsClosedWithoutApproval(t *testing.T) {
	executor := &errorExecutor{
		descriptor: sandboxedDescriptor("untyped_denial"),
		err:        errors.New("Operation not permitted"),
	}
	registry := newTestRegistry(t, strongBackend{}, executor)
	guard := newTestGuard(
		t,
		registry,
		policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		func(context.Context, ApprovalRequest) error {
			t.Fatal("untyped denial must not request additional permission")
			return nil
		},
	)
	_, err := guard.Execute(t.Context(), "untyped", "untyped_denial", json.RawMessage(`{}`))
	if err == nil || err.Error() != "Operation not permitted" {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestManagedProxyAuthorityMismatchRefreshesOnce(t *testing.T) {
	executor := &proxyRefreshExecutor{
		descriptor: sandboxedDescriptor("proxy_refresh"),
	}
	registry := newTestRegistry(t, strongBackend{}, executor)
	guard := newTestGuard(
		t,
		registry,
		policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		nil,
	)
	result, err := guard.Execute(
		t.Context(), "proxy-refresh", "proxy_refresh", json.RawMessage(`{}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "refreshed" || executor.calls.Load() != 2 {
		t.Fatalf("result = %+v calls=%d", result, executor.calls.Load())
	}
}

func TestManagedProxyAuthorityMismatchFailsClosedAfterRefresh(t *testing.T) {
	executor := &proxyRefreshExecutor{
		descriptor:     sandboxedDescriptor("proxy_refresh_fail"),
		alwaysMismatch: true,
	}
	registry := newTestRegistry(t, strongBackend{}, executor)
	guard := newTestGuard(
		t,
		registry,
		policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		nil,
	)
	_, err := guard.Execute(
		t.Context(), "proxy-refresh-fail", "proxy_refresh_fail", json.RawMessage(`{}`),
	)
	if err == nil || executor.calls.Load() != 2 {
		t.Fatalf("error = %v calls=%d", err, executor.calls.Load())
	}
	hint, ok := tool.RecoveryHintFromError(err)
	if !ok || hint.ErrorCategory != managedProxyMismatchCategory ||
		hint.RequiredAction != managedProxyMismatchAction ||
		hint.RetryOriginal {
		t.Fatalf("recovery hint = %+v ok=%t", hint, ok)
	}
}

func TestProcessSandboxHonorsAttempt(t *testing.T) {
	backend := strongBackend{}
	got, requireStrong := ProcessSandbox(context.Background(), backend)
	if got != backend || !requireStrong {
		t.Fatalf("default attempt = (%v, %v)", got, requireStrong)
	}
	ctx := WithSandboxAttempt(context.Background(), SandboxAttempt{Mode: SandboxModeNone})
	got, requireStrong = ProcessSandbox(ctx, backend)
	if got != backend || !requireStrong {
		t.Fatalf("none attempt = (%v, %v), want strong backend", got, requireStrong)
	}
}

type errorExecutor struct {
	descriptor tool.Descriptor
	err        error
}

type proxyRefreshExecutor struct {
	descriptor     tool.Descriptor
	alwaysMismatch bool
	calls          atomic.Int32
}

func (e *proxyRefreshExecutor) Descriptor() tool.Descriptor { return e.descriptor }
func (e *proxyRefreshExecutor) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	if e.calls.Add(1) == 1 || e.alwaysMismatch {
		return tool.Result{}, sandbox.Denied(sandbox.Denial{
			Operation:  sandbox.DenialNetwork,
			Resource:   "managed_proxy",
			ReasonCode: sandbox.ReasonAuthorityUnverified,
		}, errors.New("managed proxy does not match the effective permission profile"))
	}
	return tool.Result{Content: "refreshed"}, nil
}

func (e *errorExecutor) Descriptor() tool.Descriptor { return e.descriptor }
func (e *errorExecutor) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{}, e.err
}

type escalateExecutor struct {
	descriptor tool.Descriptor
	mu         sync.Mutex
	modes      []SandboxMode
	calls      atomic.Int32
}

type repeatedEscalateExecutor struct {
	descriptor tool.Descriptor
	calls      atomic.Int32
}

func (e *repeatedEscalateExecutor) Descriptor() tool.Descriptor {
	return e.descriptor
}

func (e *repeatedEscalateExecutor) Execute(
	ctx context.Context,
	_ json.RawMessage,
) (tool.Result, error) {
	call := e.calls.Add(1)
	profile, ok := sandbox.ExecutionAuthorityFromContext(ctx)
	if !ok {
		return tool.Result{}, errors.New("effective profile is missing")
	}
	return tool.Result{}, sandbox.Denied(sandbox.Denial{
		Backend: "test", Operation: sandbox.DenialWrite,
		Resource: filepath.Join(
			profile.WorkspaceRoot,
			fmt.Sprintf("amended-result-%d.txt", call),
		),
		ReasonCode: sandbox.ReasonPathWriteNotAuthorized,
	}, nil)
}

func (e *escalateExecutor) Descriptor() tool.Descriptor { return e.descriptor }
func (e *escalateExecutor) Execute(ctx context.Context, _ json.RawMessage) (tool.Result, error) {
	e.calls.Add(1)
	attempt, ok := SandboxAttemptFromContext(ctx)
	mode := SandboxModeStrong
	if ok {
		mode = attempt.Mode
	}
	e.mu.Lock()
	e.modes = append(e.modes, mode)
	e.mu.Unlock()
	profile, ok := sandbox.ExecutionAuthorityFromContext(ctx)
	if !ok {
		return tool.Result{}, errors.New("effective profile is missing")
	}
	path := filepath.Join(profile.WorkspaceRoot, "amended-result.txt")
	for _, allowed := range profile.WorkspaceWritePaths {
		if allowed == path {
			return tool.Result{Content: "amended-ok"}, nil
		}
	}
	return tool.Result{}, sandbox.Denied(sandbox.Denial{
		Backend: "test", Operation: sandbox.DenialWrite, Resource: path,
		ReasonCode: sandbox.ReasonPathWriteNotAuthorized,
	}, nil)
}

func (e *escalateExecutor) modesSnapshot() []SandboxMode {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]SandboxMode(nil), e.modes...)
}

func sandboxedDescriptor(name string) tool.Descriptor {
	descriptor := readDescriptor(name)
	descriptor.Capability = tool.CapabilityProcess
	descriptor.AccessMode = tool.AccessWrite
	descriptor.SandboxRequirement = tool.SandboxStrong
	return descriptor
}

func TestAbsoluteWorkspacePathIsRewritten(t *testing.T) {
	workspace := t.TempDir()
	sub := filepath.Join(workspace, "src")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	registry := newTestRegistry(
		t, nil, &testExecutor{descriptor: writeDescriptor()},
	)
	guard, err := New(Options{
		Registry: registry, Policy: policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "note.txt")
	args, _ := json.Marshal(map[string]any{"path": target, "value": "hello"})
	invocation, _, err := guard.prepare(
		t.Context(), "write", "abs", args, tool.CatalogBinding{},
	)
	if err != nil {
		t.Fatalf("absolute in-workspace path should be rewritten: %v", err)
	}
	var normalized map[string]any
	if err := json.Unmarshal(invocation.Arguments, &normalized); err != nil {
		t.Fatal(err)
	}
	if normalized["path"] != "note.txt" {
		t.Fatalf("path rewritten = %#v, want note.txt", normalized["path"])
	}
	outside := filepath.Join(t.TempDir(), "evil.txt")
	bad, _ := json.Marshal(map[string]any{"path": outside, "value": "x"})
	if _, _, err := guard.prepare(
		t.Context(), "write", "out", bad, tool.CatalogBinding{},
	); err == nil ||
		!strings.Contains(err.Error(), "absolute resource path") {
		t.Fatalf("outside absolute path error = %v", err)
	}
}

func newTestGuard(
	t *testing.T,
	registry *tool.Registry,
	runtime *policy.Runtime,
	approvals func(context.Context, ApprovalRequest) error,
) *Guard {
	t.Helper()
	value, err := New(Options{
		Registry: registry, Policy: runtime, Workspace: t.TempDir(),
		Approvals: approvals,
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func newTestRegistry(
	t testing.TB,
	backend sandbox.Backend,
	executors ...tool.Executor,
) *tool.Registry {
	t.Helper()
	registry := tool.NewRegistry(nil, nil)
	if backend != nil {
		registry.SetSandboxBackend(backend)
	}
	for _, executor := range executors {
		if err := registry.Register(executor); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func mustDecide(
	t *testing.T,
	guard *Guard,
	request ApprovalRequest,
	scope policy.ApprovalScope,
	replacement json.RawMessage,
) {
	t.Helper()
	if err := guard.Decide(ApprovalDecision{
		RequestID: request.RequestID, Approved: true, Scope: scope,
		ExpiresAt: request.ExpiresAt, ReplacementArguments: replacement,
	}); err != nil {
		t.Fatal(err)
	}
}

func contains(value, fragment string) bool { return strings.Contains(value, fragment) }
