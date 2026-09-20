package repoindex

import (
	"os"
	"path/filepath"
	"testing"
)

func writeAllFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		writeFile(t, root, path, content)
	}
}

// compareGraphs asserts that two indexes over the same tree hold identical
// edge multisets and identical ranks — the incremental/full equivalence
// contract, floats included.
func compareGraphs(t *testing.T, incremental, full *Index) {
	t.Helper()
	incrementalEdges, err := incremental.store.Edges(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fullEdges, err := full.store.Edges(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(incrementalEdges) != len(fullEdges) {
		t.Fatalf(
			"edge counts differ: incremental %d, full %d\nincremental: %+v\nfull:       %+v",
			len(incrementalEdges), len(fullEdges), incrementalEdges, fullEdges,
		)
	}
	for index := range incrementalEdges {
		left, right := incrementalEdges[index], fullEdges[index]
		if left.Src != right.Src || left.Dst != right.Dst ||
			left.Kind != right.Kind || left.Weight != right.Weight {
			t.Fatalf(
				"edge %d differs: incremental %+v, full %+v",
				index, left, right,
			)
		}
	}
	incrementalFiles, err := incremental.store.Files(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fullFiles, err := full.store.Files(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(incrementalFiles) != len(fullFiles) {
		t.Fatalf(
			"file counts differ: incremental %d, full %d",
			len(incrementalFiles), len(fullFiles),
		)
	}
	for path, left := range incrementalFiles {
		right, known := fullFiles[path]
		if !known {
			t.Fatalf("path %q missing from the full rebuild", path)
		}
		if left.Rank != right.Rank {
			t.Fatalf(
				"rank for %q differs: incremental %v, full %v",
				path, left.Rank, right.Rank,
			)
		}
	}
}

// An incremental update — a content edit plus a cross-file symbol rename —
// must leave exactly the graph a full rebuild produces. The rename is the
// hard case: referrers of the old and new names re-bind through the affected
// set even though their own rows never moved.
func TestIncrementalGraphMatchesFullRebuild(t *testing.T) {
	root := t.TempDir()
	writeAllFiles(t, root, map[string]string{
		"a/core.go":       "package a\n\nfunc Run() {}\n",
		"b/core.go":       "package b\n\nfunc Other() {}\n",
		"mid/mid.go":      "package mid\n\nimport a \"example/a\"\n\nfunc Wrap() { a.Run() }\n",
		"tests/a_test.go": "package tests\n\nimport m \"example/mid\"\n\nfunc TestWrap() { m.Wrap() }\n",
		"other.go":        "package other\n\nfunc Unrelated() {}\n",
	})
	index, _ := newIndex(t, root, Options{})
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	if edges := mustEdges(t, index); len(edges) == 0 {
		t.Fatal("fixture must produce graph edges")
	}

	// Content-only round: one rename with live referrers, one independent
	// edit. No path or package moves, so this takes the incremental path.
	writeFile(t, root, "a/core.go", "package a\n\nfunc Run2() {}\n")
	writeFile(t, root, "other.go", "package other\n\nfunc Helper() {}\n")
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	full, _ := newIndex(t, root, Options{})
	if _, err := ensureSettled(t, full); err != nil {
		t.Fatal(err)
	}
	compareGraphs(t, index, full)

	// The renamed symbol re-binds through the referrers, not just the file
	// that moved: both indexes must agree the reference edge through the old
	// name is gone while the import edges survive.
	for _, edge := range mustEdges(t, index) {
		if edge.Src == "mid/mid.go" && edge.Dst == "a/core.go" &&
			edge.Kind == EdgeReference {
			t.Fatalf("mid still bound to the pre-rename declaration: %+v", edge)
		}
	}

	// Path-set round: an add and a delete force the full-rebuild fallback.
	writeFile(t, root, "added.go", "package added\n\nfunc Extra() {}\n")
	if err := os.Remove(filepath.Join(root, "other.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	full, _ = newIndex(t, root, Options{})
	if _, err := ensureSettled(t, full); err != nil {
		t.Fatal(err)
	}
	compareGraphs(t, index, full)
}

func mustEdges(t *testing.T, index *Index) []graphEdge {
	t.Helper()
	edges, err := index.store.Edges(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return edges
}

// Repeated small edits keep converging to the full-rebuild graph: induction
// rounds over the incremental path, not just one step.
func TestIncrementalGraphConvergesOverRounds(t *testing.T) {
	root := t.TempDir()
	writeAllFiles(t, root, map[string]string{
		"core.go":        "package core\n\nfunc A() {}\nfunc B() {}\n",
		"left/left.go":   "package left\n\nimport c \"example/core\"\n\nfunc L() { c.A() }\n",
		"right/right.go": "package right\n\nimport c \"example/core\"\n\nfunc R() { c.B() }\n",
		"leaf/ll.go":     "package leaf\n\nimport l \"example/left\"\n\nfunc LL() { l.L() }\n",
		"leaf/lr.go":     "package leaf\n\nimport r \"example/right\"\n\nfunc LR() { r.R() }\n",
	})
	index, _ := newIndex(t, root, Options{})
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	if edges := mustEdges(t, index); len(edges) == 0 {
		t.Fatal("fixture must produce graph edges")
	}
	rounds := []map[string]string{
		{"core.go": "package core\n\nfunc A() {}\nfunc C() {}\n"},
		{"left/left.go": "package left\n\nimport c \"example/core\"\n\nfunc L() { c.C() }\n"},
		{"right/right.go": "package right\n\nfunc R() {}\n"},
		{"leaf/ll.go": "package leaf\n\nfunc LL() {}\n"},
		{"core.go": "package core\n\nfunc C() {}\n"},
	}
	for round, changes := range rounds {
		for path, content := range changes {
			writeFile(t, root, path, content)
		}
		if _, err := ensureSettled(t, index); err != nil {
			t.Fatal(err)
		}
		full, _ := newIndex(t, root, Options{})
		if _, err := ensureSettled(t, full); err != nil {
			t.Fatal(err)
		}
		compareGraphs(t, index, full)
		t.Logf("round %d equivalent", round)
	}
}
