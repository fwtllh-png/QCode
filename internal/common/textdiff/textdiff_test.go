package textdiff

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestUnifiedRendersOneHunkWithContext(t *testing.T) {
	t.Parallel()
	before := "one\ntwo\nthree\nfour\nfive\n"
	after := "one\ntwo\nTHREE\nfour\nfive\n"

	diff, stats, err := Unified("f.txt", Text([]byte(before)), Text([]byte(after)), 1)

	if err != nil {
		t.Fatal(err)
	}
	want := "--- a/f.txt\n+++ b/f.txt\n@@ -2,3 +2,3 @@\n two\n-three\n+THREE\n four\n"
	if diff != want {
		t.Fatalf("diff =\n%s\nwant\n%s", diff, want)
	}
	if stats != (Stats{Added: 1, Removed: 1}) {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestUnifiedSplitsDistantChangesIntoSeparateHunks(t *testing.T) {
	t.Parallel()
	var before, after strings.Builder
	for index := range 20 {
		fmt.Fprintf(&before, "line %d\n", index)
		switch index {
		case 0:
			after.WriteString("first\n")
		case 19:
			after.WriteString("last\n")
		default:
			fmt.Fprintf(&after, "line %d\n", index)
		}
	}

	diff, stats, err := Unified(
		"f.txt", Text([]byte(before.String())), Text([]byte(after.String())), DefaultContext,
	)

	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(diff, "@@ -"); got != 2 {
		t.Fatalf("hunks = %d, want 2:\n%s", got, diff)
	}
	if stats != (Stats{Added: 2, Removed: 2}) {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestUnifiedKeepsNearbyChangesInOneHunk(t *testing.T) {
	t.Parallel()
	before := "a\nb\nc\nd\ne\nf\n"
	after := "A\nb\nc\nd\ne\nF\n"

	diff, _, err := Unified("f.txt", Text([]byte(before)), Text([]byte(after)), DefaultContext)

	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(diff, "@@ -"); got != 1 {
		t.Fatalf("hunks = %d, want the shared context to merge them:\n%s", got, diff)
	}
}

func TestUnifiedCreateAndDeleteUseDevNull(t *testing.T) {
	t.Parallel()
	created, stats, err := Unified("new.txt", Absent(), Text([]byte("hello\n")), DefaultContext)
	if err != nil {
		t.Fatal(err)
	}
	want := "--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1 @@\n+hello\n"
	if created != want {
		t.Fatalf("created diff =\n%s\nwant\n%s", created, want)
	}
	if stats != (Stats{Added: 1}) {
		t.Fatalf("created stats = %+v", stats)
	}

	deleted, stats, err := Unified("old.txt", Text([]byte("hello\nbye\n")), Absent(), DefaultContext)
	if err != nil {
		t.Fatal(err)
	}
	want = "--- a/old.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-hello\n-bye\n"
	if deleted != want {
		t.Fatalf("deleted diff =\n%s\nwant\n%s", deleted, want)
	}
	if stats != (Stats{Removed: 2}) {
		t.Fatalf("deleted stats = %+v", stats)
	}
}

// An empty file changes no line, so creating or removing one is a diff with no
// hunks rather than a fake +0/-0 change.
func TestUnifiedEmptyFilesProduceNoHunks(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct{ before, after Content }{
		"created empty": {before: Absent(), after: Text(nil)},
		"deleted empty": {before: Text([]byte{}), after: Absent()},
		"unchanged":     {before: Text([]byte("same\n")), after: Text([]byte("same\n"))},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			diff, stats, err := Unified("f.txt", test.before, test.after, DefaultContext)
			if err != nil {
				t.Fatal(err)
			}
			if diff != "" || stats != (Stats{}) {
				t.Fatalf("diff = %q stats = %+v, want nothing", diff, stats)
			}
		})
	}
}

// A missing final newline is a real difference and must be visible, otherwise
// adding one would read as "no change".
func TestUnifiedMarksMissingFinalNewline(t *testing.T) {
	t.Parallel()
	diff, stats, err := Unified(
		"f.txt", Text([]byte("a\nb")), Text([]byte("a\nb\n")), DefaultContext,
	)

	if err != nil {
		t.Fatal(err)
	}
	want := "--- a/f.txt\n+++ b/f.txt\n@@ -1,2 +1,2 @@\n a\n" +
		"-b\n\\ No newline at end of file\n+b\n"
	if diff != want {
		t.Fatalf("diff =\n%s\nwant\n%s", diff, want)
	}
	if stats != (Stats{Added: 1, Removed: 1}) {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestUnifiedRejectsBinaryContent(t *testing.T) {
	t.Parallel()
	binary := Text([]byte{'a', 0, 'b'})
	for name, test := range map[string]struct{ before, after Content }{
		"before": {before: binary, after: Text([]byte("text\n"))},
		"after":  {before: Text([]byte("text\n")), after: binary},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := Unified("f.bin", test.before, test.after, DefaultContext); !errors.Is(err, ErrBinary) {
				t.Fatalf("Unified() error = %v, want ErrBinary", err)
			}
			if _, err := Count(test.before, test.after); !errors.Is(err, ErrBinary) {
				t.Fatalf("Count() error = %v, want ErrBinary", err)
			}
		})
	}
}

func TestCountAgreesWithUnified(t *testing.T) {
	t.Parallel()
	before := Text([]byte("keep\ndrop\nkeep2\n"))
	after := Text([]byte("keep\nkeep2\nadd1\nadd2\n"))

	_, unifiedStats, err := Unified("f.txt", before, after, DefaultContext)
	if err != nil {
		t.Fatal(err)
	}
	counted, err := Count(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if counted != unifiedStats || counted != (Stats{Added: 2, Removed: 1}) {
		t.Fatalf("count = %+v unified = %+v", counted, unifiedStats)
	}
}

// Above the LCS budget the diff degrades to delete-all/add-all rather than
// allocating a table for every line pair.
func TestUnifiedFallsBackOnHugeInputs(t *testing.T) {
	t.Parallel()
	lines := 2100 // (2100+1)^2 exceeds maxCells
	var before, after strings.Builder
	for index := range lines {
		fmt.Fprintf(&before, "old %d\n", index)
		fmt.Fprintf(&after, "new %d\n", index)
	}

	diff, stats, err := Unified(
		"big.txt", Text([]byte(before.String())), Text([]byte(after.String())), 0,
	)

	if err != nil {
		t.Fatal(err)
	}
	if stats != (Stats{Added: lines, Removed: lines}) {
		t.Fatalf("stats = %+v, want every line replaced", stats)
	}
	if got := strings.Count(diff, "@@ -"); got != 1 {
		t.Fatalf("hunks = %d, want one replace block", got)
	}
	if !strings.Contains(diff, "-old 0\n") || !strings.Contains(diff, "+new 2099\n") {
		t.Fatalf("diff does not cover both sides:\n%s", diff[:200])
	}
}
