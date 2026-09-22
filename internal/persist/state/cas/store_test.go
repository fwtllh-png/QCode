package cas

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestStorePersistsContentAndReferencesAcrossRestart(t *testing.T) {
	root := t.TempDir()
	content := []byte("persistent content")
	id := ID(content)

	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), id, content); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), id, content); err != nil {
		t.Fatal(err)
	}
	if err := store.Retain(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	store, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	if got, err := store.Get(t.Context(), id); err != nil {
		t.Fatal(err)
	} else if string(got) != string(content) {
		t.Fatalf("Get() = %q, want %q", got, content)
	}
	if refs := readReferenceCount(t, root, id); refs != 3 {
		t.Fatalf("reference count after restart = %d, want 3", refs)
	}
	if err := store.Release(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if refs := readReferenceCount(t, root, id); refs != 2 {
		t.Fatalf("reference count after release = %d, want 2", refs)
	}
}

func TestStoreDeduplicatesRepeatedPut(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	content := []byte("deduplicated")
	id := ID(content)
	for range 4 {
		if err := store.Put(t.Context(), id, content); err != nil {
			t.Fatal(err)
		}
	}
	if refs := readReferenceCount(t, root, id); refs != 4 {
		t.Fatalf("reference count = %d, want 4", refs)
	}
	entries, err := os.ReadDir(filepath.Join(root, objectParent(id)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != id[2:] {
		t.Fatalf("object entries = %v, want one deduplicated object", entryNames(entries))
	}

	for range 5 {
		if err := store.Release(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	if refs := readReferenceCount(t, root, id); refs != 0 {
		t.Fatalf("reference count after releases = %d, want 0", refs)
	}
	if _, err := store.Get(t.Context(), id); err != nil {
		t.Fatalf("zero-reference content should remain available: %v", err)
	}
}

func TestCollectUnreferencedDeletesZeroReferenceContent(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	kept := []byte("still referenced")
	keptID := ID(kept)
	dropped := []byte("unreferenced orphan")
	droppedID := ID(dropped)
	if err := store.Put(t.Context(), keptID, kept); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), droppedID, dropped); err != nil {
		t.Fatal(err)
	}
	if err := store.Release(t.Context(), droppedID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), droppedID); err != nil {
		t.Fatalf("Release deleted content: %v", err)
	}

	collected, err := store.CollectUnreferenced(t.Context())
	if err != nil || collected != 1 {
		t.Fatalf("CollectUnreferenced = %d, %v, want 1", collected, err)
	}
	if _, err := store.Get(t.Context(), droppedID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("collected content Get() = %v, want not found", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(objectPath(droppedID)))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collected object still on disk: %v", err)
	}
	if got, err := store.Get(t.Context(), keptID); err != nil || string(got) != string(kept) {
		t.Fatalf("referenced content Get() = %q, %v", got, err)
	}

	if err := store.CollectIfUnreferenced(t.Context(), keptID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), keptID); err != nil {
		t.Fatalf("CollectIfUnreferenced deleted a live reference: %v", err)
	}
	if collected, err := store.CollectUnreferenced(t.Context()); err != nil || collected != 0 {
		t.Fatalf("second CollectUnreferenced = %d, %v, want 0", collected, err)
	}
}

func TestReleaseUnreferencedDeletesWhenCountReachesZero(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	content := []byte("drop after last reference")
	id := ID(content)
	if err := store.Put(t.Context(), id, content); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), id, content); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseUnreferenced(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(t.Context(), id); err != nil || string(got) != string(content) {
		t.Fatalf("content after first ReleaseUnreferenced = %q, %v", got, err)
	}
	if err := store.ReleaseUnreferenced(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() after last ReleaseUnreferenced = %v, want not found", err)
	}
}

