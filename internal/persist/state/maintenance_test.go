package state

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestDeletedEventCleanupRecoversBothRenameCrashWindows(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued_before_rename", true: "renamed_before_projection"}[renamed], func(t *testing.T) {
			root := t.TempDir()
			options := Options{DataDir: root, ArchiveDeletedEvents: true}
			store, err := Open(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			seedThread(t, store, "thread_test", "removed", "open")
			deleted := testEvent(t, 1)
			kept := testEvent(t, 2)
			kept.ThreadID = "surviving-thread"
			if err := store.AppendEvents(t.Context(), deleted, kept,
				testEventWithData(t, 3, &protocol.OutputDeltaData{Text: "tail"})); err != nil {
				t.Fatal(err)
			}
			if _, err := store.SQLite().DB().ExecContext(t.Context(), "DELETE FROM sessions WHERE id = 'session_1'"); err != nil {
				t.Fatal(err)
			}
			if err := store.queueExpiredEvents(t.Context(), time.Now()); err != nil {
				t.Fatal(err)
			}
			if renamed {
				if _, err := store.events.Compact(t.Context(), map[protocol.Cursor]bool{1: true},
					filepath.Join(root, "event-archives")); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.CloseAll(context.Background()); err != nil {
				t.Fatal(err)
			}
			store, err = Open(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			defer store.CloseAll(context.Background())
			events, err := store.Replay(t.Context(), 0)
			if err != nil || len(events) != 1 || events[0].ID != kept.ID {
				t.Fatalf("replay after pruning: %+v %v", events, err)
			}
			if last, err := store.LastSequence(t.Context()); err != nil || last != 3 {
				t.Fatalf("watermark regressed: %d %v", last, err)
			}
			if events, err := store.Replay(t.Context(), 3); err != nil || len(events) != 0 {
				t.Fatalf("tail cursor: %+v %v", events, err)
			}
			if _, found, err := store.EventByID(t.Context(), deleted.ID); err != nil || found {
				t.Fatalf("deleted event revived: %v %v", found, err)
			}
			record, found, err := store.events.ReadRecord(t.Context(), 2)
			if err != nil || !found {
				t.Fatal(err)
			}
			assertEventProjection(t, store, record)
			if err := store.Maintain(t.Context()); err != nil {
				t.Fatal(err)
			}
			archives, err := filepath.Glob(filepath.Join(root, "event-archives", "*.gz"))
			if err != nil || len(archives) != 1 {
				t.Fatalf("archives: %v %v", archives, err)
			}
			file, err := os.Open(archives[0])
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			reader, err := gzip.NewReader(file)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(reader)
			if err != nil || !strings.Contains(string(raw), string(deleted.ID)) || strings.Contains(string(raw), string(kept.ID)) {
				t.Fatalf("archive content mismatch: %v", err)
			}
			_ = reader.Close()
			if err := store.Append(t.Context(), testEvent(t, 4)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDeletedEventRetentionBoundaryAndEmptyLog(t *testing.T) {
	root := t.TempDir()
	store, err := Open(t.Context(), Options{DataDir: root, DeletedEventRetention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer store.CloseAll(context.Background())
	seedThread(t, store, "thread_test", "deleted", "open")
	event := testEvent(t, 1)
	if err := store.Append(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	deletedAt := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	if _, err := store.SQLite().DB().ExecContext(t.Context(),
		"INSERT INTO deleted_event_threads VALUES('thread_test', ?)", timestamp(deletedAt)); err != nil {
		t.Fatal(err)
	}
	if err := store.queueExpiredEvents(t.Context(), deletedAt.Add(time.Hour-time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	if err := store.finishEventPruning(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.EventByID(t.Context(), event.ID); err != nil || !found {
		t.Fatalf("premature prune: %v %v", found, err)
	}
	if err := store.queueExpiredEvents(t.Context(), deletedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.finishEventPruning(t.Context()); err != nil {
		t.Fatal(err)
	}
	if events, err := store.Replay(t.Context(), 0); err != nil || len(events) != 0 {
		t.Fatalf("empty log replay: %+v %v", events, err)
	}
	if err := store.Append(t.Context(), event); !errors.Is(err, ErrSequenceReserved) {
		t.Fatalf("pruned sequence reused: %v", err)
	}
	if _, err := Open(t.Context(), Options{DataDir: t.TempDir(), DeletedEventRetention: -1}); err == nil {
		t.Fatal("negative retention accepted")
	}
}
