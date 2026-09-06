package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestAppendReplayReopenAndEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	firstEvidence, err := log.AppendWithEvidence(t.Context(), testEvent(1))
	if err != nil {
		t.Fatal(err)
	}
	secondEvidence, err := log.AppendWithEvidence(t.Context(), testEvent(2))
	if err != nil {
		t.Fatal(err)
	}
	if firstEvidence.Offset != 0 || firstEvidence.Length <= 1 || len(firstEvidence.SHA256) != 64 {
		t.Fatalf("first evidence = %+v", firstEvidence)
	}
	if secondEvidence.Offset != firstEvidence.Length || len(secondEvidence.SHA256) != 64 {
		t.Fatalf("second evidence = %+v", secondEvidence)
	}

	records, err := log.ReplayRecords(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Event.Sequence != 2 || records[0].Evidence != secondEvidence {
		t.Fatalf("replay records = %+v", records)
	}
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	events, err := reopened.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("replayed sequences = %+v", eventSequences(events))
	}
	last, err := reopened.LastSequence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if last != 2 {
		t.Fatalf("last sequence = %d, want 2", last)
	}
}

func TestOpenRepairsTornTailAndAllowsNextSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := log.AppendWithEvidence(t.Context(), testEvent(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	torn, err := json.Marshal(testEvent(2))
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(torn); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	repaired, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repaired.Close(context.Background()) })
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != evidence.Length {
		t.Fatalf("repaired size = %d, want %d", info.Size(), evidence.Length)
	}
	last, err := repaired.LastSequence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if last != 1 {
		t.Fatalf("last sequence = %d, want 1", last)
	}
	if err := repaired.Append(t.Context(), testEvent(2)); err != nil {
		t.Fatal(err)
	}
}

func TestSequenceAndCursorChecks(t *testing.T) {
	log, err := Open(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close(context.Background()) })

	if err := log.Append(t.Context(), testEvent(2)); err != nil {
		t.Fatal(err)
	}
	err = log.Append(t.Context(), testEvent(1))
	var sequenceErr *SequenceError
	if !errors.As(err, &sequenceErr) || !errors.Is(err, ErrSequence) {
		t.Fatalf("append error = %v, want SequenceError", err)
	}
	_, err = log.Replay(t.Context(), 3)
	var cursorErr *CursorError
	if !errors.As(err, &cursorErr) || !errors.Is(err, ErrCursorAhead) {
		t.Fatalf("replay error = %v, want CursorError", err)
	}
}

func TestOpenRejectsCommittedCorruptionAndPreservesSequenceGap(t *testing.T) {
	t.Run("malformed JSON", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		if err := os.WriteFile(path, []byte("{bad json}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Open(path)
		var corruption *CorruptionError
		if !errors.As(err, &corruption) || !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Open error = %v, want CorruptionError", err)
		}
	})

	t.Run("sequence gap", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		first, err := json.Marshal(testEvent(1))
		if err != nil {
			t.Fatal(err)
		}
		third, err := json.Marshal(testEvent(3))
		if err != nil {
			t.Fatal(err)
		}
		data := append(append(append([]byte{}, first...), '\n'), third...)
		data = append(data, '\n')
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		log, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer log.Close(context.Background())
		if _, exists := log.Evidence(2); exists {
			t.Fatal("reserved sequence gap unexpectedly has log evidence")
		}
		evidence, exists := log.Evidence(3)
		if !exists || evidence.Sequence != 3 {
			t.Fatalf("sequence 3 evidence = %+v, exists=%v", evidence, exists)
		}
	})
}

func TestReplayDetectsChangedCommittedBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close(context.Background()) })
	evidence, err := log.AppendWithEvidence(t.Context(), testEvent(1))
	if err != nil {
		t.Fatal(err)
	}

	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("["), evidence.Offset); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = log.ReplayRecords(t.Context(), 0)
	var corruption *CorruptionError
	if !errors.As(err, &corruption) || !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReplayRecords error = %v, want CorruptionError", err)
	}
}

func TestFailedWriteRollsBackAndCanRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close(context.Background()) })
	log.file = &failWriteOnceFile{durableFile: log.file}

	if err := log.Append(t.Context(), testEvent(1)); err == nil || errors.Is(err, ErrIndeterminate) {
		t.Fatalf("first append error = %v, want determinate write failure", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("size after rolled-back write = %d, want 0", info.Size())
	}
	if err := log.Append(t.Context(), testEvent(1)); err != nil {
		t.Fatalf("retry append: %v", err)
	}
}

