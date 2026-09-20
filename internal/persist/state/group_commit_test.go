package state

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func openGroupCommitStore(t testing.TB) *Store {
	t.Helper()
	store, err := Open(t.Context(), Options{DataDir: t.TempDir(), BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	return store
}

// groupCommitEvent builds a valid durable event without testing.TB failure
// paths, so worker goroutines can construct rebuilds while retrying. IDs come
// from an atomic counter: wall-clock nanoseconds collide across cores, and a
// shared ID would legitimately coalesce two appends through the idempotent
// reservation path.
var groupCommitEventIDs atomic.Uint64

func groupCommitEvent(
	sequence protocol.Cursor,
	data protocol.EventData,
) protocol.Event {
	kind := protocol.EventTurnCompleted
	switch data.(type) {
	case *protocol.OutputDeltaData:
		kind = protocol.EventOutputDelta
	}
	return protocol.Event{
		Version: protocol.Version,
		ID: protocol.EventID(fmt.Sprintf(
			"evt-gc-%d-%d", groupCommitEventIDs.Add(1), sequence,
		)),
		Sequence:    sequence,
		OperationID: "operation-group-commit",
		ThreadID:    "thread_gc",
		TurnID:      "turn_gc",
		ItemID:      "item_gc",
		Kind:        kind,
		CreatedAt:   time.Now().UTC(),
		Data:        data,
	}
}

// Concurrent appends commit through shared batches. Sequence assignment can
// race ahead of the durable watermark exactly as concurrent hubs do, so the
// producers retry like the event hub does: read the watermark, rebuild the
// event with a fresh sequence, append again.
func TestAppendGroupsConcurrentProducers(t *testing.T) {
	store := openGroupCommitStore(t)
	const producers, perProducer = 8, 10
	appendWithHubRetry := func(event protocol.Event) error {
		for attempt := 0; ; attempt++ {
			err := store.Append(t.Context(), event)
			if err == nil {
				return nil
			}
			if !errors.Is(err, ErrSequenceReserved) || attempt >= 64 {
				return err
			}
			last, sequenceErr := store.LastSequence(t.Context())
			if sequenceErr != nil {
				return sequenceErr
			}
			event = groupCommitEvent(last+1, event.Data)
		}
	}
	var wg sync.WaitGroup
	failures := make(chan error, producers)
	for producer := 0; producer < producers; producer++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for index := 0; index < perProducer; index++ {
				sequence := protocol.Cursor(base + index + 1)
				event := groupCommitEvent(sequence, &protocol.TurnCompletedData{Text: "ok"})
				if err := appendWithHubRetry(event); err != nil {
					failures <- err
					return
				}
			}
		}(producer * perProducer)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("concurrent append: %v", err)
	}
	replayed, err := store.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != producers*perProducer {
		t.Fatalf("replayed %d events, want %d", len(replayed), producers*perProducer)
	}
	seen := make(map[protocol.Cursor]bool, len(replayed))
	for index, event := range replayed {
		if index != 0 && event.Sequence <= replayed[index-1].Sequence {
			t.Fatalf("log order broke at position %d: %d after %d",
				index, event.Sequence, replayed[index-1].Sequence)
		}
		if seen[event.Sequence] {
			t.Fatalf("sequence %d committed twice", event.Sequence)
		}
		seen[event.Sequence] = true
	}
	assertReservationCounts(t, store, producers*perProducer, 0)
}

// A true sequence collision keeps single-event semantics: exactly one of the
// racing appends commits and the other fails with ErrSequenceReserved.
func TestAppendSequenceCollisionFallsBackPerEvent(t *testing.T) {
	store := openGroupCommitStore(t)
	winner := groupCommitEvent(1, &protocol.TurnCompletedData{Text: "ok"})
	loser := groupCommitEvent(1, &protocol.TurnCompletedData{Text: "collision"})
	results := make(chan error, 2)
	for _, event := range []protocol.Event{winner, loser} {
		event := event
		go func() { results <- store.Append(t.Context(), event) }()
	}
	first, second := <-results, <-results
	var successes int
	for _, err := range []error{first, second} {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrSequenceReserved):
		default:
			t.Fatalf("collision append error = %v, want nil or ErrSequenceReserved", err)
		}
	}
	if successes != 1 {
		t.Fatalf("collision successes = %d, want exactly 1", successes)
	}
	replayed, err := store.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 1 {
		t.Fatalf("replayed %d events, want 1", len(replayed))
	}
}

