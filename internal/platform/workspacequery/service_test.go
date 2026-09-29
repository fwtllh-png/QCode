package workspacequery

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/authority"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/internal/security/vcsbroker"
)

func TestServiceBrowsesSearchesAndReadsWithinWorkspace(t *testing.T) {
	root := t.TempDir()
	initRepository(t, root)
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "src", "main.go"),
		[]byte("package main\n\nfunc hello() {}\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	service, err := New(root, queryTestBackend{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	browse, err := service.Browse(t.Context(), ".", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(browse.Entries) != 1 ||
		browse.Entries[0].Path != "src" ||
		browse.Entries[0].Kind != "directory" {
		t.Fatalf("browse = %+v", browse)
	}
	search, err := service.Search(t.Context(), "hello", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Matches) != 1 ||
		search.Matches[0].Path != "src/main.go" ||
		search.Matches[0].Line != 3 {
		t.Fatalf("search = %+v", search)
	}
	resource, err := service.Resource(t.Context(), "src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if resource.Content != "package main\n\nfunc hello() {}\n" ||
		resource.Digest == "" {
		t.Fatalf("resource = %+v", resource)
	}
}

func TestSwitchBranchRequiresVCSBroker(t *testing.T) {
	root := t.TempDir()
	initRepository(t, root)
	runGit(t, root, "commit", "--allow-empty", "-m", "initial")
	runGit(t, root, "branch", "feature")
	service, err := New(root, queryTestBackend{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SwitchBranch(
		t.Context(),
		"feature",
	); err == nil || !strings.Contains(err.Error(), "VCS Broker") {
		t.Fatalf("missing VCS Broker error = %v", err)
	}
}

func TestServiceRejectsEscapesAndSkippedPaths(t *testing.T) {
	root := t.TempDir()
	initRepository(t, root)
	service, err := New(root, queryTestBackend{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resource(t.Context(), "../secret"); err == nil {
		t.Fatal("expected traversal rejection")
	}
	if _, err := service.Resource(t.Context(),
		filepath.Join(filepath.Dir(root), "secret"),
	); err == nil {
		t.Fatal("expected absolute resource path rejection")
	}
	if _, err := service.Resource(t.Context(), "."); err == nil {
		t.Fatal("expected directory resource rejection")
	}
	if _, err := service.Resource(t.Context(), "node_modules/secret"); err == nil {
		t.Fatal("expected skipped path rejection")
	}
}

func TestServiceDoesNotExposeGitIgnoredFiles(t *testing.T) {
	root := t.TempDir()
	initRepository(t, root)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=hidden\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "add", "-f", ".env")
	command.Dir = root
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git add ignored file: %v: %s", err, output)
	}
	service, err := New(root, queryTestBackend{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Search(t.Context(), "hidden", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 0 {
		t.Fatalf("ignored search matches = %+v", result.Matches)
	}
	if _, err := service.Resource(t.Context(), ".env"); err == nil {
		t.Fatal("expected ignored resource rejection")
	}
}

func TestServiceReportsAndSwitchesCleanGitBranches(t *testing.T) {
	root := t.TempDir()
	initRepository(t, root)
	runGit(t, root, "commit", "--allow-empty", "-m", "initial")
	runGit(t, root, "branch", "feature")
	broker, err := vcsbroker.New(
		root,
		authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}),
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(root, queryTestBackend{}, broker)
	if err != nil {
		t.Fatal(err)
	}
	before, err := service.GitState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !before.Repository || before.Branch == "" ||
		len(before.Branches) != 2 || before.Dirty {
		t.Fatalf("GitState() = %+v", before)
	}
	after, err := service.SwitchBranch(t.Context(), "feature")
	if err != nil {
		t.Fatal(err)
	}
	if after.Branch != "feature" || after.Detached || after.Dirty {
		t.Fatalf("SwitchBranch() = %+v", after)
	}
	if err := os.WriteFile(filepath.Join(root, "dirty.txt"), []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirty, err := service.SwitchBranch(t.Context(), before.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if dirty.Branch != before.Branch || !dirty.Dirty {
		t.Fatalf("dirty branch switch = %+v", dirty)
	}
}

func TestServiceReportsGitStateWhenSandboxRejectsUnrelatedLinks(t *testing.T) {
	root := t.TempDir()
	initRepository(t, root)
	runGit(t, root, "commit", "--allow-empty", "-m", "initial")
	prepares := 0
	service, err := New(root, rejectingQueryBackend{prepares: &prepares}, nil)
	if err != nil {
		t.Fatal(err)
	}
	state, err := service.GitState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !state.Repository || state.Branch == "" {
		t.Fatalf("GitState() = %+v", state)
	}
	if prepares != 0 {
		t.Fatalf("read-only Git metadata used sandbox Prepare %d times", prepares)
	}
}

func TestServiceReadsOnlyEnumeratedSupportedImages(t *testing.T) {
	root := t.TempDir()
	initRepository(t, root)
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("fixture")...)
	if err := os.WriteFile(filepath.Join(root, "diagram.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fake.png"), []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := New(root, queryTestBackend{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	image, err := service.Image(t.Context(), "diagram.png")
	if err != nil {
		t.Fatal(err)
	}
	if image.Path != "diagram.png" ||
		image.MediaType != "image/png" ||
		image.Digest == "" ||
		string(image.Data) != string(png) {
		t.Fatalf("image = %+v", image)
	}
	if _, err := service.Image(t.Context(), "fake.png"); err == nil {
		t.Fatal("text file with an image suffix was accepted")
	}
	if _, err := service.Image(t.Context(), "../diagram.png"); err == nil {
		t.Fatal("image traversal was accepted")
	}
}

func initRepository(t *testing.T, root string) {
	t.Helper()
	command := exec.Command("git", "init", "-q")
	command.Dir = root
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
}

func runGit(t *testing.T, root string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = root
	command.Env = append(
		os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root,
		"GIT_AUTHOR_NAME=QCode", "GIT_AUTHOR_EMAIL=fixture@invalid",
		"GIT_COMMITTER_NAME=QCode", "GIT_COMMITTER_EMAIL=fixture@invalid",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

type queryTestBackend struct{}

type rejectingQueryBackend struct{ prepares *int }

func (rejectingQueryBackend) Capability() sandbox.Capability {
	return queryTestBackend{}.Capability()
}

func (b rejectingQueryBackend) Prepare(
	context.Context,
	sandbox.Command,
) (sandbox.Command, error) {
	if b.prepares != nil {
		(*b.prepares)++
	}
	return sandbox.Command{}, errors.New("workspace link validation failed")
}

func (queryTestBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "test", Backend: "passthrough",
		Available: true,
		Effective: securitymodel.Controls{FilesystemRead: securitymodel.FilesystemReadDeclaredRoots,

			FilesystemWrite: securitymodel.FilesystemWriteExactPaths, Network: securitymodel.NetworkDenied,

			ProcessTree: securitymodel.ProcessTreeGroupKill, CrossProcess: securitymodel.CrossProcessUnrestricted,
			Syscall: securitymodel.SyscallDenyDangerous, IPC: securitymodel.IPCUnrestricted,
			PathIdentity:   securitymodel.PathIdentityDescriptorRelative,
			ArtifactOrigin: securitymodel.ArtifactOriginUnverifiedPath, DurableRecovery: securitymodel.DurableRecoveryMemoryOnly,
		},
	}
}

func (queryTestBackend) Prepare(
	_ context.Context,
	command sandbox.Command,
) (sandbox.Command, error) {
	command.PreparedReadOnly = command.WorkspaceReadOnly
	command.PreparedReadPaths = append(
		[]string(nil), command.AdditionalReadPaths...,
	)
	command.PreparedWritePaths = append(
		[]string(nil), command.WorkspaceWritePaths...,
	)
	command.PreparedNetworkDenied = command.DenyNetwork
	return command, nil
}
