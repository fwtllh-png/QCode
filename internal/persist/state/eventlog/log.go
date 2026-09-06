// Package eventlog provides QCode's durable, append-only protocol event log.
package eventlog

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

var (
	ErrClosed        = errors.New("event log is closed")
	ErrCorrupt       = errors.New("event log is corrupt")
	ErrSequence      = errors.New("event sequence is not globally increasing")
	ErrCursorAhead   = errors.New("event replay cursor is ahead of the log")
	ErrIndeterminate = errors.New("event append outcome is indeterminate")
)

// Evidence identifies the exact durable bytes for an event. Offset is the
// beginning of the JSON payload and Length includes its newline commit marker.
// SHA256 hashes the JSON payload without the newline.
type Evidence struct {
	Sequence protocol.Cursor
	Offset   int64
	Length   int64
	SHA256   string
}

// Record couples a replayed event with evidence for its on-disk representation.
type Record struct {
	Event    protocol.Event
	Evidence Evidence
}

// CorruptionError reports a committed record that cannot be trusted.
type CorruptionError struct {
	Path   string
	Offset int64
	Err    error
}

func (e *CorruptionError) Error() string {
	return fmt.Sprintf("event log %q is corrupt at offset %d: %v", e.Path, e.Offset, e.Err)
}

func (e *CorruptionError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrCorrupt}
	}
	return []error{ErrCorrupt, e.Err}
}

// SequenceError reports a gap, duplicate, or reordered event.
type SequenceError struct {
	Expected protocol.Cursor
	Actual   protocol.Cursor
	Offset   int64
}

func (e *SequenceError) Error() string {
	return fmt.Sprintf(
		"event sequence %d at offset %d is not greater than %d",
		e.Actual, e.Offset, e.Expected-1,
	)
}

func (e *SequenceError) Unwrap() error { return ErrSequence }

// CursorError reports a replay request beyond the latest committed sequence.
type CursorError struct {
	Requested protocol.Cursor
	Latest    protocol.Cursor
}

func (e *CursorError) Error() string {
	return fmt.Sprintf("event replay cursor %d is ahead of latest sequence %d", e.Requested, e.Latest)
}

func (e *CursorError) Unwrap() error { return ErrCursorAhead }

// IndeterminateAppendError means the append failed and restoring the exact
// pre-append durable offset also failed. The caller must reopen and reconcile.
type IndeterminateAppendError struct {
	Sequence    protocol.Cursor
	Offset      int64
	AppendErr   error
	RollbackErr error
}

func (e *IndeterminateAppendError) Error() string {
	return fmt.Sprintf(
		"event %d append failed at offset %d (%v), and rollback failed (%v)",
		e.Sequence, e.Offset, e.AppendErr, e.RollbackErr,
	)
}

func (e *IndeterminateAppendError) Unwrap() []error {
	return []error{ErrIndeterminate, e.AppendErr, e.RollbackErr}
}

type durableFile interface {
	io.ReaderAt
	io.Writer
	io.Seeker
	Stat() (os.FileInfo, error)
	Truncate(int64) error
	Sync() error
	Close() error
}

// Log is a concurrency-safe JSONL event log. Each newline is the commit marker
// for the preceding JSON event. Appends take the write lock; replays and
// evidence lookups take read locks and run concurrently with appends —
// committed regions of the file are append-only, and a failed append only
// ever truncates bytes beyond the previously committed end.
type Log struct {
	mu       sync.RWMutex
	path     string
	file     durableFile
	entries  []Evidence
	last     protocol.Cursor
	end      int64
	closed   bool
	closeErr error
}

// Store is an integration-friendly alias for Log.
type Store = Log

