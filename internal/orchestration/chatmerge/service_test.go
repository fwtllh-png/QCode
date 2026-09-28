package chatmerge

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
)

func TestMergeBatchAndPlanCompaction(t *testing.T) {
	changes := make([]filetool.Change, 129)
	batches := chunkChatMergeChanges(changes)
	if len(batches) != 3 {
		t.Fatalf("batch count = %d", len(batches))
	}
	if len(batches[0]) != 64 || len(batches[1]) != 64 || len(batches[2]) != 1 {
		t.Fatalf("batches = %v", []int{len(batches[0]), len(batches[1]), len(batches[2])})
	}
	file := compactChatMergePlanFile(tool.EditPlanFile{
		Path: "file.go", Before: "before", After: "after",
	})
	if file.Path != "file.go" || file.Before != "" || file.After != "" {
		t.Fatalf("compacted file = %+v", file)
	}
}

func TestMatchPathPrefix(t *testing.T) {
	if !matchPathPrefix("generated/out.txt", nil) {
		t.Fatal("empty prefixes should match all paths")
	}
	if !matchPathPrefix("generated/out.txt", []string{"generated"}) {
		t.Fatal("prefix should match nested path")
	}
	if matchPathPrefix("user.txt", []string{"generated"}) {
		t.Fatal("unrelated path matched write tree")
	}
	if !matchPathPrefix("generated/out.txt", []string{"."}) {
		t.Fatal("workspace root prefix should match")
	}
}

func TestReadChatMergeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := readChatMergeFile(path)
	if err != nil || !file.exists || string(file.data) != "content\n" {
		t.Fatalf("file = %+v, err = %v", file, err)
	}
	missing, err := readChatMergeFile(path + ".missing")
	if err != nil || missing.exists {
		t.Fatalf("missing = %+v, err = %v", missing, err)
	}
	if err := os.WriteFile(path, []byte{0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readChatMergeFile(path); err == nil ||
		errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
}

func TestChatMergeFileTracksGitVisibleMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restrictive, err := readChatMergeFile(path)
	if err != nil || !restrictive.exists || restrictive.mode != 0o644 {
		t.Fatalf("restrictive file = %+v, err = %v", restrictive, err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	executable, err := readChatMergeFile(path)
	if err != nil || executable.mode != 0o755 {
		t.Fatalf("executable file = %+v, err = %v", executable, err)
	}

	base := chatMergeFile{exists: true, data: []byte("x"), mode: 0o644}
	changed := base
	changed.mode = 0o755
	if equalChatMergeFile(base, changed) {
		t.Fatal("executable-bit-only change must not read as clean")
	}

	promoted := chatMergeChange("file.txt", changed, base)
	if promoted.Mode != 0o755 {
		t.Fatalf("promoted change = %+v", promoted)
	}
	unchanged := chatMergeChange("file.txt", base, base)
	if unchanged.Mode != 0 {
		t.Fatalf("unchanged change = %+v", unchanged)
	}
}
