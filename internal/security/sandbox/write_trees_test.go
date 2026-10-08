package sandbox

import (
	"context"
	"errors"
	"fmt"
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
	files, err := CollectWriteTreeFiles(t.Context(), root, tree)
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

func TestCollectWriteTreeFilesDoesNotCountDirectoryContentsAsGrants(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "generated")
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	count := MaxExactWorkspaceWritePaths + 1
	for i := range count {
		if err := os.WriteFile(filepath.Join(tree, fmt.Sprintf("%04d.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := CollectWriteTreeFiles(t.Context(), root, tree)
	if err != nil || len(files) != count {
		t.Fatalf("files=%d error=%v", len(files), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := CollectWriteTreeFiles(ctx, root, tree); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled walk: %v", err)
	}
}
