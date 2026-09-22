//go:build capability && darwin

package process_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	platformenv "github.com/fwtllh-png/QCode/internal/platform/environment"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestIsolatedSandboxEnvironmentBaseline(t *testing.T) {
	root := t.TempDir()
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot:       root,
		PrivateTemp:         t.TempDir(),
		AllowNetwork:        false,
		EnvironmentContract: "v1",
		EnvironmentProfile:  "isolated",
	})
	if err != nil {
		t.Fatalf("unavailable: construct sandbox: %v", err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Fatalf("unavailable: %v", err)
	}
	policy, ok := sandbox.BackendPolicy(backend)
	if !ok {
		t.Fatalf("unavailable: sandbox backend has no policy")
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatalf("unavailable: %v", err)
	}
	directory, err := workspace.OpenDirectory(".")
	if err != nil {
		t.Fatalf("unavailable: %v", err)
	}
	t.Cleanup(func() { _ = directory.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	temp, err := process.Run(ctx, process.Options{
		Dir: workspace.Root(), DirFile: directory,
		Command: `printf '%s\n' "$HOME" "$TMPDIR"; mktemp -d`,
		Sandbox: backend, RequireSandbox: true, WorkspaceReadOnly: true,
	})
	if err != nil {
		t.Fatalf("unavailable: run mktemp probe: %v", err)
	}
	if !strings.Contains(temp.Stdout, policy.PrivateTemp) {
		t.Fatalf(
			"isolated HOME/TMPDIR baseline drifted: stdout=%q private=%q",
			temp.Stdout,
			policy.PrivateTemp,
		)
	}
	if temp.ExitCode == 0 {
		t.Fatal("isolated mktemp -d succeeded; isolated contract should deny the Darwin user temp")
	}
	if !strings.Contains(temp.Stderr+temp.Stdout, "Operation not permitted") &&
		!strings.Contains(temp.Stderr+temp.Stdout, "not permitted") {
		t.Fatalf(
			"isolated mktemp failure is not the documented EPERM baseline: exit=%d stdout=%q stderr=%q",
			temp.ExitCode,
			temp.Stdout,
			temp.Stderr,
		)
	}

	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("unavailable: go toolchain: %v", err)
	}
	sandboxed, err := process.Run(ctx, process.Options{
		Dir: workspace.Root(), DirFile: directory,
		Command: `go env HOME GOENV`,
		Sandbox: backend, RequireSandbox: true, WorkspaceReadOnly: true,
	})
	if err != nil {
		t.Fatalf("unavailable: run go env probe: %v", err)
	}
	if sandboxed.ExitCode != 0 {
		t.Fatalf(
			"unavailable: go env failed in sandbox: stdout=%q stderr=%q",
			sandboxed.Stdout,
			sandboxed.Stderr,
		)
	}
	if !strings.Contains(sandboxed.Stdout, policy.PrivateTemp) {
		t.Fatalf("sandbox go env HOME/GOENV is not private temp: %q", sandboxed.Stdout)
	}
}

func TestV1NativeSharedUserTempAllowsMktemp(t *testing.T) {
	userTemp, err := platformenv.UserTempDir()
	if err != nil {
		t.Fatalf("unavailable: resolve Darwin user temp: %v", err)
	}
	root := t.TempDir()
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot:       root,
		PrivateTemp:         t.TempDir(),
		AllowNetwork:        false,
		EnvironmentContract: "v1",
		EnvironmentProfile:  "native",
		SharedUserTemp:      true,
		HostWriteRoots:      []string{userTemp},
	})
	if err != nil {
		t.Fatalf("unavailable: construct sandbox: %v", err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Fatalf("unavailable: %v", err)
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatalf("unavailable: %v", err)
	}
	directory, err := workspace.OpenDirectory(".")
	if err != nil {
		t.Fatalf("unavailable: %v", err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := process.Run(ctx, process.Options{
		Dir: workspace.Root(), DirFile: directory,
		Command: `printf '%s\n' "$TMPDIR"; created=$(mktemp -d) || exit 1; trap 'rmdir "$created"' EXIT; printf '%s\n' "$created"`,
		Env: []string{
			"HOME=/host/home",
			"TMPDIR=" + userTemp,
			"TMP=" + userTemp,
			"TEMP=" + userTemp,
		},
		Sandbox: backend, RequireSandbox: true, WorkspaceReadOnly: true,
	})
	if err != nil {
		t.Fatalf("unavailable: run shared mktemp: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf(
			"native shared_user_temp mktemp -d failed: exit=%d stdout=%q stderr=%q",
			result.ExitCode, result.Stdout, result.Stderr,
		)
	}
	if !strings.Contains(result.Stdout, userTemp) {
		t.Fatalf("shared mktemp output %q does not use %q", result.Stdout, userTemp)
	}
}