func TestGetFailsClosedWhenObjectIsTampered(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	content := []byte("authentic")
	id := ID(content)
	if err := store.Put(t.Context(), id, content); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, filepath.FromSlash(objectPath(id)))
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(t.Context(), id)
	if got != nil {
		t.Fatalf("Get() returned tampered bytes %q", got)
	}
	if !errors.Is(err, ErrCorrupt) || !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Get() error = %v, want corruption and digest mismatch", err)
	}
	if err := store.Retain(t.Context(), id); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Retain() error = %v, want digest mismatch", err)
	}
}

func TestStoreRejectsInvalidIDsPathsAndMismatchedContent(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	for _, id := range []string{
		"",
		"../" + strings.Repeat("a", 61),
		strings.Repeat("A", 64),
		strings.Repeat("g", 64),
		strings.Repeat("a", 65),
		"sha256:" + strings.Repeat("a", 64),
	} {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			if err := store.Put(t.Context(), id, nil); !errors.Is(err, ErrInvalidID) {
				t.Fatalf("Put() error = %v, want invalid ID", err)
			}
			if _, err := store.Get(t.Context(), id); !errors.Is(err, ErrInvalidID) {
				t.Fatalf("Get() error = %v, want invalid ID", err)
			}
		})
	}

	content := []byte("right bytes")
	wrongID := ID([]byte("wrong bytes"))
	if err := store.Put(t.Context(), wrongID, content); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Put() error = %v, want digest mismatch", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(objectPath(wrongID)))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched Put created an object: %v", err)
	}

	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil {
		t.Fatal("Open() accepted a symbolic-link root")
	}
}

func TestStoreRejectsSymlinkedObject(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	content := []byte("content")
	id := ID(content)
	if err := store.Put(t.Context(), id, content); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, filepath.FromSlash(objectPath(id)))
	backup := path + ".backup"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(backup), path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), id); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get() error = %v, want corruption", err)
	}
}

func TestStoreCoordinatesConcurrentInstances(t *testing.T) {
	root := t.TempDir()
	first, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = first.Close(context.Background())
		_ = second.Close(context.Background())
	})

	content := []byte("shared concurrent content")
	id := ID(content)
	const workers = 64
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wait sync.WaitGroup
	for worker := range workers {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			<-start
			store := first
			if worker%2 != 0 {
				store = second
			}
			if err := store.Put(t.Context(), id, content); err != nil {
				errs <- err
				return
			}
			got, err := store.Get(t.Context(), id)
			if err != nil {
				errs <- err
			} else if string(got) != string(content) {
				errs <- fmt.Errorf("content = %q", got)
			}
		}(worker)
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if refs := readReferenceCount(t, root, id); refs != workers {
		t.Fatalf("concurrent reference count = %d, want %d", refs, workers)
	}
}

func TestStoreDeleteCloseAndContext(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("delete me")
	id := ID(content)
	if err := store.Put(t.Context(), id, content); err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Retain(cancelled, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("Retain() error = %v, want canceled", err)
	}
	if err := store.Close(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close() error = %v, want canceled", err)
	}
	if _, err := store.Get(t.Context(), id); err != nil {
		t.Fatalf("canceled Close closed store: %v", err)
	}

	if err := store.Delete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() after Delete error = %v, want not found", err)
	}
	if err := store.Delete(t.Context(), id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete() error = %v, want not found", err)
	}
	if err := store.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(t.Context()); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if err := store.Put(t.Context(), id, content); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put() after Close error = %v, want closed", err)
	}
}

func TestStoreRejectsTamperedReferenceMetadata(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	content := []byte("metadata")
	id := ID(content)
	if err := store.Put(t.Context(), id, content); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, filepath.FromSlash(referencePath(id)))
	if err := os.WriteFile(path, []byte("v1 not-a-number\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), id); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get() error = %v, want corruption", err)
	}
}

func readReferenceCount(t *testing.T, root, id string) uint64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(referencePath(id))))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 || fields[0] != metadataVersion {
		t.Fatalf("invalid reference metadata %q", data)
	}
	count, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names
}
