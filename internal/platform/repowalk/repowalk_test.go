package repowalk

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestListDefersEveryIgnoreRuleToGit(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-q")
	globalIgnore := filepath.Join(root, "global-ignore")
	write(t, globalIgnore, "global.txt\n")
	git(t, root, "config", "core.excludesFile", globalIgnore)
	write(t, filepath.Join(root, ".gitignore"), "ignored.txt\nignored-dir/\n")
	write(t, filepath.Join(root, "nested", ".gitignore"), "local.txt\n")
	write(t, filepath.Join(root, ".git", "info", "exclude"), "info.txt\n")
	for _, name := range []string{
		"visible.txt", "ignored.txt", "ignored-dir/deep.txt", "info.txt", "global.txt",
		"nested/kept.txt", "nested/local.txt", "untracked.txt",
	} {
		write(t, filepath.Join(root, name), "body\n")
	}
	git(t, root, "add", "visible.txt", "nested/kept.txt")
	git(t, root, "add", "-f", "ignored.txt")

	listing := list(t, root)
	if listing.Source != SourceGit {
		t.Fatalf("source = %q", listing.Source)
	}
	// The untracked file is listed because it is not ignored; every file an
	// ignore rule covers is absent, including the one a nested .gitignore names.
	if got := paths(listing); !equal(got, []string{
		".gitignore", "global-ignore", "nested/.gitignore", "nested/kept.txt",
		"untracked.txt", "visible.txt",
	}) {
		t.Fatalf("paths = %#v", got)
	}
}

func TestListFallsBackToAPlainWalkWithoutGit(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".gitignore"), "ignored.txt\n")
	write(t, filepath.Join(root, "ignored.txt"), "body\n")
	write(t, filepath.Join(root, "kept.txt"), "body\n")

	listing := list(t, root)
	if listing.Source != SourceWalk {
		t.Fatalf("source = %q", listing.Source)
	}
	// Without a repository nothing can honour .gitignore, so the file is listed
	// rather than silently dropped.
	if got := paths(listing); !equal(got, []string{".gitignore", "ignored.txt", "kept.txt"}) {
		t.Fatalf("paths = %#v", got)
	}
}

