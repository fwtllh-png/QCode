//go:build capability && darwin

package process_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/envprep"
	"github.com/fwtllh-png/QCode/internal/environment"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestDeclaredGoModuleCacheWritableInSandbox(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/t\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "probe_test.go"), []byte(
		"package probe\nimport (\"testing\"; \"strings\")\nfunc TestProbe(t *testing.T) { if strings.TrimSpace(\" ok \") != \"ok\" { t.Fatal(\"probe\") } }\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	toolchain := exec.Command("go", "env", "GOROOT")
	toolchain.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	goRoot, err := toolchain.Output()
	if err != nil {
		t.Fatalf("resolve fixture toolchain: %v", err)
	}
	backend, err := declaredToolBackend(t, root, []environment.ResourceRequest{
		{Name: "toolchain", Namespace: environment.NamespaceHostToolchain, Access: environment.AccessRead, Path: strings.TrimSpace(string(goRoot)), Env: "GOROOT"},
		{Name: "build", Namespace: environment.NamespaceCache, Access: environment.AccessWrite, Path: "sandbox-home/cache/build", Env: "GOCACHE", Tree: true},
		{Name: "modules", Namespace: environment.NamespaceCache, Access: environment.AccessWrite, Path: "sandbox-home/cache/modules", Env: "GOMODCACHE", Tree: true},
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
	result, err := process.Run(ctx, process.Options{
		Dir: ws.Root(), DirFile: pinned,
		Command: `go env GOMODCACHE GOCACHE GOTMPDIR HOME && go list -m && go test ./... && go vet ./...`,
		Env:     []string{"GOPROXY=off", "GOTOOLCHAIN=local"},
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

func TestDeclaredHostToolchainIsReusableInSandbox(t *testing.T) {
	root := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	rustup := os.Getenv("RUSTUP_HOME")
	if rustup == "" {
		rustup = filepath.Join(home, ".rustup")
	}
	backend, err := declaredToolBackend(t, root, []environment.ResourceRequest{
		{Name: "toolchain", Namespace: environment.NamespaceHostToolchain, Access: environment.AccessRead, Path: rustup, Env: "RUSTUP_HOME", Tree: true},
		{Name: "cache", Namespace: environment.NamespaceCache, Access: environment.AccessWrite, Path: "sandbox-home/cache/cargo", Env: "CARGO_HOME", Tree: true},
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
	result, err := process.Run(ctx, process.Options{
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

func TestDeclaredNodeRuntimeIsReusableInSandbox(t *testing.T) {
	root := t.TempDir()
	backend, err := declaredToolBackend(t, root, []environment.ResourceRequest{declaredTLSConfig(t)})
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
	result, err := process.Run(ctx, process.Options{
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

func declaredToolBackend(t *testing.T, workspace string, declarations []environment.ResourceRequest) (sandbox.Backend, error) {
	t.Helper()
	private, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		return nil, err
	}
	prepared, err := envprep.Prepare(t.Context(), envprep.Options{
		Sandbox:      sandbox.Options{WorkspaceRoot: workspace, PrivateTemp: private, EnvironmentProfile: environment.ProfileIsolated},
		Declarations: declarations,
	})
	if err != nil {
		return nil, err
	}
	return sandbox.NewPlatformBackend(prepared.Sandbox)
}

// Runtime crypto configuration is an explicit file declaration, separate from
// automatically bound library files and validated public trust certificates.
func declaredTLSConfig(t *testing.T) environment.ResourceRequest {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crypto.cnf")
	if err := os.WriteFile(path, []byte("# test crypto configuration\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return environment.ResourceRequest{Name: "crypto-config", Namespace: environment.NamespaceHostConfig, Access: environment.AccessRead, Path: path, Env: "OPENSSL_CONF"}
}
