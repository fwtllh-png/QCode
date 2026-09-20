package repoindex

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Callers that queue behind an in-flight refresh must share its result: one
// walk answers all of them. A repository big enough to keep the first refresh
// running while the others arrive keeps the overlap virtually certain.
func TestEnsureDedupesConcurrentWaiters(t *testing.T) {
	root := t.TempDir()
	for directory := 0; directory < 40; directory++ {
		for file := 0; file < 5; file++ {
			writeFile(
				t,
				root,
				fmt.Sprintf("pkg%02d/file%02d.go", directory, file),
				fmt.Sprintf("package pkg%02d\n\nfunc F%02d() {}\n", directory, file),
			)
		}
	}
	index, _ := newIndex(t, root, Options{})

	const callers = 8
	results := make(chan Snapshot, callers)
	var start sync.WaitGroup
	start.Add(callers)
	var release sync.WaitGroup
	release.Add(callers)
	for caller := 0; caller < callers; caller++ {
		go func() {
			start.Done()
			start.Wait()
			snapshot, err := ensureSettled(t, index)
			if err != nil {
				t.Error(err)
			}
			results <- snapshot
			release.Done()
		}()
	}
	release.Wait()
	close(results)
	first, distinct := time.Time{}, 0
	for snapshot := range results {
		if !snapshot.Ready() {
			t.Fatalf("snapshot not ready: %+v", snapshot)
		}
		if distinct == 0 {
			first = snapshot.Meta.RefreshedAt
		} else if !snapshot.Meta.RefreshedAt.Equal(first) {
			distinct++
		}
	}
	if distinct != 0 {
		t.Fatalf(
			"%d of %d concurrent callers ran their own refresh",
			distinct+1, callers,
		)
	}
}

// The dedupe covers only overlapping callers: a caller that arrives after a
// refresh completed must still refresh — this pins that no refresh interval
// snuck in with the change.
func TestEnsureSequentialCallsStillRefresh(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "api.go", "package api\n\nfunc Serve() {}\n")
	index, _ := newIndex(t, root, Options{})
	first, err := ensureSettled(t, index)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	second, err := ensureSettled(t, index)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Meta.RefreshedAt.After(first.Meta.RefreshedAt) {
		t.Fatalf(
			"sequential refresh reused the previous snapshot: %v then %v",
			first.Meta.RefreshedAt, second.Meta.RefreshedAt,
		)
	}
}

// SymbolsFrom answers from the generation its snapshot confirmed: a change
// made after that snapshot stays invisible until a refreshing call runs.
func TestSymbolsFromSkipsTheRefresh(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "api.go", "package api\n\nfunc Serve() {}\n")
	index, _ := newIndex(t, root, Options{})
	snapshot, err := ensureSettled(t, index)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, root, "api.go", "package api\n\nfunc Serve() {}\n\nfunc Close() {}\n")
	pinned, _, err := index.SymbolsFrom(
		t.Context(),
		snapshot,
		Query{Name: "Close", Exact: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(pinned) != 0 {
		t.Fatalf("pinned generation saw the later change: %#v", pinned)
	}
	refreshed, refreshedSnapshot, err := index.Symbols(
		t.Context(),
		Query{Name: "Close", Exact: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshed) != 1 || refreshed[0].Path != "api.go" {
		t.Fatalf("refreshing query = %#v", refreshed)
	}
	nonReady, _, err := index.SymbolsFrom(
		t.Context(),
		Snapshot{Status: StatusDegraded},
		Query{Name: "Serve", Exact: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(nonReady) != 1 || !refreshedSnapshot.Ready() {
		t.Fatalf("non-ready snapshot must fall back to the refreshing path")
	}
}
