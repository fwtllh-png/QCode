package repoindex

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSnapshotDoesNotWaitForRefresh(t *testing.T) {
	for _, initialBuild := range []bool{true, false} {
		name := "refresh"
		if initialBuild {
			name = "initial build"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "api.go", "package api\nfunc Serve() {}\n")
			var calls, pauseAt atomic.Int64
			entered, release := make(chan struct{}), make(chan struct{})
			var unblock sync.Once
			index, _ := newIndex(t, root, Options{Now: func() time.Time {
				if calls.Add(1) == pauseAt.Load() {
					close(entered)
					<-release
				}
				return time.Now()
			}})
			if !initialBuild {
				if _, err := ensureSettled(t, index); err != nil {
					t.Fatal(err)
				}
				writeFile(t, root, "api.go", "package api\nfunc Serve() {}\nfunc Close() {}\n")
			}
			previous := index.Snapshot()
			// The first clock call is admission; the second runs inside scan
			// while Ensure holds the refresh mutex.
			pauseAt.Store(calls.Load() + 2)
			type outcome struct {
				snapshot Snapshot
				err      error
			}
			done := make(chan outcome, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				snapshot, err := index.Ensure(t.Context())
				done <- outcome{snapshot, err}
			}()
			t.Cleanup(func() {
				unblock.Do(func() { close(release) })
				<-finished
				index.waitGraphSettled()
			})
			select {
			case <-entered:
			case result := <-done:
				t.Fatalf("refresh did not reach scan: %+v", result)
			case <-time.After(5 * time.Second):
				t.Fatal("refresh did not start")
			}
			read := make(chan Snapshot, 1)
			go func() { read <- index.Snapshot() }()
			select {
			case snapshot := <-read:
				if snapshot != previous {
					t.Fatalf("in-flight refresh published partial state: got %+v, want %+v", snapshot, previous)
				}
			case <-time.After(5 * time.Second):
				unblock.Do(func() { close(release) })
				<-read
				<-done
				t.Fatal("status read waited for the refresh")
			}
			unblock.Do(func() { close(release) })
			result := <-done
			if result.err != nil || !result.snapshot.Ready() {
				t.Fatalf("refresh = %+v", result)
			}
			if snapshot := index.Snapshot(); snapshot != result.snapshot {
				t.Fatalf("completed snapshot = %+v, want %+v", snapshot, result.snapshot)
			}
			result.snapshot.Meta.FileCount = -1
			if index.Snapshot().Meta.FileCount == -1 {
				t.Fatal("caller mutated the published snapshot")
			}
		})
	}
}

func TestSnapshotPreservesPublishedStateOnCancellation(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "api.go", "package api\nfunc Serve() {}\n")
	index, _ := newIndex(t, root, Options{})
	for attempt := 0; attempt < 2; attempt++ {
		previous := index.Snapshot()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := index.Ensure(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled refresh error = %v", err)
		}
		if snapshot := index.Snapshot(); snapshot != previous {
			t.Fatalf("cancellation replaced published state: %+v -> %+v", previous, snapshot)
		}
		if _, err := ensureSettled(t, index); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSnapshotPublishesDegradedState(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "api.go", "package api\nfunc Serve() {}\n")
	index, store := newIndex(t, root, Options{})
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), "DROP TABLE repo_index_symbols"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "api.go", "package api\nfunc Serve() {}\nfunc Close() {}\n")
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	snapshot := index.Snapshot()
	if snapshot.Status != StatusDegraded || snapshot.Detail == "" {
		t.Fatalf("failed refresh did not publish degradation: %+v", snapshot)
	}
}
