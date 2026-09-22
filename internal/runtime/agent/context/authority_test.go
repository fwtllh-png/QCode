package agentcontext

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAuthorityCloneIsolatesMutableState(t *testing.T) {
	source := NewAuthority()
	source.WorkingSet().Observe(SourceRead, 1, "before.go")
	window, err := NewWindowLedger("window-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	source.SetWindow(window)

	cloned := source.Clone()
	cloned.WorkingSet().Observe(SourceEdited, 2, "after.go")
	clonedWindow := cloned.Window()
	clonedWindow.Number = 2
	cloned.SetWindow(clonedWindow)

	if got := source.WorkingSet().PathsObservedAt(SourceEdited, 2); len(got) != 0 {
		t.Fatalf("source working set changed through clone: %v", got)
	}
	if source.Window().Number != 1 {
		t.Fatalf("source window changed through clone: %d", source.Window().Number)
	}
}

func TestCaptureWorkspaceBindingSkipsUnbindableEvidencePaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "keep.go"), []byte("package keep\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	binding, err := CaptureWorkspaceBindingForEvidence(
		root, "", 1, EvidenceDelta{
			Facts: []EvidenceFact{{Path: "/Users/bytedance/eds/eds_metaserver/go.mod"}},
			Changes: []EvidenceChange{{
				Path: "eds_metaserver/lock.go",
			}},
			Reads: []EvidenceReadState{{Path: "keep.go"}},
		},
	)
	if err != nil {
		t.Fatalf("poisoned evidence failed the snapshot: %v", err)
	}
	for _, bound := range binding.BoundPaths {
		if filepath.IsAbs(bound.Path) {
			t.Fatalf("absolute path entered the binding: %+v", bound)
		}
	}
	found := false
	for _, bound := range binding.BoundPaths {
		if bound.Path == "eds_metaserver/lock.go" || bound.Path == "keep.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("bindable evidence was dropped: %+v", binding.BoundPaths)
	}
}
