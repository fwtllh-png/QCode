package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestMemoryEventStoreReplayCursorAndClose(t *testing.T) {
	store := NewMemoryEventStore(2)
	for sequence := protocol.Cursor(1); sequence <= 3; sequence++ {
		event := protocol.Event{
			Version: protocol.Version, ID: protocol.EventID("evt_test"),
			Sequence: sequence, OperationID: "op", ThreadID: "thread", TurnID: "turn",
			ItemID: "item", Kind: protocol.EventTurnCompleted,
			CreatedAt: time.Now().UTC(), Data: &protocol.TurnCompletedData{},
		}
		if err := store.Append(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Replay(t.Context(), 0); !errors.Is(err, ErrCursorGap) {
		t.Fatalf("stale replay error = %v", err)
	}
	if _, err := store.Replay(t.Context(), 4); !errors.Is(err, ErrCursorAhead) {
		t.Fatalf("ahead replay error = %v", err)
	}
	replay, err := store.Replay(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay) != 2 || replay[0].Sequence != 2 || replay[1].Sequence != 3 {
		t.Fatalf("replay = %+v", replay)
	}
	if err := store.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Replay(t.Context(), 3); !errors.Is(err, ErrClosed) {
		t.Fatalf("replay after close error = %v", err)
	}
}

func TestMemoryEventStoreReplayTurnFiltersRetainedEvents(t *testing.T) {
	store := NewMemoryEventStore(8)
	first := protocol.Event{
		Version: protocol.Version, ID: "evt_a", Sequence: 1, OperationID: "op_a",
		ThreadID: "thread", TurnID: "turn_a", ItemID: "item_a",
		Kind: protocol.EventTurnCompleted, CreatedAt: time.Now().UTC(),
		Data: &protocol.TurnCompletedData{Text: "a"},
	}
	second := protocol.Event{
		Version: protocol.Version, ID: "evt_b", Sequence: 2, OperationID: "op_b",
		ThreadID: "thread", TurnID: "turn_b", ItemID: "item_b",
		Kind: protocol.EventOutputDelta, CreatedAt: time.Now().UTC(),
		Data: &protocol.OutputDeltaData{Text: "chunk"},
	}
	third := protocol.Event{
		Version: protocol.Version, ID: "evt_a2", Sequence: 3, OperationID: "op_a2",
		ThreadID: "thread", TurnID: "turn_a", ItemID: "item_a2",
		Kind: protocol.EventTurnCompleted, CreatedAt: time.Now().UTC(),
		Data: &protocol.TurnCompletedData{Text: "a2"},
	}
	for _, event := range []protocol.Event{first, second, third} {
		if err := store.Append(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.ReplayTurn(t.Context(), "turn_a")
	if err != nil || len(events) != 2 || events[0].ID != first.ID || events[1].ID != third.ID {
		t.Fatalf("ReplayTurn = %+v err=%v", events, err)
	}
	kinds, err := store.ReplayKind(t.Context(), protocol.EventOutputDelta)
	if err != nil || len(kinds) != 1 || kinds[0].ID != second.ID {
		t.Fatalf("ReplayKind = %+v err=%v", kinds, err)
	}
	empty, err := store.ReplayTurn(t.Context(), "")
	if err != nil || empty != nil {
		t.Fatalf("empty ReplayTurn = %+v err=%v", empty, err)
	}
	if err := store.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplayTurn(t.Context(), "turn_a"); !errors.Is(err, ErrClosed) {
		t.Fatalf("ReplayTurn after close error = %v", err)
	}
}

func TestMemoryEventStoreRejectsSequenceGap(t *testing.T) {
	store := NewMemoryEventStore(2)
	event := protocol.Event{
		Version: protocol.Version, ID: "evt_test", Sequence: 2, OperationID: "op",
		ThreadID: "thread", TurnID: "turn", ItemID: "item", Kind: protocol.EventTurnCompleted,
		CreatedAt: time.Now().UTC(), Data: &protocol.TurnCompletedData{},
	}
	if err := store.Append(context.Background(), event); !protocol.IsCode(err, protocol.CodeConflict) {
		t.Fatalf("Append() error = %v", err)
	}
}

func TestMemoryContentStoreCopiesValuesAndCloses(t *testing.T) {
	store := NewMemoryContentStore()
	input := []byte("hello")
	if err := store.Put(t.Context(), "content", input); err != nil {
		t.Fatal(err)
	}
	input[0] = 'x'
	got, err := store.Get(t.Context(), "content")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("content = %q", got)
	}
	got[0] = 'x'
	again, _ := store.Get(t.Context(), "content")
	if string(again) != "hello" {
		t.Fatal("Get returned aliased content")
	}
	if err := store.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), "other", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put after close error = %v", err)
	}
}
