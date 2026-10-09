//go:build capability

package shell

import (
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

// Reexec gives the child a fresh attack probe, just as a fixture Runtime gets.
// Reapplying an identical parent profile does not exercise this boundary.
func TestHostExecutionCanStartFreshSandbox(t *testing.T) {
	const marker = "QCODE_HOST_SANDBOX_TEST"
	if os.Getenv(marker) == "1" {
		root := t.TempDir()
		backend, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: root, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true})
		if err != nil {
			t.Fatal(err)
		}
		defer sandbox.CloseBackend(backend)
		if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
			t.Fatal(err)
		}
		directory, err := os.Open(root)
		if err != nil {
			t.Fatal(err)
		}
		defer directory.Close()
		result, err := process.Run(t.Context(), process.Options{Path: "/bin/sh", Args: []string{"-c", "printf child-ok; printf forbidden > denied"}, Dir: root, DirFile: directory, Sandbox: backend, RequireSandbox: true, WorkspaceReadOnly: true, DenyNetwork: true})
		if err != nil {
			t.Fatal(err)
		}
		if result.ExitCode == 0 {
			t.Fatal("child sandbox allowed forbidden write")
		}
		if _, err := os.Stat(filepath.Join(root, "denied")); !os.IsNotExist(err) {
			t.Fatal("child sandbox failed to enforce read-only")
		}
		fmt.Println("fresh-child-sandbox-enforced")
		return
	}
	root := t.TempDir()
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: root, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Fatal(err)
	}
	manager := process.NewSessionManager(65536)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, root, manager, backend); err != nil {
		t.Fatal(err)
	}
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
	guarded, err := toolguard.New(toolguard.Options{Registry: registry, Workspace: root, Policy: runtime})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := "'" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestHostExecutionCanStartFreshSandbox$ -test.v"
	for _, target := range []string{"sandbox", "host"} {
		raw, err := json.Marshal(map[string]any{"command": command, "execution_target": target, "env": map[string]string{marker: "1"}, "yield_time_ms": 30000})
		if err != nil {
			t.Fatal(err)
		}
		result, err := guarded.Execute(t.Context(), "fresh-"+target, "exec_command", raw)
		if err != nil {
			t.Fatal(err)
		}
		if target == "host" {
			if result.IsError || !strings.Contains(result.Content, "fresh-child-sandbox-enforced") {
				t.Fatalf("host child: %+v", result)
			}
		} else if result.IsError {
			if !strings.Contains(result.Content, "sandbox_unavailable") {
				t.Fatalf("unexpected nested failure: %+v", result)
			}
			t.Logf("nested sandbox unavailable, no fallback: %s", result.Content)
		} else if !strings.Contains(result.Content, "fresh-child-sandbox-enforced") {
			t.Fatalf("nested execution lost enforcement: %+v", result)
		}
		if len(result.Execution.Attempts) != 1 {
			t.Fatal("automatically replayed command")
		}
	}
}
