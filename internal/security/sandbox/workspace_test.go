package sandbox

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestWorkspaceRejectsEscapeAndUnsafeFilesystemObjects(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFixture(t, filepath.Join(root, "safe.txt"))
	writeFixture(t, filepath.Join(outside, "outside.txt"))
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(
		filepath.Join(outside, "outside.txt"),
		filepath.Join(root, "hardlink.txt"),
	); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"../outside.txt",
		filepath.Join(outside, "outside.txt"),
		"linked/outside.txt",
		"hardlink.txt",
	} {
		if _, err := workspace.Resolve(name, MustExist); err == nil {
			t.Errorf("Resolve(%q) succeeded", name)
		}
	}
	if got, err := workspace.Resolve("safe.txt", MustExist); err != nil || got != filepath.Join(workspace.Root(), "safe.txt") {
		t.Fatalf("Resolve(safe.txt) = %q, %v", got, err)
	}
}

func TestWorkspaceRequiresExactCaseAndSafeExistingParent(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Exact"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Resolve(filepath.Join("Exact", "new.txt"), AllowMissing); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Resolve(filepath.Join("exact", "new.txt"), AllowMissing); err == nil ||
		!strings.Contains(err.Error(), "incorrect case") {
		t.Fatalf("case-confused path error = %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "unsafe")); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Resolve(filepath.Join("unsafe", "new.txt"), AllowMissing); err == nil {
		t.Fatal("missing file below symlink parent was accepted")
	}
}

func TestWorkspaceDetectsRootReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, filepath.Join(parent, "replaced")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Resolve(".", MustExist); err == nil ||
		!strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("replacement error = %v", err)
	}
}

func TestWorkspaceRevalidationDetectsDirectoryReplacement(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "dir")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(directory, "value"))
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Resolve("dir/value", MustExist); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(directory, filepath.Join(root, "old-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), directory); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Resolve("dir/value", MustExist); err == nil ||
		!strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("replacement error = %v", err)
	}
}

func TestDescriptorRelativeIOResistsConcurrentSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	directory := filepath.Join(root, "dir")
	holding := filepath.Join(root, "holding")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "value"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "value"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	var swapErr error
	var swapMu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			if err := os.Rename(directory, holding); err != nil {
				continue
			}
			if err := os.Symlink(outside, directory); err != nil {
				_ = os.Rename(holding, directory)
				continue
			}
			if err := os.Remove(directory); err != nil {
				swapMu.Lock()
				swapErr = err
				swapMu.Unlock()
				return
			}
			if err := os.Rename(holding, directory); err != nil {
				// A safe descriptor-relative writer may recreate the missing
				// in-workspace directory while the swap fixture is between
				// unlink and restore. Remove that internal replacement and
				// continue the adversarial swap.
				_ = os.RemoveAll(directory)
				if retryErr := os.Rename(holding, directory); retryErr == nil {
					continue
				}
				swapMu.Lock()
				swapErr = err
				swapMu.Unlock()
				return
			}
		}
	}()
	for index := range 500 {
		file, openErr := workspace.OpenFile("dir/value")
		if openErr == nil {
			data, readErr := io.ReadAll(file)
			_ = file.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(data) != "inside" {
				t.Fatalf("iteration %d read escaped content %q", index, data)
			}
		}
		_ = workspace.AtomicWrite("dir/output", []byte("inside-write"), 0o600)
		if _, err := os.Stat(filepath.Join(outside, "output")); !os.IsNotExist(err) {
			t.Fatalf("iteration %d created outside output: %v", index, err)
		}
	}
	stop.Store(true)
	<-done
	swapMu.Lock()
	defer swapMu.Unlock()
	if swapErr != nil {
		t.Fatal(swapErr)
	}
}

func TestAtomicCreateNeverClobbersRacingCreator(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, content := range []string{"first", "second"} {
		content := content
		go func() {
			<-start
			results <- workspace.AtomicCreate("race.txt", []byte(content), 0o600)
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("atomic create successes = %d, want 1", successes)
	}
	data, err := os.ReadFile(filepath.Join(root, "race.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first" && string(data) != "second" {
		t.Fatalf("atomic create content = %q", data)
	}
}

func writeFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
}
