package agentcontext

import (
	"reflect"
	"sync"
	"testing"
)

func TestObserveMergesSourcesIntoOneEntry(t *testing.T) {
	ledger := NewWorkingSet()
	ledger.Observe(SourceRead, 1, "internal/foo.go")
	ledger.Observe(SourceEdited, 2, "internal/foo.go")
	ledger.Observe(SourceRead, 3, "internal/foo.go")

	entries := ledger.Select(3, 10)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want one merged entry", entries)
	}
	entry := entries[0]
	if entry.FirstTurn != 1 || entry.LastTurn != 3 {
		t.Fatalf("turns = %d..%d, want 1..3", entry.FirstTurn, entry.LastTurn)
	}
	if len(entry.Sources) != 2 || entry.Sources[0] != SourceEdited || entry.Sources[1] != SourceRead {
		t.Fatalf("sources = %v, want edited before read", entry.Sources)
	}
	if want := weightEdited + weightRead; entry.Score != want {
		t.Fatalf("score = %d, want %d for a path touched this turn", entry.Score, want)
	}
}

func TestObserveIgnoresBlankPathsAndUnknownSources(t *testing.T) {
	ledger := NewWorkingSet()
	ledger.Observe(SourceRead, 1, "   ")
	ledger.Observe(WorkingSetSource("invented"), 1, "internal/foo.go")
	if ledger.Len() != 0 {
		t.Fatalf("entries = %d, want none", ledger.Len())
	}
}

func TestDecayLetsARecentReadOvertakeAnOldEdit(t *testing.T) {
	ledger := NewWorkingSet()
	ledger.Observe(SourceEdited, 1, "old.go")
	ledger.Observe(SourceRead, 1, "new.go")

	if entries := ledger.Select(1, 10); entries[0].Path != "old.go" {
		t.Fatalf("at turn 1 order = %+v, want the edit first", entries)
	}
	// An edit outranks a read for several turns, which is deliberate: the agent
	// has to remember what it changed. Long enough silence still buries it.
	ledger.Observe(SourceRead, 12, "new.go")
	entries := ledger.Select(12, 10)
	if entries[0].Path != "new.go" {
		t.Fatalf("at turn 12 order = %+v, want the fresh read first", entries)
	}
	if entries[1].Score >= entries[0].Score {
		t.Fatalf("scores = %d then %d, want the stale edit lower", entries[0].Score, entries[1].Score)
	}
}

func TestCriticalPathsSortFirstAndSurviveTheLimit(t *testing.T) {
	ledger := NewWorkingSet()
	ledger.Observe(SourcePinned, 1, "pinned.go")
	ledger.Observe(SourcePlan, 1, "planned.go")
	for _, path := range []string{"a.go", "b.go", "c.go"} {
		ledger.Observe(SourceEdited, 9, path)
	}

	// Ten turns of silence must not push a pinned path below a fresh edit, and
	// the pinned paths must not consume the budget meant for discovered ones.
	entries := ledger.Select(11, 2)
	if len(entries) != 4 {
		t.Fatalf("entries = %+v, want both critical paths plus 2 discovered", entries)
	}
	if entries[0].Path != "pinned.go" || entries[1].Path != "planned.go" {
		t.Fatalf("order = %+v, want critical paths first", entries)
	}
	if !entries[0].Critical || entries[2].Critical {
		t.Fatalf("critical flags = %+v", entries)
	}
	for _, entry := range entries[2:] {
		if entry.Path == "c.go" {
			t.Fatalf("entries = %+v, want c.go cut by the limit", entries)
		}
	}
}

func TestSelectBreaksTiesOnPath(t *testing.T) {
	ledger := NewWorkingSet()
	for _, path := range []string{"z.go", "m.go", "a.go"} {
		ledger.Observe(SourceRead, 4, path)
	}
	entries := ledger.Select(4, 0)
	if len(entries) != 3 {
		t.Fatalf("entries = %+v", entries)
	}
	for index, want := range []string{"a.go", "m.go", "z.go"} {
		if entries[index].Path != want {
			t.Fatalf("order = %+v, want lexical tie-break", entries)
		}
	}
}

