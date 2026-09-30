//go:build darwin

package process

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestRunPinsWorkingDirectoryToDescriptor(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	directory := filepath.Join(root, "dir")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "marker"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "marker"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	directoryFile, err := workspace.OpenDirectory("dir")
	if err != nil {
		t.Fatal(err)
	}
	defer directoryFile.Close()
	if err := os.Rename(directory, filepath.Join(root, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, directory); err != nil {
		t.Fatal(err)
	}

	backend := &recordingBackend{root: root}
	result, err := Run(t.Context(), Options{
		Command: "cat marker", Dir: directory, DirFile: directoryFile,
		Sandbox: backend, RequireSandbox: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "inside" || result.ExitCode != 0 {
		t.Fatalf("descriptor cwd result = %+v", result)
	}
	if backend.command.DirectoryFD != 3 {
		t.Fatalf("prepared directory fd = %d, want 3", backend.command.DirectoryFD)
	}
}
