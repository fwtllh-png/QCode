package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollectWriteTreeFilesSkipsProtectedAndSymlinks(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "generated")
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(tree, "keep.txt")
	if err := os.WriteFile(keep, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(tree, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, ".git", "config"), []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(keep, filepath.Join(tree, "link.txt")); err != nil {
		t.Fatal(err)
	}
	files, err := CollectWriteTreeFiles(root, tree, MaxExactWorkspaceWritePaths)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("collected = %v", files)
	}
	workspace, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.Rel(workspace, files[0])
	if err != nil || got != filepath.Join("generated", "keep.txt") {
		t.Fatalf("collected = %v", files)
	}
}

func TestCollectWriteTreeFilesHonorsLimit(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "generated")
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "b.txt"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectWriteTreeFiles(root, tree, 1); err == nil {
		t.Fatal("over-limit write tree was accepted")
	}
}
