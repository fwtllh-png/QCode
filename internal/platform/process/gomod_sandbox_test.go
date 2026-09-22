//go:build capability && darwin

package process

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestGoModuleCacheWritableInSandbox(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/t\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "probe_test.go"), []byte(
		"package probe\nimport (\"testing\"; \"strings\")\nfunc TestProbe(t *testing.T) { if strings.TrimSpace(\" ok \") != \"ok\" { t.Fatal(\"probe\") } }\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	// Empty PrivateTemp matches production: auto-created under the system temp.
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: root, AllowNetwork: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skip(err)
	}
	ws, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := ws.OpenDirectory(".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pinned.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := Run(ctx, Options{
		Dir: ws.Root(), DirFile: pinned,
		Command: `go env GOMODCACHE GOCACHE GOTMPDIR HOME && go list -m && go test ./... && go vet ./...`,
		Sandbox: backend, RequireSandbox: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := result.Stdout + "\n" + result.Stderr
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d out=%s", result.ExitCode, out)
	}
	if strings.Contains(out, "mkdir /var: file exists") ||
		strings.Contains(out, "could not create module cache") {
		t.Fatalf("module cache still broken: %s", out)
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "/var/") {
			t.Fatalf("cache path still /var symlink form: %q\nfull:\n%s", line, result.Stdout)
		}
	}
}

func TestInstalledHostToolchainIsReusableInSandbox(t *testing.T) {
	root := t.TempDir()
	privateHome := t.TempDir()
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: root,
		PrivateTemp:   privateHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skip(err)
	}
	policy, ok := sandbox.BackendPolicy(backend)
	if !ok || !hasToolchainExecutable(policy.Toolchains.BinDirs, "cargo") {
		t.Skip("an exposed cargo toolchain is unavailable")
	}
	ws, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := ws.OpenDirectory(".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pinned.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := Run(ctx, Options{
		Dir: ws.Root(), DirFile: pinned,
		Command: `cargo --version && rustc --version`,
		Sandbox: backend, RequireSandbox: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf(
			"host toolchain was not reusable: exit=%d stdout=%s stderr=%s",
			result.ExitCode,
			result.Stdout,
			result.Stderr,
		)
	}
}

func TestInstalledHostNodeRuntimeIsReusableInSandbox(t *testing.T) {
	root := t.TempDir()
	privateHome := t.TempDir()
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: root,
		PrivateTemp:   privateHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skip(err)
	}
	policy, ok := sandbox.BackendPolicy(backend)
	if !ok || !hasToolchainExecutable(policy.Toolchains.BinDirs, "node") {
		t.Skip("an exposed Node.js runtime is unavailable")
	}
	ws, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := ws.OpenDirectory(".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pinned.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := Run(ctx, Options{
		Dir: ws.Root(), DirFile: pinned,
		Command: `node --version && npm --version && ` +
			`node -e 'const c=require("node:child_process");` +
			`const r=c.spawnSync("/bin/sh",["-c","exit 0"]);` +
			`if(r.error)throw r.error;process.exit(r.status??1)'`,
		Sandbox: backend, RequireSandbox: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf(
			"host Node.js runtime was not reusable: exit=%d stdout=%s stderr=%s",
			result.ExitCode,
			result.Stdout,
			result.Stderr,
		)
	}
}

func hasToolchainExecutable(directories []string, name string) bool {
	for _, directory := range directories {
		info, err := os.Stat(filepath.Join(directory, name))
		if err == nil && info.Mode().IsRegular() &&
			info.Mode().Perm()&0o111 != 0 {
			return true
		}
	}
	return false
}
