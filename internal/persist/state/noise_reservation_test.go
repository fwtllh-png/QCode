package state

import (
	"context"
	"errors"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestNoiseReservationCommitsFinalStateOnceAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(ctx, Options{DataDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.CloseAll(ctx) }()
	// Reject an intermediate state: the elided slots must become abandoned
	// in their INSERT transaction, not via a later UPDATE.
	if _, err := store.SQLite().DB().ExecContext(ctx, `
		CREATE TRIGGER noise_must_be_final BEFORE INSERT ON event_reservations
		WHEN NEW.sequence IN (1, 3) AND NEW.status <> 'abandoned'
		BEGIN SELECT RAISE(ABORT, 'noise reservation is not final'); END`); err != nil {
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
			t.Fatalf("noise %s changed %d rows, want one reservation INSERT", event.Kind, writes)
		}
		status, id, err := store.reservation(ctx, event.Sequence)
		if err != nil || status != "abandoned" || id != string(event.ID) {
			t.Fatalf("reservation = %s, %s, %v", status, id, err)
		}
		before = changes()
		if err := store.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
		if writes := changes() - before; writes != 0 {
			t.Fatalf("idempotent noise retry changed %d rows", writes)
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
		before := changes()
		if err := store.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
		if changes() != before {
			t.Fatal("restarted noise retry wrote to storage")
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