// Open opens or creates path, validates all committed records, and truncates an
// incomplete final record that has no newline commit marker.
func Open(path string) (*Log, error) {
	if path == "" {
		return nil, errors.New("event log path is required")
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	entries, last, end, err := scanAndRepair(path, file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &Log{
		path: path, file: file, entries: entries, last: last, end: end,
	}, nil
}

func scanAndRepair(path string, file *os.File) ([]Evidence, protocol.Cursor, int64, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, 0, 0, fmt.Errorf("seek event log: %w", err)
	}
	reader := bufio.NewReader(file)
	entries := make([]Evidence, 0)
	var offset int64
	var last protocol.Cursor

	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			if len(line) != 0 {
				if truncateErr := file.Truncate(offset); truncateErr != nil {
					return nil, 0, 0, fmt.Errorf("repair torn event log tail: %w", truncateErr)
				}
				if syncErr := file.Sync(); syncErr != nil {
					return nil, 0, 0, fmt.Errorf("sync repaired event log: %w", syncErr)
				}
			}
			if _, seekErr := file.Seek(offset, io.SeekStart); seekErr != nil {
				return nil, 0, 0, fmt.Errorf("seek repaired event log: %w", seekErr)
			}
			return entries, last, offset, nil
		}
		if err != nil {
			return nil, 0, 0, fmt.Errorf("read event log at offset %d: %w", offset, err)
		}

		payload := line[:len(line)-1]
		if len(payload) == 0 {
			return nil, 0, 0, &CorruptionError{
				Path: path, Offset: offset, Err: errors.New("empty committed record"),
			}
		}
		var event protocol.Event
		if decodeErr := json.Unmarshal(payload, &event); decodeErr != nil {
			return nil, 0, 0, &CorruptionError{Path: path, Offset: offset, Err: decodeErr}
		}
		expected := last + 1
		if event.Sequence < expected {
			return nil, 0, 0, &CorruptionError{
				Path: path, Offset: offset,
				Err: &SequenceError{Expected: expected, Actual: event.Sequence, Offset: offset},
			}
		}
		entries = append(entries, makeEvidence(event.Sequence, offset, line))
		last = event.Sequence
		offset += int64(len(line))
	}
}

func makeEvidence(sequence protocol.Cursor, offset int64, record []byte) Evidence {
	payload := record[:len(record)-1]
	digest := sha256.Sum256(payload)
	return Evidence{
		Sequence: sequence,
		Offset:   offset,
		Length:   int64(len(record)),
		SHA256:   hex.EncodeToString(digest[:]),
	}
}

// Append writes and fsyncs one event. The caller-supplied sequence must be
// greater than the latest committed sequence; reserved gaps are never reused.
func (l *Log) Append(ctx context.Context, event protocol.Event) error {
	_, err := l.AppendWithEvidence(ctx, event)
	return err
}

// AppendWithEvidence appends an event and returns its durable byte evidence.
func (l *Log) AppendWithEvidence(ctx context.Context, event protocol.Event) (Evidence, error) {
	if err := ctx.Err(); err != nil {
		return Evidence{}, err
	}
	if err := event.Validate(); err != nil {
		return Evidence{}, fmt.Errorf("validate event: %w", err)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return Evidence{}, fmt.Errorf("encode event: %w", err)
	}
	record := append(payload, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return Evidence{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return Evidence{}, err
	}
	expected := l.last + 1
	if event.Sequence < expected {
		return Evidence{}, &SequenceError{Expected: expected, Actual: event.Sequence, Offset: l.end}
	}

	offset := l.end
	if _, err := l.file.Seek(offset, io.SeekStart); err != nil {
		return Evidence{}, fmt.Errorf("seek event log for append: %w", err)
	}
	if err := writeFull(l.file, record); err != nil {
		return Evidence{}, l.rollbackFailedAppend(event.Sequence, offset, err)
	}
	if err := l.file.Sync(); err != nil {
		return Evidence{}, l.rollbackFailedAppend(event.Sequence, offset, fmt.Errorf("fsync event: %w", err))
	}

	evidence := makeEvidence(event.Sequence, offset, record)
	l.entries = append(l.entries, evidence)
	l.last = event.Sequence
	l.end += int64(len(record))
	return evidence, nil
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if written > 0 {
			data = data[written:]
		}
		if err != nil {
			return fmt.Errorf("write event: %w", err)
		}
		if written == 0 {
			return fmt.Errorf("write event: %w", io.ErrShortWrite)
		}
	}
	return nil
}

func (l *Log) rollbackFailedAppend(sequence protocol.Cursor, offset int64, appendErr error) error {
	var rollbackErr error
	if err := l.file.Truncate(offset); err != nil {
		rollbackErr = errors.Join(rollbackErr, fmt.Errorf("truncate: %w", err))
	}
	if _, err := l.file.Seek(offset, io.SeekStart); err != nil {
		rollbackErr = errors.Join(rollbackErr, fmt.Errorf("seek: %w", err))
	}
	if err := l.file.Sync(); err != nil {
		rollbackErr = errors.Join(rollbackErr, fmt.Errorf("fsync: %w", err))
	}
	if rollbackErr != nil {
		return &IndeterminateAppendError{
			Sequence: sequence, Offset: offset, AppendErr: appendErr, RollbackErr: rollbackErr,
		}
	}
	return appendErr
}

// Replay returns committed events whose sequence is greater than cursor.
func (l *Log) Replay(ctx context.Context, cursor protocol.Cursor) ([]protocol.Event, error) {
	records, err := l.ReplayRecords(ctx, cursor)
	if err != nil {
		return nil, err
	}
	events := make([]protocol.Event, len(records))
	for index := range records {
		events[index] = records[index].Event
	}
	return events, nil
}

