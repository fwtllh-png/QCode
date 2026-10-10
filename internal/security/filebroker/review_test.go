package filebroker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestReviewContentBudgetAuthorityAndOwnership(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "build.sh"), []byte("abc"), 0700); err != nil {
		t.Fatal(err)
	}
	w, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	allow := func(string) error { return nil }
	if _, err := CaptureReviewContent(t.Context(), w, ".", []string{"build.sh"}, 2, allow); err == nil {
		t.Fatal("content was silently truncated")
	}
	if _, err := CaptureReviewContent(t.Context(), w, ".", []string{"build.sh"}, 3, func(string) error { return errors.New("denied") }); err == nil {
		t.Fatal("read authority bypassed")
	}
	r, err := CaptureReviewContent(t.Context(), w, ".", []string{"build.sh"}, 3, allow)
	if err != nil {
		t.Fatal(err)
	}
	body := r.Bytes("build.sh")
	body[0] = 'x'
	entries := r.Entries()
	entries[0].Digest = "forged"
	if string(r.Bytes("build.sh")) != "abc" || r.Entries()[0].Digest == "forged" {
		t.Fatal("reviewed content can be mutated")
	}
	if err := os.WriteFile(filepath.Join(root, "build.sh"), []byte("xyz"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(t.Context()); err == nil {
		t.Fatal("same-size script replacement retained evidence")
	}
}

func TestReviewContentRejectsEscapesAndProtectedFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("source", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	w, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../source", filepath.Join(root, "source"), "link", ".git/config", "secrets/value", ".netrc"} {
		t.Run(path, func(t *testing.T) {
			if _, err := CaptureReviewContent(t.Context(), w, ".", []string{path}, 100, func(string) error { return nil }); err == nil {
				t.Fatal("unsafe evidence path admitted")
			}
		})
	}
}

func TestReviewContentRejectsCWDReplacement(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "cwd"), 0700); err != nil {
		t.Fatal(err)
	}
	w, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := CaptureReviewContent(t.Context(), w, "cwd", nil, 1, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "cwd"), filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "cwd"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(t.Context()); err == nil {
		t.Fatal("cwd replacement retained evidence")
	}
}

func TestReviewContentMustRemainOutsideWriteAuthority(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"scripts", "generated"} {
		if err := os.Mkdir(filepath.Join(root, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "build.sh"), []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	w, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := CaptureReviewContent(t.Context(), w, ".", []string{"scripts/build.sh"}, 100, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateReadOnly(t.Context(), []string{"generated", "new-file"}); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{".", "scripts", "scripts/build.sh"} {
		if err := r.ValidateReadOnly(t.Context(), []string{scope}); err == nil {
			t.Fatalf("content is writable through %q", scope)
		}
	}
	t.Run("filesystem case alias", func(t *testing.T) {
		lower, err := os.Stat(filepath.Join(root, "scripts"))
		if err != nil {
			t.Fatal(err)
		}
		upper, err := os.Stat(filepath.Join(root, "SCRIPTS"))
		if os.IsNotExist(err) {
			t.Skip("filesystem is case-sensitive")
		}
		if err != nil || !os.SameFile(lower, upper) {
			t.Fatalf("case alias fixture: %v", err)
		}
		if err := r.ValidateReadOnly(t.Context(), []string{"SCRIPTS"}); err == nil {
			t.Fatal("case alias hid writable content")
		}
	})
}
