package workspacejournal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
)

const testWorkspaceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestJournalCommitRevertAndConflict(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := contentstore.NewMemory(contentstore.Options{MaxBytes: 1 << 20, MaxEntries: 32})
	manager, err := New(root, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Begin("turn-1"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Before(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.After(path); err != nil {
		t.Fatal(err)
	}
	if err := manager.Commit("turn-1"); err != nil {
		t.Fatal(err)
	}
	receipt, err := manager.Revert(t.Context(), "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Restored) != 1 || receipt.NonFileSideEffectsReverted {
		t.Fatalf("revert receipt = %+v", receipt)
	}
	if data, _ := os.ReadFile(path); string(data) != "before" {
		t.Fatalf("restored data = %q", data)
	}

	if err := manager.Begin("turn-2"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Before(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("turn-two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.After(path); err != nil {
		t.Fatal(err)
	}
	if err := manager.Commit("turn-2"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	receipt, err = manager.Revert(t.Context(), "turn-2")
	if err == nil || len(receipt.Conflicts) != 1 {
		t.Fatalf("conflict receipt=%+v error=%v", receipt, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "external" {
		t.Fatalf("conflict clobbered external data = %q", data)
	}
}

func TestJournalCommitRetainsOwnershipUntilLedgerPersists(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(t.TempDir(), "journal")
	manager, err := Open(root, directory, testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Begin("turn-retry"); err != nil {
		t.Fatal(err)
	}
	if err := manager.ledger.close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Commit("turn-retry"); err == nil {
		t.Fatal("commit succeeded after the durable ledger was closed")
	}
	if manager.active == nil ||
		manager.active.id != "turn-retry" ||
		manager.committed["turn-retry"] != nil {
		t.Fatalf(
			"failed commit lost journal ownership: active=%+v committed=%+v",
			manager.active,
			manager.committed,
		)
	}
	reopened, err := os.OpenFile(
		filepath.Join(directory, ledgerName),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0o600,
	)
	if err != nil {
		t.Fatal(err)
	}
	reopenedRoot, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	manager.ledger.file = reopened
	manager.ledger.root = reopenedRoot
	if err := manager.Commit("turn-retry"); err != nil {
		t.Fatal(err)
	}
	if manager.active != nil ||
		manager.committed["turn-retry"] == nil {
		t.Fatalf(
			"retried commit state: active=%+v committed=%+v",
			manager.active,
			manager.committed,
		)
	}
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestJournalDraftResumeCommitsOriginalBaseline(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := New(root, contentstore.NewMemory(contentstore.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Begin("source"); err != nil {
		t.Fatal(err)
	}
	write(t, manager, path, "draft\n")
	if err := manager.Suspend("source"); err != nil {
		t.Fatal(err)
	}
	if !manager.HasDraft("source") {
		t.Fatal("suspended Turn did not retain a draft")
	}
	if ids := manager.DraftTurnIDs(); len(ids) != 1 || ids[0] != "source" {
		t.Fatalf("draft turn ids = %v", ids)
	}
	if err := manager.Begin("unrelated"); err == nil {
		t.Fatal("unrelated Turn began on top of a retained draft")
	}
	if err := manager.ResumeDraft("source", "recovery"); err != nil {
		t.Fatal(err)
	}
	write(t, manager, path, "verified\n")
	if err := manager.Commit("recovery"); err != nil {
		t.Fatal(err)
	}

	receipt, err := manager.Revert(t.Context(), "recovery")
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Restored) != 1 {
		t.Fatalf("revert receipt = %+v", receipt)
	}
	if data, _ := os.ReadFile(path); string(data) != "before\n" {
		t.Fatalf("reverted recovery = %q, want original baseline", data)
	}
}

func TestJournalDraftStaysOwnedByAnInFlightRevert(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "a.txt")
	second := filepath.Join(root, "b.txt")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := &blockingGetStore{
		Store:   contentstore.NewMemory(contentstore.Options{}),
		entered: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	manager, err := New(root, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Begin("source"); err != nil {
		t.Fatal(err)
	}
	write(t, manager, first, "draft\n")
	write(t, manager, second, "draft\n")
	if err := manager.Suspend("source"); err != nil {
		t.Fatal(err)
	}
	store.block.Store(true)

	done := make(chan error, 1)
	go func() {
		_, err := manager.Revert(context.Background(), "source")
		done <- err
	}()
	<-store.entered
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for range 100 {
			_ = manager.DraftChanges("source")
		}
	}()
	if err := manager.ResumeDraft("source", "recovery"); err == nil {
		t.Fatal("draft resumed while its revert was still restoring files")
	}
	close(store.release)
	readers.Wait()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if manager.HasDraft("source") {
		t.Fatal("reverted draft is still retained")
	}
}

type blockingGetStore struct {
	contentstore.Store
	block   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *blockingGetStore) Get(ctx context.Context, handle string) ([]byte, error) {
	if s.block.Load() {
		s.entered <- struct{}{}
		<-s.release
	}
	return s.Store.Get(ctx, handle)
}

func TestJournalDoesNotRetainEmptyDraft(t *testing.T) {
	manager, err := New(
		t.TempDir(),
		contentstore.NewMemory(contentstore.Options{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Begin("empty"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Suspend("empty"); err != nil {
		t.Fatal(err)
	}
	if manager.HasDraft("empty") {
		t.Fatal("read-only Turn retained an empty draft")
	}
	if err := manager.Begin("next"); err != nil {
		t.Fatalf("begin after empty draft: %v", err)
	}
}

// write records a turn write end to end: before-image, the write itself, and the
// after fingerprint.
func write(t *testing.T, manager *Manager, path, content string) {
	t.Helper()
	if err := manager.Before(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if content == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.After(path); err != nil {
		t.Fatal(err)
	}
}

// canonicalRoot matches the journal's own path canonicalisation, so tests can
// compare the paths it reports (on macOS /var is a symlink to /private/var).
func canonicalRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// A rollback that conflicts on one file must still restore the others, and must
// keep the before-images of what it could not restore so a retry can finish the
// job once the cause is gone. Here the cause is a directory the process cannot
// write to, which is exactly the kind of conflict retrying can fix.
func TestJournalRollbackKeepsBeforeImagesForRetryAfterConflict(t *testing.T) {
	root := canonicalRoot(t)
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	clean := filepath.Join(root, "clean.txt")
	conflicted := filepath.Join(locked, "conflicted.txt")
	for _, path := range []string{clean, conflicted} {
		if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := New(root, contentstore.NewMemory(contentstore.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Begin("turn-1"); err != nil {
		t.Fatal(err)
	}
	write(t, manager, clean, "after\n")
	write(t, manager, conflicted, "after\n")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	receipt, err := manager.Rollback(t.Context(), "turn-1")

	if err == nil {
		t.Fatal("Rollback() error = nil, want a conflict")
	}
	if len(receipt.Restored) != 1 || receipt.Restored[0] != clean {
		t.Fatalf("restored = %v, want only the writable file", receipt.Restored)
	}
	if len(receipt.Conflicts) != 1 || receipt.Conflicts[0].Path != conflicted {
		t.Fatalf("conflicts = %+v", receipt.Conflicts)
	}
	if data, _ := os.ReadFile(conflicted); string(data) != "after\n" {
		t.Fatalf("conflicted file = %q, want the turn's content still in place", data)
	}

	if err := os.Chmod(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	retry, err := manager.Rollback(t.Context(), "turn-1")

	if err != nil {
		t.Fatalf("retry error = %v, want the retained before-image to be usable", err)
	}
	if len(retry.Restored) != 1 || retry.Restored[0] != conflicted {
		t.Fatalf("retry restored = %v, want only the file left over", retry.Restored)
	}
	for _, path := range []string{clean, conflicted} {
		if data, _ := os.ReadFile(path); string(data) != "before\n" {
			t.Fatalf("%s = %q, want the pre-turn content", path, data)
		}
	}
}

// An external edit is a conflict the journal must never resolve by clobbering,
// so retrying it stays a conflict.
func TestJournalRollbackNeverClobbersAnExternalEdit(t *testing.T) {
	root := canonicalRoot(t)
	path := filepath.Join(root, "value.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := New(root, contentstore.NewMemory(contentstore.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Begin("turn-1"); err != nil {
		t.Fatal(err)
	}
	write(t, manager, path, "after\n")
	if err := os.WriteFile(path, []byte("external\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for attempt := range 2 {
		receipt, err := manager.Rollback(t.Context(), "turn-1")
		if err == nil || len(receipt.Conflicts) != 1 {
			t.Fatalf("attempt %d receipt = %+v error = %v", attempt, receipt, err)
		}
		if data, _ := os.ReadFile(path); string(data) != "external\n" {
			t.Fatalf("attempt %d clobbered the external edit: %q", attempt, data)
		}
	}
}

// A store that cannot hold a before-image must fail the write outright: without
// the image the turn is no longer revertible, so proceeding would silently give
// up the rollback guarantee.
func TestJournalBeforeFailsClosedWhenTheImageCannotBeStored(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	// One byte of capacity cannot hold ten bytes of before-image.
	store := contentstore.NewMemory(contentstore.Options{MaxBytes: 1, MaxEntries: 1})
	manager, err := New(root, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Begin("turn-1"); err != nil {
		t.Fatal(err)
	}

	err = manager.Before(t.Context(), path)

	if !errors.Is(err, contentstore.ErrCapacity) {
		t.Fatalf("Before() error = %v, want ErrCapacity", err)
	}
	if len(manager.Changes()) != 0 {
		t.Fatalf("changes = %+v, want no record for the refused write", manager.Changes())
	}
}

func TestJournalChangesAndBeforeImageDescribeTheActiveTurn(t *testing.T) {
	root := canonicalRoot(t)
	modified := filepath.Join(root, "modified.txt")
	deleted := filepath.Join(root, "deleted.txt")
	created := filepath.Join(root, "created.txt")
	untouched := filepath.Join(root, "rewritten.txt")
	for _, path := range []string{modified, deleted, untouched} {
		if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := New(root, contentstore.NewMemory(contentstore.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(manager.Changes()) != 0 {
		t.Fatal("changes reported without an active turn")
	}
	if err := manager.Begin("turn-1"); err != nil {
		t.Fatal(err)
	}
	write(t, manager, modified, "after\n")
	write(t, manager, deleted, "")
	write(t, manager, created, "new\n")
	write(t, manager, untouched, "before\n") // rewritten with identical bytes

	changes := manager.Changes()

	want := map[string]string{
		modified: ChangeModified, deleted: ChangeDeleted, created: ChangeCreated,
	}
	if len(changes) != len(want) {
		t.Fatalf("changes = %+v, want %d entries", changes, len(want))
	}
	for _, change := range changes {
		if want[change.Path] != change.Kind {
			t.Fatalf("change %+v, want kind %q", change, want[change.Path])
		}
	}

	data, existed, found, err := manager.BeforeImage(t.Context(), modified)
	if err != nil {
		t.Fatal(err)
	}
	if !found || !existed || string(data) != "before\n" {
		t.Fatalf("before image = %q existed=%v found=%v", data, existed, found)
	}
	if _, existed, found, err := manager.BeforeImage(t.Context(), created); err != nil ||
		!found || existed {
		t.Fatalf("created before image existed=%v found=%v err=%v", existed, found, err)
	}
	if _, _, found, err := manager.BeforeImage(
		t.Context(), filepath.Join(root, "never.txt"),
	); err != nil || found {
		t.Fatalf("unknown path found=%v err=%v", found, err)
	}
}

func TestJournalRollbackRejectsUnknownTurn(t *testing.T) {
	manager, err := New(t.TempDir(), contentstore.NewMemory(contentstore.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Rollback(context.Background(), "missing"); err == nil {
		t.Fatal("Rollback() of an unknown turn succeeded")
	}
}

func TestJournalRollbackDeletesCreatedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "created.txt")
	manager, err := New(root, contentstore.NewMemory(contentstore.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Begin("failed-turn"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Before(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.After(path); err != nil {
		t.Fatal(err)
	}
	receipt, err := manager.Rollback(t.Context(), "failed-turn")
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Restored) != 1 {
		t.Fatalf("rollback receipt = %+v", receipt)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("created file remains: %v", err)
	}
}