// ReplayLimit returns at most limit committed events and reports whether more
// were available. It bounds both decoded records and response memory.
func (l *Log) ReplayLimit(
	ctx context.Context,
	cursor protocol.Cursor,
	limit int,
) ([]protocol.Event, bool, error) {
	if limit <= 0 {
		return nil, false, errors.New("event replay limit must be positive")
	}
	records, more, err := l.replayRecords(ctx, cursor, limit)
	if err != nil {
		return nil, false, err
	}
	events := make([]protocol.Event, len(records))
	for index := range records {
		events[index] = records[index].Event
	}
	return events, more, nil
}

// ReplayRecords returns events and verifies their byte offsets and hashes.
func (l *Log) ReplayRecords(ctx context.Context, cursor protocol.Cursor) ([]Record, error) {
	records, _, err := l.replayRecords(ctx, cursor, 0)
	return records, err
}

func (l *Log) replayRecords(
	ctx context.Context,
	cursor protocol.Cursor,
	limit int,
) ([]Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return nil, false, ErrClosed
	}
	if cursor > l.last {
		return nil, false, &CursorError{Requested: cursor, Latest: l.last}
	}

	start := sort.Search(len(l.entries), func(index int) bool {
		return l.entries[index].Sequence > cursor
	})
	available := len(l.entries) - start
	count := available
	more := false
	if limit > 0 && count > limit {
		count = limit
		more = true
	}
	records := make([]Record, 0, count)
	for _, evidence := range l.entries[start : start+count] {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		record, err := l.readVerifiedRecord(evidence)
		if err != nil {
			return nil, false, err
		}
		records = append(records, record)
	}
	return records, more, nil
}

// ReadRecord returns the committed record for sequence with byte
// verification, without replaying any other record. The boolean is false
// when the sequence is not committed.
func (l *Log) ReadRecord(
	ctx context.Context,
	sequence protocol.Cursor,
) (Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, false, err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return Record{}, false, ErrClosed
	}
	index := sort.Search(len(l.entries), func(index int) bool {
		return l.entries[index].Sequence >= sequence
	})
	if index == len(l.entries) || l.entries[index].Sequence != sequence {
		return Record{}, false, nil
	}
	record, err := l.readVerifiedRecord(l.entries[index])
	if err != nil {
		return Record{}, false, err
	}
	return record, true, nil
}

// readVerifiedRecord reads and verifies one committed record; the caller
// holds the read or write lock.
func (l *Log) readVerifiedRecord(evidence Evidence) (Record, error) {
	recordBytes := make([]byte, evidence.Length)
	read, err := l.file.ReadAt(recordBytes, evidence.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return Record{}, &CorruptionError{Path: l.path, Offset: evidence.Offset, Err: err}
	}
	if int64(read) != evidence.Length || len(recordBytes) == 0 || recordBytes[len(recordBytes)-1] != '\n' {
		return Record{}, &CorruptionError{
			Path: l.path, Offset: evidence.Offset, Err: errors.New("committed record is truncated"),
		}
	}
	payload := recordBytes[:len(recordBytes)-1]
	digest := sha256.Sum256(payload)
	if !bytes.Equal(digest[:], mustDecodeHash(evidence.SHA256)) {
		return Record{}, &CorruptionError{
			Path: l.path, Offset: evidence.Offset, Err: errors.New("committed record hash mismatch"),
		}
	}
	var event protocol.Event
	if err := json.Unmarshal(payload, &event); err != nil {
		return Record{}, &CorruptionError{Path: l.path, Offset: evidence.Offset, Err: err}
	}
	if event.Sequence != evidence.Sequence {
		return Record{}, &CorruptionError{
			Path: l.path, Offset: evidence.Offset,
			Err: &SequenceError{
				Expected: evidence.Sequence, Actual: event.Sequence, Offset: evidence.Offset,
			},
		}
	}
	return Record{Event: event, Evidence: evidence}, nil
}

func mustDecodeHash(hash string) []byte {
	decoded, _ := hex.DecodeString(hash)
	return decoded
}

// Evidence returns the byte evidence for sequence when it is present.
func (l *Log) Evidence(sequence protocol.Cursor) (Evidence, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	index := sort.Search(len(l.entries), func(index int) bool {
		return l.entries[index].Sequence >= sequence
	})
	if index == len(l.entries) || l.entries[index].Sequence != sequence {
		return Evidence{}, false
	}
	evidence := l.entries[index]
	return evidence, true
}

// LastSequence returns the latest committed global sequence.
func (l *Log) LastSequence(ctx context.Context) (protocol.Cursor, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return l.last, ErrClosed
	}
	return l.last, nil
}

// Close closes the log. It is safe to call more than once.
func (l *Log) Close(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return l.closeErr
	}
	l.closed = true
	l.closeErr = l.file.Close()
	return l.closeErr
}