// Concurrent noise appends share batched reservations without writing any
// JSONL record; the global watermark still advances monotonically.
func TestAppendGroupsNoiseReservations(t *testing.T) {
	store := openGroupCommitStore(t)
	const producers, perProducer = 4, 25
	appendNoise := func(sequence protocol.Cursor) error {
		event := groupCommitEvent(sequence, &protocol.OutputDeltaData{Text: "chunk"})
		for attempt := 0; ; attempt++ {
			err := store.Append(t.Context(), event)
			if err == nil {
				return nil
			}
			if !errors.Is(err, ErrSequenceReserved) || attempt >= 64 {
				return err
			}
			last, sequenceErr := store.LastSequence(t.Context())
			if sequenceErr != nil {
				return sequenceErr
			}
			event = groupCommitEvent(
				last+1,
				&protocol.OutputDeltaData{Text: "chunk"},
			)
		}
	}
	var wg sync.WaitGroup
	failures := make(chan error, producers)
	for producer := 0; producer < producers; producer++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for index := 0; index < perProducer; index++ {
				if err := appendNoise(protocol.Cursor(base + index + 1)); err != nil {
					failures <- err
					return
				}
			}
		}(producer * perProducer)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("noise append: %v", err)
	}
	replayed, err := store.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 0 {
		t.Fatalf("noise wrote %d log records, want 0", len(replayed))
	}
	// Every successful noise append owns exactly one abandoned reservation;
	// sequence reassignment during retries may push the watermark higher.
	assertReservationCounts(t, store, 0, producers*perProducer)
}

// Appends queued across a Close must fail closed instead of stranding their
// producers on the group-commit wait.
func TestAppendAfterCloseFailsClosed(t *testing.T) {
	store, err := Open(
		t.Context(),
		Options{DataDir: t.TempDir(), BusyTimeout: time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(
		t.Context(),
		groupCommitEvent(1, &protocol.TurnCompletedData{Text: "ok"}),
	); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(
		t.Context(),
		groupCommitEvent(2, &protocol.TurnCompletedData{Text: "ok"}),
	); !errors.Is(err, ErrClosed) {
		t.Fatalf("append after close = %v, want ErrClosed", err)
	}
}

func benchmarkConcurrentAppend(b *testing.B, data protocol.EventData) {
	b.Helper()
	store, err := Open(
		b.Context(),
		Options{DataDir: b.TempDir(), BusyTimeout: time.Second},
	)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	var next atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Hub-style sequence assignment: take a frontier number and, if
			// a racing producer committed past it first, rebuild at the
			// durable watermark exactly like the event hub retry does.
			event := groupCommitEvent(protocol.Cursor(next.Add(1)), data)
			for {
				err := store.Append(b.Context(), event)
				if err == nil {
					break
				}
				if !errors.Is(err, ErrSequenceReserved) {
					b.Error(err)
					return
				}
				last, sequenceErr := store.LastSequence(b.Context())
				if sequenceErr != nil {
					b.Error(sequenceErr)
					return
				}
				event = groupCommitEvent(last+1, data)
			}
		}
	})
}

func BenchmarkAppendConcurrentDurable(b *testing.B) {
	benchmarkConcurrentAppend(b, &protocol.TurnCompletedData{Text: "ok"})
}

func BenchmarkAppendConcurrentNoise(b *testing.B) {
	benchmarkConcurrentAppend(b, &protocol.OutputDeltaData{Text: "chunk"})
}

func assertReservationCounts(
	t *testing.T,
	store *Store,
	committed, abandoned int,
) {
	t.Helper()
	var committedCount, abandonedCount, reservedCount int
	if err := store.SQLite().DB().QueryRowContext(
		t.Context(),
		`SELECT
		 COALESCE(SUM(status = 'committed'), 0),
		 COALESCE(SUM(status = 'abandoned'), 0),
		 COALESCE(SUM(status = 'reserved'), 0)
		 FROM event_reservations`,
	).Scan(&committedCount, &abandonedCount, &reservedCount); err != nil {
		t.Fatal(err)
	}
	if committedCount != committed {
		t.Fatalf("committed reservations = %d, want %d", committedCount, committed)
	}
	if abandonedCount != abandoned {
		t.Fatalf("abandoned reservations = %d, want %d", abandonedCount, abandoned)
	}
	if reservedCount != 0 {
		t.Fatalf("reserved (stuck) reservations = %d, want 0", reservedCount)
	}
}

func benchmarkSequentialAppend(b *testing.B, data protocol.EventData) {
	b.Helper()
	store, err := Open(
		b.Context(),
		Options{DataDir: b.TempDir(), BusyTimeout: time.Second},
	)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		event := groupCommitEvent(protocol.Cursor(index+1), data)
		for {
			err := store.Append(b.Context(), event)
			if err == nil {
				break
			}
			if !errors.Is(err, ErrSequenceReserved) {
				b.Fatal(err)
			}
			last, sequenceErr := store.LastSequence(b.Context())
			if sequenceErr != nil {
				b.Fatal(sequenceErr)
			}
			event = groupCommitEvent(last+1, data)
		}
	}
}

func BenchmarkAppendSequentialDurable(b *testing.B) {
	benchmarkSequentialAppend(b, &protocol.TurnCompletedData{Text: "ok"})
}

func BenchmarkAppendSequentialNoise(b *testing.B) {
	benchmarkSequentialAppend(b, &protocol.OutputDeltaData{Text: "chunk"})
}
