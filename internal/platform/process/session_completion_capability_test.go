//go:build capability && (darwin || linux)

package process

import (
	"os"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestRealSandboxSessionCompletionReapsBackgroundGroup(t *testing.T) {
	if os.Getenv("QCODE_SANDBOX_STAGE") != "1" {
		t.Skip("requires staged platform sandbox")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(), SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if !backend.Capability().Available {
		t.Fatalf("sandbox_unavailable: %+v", backend.Capability())
	}
	base, ok := sandbox.BackendPolicy(backend)
	if !ok {
		t.Fatal("missing sandbox policy")
	}
	pinned, err := OpenPinnedDirectory(backend, base.WorkspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pinned.Close() })
	ctx, err := sandbox.WithExecutionAuthority(t.Context(), sandbox.ExecutionAuthority{
		Digest: strings.Repeat("1", 64), Enforcement: "strong",
		WorkspaceRoot: base.WorkspaceRoot, AllowProcess: true,
		ReadPaths: []string{base.WorkspaceRoot}, RequiredControls: sandbox.DefaultProcessRequirements(),
	})
	if err != nil {
		t.Fatal(err)
	}
	testSessionCompletionReapsBackgroundGroup(t, ctx, SessionOptions{
		Dir: base.WorkspaceRoot, DirFile: pinned, Sandbox: backend,
		RequireSandbox: true, WorkspaceReadOnly: true, DenyNetwork: true,
	})
}
