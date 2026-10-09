package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestToolchainBindsEveryExecutableSymlinkWithoutAdjacentFiles(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin, middle, real := filepath.Join(root, "bin"), filepath.Join(root, "package", "bin"), filepath.Join(root, "package", "libexec", "bin")
	for _, dir := range []string{bin, middle, real} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	entry, alias, target := filepath.Join(bin, "tool"), filepath.Join(middle, "tool"), filepath.Join(real, "tool")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nprintf ok"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../libexec/bin/tool", alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(alias, entry); err != nil {
		t.Fatal(err)
	}
	policy, err := BuildPolicy(Options{WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(), EnvironmentValues: []string{"PATH=" + bin}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(policy.HostReadFiles, alias) || !policy.ExecutableReadable(entry, nil) {
		t.Fatalf("intermediate alias not granted: %v", policy.HostReadFiles)
	}
	adjacent := filepath.Join(middle, "unrelated")
	if err := os.WriteFile(adjacent, []byte("private"), 0700); err != nil {
		t.Fatal(err)
	}
	if policy.ExecutableReadable(adjacent, nil) {
		t.Fatal("adjacent file was exposed")
	}
	policy.HostReadFiles = nil
	if policy.ExecutableReadable(entry, nil) {
		t.Fatal("preflight missed the ungranted intermediate link")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(entry, alias); err != nil {
		t.Fatal(err)
	}
	if policy.ExecutableReadable(entry, nil) {
		t.Fatal("cyclic link accepted")
	}
}
