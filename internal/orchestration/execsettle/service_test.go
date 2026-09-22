package execsettle

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool/builtin"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestIsolatedTreeWriteDoesNotTakeUserConcurrentEdit(t *testing.T) {
	workspace := newGitWorkspace(t)
	if err := os.Mkdir(filepath.Join(workspace, "generated"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "generated", "keep.txt"), []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "user.txt"), []byte("parent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspace, "add", "generated/keep.txt", "user.txt")
	runGit(t, workspace, "commit", "--quiet", "-m", "seed files")

	service := newTestService(t, workspace)
	isolated, err := service.Begin(t.Context(), "call-overlap", []string{"generated"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = isolated.Close() })
	if isolated.Root() == workspace {
		t.Fatal("command ran in the parent workspace")
	}
	if err := os.WriteFile(
		filepath.Join(isolated.Root(), "generated", "agent.txt"),
		[]byte("agent\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "user.txt"), []byte("user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changes, err := isolated.Settle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range changes {
		if change.Path == "generated/agent.txt" {
			found = true
		}
		if change.Path == "user.txt" {
			t.Fatalf("user concurrent edit was settled as an agent change: %+v", changes)
		}
	}
	if !found {
		t.Fatalf("agent tree write was not settled: %+v", changes)
	}
	if body, err := os.ReadFile(filepath.Join(workspace, "user.txt")); err != nil || string(body) != "user\n" {
		t.Fatalf("user.txt = %q err=%v", body, err)
	}
	if body, err := os.ReadFile(filepath.Join(workspace, "generated", "agent.txt")); err != nil || string(body) != "agent\n" {
		t.Fatalf("agent.txt = %q err=%v", body, err)
	}
}

func TestIsolatedTreeWriteConflictsOnOverlappingUserEdit(t *testing.T) {
	workspace := newGitWorkspace(t)
	if err := os.Mkdir(filepath.Join(workspace, "generated"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "generated", "shared.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspace, "add", "generated/shared.txt")
	runGit(t, workspace, "commit", "--quiet", "-m", "shared")

	service := newTestService(t, workspace)
	isolated, err := service.Begin(t.Context(), "call-conflict", []string{"generated"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = isolated.Close() })
	if err := os.WriteFile(
		filepath.Join(isolated.Root(), "generated", "shared.txt"),
		[]byte("agent\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(workspace, "generated", "shared.txt"),
		[]byte("user\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := isolated.Settle(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "drifted") {
		t.Fatalf("overlapping user edit error = %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(workspace, "generated", "shared.txt")); err != nil ||
		string(body) != "user\n" {
		t.Fatalf("conflicting parent file = %q err=%v", body, err)
	}
}

func TestContentIsolateSettlesWithoutParentGit(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "generated"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "generated", "seed.txt"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := newTestService(t, workspace)
	isolated, err := service.Begin(t.Context(), "call-content", []string{"generated"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = isolated.Close() })
	if err := os.WriteFile(
		filepath.Join(isolated.Root(), "generated", "out.txt"),
		[]byte("created\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	changes, err := isolated.Settle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range changes {
		if change.Path == "generated/out.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("content isolate changes = %+v", changes)
	}
	if body, err := os.ReadFile(filepath.Join(workspace, "generated", "out.txt")); err != nil ||
		string(body) != "created\n" {
		t.Fatalf("settled content = %q err=%v", body, err)
	}
}

func TestPrepareBackendInheritsParentToolchainsAndEnvironment(t *testing.T) {
	workspace := newGitWorkspace(t)
	if err := os.Mkdir(filepath.Join(workspace, "generated"), 0o700); err != nil {
		t.Fatal(err)
	}
	service := newTestService(t, workspace)
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	parent, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: workspace, PrivateTemp: t.TempDir(),
		SkipPATHReadRoots: true,
		Toolchains: &sandbox.ToolchainExposure{
			BinDirs:     []string{bin},
			Environment: []string{"GOROOT=/opt/go"},
		},
		EnvironmentValues: []string{"GOMODCACHE=/cache/go-mod"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(parent) })

	var captured sandbox.Options
	service.newBackend = func(options sandbox.Options) (sandbox.Backend, error) {
		captured = options
		return sandbox.NewPlatformBackend(options)
	}
	isolated, err := service.Begin(t.Context(), "inherit-backend", []string{"generated"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = isolated.Close() })
	backend, closeBackend, err := isolated.PrepareBackend(parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeBackend() })

	if captured.Toolchains == nil ||
		len(captured.Toolchains.BinDirs) != 1 || captured.Toolchains.BinDirs[0] != bin {
		t.Fatalf("captured toolchains = %+v, want bin %s", captured.Toolchains, bin)
	}
	if !strings.Contains(
		strings.Join(captured.EnvironmentValues, ","), "GOMODCACHE=/cache/go-mod",
	) {
		t.Fatalf("captured environment values = %v", captured.EnvironmentValues)
	}
	policy, ok := sandbox.BackendPolicy(backend)
	if !ok {
		t.Fatal("isolated backend has no policy")
	}
	if !slices.Contains(policy.Toolchains.BinDirs, bin) {
		t.Fatalf("isolated toolchain bin dirs = %v", policy.Toolchains.BinDirs)
	}
	if !slices.Contains(policy.HostReadRoots, bin) {
		t.Fatalf("isolated host read roots = %v, want %s", policy.HostReadRoots, bin)
	}
	if !slices.Contains(policy.EnvironmentValues, "GOMODCACHE=/cache/go-mod") {
		t.Fatalf("isolated environment values = %v", policy.EnvironmentValues)
	}
	if !slices.Contains(policy.Toolchains.Environment, "GOROOT=/opt/go") {
		t.Fatalf("isolated toolchain environment = %v", policy.Toolchains.Environment)
	}
}

func newTestService(t *testing.T, workspace string) *Service {
	t.Helper()
	leases := authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{})
	brokers, err := builtin.NewWorkspaceBroker(workspace, leases, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: workspace, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	parent, err := filetool.NewWithBackend(workspace, backend)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := workspacejournal.New(workspace, contentstore.NewMemory(contentstore.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close(t.Context()) })
	if err := journal.Begin("exec-settle-test"); err != nil {
		t.Fatal(err)
	}
	service := New(Options{
		Repository: workspace,
		Scratch:    t.TempDir(),
		Parent:     parent,
		Journal:    journal,
		Gate:       agentengine.NewWorkspaceTurnGate(),
		Brokers:    brokers,
		AllowApply: true,
	})
	if service == nil {
		t.Fatal("execsettle service is nil")
	}
	return service
}

func newGitWorkspace(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	workspace, err := os.MkdirTemp("", "qcode-execsettle-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspace, "init", "--quiet")
	runGit(t, workspace, "config", "user.email", "fixture@example.com")
	runGit(t, workspace, "config", "user.name", "Fixture")
	runGit(t, workspace, "config", "commit.gpgsign", "false")
	runGit(t, workspace, "add", "README.md")
	runGit(t, workspace, "commit", "--quiet", "-m", "seed")
	return workspace
}

func runGit(t *testing.T, workspace string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = workspace
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, out)
	}
}

func TestShadowSessionDiscardsWritesWithoutSettling(t *testing.T) {
	workspace := newGitWorkspace(t)
	if err := os.Mkdir(filepath.Join(workspace, "generated"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(workspace, "generated", "keep.txt"), []byte("keep\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspace, "add", "generated/keep.txt")
	runGit(t, workspace, "commit", "--quiet", "-m", "seed")
	service := newTestService(t, workspace)
	shadow, err := service.BeginShadow(t.Context(), "shadow-1", []string{"generated"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shadow.Close() })
	if err := os.WriteFile(
		filepath.Join(shadow.Root(), "generated", "agent.txt"),
		[]byte("discarded\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	changes, err := shadow.Settle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range changes {
		if change.Path == "generated/agent.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("shadow settle hid the planned change: %+v", changes)
	}
	if _, err := os.Stat(filepath.Join(workspace, "generated", "agent.txt")); !os.IsNotExist(err) {
		t.Fatal("shadow settle wrote into the parent workspace")
	}
	// A later settling Begin on the same trees must not observe the shadow's
	// leftovers: the parent state is untouched.
	body, err := os.ReadFile(filepath.Join(workspace, "generated", "keep.txt"))
	if err != nil || string(body) != "keep\n" {
		t.Fatalf("keep.txt = %q err=%v", body, err)
	}
}

func TestCopyWorkspaceRecreatesSymlinks(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "real.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(source, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(source, "real.txt"), filepath.Join(source, "absolute-link")); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := copyWorkspace(source, target); err != nil {
		t.Fatal(err)
	}
	if link, err := os.Readlink(filepath.Join(target, "link.txt")); err != nil ||
		link != "real.txt" {
		t.Fatalf("relative link = %q err=%v", link, err)
	}
	if link, err := os.Readlink(filepath.Join(target, "absolute-link")); err != nil ||
		link != filepath.Join(source, "real.txt") {
		t.Fatalf("absolute link = %q err=%v", link, err)
	}
	if body, err := os.ReadFile(filepath.Join(target, "link.txt")); err != nil ||
		string(body) != "data" {
		t.Fatalf("linked read = %q err=%v", body, err)
	}
}
