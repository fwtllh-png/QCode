//go:build capability && darwin

package process_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/envprep"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// Isolated posture: HOME/TMPDIR stay in PrivateTemp so compiler outputs
// do not land on host /tmp. The shared-temp posture is
// TestV1NativeSharedUserTempAllowsMktemp.
func TestSandboxCompilerUsesPrivateTempAndHostTmpRemainsDenied(t *testing.T) {
	if err := exec.Command("/usr/bin/xcrun", "--find", "clang++").Run(); err != nil {
		t.Skipf("xcrun clang++ unavailable: %v", err)
	}
	t.Setenv("TMPDIR", "/var/folders/host/T")
	t.Setenv("TMP", "/tmp")
	t.Setenv("TEMP", "/private/tmp")
	root, err := os.MkdirTemp("/private/tmp", "qcode-compiler-workspace-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	private, err := os.MkdirTemp("/private/tmp", "qcode-compiler-private-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(private) })

	if err := os.WriteFile(
		filepath.Join(root, "probe.cc"),
		[]byte("#include <cassert>\n#include <vector>\nint main() { std::vector<int> v(1, 42); assert(v[0] == 42); }\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	prepared, err := envprep.Prepare(t.Context(), envprep.Options{
		Sandbox: sandbox.Options{
			WorkspaceRoot:       root,
			PrivateTemp:         private,
			AllowNetwork:        false,
			EnvironmentContract: "v1",
			EnvironmentProfile:  "isolated",
		},
		SourceEnv: os.Environ(),
	})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(prepared.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skipf("strong sandbox unavailable: %v", err)
	}
	policy, ok := sandbox.BackendPolicy(backend)
	if !ok {
		t.Fatal("sandbox backend has no policy")
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := workspace.OpenDirectory(".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })

	hostTmpTarget := fmt.Sprintf("/tmp/qcode-private-temp-%d", os.Getpid())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := process.Run(ctx, process.Options{
		Dir: workspace.Root(), DirFile: directory,
		Command: fmt.Sprintf(
			`printf '%%s\n' "$TMPDIR" "$TMP" "$TEMP"; `+
				`printf private > "$TMPDIR/probe"; `+
				`if printf escaped > %q; then exit 91; fi; `+
				`clang++ probe.cc -o "$TMPDIR/probe.o" && "$TMPDIR/probe.o"`,
			hostTmpTarget,
		),
		Sandbox: backend, RequireSandbox: true, WorkspaceReadOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf(
			"compiler failed: exit=%d stdout=%q stderr=%q",
			result.ExitCode,
			result.Stdout,
			result.Stderr,
		)
	}
	wantEnvironment := strings.Repeat(policy.PrivateTemp+"\n", 3)
	if result.Stdout != wantEnvironment {
		t.Fatalf("temporary environment = %q, want %q", result.Stdout, wantEnvironment)
	}
	for _, name := range []string{"probe", "probe.o"} {
		if _, err := os.Stat(filepath.Join(policy.PrivateTemp, name)); err != nil {
			t.Fatalf("private temp artifact %s: %v", name, err)
		}
	}
	baseline, baselineErr := process.Run(ctx, process.Options{
		Dir: workspace.Root(), DirFile: directory,
		Command: `unset SDKROOT
clang++ probe.cc -o "$TMPDIR/probe-without-sdk"`,
		Sandbox: backend, RequireSandbox: true, WorkspaceReadOnly: true,
	})
	t.Logf("without projected SDKROOT: exit=%d error=%v stderr=%s",
		baseline.ExitCode, baselineErr, baseline.Stderr)
	if _, err := os.Stat(hostTmpTarget); !os.IsNotExist(err) {
		t.Fatalf("host /tmp write escaped sandbox: %v", err)
	}
}
