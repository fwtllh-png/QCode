package state

import (
	"context"
	"errors"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestNoiseOnlyAdvancesWatermarkAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(ctx, Options{DataDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.CloseAll(ctx) }()
	// Noise must never create a reservation, even transiently.
	if _, err := store.SQLite().DB().ExecContext(ctx, `
		CREATE TRIGGER noise_must_be_final BEFORE INSERT ON event_reservations
		WHEN NEW.sequence IN (1, 3)
		BEGIN SELECT RAISE(ABORT, 'noise reservation is forbidden'); END`); err != nil {
		t.Fatal(err)
	}
	changes := func() int {
		t.Helper()
		var count int
		if err := store.SQLite().DB().QueryRowContext(ctx, "SELECT total_changes()").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	noise := []protocol.Event{
		testEventWithData(t, 1, &protocol.OutputDeltaData{Text: "chunk"}),
		testEventWithData(t, 3, &protocol.ToolStateData{State: "running_tools"}),
	}
	for _, event := range noise {
		before := changes()
		if err := store.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
		if writes := changes() - before; writes != 1 {
			t.Fatalf("noise %s changed %d rows, want one watermark UPDATE", event.Kind, writes)
		}
		status, id, err := store.reservation(ctx, event.Sequence)
		if err != nil || status != "" || id != "" {
			t.Fatalf("reservation = %s, %s, %v", status, id, err)
		}
		if err := store.Append(ctx, event); !errors.Is(err, ErrSequenceReserved) {
			t.Fatalf("old noise sequence was accepted: %v", err)
		}
	}
	if err := store.CloseAll(ctx); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, Options{DataDir: root})
	if err != nil {
		t.Fatal(err)
	}
	last, err := store.LastSequence(ctx)
	if err != nil || last != 3 {
		t.Fatalf("restarted watermark = %d, %v", last, err)
	}
	for _, event := range noise {
		if err := store.Append(ctx, event); !errors.Is(err, ErrSequenceReserved) {
			t.Fatalf("restarted noise sequence was accepted: %v", err)
		}
	}
	for _, sequence := range []protocol.Cursor{1, 2, 3} {
		event := testEventWithData(t, sequence, &protocol.OutputDeltaData{Text: "conflict"})
		if err := store.Append(ctx, event); !errors.Is(err, ErrSequenceReserved) {
			t.Fatalf("reuse sequence %d = %v, want ErrSequenceReserved", sequence, err)
		}
	}
	audit := testEventWithData(t, 4, &protocol.TurnCompletedData{Text: "done"})
	if err := store.Append(ctx, audit); err != nil {
		t.Fatal(err)
	}
	events, err := store.Replay(ctx, 0)
	if err != nil || len(events) != 1 || events[0].ID != audit.ID {
		t.Fatalf("replay = %+v, %v; want only durable audit", events, err)
	}
}

func TestMixedBatchKeepsAcceptedNoiseWhenDurableWriteFails(t *testing.T) {
	for _, failure := range []string{"log", "projection"} {
		t.Run(failure, func(t *testing.T) {
			store := openGroupCommitStore(t)
			if failure == "log" {
				if err := store.events.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else if _, err := store.SQLite().DB().ExecContext(t.Context(), `
				CREATE TRIGGER fail_projection BEFORE INSERT ON event_index
				BEGIN SELECT RAISE(ABORT, 'injected projection failure'); END`); err != nil {
				t.Fatal(err)
			}
			batch := []batchEntry{
				{event: groupCommitEvent(1, &protocol.OutputDeltaData{Text: "accepted"}), done: make(chan error, 1)},
				{event: groupCommitEvent(2, &protocol.TurnCompletedData{Text: "done"}), done: make(chan error, 1)},
			}
			store.flushBatch(batch)
			if err := <-batch[0].done; err != nil {
				t.Fatalf("accepted noise became a retry: %v", err)
			}
			if err := <-batch[1].done; err == nil {
				t.Fatal("durable write failure was lost")
			}
			if last, err := store.LastSequence(t.Context()); err != nil || last != 2 {
				t.Fatalf("reserved watermark lost: %d %v", last, err)
			}
			if status, _, err := store.reservation(t.Context(), 1); err != nil || status != "" {
				t.Fatalf("noise created reservation: %q %v", status, err)
			}
		})
	}
}
