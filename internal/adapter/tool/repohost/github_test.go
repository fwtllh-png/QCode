package repohost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
)

type githubTestBackend struct{}

func (githubTestBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "test", Backend: "passthrough", Available: true,
		Effective: controlmatrix.Matrix{
			FilesystemRead:  controlmatrix.FilesystemReadDeclaredRoots,
			FilesystemWrite: controlmatrix.FilesystemWriteExactPaths,
			Network:         controlmatrix.NetworkDirect,
			ProcessTree:     controlmatrix.ProcessTreeGroupKill,
			CrossProcess:    controlmatrix.CrossProcessUnrestricted,
			Syscall:         controlmatrix.SyscallDenyDangerous,
			IPC:             controlmatrix.IPCUnrestricted,
			PathIdentity:    controlmatrix.PathIdentityDescriptorRelative,
			ArtifactOrigin:  controlmatrix.ArtifactOriginUnverifiedPath,
			DurableRecovery: controlmatrix.DurableRecoveryMemoryOnly,
		},
	}
}

func (githubTestBackend) Prepare(
	_ context.Context,
	command sandbox.Command,
) (sandbox.Command, error) {
	command.PreparedReadOnly = command.WorkspaceReadOnly
	command.PreparedNetworkDenied = command.DenyNetwork
	return command, nil
}

func TestGitHubToolUsesFixedArgumentShape(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "gh")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.BindPolicy(
		githubTestBackend{}, sandbox.Options{WorkspaceRoot: root},
	)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	root = workspace.Root()
	binary = filepath.Join(root, "gh")
	instance := &githubTool{
		kind: "github_pr_view", root: root, backend: backend, binary: binary,
		runProcess: func(_ context.Context, options process.Options) (process.Result, error) {
			return process.Result{Stdout: strings.Join(options.Args, "\n")}, nil
		},
	}
	runtime, err := typedExecutorForTest(instance)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(runtime); err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      "github_pr_view",
		Arguments: json.RawMessage(`{"repository":"owner/repo","number":42}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "owner/repo") ||
		!strings.Contains(result.Content, "42") {
		t.Fatalf("GitHub command output = %+v", result)
	}
}

func TestGitHubCreateRequiresIrreversibleApproval(t *testing.T) {
	instance := &githubTool{kind: "github_pr_create", binary: "/usr/bin/true"}
	binding := instance.binding()
	if binding.Effect.Kind != tool.EffectExternalMutation ||
		binding.Effect.Reversibility != tool.Irreversible ||
		binding.Effect.Approval != tool.ApprovalPolicyOnce {
		t.Fatalf("create binding = %+v", binding)
	}
}

func typedExecutorForTest(instance *githubTool) (tool.Executor, error) {
	runtime, err := typed.Define(typed.Spec[githubInput, tool.Result]{
		Descriptor: instance.Descriptor(), Disposition: tool.DispositionWaitForTeardown,
		Run: instance.run,
		Encode: func(result tool.Result) (tool.Result, error) {
			return result, nil
		},
	})
	if err != nil {
		return nil, err
	}
	outcome := runtime.(tool.OutcomeExecutor)
	return &githubExecutor{
		OutcomeExecutor: outcome, binding: instance.binding(),
	}, nil
}