func TestListFallsBackWhenGitCannotRun(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-q")
	write(t, filepath.Join(root, "kept.txt"), "body\n")

	walker, err := New(root, walkTestBackend{}, Options{
		Run: func(context.Context, process.Options) (process.Result, error) {
			return process.Result{}, errors.New("git is not installed")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	listing, err := walker.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if listing.Source != SourceWalk || !contains(paths(listing), "kept.txt") {
		t.Fatalf("listing = %+v", listing)
	}
}

func TestListSkipsVendorDirectoriesEvenWhenTracked(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-q")
	for _, name := range []string{
		"main.go", "vendor/dep/dep.go", "node_modules/pkg/index.js", "bin/tool",
		".qcode/state.db",
	} {
		write(t, filepath.Join(root, name), "body\n")
	}
	git(t, root, "add", ".")

	listing := list(t, root)
	if got := paths(listing); !equal(got, []string{"main.go"}) {
		t.Fatalf("paths = %#v", got)
	}
	if listing.Skips.Ignored != 4 {
		t.Fatalf("skipped ignored = %d", listing.Skips.Ignored)
	}
	if !Skippable("vendor/dep/dep.go") || Skippable("main.go") {
		t.Fatal("Skippable disagrees with the listing")
	}
}

func TestListSkipsTargetBuildArtifactsEvenWhenTracked(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-q")
	write(t, filepath.Join(root, "main.go"), "body\n")
	write(t, filepath.Join(root, "target", "debug", "deps.rs"), "body\n")
	git(t, root, "add", ".")

	listing := list(t, root)
	if got := paths(listing); !equal(got, []string{"main.go"}) {
		t.Fatalf("paths = %#v", got)
	}
	if listing.Skips.Ignored < 1 {
		t.Fatalf("skipped ignored = %d", listing.Skips.Ignored)
	}
	if !Skippable("target/debug/deps.rs") {
		t.Fatal("target build artifacts must be skippable")
	}
}

func TestReadSkipsMultiplyLinkedFiles(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "original.txt")
	linked := filepath.Join(root, "linked.txt")
	write(t, original, "secret-should-not-fail-search\n")
	if err := os.Link(original, linked); err != nil {
		t.Fatal(err)
	}
	walker, err := New(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	content, reason, err := walker.Read(Entry{Path: "linked.txt"}, 0)
	if err != nil || reason != SkipLinked || content.Data != nil {
		t.Fatalf("content = %+v reason=%q err=%v", content, reason, err)
	}
}

func TestListLeavesOutSymlinksAndDeletedIndexEntries(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-q")
	write(t, filepath.Join(root, "real.txt"), "body\n")
	write(t, filepath.Join(root, "gone.txt"), "body\n")
	if err := os.Symlink("real.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", ".")
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	listing := list(t, root)
	if got := paths(listing); !equal(got, []string{"real.txt"}) {
		t.Fatalf("paths = %#v", got)
	}
	if listing.Skips.Symlink != 1 || listing.Skips.Missing != 1 {
		t.Fatalf("skips = %+v", listing.Skips)
	}
}

func TestListStopsOnCancellation(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "kept.txt"), "body\n")
	walker, err := New(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := walker.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadAppliesTheSharedFilePolicy(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "text.txt"), "body\n")
	write(t, filepath.Join(root, "large.txt"), strings.Repeat("x", 64))
	if err := os.WriteFile(filepath.Join(root, "binary.txt"), []byte("a\x00b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "invalid.txt"), []byte{0xff, 'a'}, 0o600); err != nil {
		t.Fatal(err)
	}
	walker, err := New(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	listing, err := walker.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	entries := make(map[string]Entry, len(listing.Files))
	for _, entry := range listing.Files {
		entries[entry.Path] = entry
	}

	for name, want := range map[string]SkipReason{
		"text.txt": SkipNone, "large.txt": SkipLarge,
		"binary.txt": SkipBinary, "invalid.txt": SkipEncoding,
	} {
		content, reason, err := walker.Read(entries[name], 32)
		if err != nil {
			t.Fatal(err)
		}
		if reason != want {
			t.Fatalf("%s reason = %q, want %q", name, reason, want)
		}
		if want == SkipNone && (string(content.Data) != "body\n" || content.Digest != Digest([]byte("body\n"))) {
			t.Fatalf("%s content = %+v", name, content)
		}
		if want != SkipNone && content.Data != nil {
			t.Fatalf("%s returned data for a skipped file", name)
		}
	}

	// A file whose size passes but whose body exceeds the limit is still large:
	// the limit is on bytes read, not on what the directory entry claimed.
	if _, reason, err := walker.Read(Entry{Path: "large.txt"}, 32); err != nil || reason != SkipLarge {
		t.Fatalf("reason = %q, err = %v", reason, err)
	}
}

func TestReadReportsAVanishedFileWithoutFailing(t *testing.T) {
	root := t.TempDir()
	walker, err := New(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	content, reason, err := walker.Read(Entry{Path: "absent.txt"}, 0)
	if err != nil || reason != SkipMissing || content.Data != nil {
		t.Fatalf("content = %+v, reason = %q, err = %v", content, reason, err)
	}
}

func list(t *testing.T, root string) Listing {
	t.Helper()
	walker, err := New(root, walkTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	listing, err := walker.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return listing
}

func paths(listing Listing) []string {
	result := make([]string, 0, len(listing.Files))
	for _, entry := range listing.Files {
		result = append(result, entry.Path)
	}
	return result
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+directory,
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

type walkTestBackend struct{}

func (walkTestBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "test", Backend: "passthrough",
		Available: true,
		Effective: controlmatrix.Matrix{FilesystemRead: controlmatrix.
			FilesystemReadDeclaredRoots,

			FilesystemWrite: controlmatrix.
				FilesystemWriteExactPaths, Network: controlmatrix.NetworkDenied,
			ProcessTree: controlmatrix.ProcessTreeGroupKill, CrossProcess: controlmatrix.CrossProcessUnrestricted, Syscall: controlmatrix.SyscallDenyDangerous,
			IPC: controlmatrix.
				IPCUnrestricted, PathIdentity: controlmatrix.PathIdentityDescriptorRelative,
			ArtifactOrigin: controlmatrix.ArtifactOriginUnverifiedPath, DurableRecovery: controlmatrix.DurableRecoveryMemoryOnly},
	}
}

func (walkTestBackend) Prepare(_ context.Context, command sandbox.Command) (sandbox.Command, error) {
	return command, nil
}