func TestRollbackFailureIsIndeterminate(t *testing.T) {
	log, err := Open(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close(context.Background()) })
	log.file = &rollbackFailureFile{durableFile: log.file}

	err = log.Append(t.Context(), testEvent(1))
	var indeterminate *IndeterminateAppendError
	if !errors.As(err, &indeterminate) || !errors.Is(err, ErrIndeterminate) {
		t.Fatalf("append error = %v, want IndeterminateAppendError", err)
	}
	if indeterminate.Sequence != 1 || indeterminate.Offset != 0 {
		t.Fatalf("indeterminate evidence = %+v", indeterminate)
	}
}

func TestCloseAndCanceledContext(t *testing.T) {
	log, err := Open(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := log.Append(ctx, testEvent(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("append error = %v, want context canceled", err)
	}
	if err := log.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(context.Background(), testEvent(1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("append error = %v, want ErrClosed", err)
	}
}

type failWriteOnceFile struct {
	durableFile
	failed bool
}

func (f *failWriteOnceFile) Write(data []byte) (int, error) {
	if f.failed {
		return f.durableFile.Write(data)
	}
	f.failed = true
	limit := 7
	if len(data) < limit {
		limit = len(data)
	}
	written, err := f.durableFile.Write(data[:limit])
	if err != nil {
		return written, err
	}
	return written, errors.New("injected write failure")
}

type rollbackFailureFile struct {
	durableFile
}

func (f *rollbackFailureFile) Write([]byte) (int, error) {
	return 0, errors.New("injected write failure")
}

func (f *rollbackFailureFile) Truncate(int64) error {
	return errors.New("injected truncate failure")
}

func testEvent(sequence protocol.Cursor) protocol.Event {
	return protocol.Event{
		Version:     protocol.Version,
		ID:          protocol.EventID(fmt.Sprintf("evt-%d", sequence)),
		Sequence:    sequence,
		OperationID: "operation",
		ThreadID:    "thread",
		TurnID:      "turn",
		ItemID:      "item",
		Kind:        protocol.EventTurnCompleted,
		CreatedAt:   time.Date(2026, 7, 28, 1, 2, int(sequence), 0, time.UTC),
		Data:        &protocol.TurnCompletedData{Text: fmt.Sprintf("event %d", sequence)},
	}
}

func eventSequences(events []protocol.Event) []protocol.Cursor {
	sequences := make([]protocol.Cursor, len(events))
	for index := range events {
		sequences[index] = events[index].Sequence
	}
	return sequences
}

var _ interface {
	Append(context.Context, protocol.Event) error
	Replay(context.Context, protocol.Cursor) ([]protocol.Event, error)
	LastSequence(context.Context) (protocol.Cursor, error)
	Close(context.Context) error
} = (*Log)(nil)

var _ io.Writer = (*failWriteOnceFile)(nil)

func TestReadRecordReturnsExactCommittedRecord(t *testing.T) {
	dir := t.TempDir()
	log, err := Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close(context.Background()) }()
	var last protocol.Cursor
	for sequence := 1; sequence <= 5; sequence++ {
		event, err := protocol.NewEvent(protocol.EventMeta{
			Sequence:    protocol.Cursor(sequence),
			OperationID: "op_read_record",
			ThreadID:    "thread_read_record",
			TurnID:      "turn_read_record",
			ItemID:      protocol.ItemID(fmt.Sprintf("item_%d", sequence)),
		}, &protocol.TurnCompletedData{Text: "ok"})
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Append(context.Background(), event); err != nil {
			t.Fatal(err)
		}
		last = event.Sequence
	}
	record, found, err := log.ReadRecord(context.Background(), last)
	if err != nil || !found {
		t.Fatalf("ReadRecord = found=%v err=%v", found, err)
	}
	if record.Event.Sequence != last {
		t.Fatalf("ReadRecord sequence = %d, want %d", record.Event.Sequence, last)
	}
	if record.Evidence.Offset < 0 || record.Evidence.Length <= 0 {
		t.Fatalf("ReadRecord evidence = %+v", record.Evidence)
	}
	if _, found, err := log.ReadRecord(
		context.Background(), last+1,
	); err != nil || found {
		t.Fatalf("uncommitted ReadRecord = found=%v err=%v", found, err)
	}
}
