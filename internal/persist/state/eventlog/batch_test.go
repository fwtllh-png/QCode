package eventlog

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type countingSyncFile struct {
	durableFile
	syncs atomic.Int64
}

func (f *countingSyncFile) Sync() error {
	f.syncs.Add(1)
	return f.durableFile.Sync()
}

func openCountingLog(t *testing.T) (*Log, *countingSyncFile) {
	t.Helper()
	log, err := Open(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	counting := &countingSyncFile{durableFile: log.file}
	log.file = counting
	t.Cleanup(func() { _ = log.Close(context.Background()) })
	return log, counting
}

// A batch appends every record and fsyncs exactly once for all of them.
func TestAppendBatchSyncsOnce(t *testing.T) {
	log, counting := openCountingLog(t)
	batch := []protocol.Event{
		testEvent(1), testEvent(2), testEvent(3), testEvent(4), testEvent(5),
	}
	evidences, err := log.AppendBatchWithEvidence(t.Context(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidences) != len(batch) {
		t.Fatalf("evidences = %d, want %d", len(evidences), len(batch))
	}
	if counting.syncs.Load() != 1 {
		t.Fatalf("batch fsyncs = %d, want 1", counting.syncs.Load())
	}
	events, err := log.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(batch) {
		t.Fatalf("replayed %d events, want %d", len(events), len(batch))
	}
	for index, event := range events {
		if event.Sequence != protocol.Cursor(index+1) {
			t.Fatalf("event %d has sequence %d", index, event.Sequence)
		}
		if event.ID != batch[index].ID {
			t.Fatalf("event %d identity = %s, want %s", index, event.ID, batch[index].ID)
		}
	}
}

// A non-increasing sequence inside a batch rolls the whole batch back: the
// durable log keeps none of it and the same sequences can be retried.
func TestAppendBatchRollsBackOnSequenceViolation(t *testing.T) {
	log, counting := openCountingLog(t)
	batch := []protocol.Event{testEvent(3), testEvent(3)}
	_, err := log.AppendBatchWithEvidence(t.Context(), batch)
	var sequenceErr *SequenceError
	if !errors.As(err, &sequenceErr) || !errors.Is(err, ErrSequence) {
		t.Fatalf("batch error = %v, want SequenceError", err)
	}
	events, err := log.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("replayed %d events after rollback, want 0", len(events))
	}
	if counting.syncs.Load() != 1 {
		t.Fatalf("fsyncs = %d, want the single rollback sync", counting.syncs.Load())
	}
	// The rolled-back sequences remain available.
	if _, err := log.AppendBatchWithEvidence(
		t.Context(),
		[]protocol.Event{testEvent(1), testEvent(2)},
	); err != nil {
		t.Fatal(err)
	}
	last, err := log.LastSequence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if last != 2 {
		t.Fatalf("last sequence = %d, want 2", last)
	}
}

// A mid-batch write failure removes earlier records of the same batch, so a
// retry of the same events never duplicates log bytes.
func TestAppendBatchWriteFailureRollsBackWholeBatch(t *testing.T) {
	log, counting := openCountingLog(t)
	log.file = &failWriteOnSecondWriteFile{durableFile: counting}
	batch := []protocol.Event{testEvent(1), testEvent(2), testEvent(3)}
	if _, err := log.AppendBatchWithEvidence(t.Context(), batch); err == nil {
		t.Fatal("batch append must fail on injected write failure")
	}
	// Swap back a healthy file and verify nothing from the batch survived.
	log.file = counting
	events, err := log.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("replayed %d events after failed batch, want 0", len(events))
	}
	if _, err := log.AppendBatchWithEvidence(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	events, err = log.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(batch) {
		t.Fatalf("retried batch replayed %d events, want %d", len(events), len(batch))
	}
}

type failWriteOnSecondWriteFile struct {
	durableFile
	writes int
}

func (f *failWriteOnSecondWriteFile) Write(data []byte) (int, error) {
	f.writes++
	if f.writes < 2 {
		return f.durableFile.Write(data)
	}
	written, err := f.durableFile.Write(data)
	if err != nil {
		return written, err
	}
	return written, errors.New("injected batch write failure")
}
