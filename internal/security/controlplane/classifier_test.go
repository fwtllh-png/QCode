package controlplane

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestClassifierProtectsRuntimeAndAgentMetadata(t *testing.T) {
	root := t.TempDir()
	classifier, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		".qcode/permissions.toml",
		"nested/.git/config",
		".agents/policy.json",
		".codex/config.toml",
		".qcode-worktree",
	} {
		t.Run(path, func(t *testing.T) {
			if err := classifier.CheckWrite(path, false); !errors.Is(err, ErrProtected) {
				t.Fatalf("CheckWrite(%q) error = %v", path, err)
			}
		})
	}
}

func TestClassifierAllowsBoundedWorkloadWrite(t *testing.T) {
	classifier, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := classifier.CheckWrite("internal/security/policy.go", false); err != nil {
		t.Fatal(err)
	}
}

func TestClassifierRejectsTreeAndOutsideWrites(t *testing.T) {
	root := t.TempDir()
	classifier, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := classifier.CheckWrite("internal", true); err != nil {
		t.Fatalf("bounded subtree write error = %v", err)
	}
	if err := classifier.CheckWrite(".", true); !errors.Is(err, ErrProtected) {
		t.Fatalf("workspace root tree write error = %v", err)
	}
	if err := classifier.CheckWrite(filepath.Join(root, "..", "outside"), false); err == nil {
		t.Fatal("outside write was accepted")
	}
}
