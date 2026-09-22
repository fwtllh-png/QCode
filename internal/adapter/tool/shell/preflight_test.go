package shell

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/environment"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func preflightRegistry(t *testing.T) (*tool.Registry, string) {
	t.Helper()
	root := t.TempDir()
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: root, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, root, manager, backend); err != nil {
		t.Fatal(err)
	}
	return registry, root
}

func TestExecCommandPreflightDeniesUnreadableExecutable(t *testing.T) {
	registry, _ := preflightRegistry(t)
	outside := filepath.Join(t.TempDir(), "host-tool")
	if err := os.WriteFile(
		outside, []byte("#!/bin/sh\nprintf host\n"), 0o755,
	); err != nil {
		t.Fatal(err)
	}
	result := executeProcessTool(t, registry, processTestThread, "exec_command", map[string]any{
		"command": outside,
	})
	if !result.IsError {
		t.Fatalf("unreadable executable ran: %+v", result)
	}
	if result.Metadata["error_category"] != environment.CategoryFilesystemAccessDenied {
		t.Fatalf("error_category = %v metadata=%v", result.Metadata["error_category"], result.Metadata)
	}
	if result.Metadata["required_action"] != environment.ActionApproveHostConfig {
		t.Fatalf("required_action = %v", result.Metadata["required_action"])
	}
	if result.Metadata["executable"] != filepath.Clean(outside) {
		t.Fatalf("executable = %v", result.Metadata["executable"])
	}
	if result.Metadata["retry_original"] != false {
		t.Fatalf("retry_original = %v", result.Metadata["retry_original"])
	}
}

func TestExecCommandPreflightKeepsWorkspaceExecutablesRunning(t *testing.T) {
	registry, root := preflightRegistry(t)
	probe := filepath.Join(root, "probe.sh")
	if err := os.WriteFile(
		probe, []byte("#!/bin/sh\nprintf inside\n"), 0o755,
	); err != nil {
		t.Fatal(err)
	}
	result := executeProcessTool(t, registry, processTestThread, "exec_command", map[string]any{
		"command": "./probe.sh",
	})
	if result.IsError || result.Content != "inside" {
		t.Fatalf("workspace executable result = %+v", result)
	}
	// Builtins and missing programs are left to the shell itself.
	result = executeProcessTool(t, registry, processTestThread, "exec_command", map[string]any{
		"command": "printf '%s' builtin-ok",
	})
	if result.IsError || result.Content != "builtin-ok" {
		t.Fatalf("builtin result = %+v", result)
	}
	result = executeProcessTool(t, registry, processTestThread, "exec_command", map[string]any{
		"command": "definitely-missing-tool-xyz",
	})
	if !result.IsError ||
		result.Metadata["error_category"] == environment.CategoryFilesystemAccessDenied {
		t.Fatalf("missing tool must fail as the shell reports it: %+v", result.Metadata)
	}
}