func TestPathsObservedAtReportsOneTurnOfOneSource(t *testing.T) {
	ledger := NewWorkingSet()
	ledger.Observe(SourceRead, 1, "read-earlier.go")
	ledger.Observe(SourceRead, 2, "read-now.go")
	ledger.Observe(SourceEdited, 2, "written-now.go")
	// A path read again in turn 2 belongs to turn 2, not turn 1.
	ledger.Observe(SourceRead, 2, "read-earlier.go")

	read := ledger.PathsObservedAt(SourceRead, 2)
	if len(read) != 2 || read[0] != "read-earlier.go" || read[1] != "read-now.go" {
		t.Fatalf("read paths = %v", read)
	}
	if paths := ledger.PathsObservedAt(SourceRead, 1); len(paths) != 0 {
		t.Fatalf("turn 1 read paths = %v, want none after the reread", paths)
	}
	if paths := ledger.PathsObservedAt(SourceEdited, 2); len(paths) != 1 {
		t.Fatalf("edited paths = %v", paths)
	}
	allReads := ledger.PathsWithSource(SourceRead)
	if len(allReads) != 2 || allReads[0] != "read-earlier.go" ||
		allReads[1] != "read-now.go" {
		t.Fatalf("paths with source = %v", allReads)
	}
}

func TestCloneDoesNotShareFutureObservations(t *testing.T) {
	parent := NewWorkingSet()
	parent.Observe(SourceEdited, 1, "shared.go")
	child := parent.Clone()
	child.Observe(SourceRead, 2, "child-only.go")
	parent.Observe(SourceRead, 2, "parent-only.go")

	if child.Len() != 2 || parent.Len() != 2 {
		t.Fatalf("parent = %d, child = %d, want two each", parent.Len(), child.Len())
	}
	if paths := child.PathsObservedAt(SourceRead, 2); len(paths) != 1 || paths[0] != "child-only.go" {
		t.Fatalf("child read paths = %v", paths)
	}
	if paths := parent.PathsObservedAt(SourceRead, 2); len(paths) != 1 || paths[0] != "parent-only.go" {
		t.Fatalf("parent read paths = %v", paths)
	}
}

func TestNilLedgerIsUsable(t *testing.T) {
	var ledger *WorkingSetLedger
	ledger.Observe(SourceRead, 1, "foo.go")
	ledger.ObserveAll(SourceEdited, 1, []string{"bar.go"})
	if ledger.Len() != 0 || ledger.Select(1, 5) != nil || ledger.Clone() != nil {
		t.Fatal("a nil ledger must behave as an empty one")
	}
	if paths := ledger.PathsObservedAt(SourceRead, 1); paths != nil {
		t.Fatalf("paths = %v", paths)
	}
}

func TestRetainedDeltaKeepsSourceReadOutsideSelectLimit(t *testing.T) {
	ledger := NewWorkingSet()
	for _, path := range []string{"late.go", "a.go", "b.go"} {
		ledger.Observe(SourceRead, 1, path)
	}
	ledger.Observe(SourceEdited, 1, "edited-only.go")
	ledger.Observe(SourceSearch, 1, "search-only.go")
	selected := ledger.Select(1, 2)
	if len(selected) != 2 {
		t.Fatalf("select = %+v, want two prompt entries", selected)
	}
	kept := map[string]struct{}{}
	for _, entry := range selected {
		kept[entry.Path] = struct{}{}
	}
	if _, ok := kept["late.go"]; ok {
		t.Fatalf("select kept late.go inside the top-2: %+v", selected)
	}
	if _, ok := kept["search-only.go"]; ok {
		t.Fatalf("select kept search-only.go inside the top-2: %+v", selected)
	}
	delta := ledger.RetainedDelta(1, 2, 0)
	var reads []string
	var searchOnly bool
	for _, observation := range delta.Observations {
		if observation.Source == SourceRead {
			reads = append(reads, observation.Path)
		}
		if observation.Path == "search-only.go" {
			searchOnly = true
		}
	}
	if len(reads) != 3 || reads[0] != "a.go" || reads[1] != "b.go" || reads[2] != "late.go" {
		t.Fatalf("retained reads = %v, want all SourceRead paths", reads)
	}
	if searchOnly {
		t.Fatal("retained a search hit that was outside the prompt working set")
	}
}

func TestDeltaRoundTripPreservesObservations(t *testing.T) {
	ledger := NewWorkingSet()
	ledger.Observe(SourceRead, 2, "a.go")
	ledger.Observe(SourceEdited, 3, "a.go")
	ledger.Observe(SourcePlan, 4, "b.go")
	restored := ApplyWorkingSetDelta(ledger.Delta())
	if got, want := restored.Select(4, 10), ledger.Select(4, 10); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored = %+v, want %+v", got, want)
	}
}

func TestConcurrentObservationsAreSerialized(t *testing.T) {
	ledger := NewWorkingSet()
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for turn := uint64(1); turn <= 20; turn++ {
				ledger.Observe(SourceRead, turn, "shared.go")
				ledger.ObserveAll(SourceEdited, turn, []string{"shared.go", "other.go"})
				_ = ledger.Select(turn, 4)
			}
		}(worker)
	}
	wait.Wait()
	if ledger.Len() != 2 {
		t.Fatalf("entries = %d, want two paths", ledger.Len())
	}
}
