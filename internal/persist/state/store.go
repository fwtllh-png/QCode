// Package state composes QCode's v1 relational projection, durable event
// log, and content-addressed object store.
//
// Write boundaries (W4.1):
//
//   - Append / AppendEvents — durable eventlog + event_index / reservations
//     only. They MUST NOT mutate relational thread metadata (title, status,
//     parent_thread_id, …). Lifecycle projection of events may still bump
//     threads.updated_at via host/runtimeapi, but that is not this API.
//   - PatchThreadMeta — relational thread fields only. It MUST NOT append to
//     the eventlog or advance event sequences.
//
// Callers must not mix these concerns in a single store method. Fork lineage
// (parent + source cursor) is owned by W4.2, not PatchThreadMeta.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/state/cas"
	"github.com/fwtllh-png/QCode/internal/persist/state/eventlog"
	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

var (
	ErrClosed           = errors.New("state store is closed")
	ErrSequenceReserved = errors.New("event sequence was already reserved")
	ErrProjection       = errors.New("event projection is inconsistent with the durable log")
	ErrThreadNotFound   = errors.New("thread record not found")
	ErrEmptyMetaPatch   = errors.New("thread meta patch has no fields")
)

type Options struct {
	DataDir     string
	BusyTimeout time.Duration
}

type Store struct {
	// mu serializes durable writes and guards closed. Event readers hold it
	// only to freeze committed cursors or identity, never during log I/O.
	// readers keeps storage open for those reads without blocking appends.
	readers sync.RWMutex
	mu      sync.RWMutex
	root    string
	sqlite  *sqlitestate.Store
	events  *eventlog.Log
	content *cas.Store
	closed  bool
	// batchMu guards the group-commit queue: appends that arrive while a
	// flush is active join its batch, sharing one reservation transaction,
	// one eventlog fsync, and one projection transaction. Producers enqueue
	// under batchMu and wait on their entry; a lazily started flush loop
	// drains until the queue is empty.
	batchMu    sync.Mutex
	batchQueue []batchEntry
	flushing   bool
}

type batchEntry struct {
	event protocol.Event
	done  chan error
}

func Open(ctx context.Context, options Options) (_ *Store, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.DataDir == "" {
		return nil, errors.New("state data directory is required")
	}
	root, err := filepath.Abs(options.DataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve state data directory: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create state data directory: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("secure state data directory: %w", err)
	}

	database, err := sqlitestate.Open(
		ctx,
		filepath.Join(root, "state-v1.db"),
		sqlitestate.Options{BusyTimeout: options.BusyTimeout},
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			_ = database.Close()
		}
	}()
	log, err := eventlog.Open(filepath.Join(root, "events-v1.jsonl"))
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			_ = log.Close(context.Background())
		}
	}()
	content, err := cas.Open(filepath.Join(root, "cas-v1"))
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			_ = content.Close(context.Background())
		}
	}()

	store := &Store{
		root: root, sqlite: database, events: log, content: content,
	}
	if err := store.ensureSessionSearch(ctx); err != nil {
		return nil, err
	}
	if err := store.reconcile(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Root() string               { return s.root }
func (s *Store) SQLite() *sqlitestate.Store { return s.sqlite }
func (s *Store) Content() *cas.Store        { return s.content }

// Append persists one protocol event to the durable eventlog and event_index.
// It does not patch thread title/status/parent metadata — use PatchThreadMeta.
func (s *Store) Append(ctx context.Context, event protocol.Event) error {
	return s.AppendEvents(ctx, event)
}

// AppendEvents persists protocol events to the durable eventlog and updates
// event_index / reservations. It MUST NOT mutate relational thread metadata.
// Streaming noise (see eventlog.ShouldPersist) still consumes a sequence slot
// via an abandoned reservation so LastSequence / fork cursors stay monotonic,
// but is not written to JSONL.
func (s *Store) AppendEvents(ctx context.Context, events ...protocol.Event) error {
	for _, event := range events {
		if err := s.appendOne(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

// appendOne validates the event and joins the group-commit queue. Concurrent
// appends — including noise reservations — coalesce into batches whose
// durability wait covers one shared fsync instead of one per event. A
// canceled producer context can no longer abort the append after this point:
// the batch flushes to completion and the producer observes the outcome.
func (s *Store) appendOne(ctx context.Context, event protocol.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := event.Validate(); err != nil {
		return fmt.Errorf("validate durable event: %w", err)
	}
	entry := batchEntry{event: event, done: make(chan error, 1)}
	s.batchMu.Lock()
	s.batchQueue = append(s.batchQueue, entry)
	if !s.flushing {
		s.flushing = true
		go s.flushQueued()
	}
	s.batchMu.Unlock()
	return <-entry.done
}

// flushQueued drains queued appends until the queue is empty, then releases
// leadership. The empty-check and the leadership clear happen under one
// batchMu hold, so an enqueue that still sees an active flush is either
// picked up by this loop's next round or re-promotes a new one.
func (s *Store) flushQueued() {
	for {
		s.batchMu.Lock()
		if len(s.batchQueue) == 0 {
			s.flushing = false
			s.batchMu.Unlock()
			return
		}
		batch := s.batchQueue
		s.batchQueue = nil
		s.batchMu.Unlock()
		s.flushBatch(batch)
	}
}

// flushBatch persists one drained batch under the store write lock. Sorting
// by sequence keeps the durable log strictly increasing even when concurrent
// publishers from different hubs interleave. Any batch-level failure falls
// back to the single-event append with its exact conflict, idempotency, and
// self-healing semantics.
func (s *Store) flushBatch(batch []batchEntry) {
	sort.SliceStable(batch, func(i, j int) bool {
		return batch[i].event.Sequence < batch[j].event.Sequence
	})
	events := make([]protocol.Event, len(batch))
	for index := range batch {
		events[index] = batch[index].event
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.appendBatchLocked(context.Background(), events)
	if err == nil {
		for index := range batch {
			batch[index].done <- nil
		}
		return
	}
	if errors.Is(err, ErrClosed) {
		for index := range batch {
			batch[index].done <- ErrClosed
		}
		return
	}
	for index := range batch {
		batch[index].done <- s.appendWithSelfHealLocked(
			context.Background(),
			batch[index].event,
		)
	}
}

// appendBatchLocked persists a validated, sequence-sorted batch while the
// caller owns s.mu: one reservation transaction, one eventlog batch fsync,
// one projection transaction. It refuses the batch — without writing — when
// any sequence already has a reservation, leaving those cases to the
// single-event path.
func (s *Store) appendBatchLocked(
	ctx context.Context,
	events []protocol.Event,
) error {
	if s.closed {
		return ErrClosed
	}
	for _, event := range events {
		status, eventID, err := s.reservation(ctx, event.Sequence)
		if err != nil {
			return err
		}
		if status != "" {
			return fmt.Errorf(
				"%w: sequence=%d status=%s event_id=%s",
				ErrSequenceReserved, event.Sequence, status, eventID,
			)
		}
	}
	if err := s.reserveBatch(ctx, events); err != nil {
		return err
	}
	persisted := make([]protocol.Event, 0, len(events))
	for _, event := range events {
		if eventlog.ShouldPersist(event.Kind) {
			persisted = append(persisted, event)
		}
	}
	if len(persisted) == 0 {
		return nil
	}
	evidences, err := s.events.AppendBatchWithEvidence(ctx, persisted)
	if err != nil {
		if !errors.Is(err, eventlog.ErrIndeterminate) {
			// A clean batch failure proves the log holds none of the batch;
			// release the reservations so the fallback retries honestly.
			for _, event := range persisted {
				_ = s.markReservation(context.Background(), event.Sequence, "abandoned")
			}
		}
		return err
	}
	if err := s.commitProjectionBatch(ctx, persisted, evidences); err != nil {
		return errors.Join(ErrProjection, err)
	}
	return nil
}

// reserveBatch inserts one reservation row per event in a single
// transaction, enforcing strict monotonicity against the durable watermark.
func (s *Store) reserveBatch(
	ctx context.Context,
	events []protocol.Event,
) error {
	return s.sqlite.Transaction(ctx, func(tx *sql.Tx) error {
		var last protocol.Cursor
		if err := tx.QueryRowContext(
			ctx, "SELECT COALESCE(MAX(sequence), 0) FROM event_reservations",
		).Scan(&last); err != nil {
			return fmt.Errorf("read event sequence high watermark: %w", err)
		}
		for _, event := range events {
			if event.Sequence <= last {
				return fmt.Errorf(
					"%w: sequence=%d high_watermark=%d",
					ErrSequenceReserved, event.Sequence, last,
				)
			}
			status := "reserved"
			if !eventlog.ShouldPersist(event.Kind) {
				// No log append follows streaming noise. Reserve its
				// sequence in the final state atomically instead of
				// committing a second UPDATE.
				status = "abandoned"
			}
			if _, err := tx.ExecContext(
				ctx,
				`INSERT INTO event_reservations(sequence, event_id, status, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?)`,
				event.Sequence, event.ID, status, timestamp(event.CreatedAt), timestamp(time.Now()),
			); err != nil {
				return fmt.Errorf("reserve event sequence %d: %w", event.Sequence, err)
			}
			last = event.Sequence
		}
		return nil
	})
}

// appendWithSelfHealLocked appends one validated event while the caller owns
// s.mu. A reservation conflict or projection failure caused by an earlier
// crash or failed commit is repaired once from the durable log before the
// same event is retried; a genuine sequence conflict fails again.
func (s *Store) appendWithSelfHealLocked(ctx context.Context, event protocol.Event) error {
	err := s.appendOneLocked(ctx, event)
	if err == nil || !(errors.Is(err, ErrSequenceReserved) || errors.Is(err, ErrProjection)) {
		return err
	}
	if repairErr := s.reconcile(ctx); repairErr != nil {
		return errors.Join(err, repairErr)
	}
	return s.appendOneLocked(ctx, event)
}

// appendOneLocked persists a validated event while the caller owns s.mu.
func (s *Store) appendOneLocked(ctx context.Context, event protocol.Event) error {
	if s.closed {
		return ErrClosed
	}

	status, eventID, err := s.reservation(ctx, event.Sequence)
	if err != nil {
		return err
	}
	if status == "committed" && eventID == string(event.ID) {
		return nil
	}
	persist := eventlog.ShouldPersist(event.Kind)
	if status == "abandoned" && eventID == string(event.ID) {
		// Streaming noise keeps its reserved slot without a log record. A
		// durable event whose previous append failed cleanly is retried
		// honestly below instead of reporting success without a record.
		if !persist {
			return nil
		}
	} else if status != "" {
		return fmt.Errorf(
			"%w: sequence=%d status=%s event_id=%s",
			ErrSequenceReserved, event.Sequence, status, eventID,
		)
	} else if err := s.reserve(ctx, event); err != nil {
		return err
	}

	if !persist {
		return nil
	}

	evidence, err := s.events.AppendWithEvidence(ctx, event)
	if err != nil {
		if !errors.Is(err, eventlog.ErrIndeterminate) {
			// A clean failure proves the log does not hold the event. An
			// indeterminate append keeps the reservation so the next
			// attempt reconciles from the log of record instead of
			// risking a duplicate record.
			_ = s.markReservation(context.Background(), event.Sequence, "abandoned")
		}
		return err
	}
	if err := s.commitProjection(ctx, event, evidence); err != nil {
		return errors.Join(ErrProjection, err)
	}
	return nil
}

// ThreadMetaPatch updates relational thread fields only. Parent/source cursor
// lineage belongs to ForkThread (W4.2), not this patch.
type ThreadMetaPatch struct {
	ThreadID protocol.ThreadID
	Title    *string
	Status   *string
}

// PatchThreadMeta updates relational thread metadata without appending events
// or advancing event sequences.
func (s *Store) PatchThreadMeta(ctx context.Context, patch ThreadMetaPatch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if patch.ThreadID == "" {
		return errors.New("thread id is required")
	}
	if patch.Title == nil && patch.Status == nil {
		return ErrEmptyMetaPatch
	}
	if patch.Status != nil {
		status := strings.TrimSpace(*patch.Status)
		if status == "" {
			return errors.New("thread status must not be empty")
		}
		patch.Status = &status
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	var sets []string
	var args []any
	if patch.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *patch.Title)
	}
	if patch.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, *patch.Status)
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().UTC().Format(time.RFC3339Nano))
	args = append(args, string(patch.ThreadID))
	result, err := s.sqlite.DB().ExecContext(
		ctx,
		"UPDATE threads SET "+strings.Join(sets, ", ")+" WHERE id = ?",
		args...,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrThreadNotFound
	}
	return nil
}

func (s *Store) Replay(ctx context.Context, cursor protocol.Cursor) ([]protocol.Event, error) {
	events, _, err := s.replay(ctx, cursor, nil, 0)
	return events, err
}

func (s *Store) EventByID(
	ctx context.Context,
	eventID protocol.EventID,
) (protocol.Event, bool, error) {
	s.readers.RLock()
	defer s.readers.RUnlock()
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return protocol.Event{}, false, ErrClosed
	}
	var sequence protocol.Cursor
	err := s.sqlite.DB().QueryRowContext(
		ctx,
		`SELECT sequence FROM event_index WHERE event_id = ?`,
		eventID,
	).Scan(&sequence)
	s.mu.RUnlock()
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.Event{}, false, nil
	}
	if err != nil {
		return protocol.Event{}, false, err
	}
	record, found, err := s.events.ReadRecord(ctx, sequence)
	if err != nil || !found {
		return protocol.Event{}, false, fmt.Errorf(
			"event index %q has no matching log event",
			eventID,
		)
	}
	if record.Event.ID != eventID {
		return protocol.Event{}, false, fmt.Errorf(
			"event index %q has no matching log event",
			eventID,
		)
	}
	return record.Event, true, nil
}

// ReplayLimit bounds durable replay at a frozen committed watermark.
func (s *Store) ReplayLimit(
	ctx context.Context,
	cursor protocol.Cursor,
	limit int,
) ([]protocol.Event, bool, error) {
	if limit <= 0 {
		return nil, false, errors.New("event replay limit must be positive")
	}
	return s.replay(ctx, cursor, nil, limit)
}

// ReplayThrough reads (cursor, through], excluding concurrent appends.
// A nonpositive limit returns the entire range.
func (s *Store) ReplayThrough(ctx context.Context, cursor, through protocol.Cursor, limit int) ([]protocol.Event, bool, error) {
	return s.replay(ctx, cursor, &through, limit)
}

func (s *Store) replay(ctx context.Context, cursor protocol.Cursor, through *protocol.Cursor, limit int) ([]protocol.Event, bool, error) {
	s.readers.RLock()
	defer s.readers.RUnlock()
	fence, err := s.replayFence(ctx, cursor)
	if err != nil {
		return nil, false, err
	}
	if through != nil {
		fence = min(fence, *through)
	}
	if cursor >= fence {
		return []protocol.Event{}, false, nil
	}
	return s.events.ReplayThrough(ctx, cursor, fence, limit)
}

func (s *Store) replayFence(ctx context.Context, cursor protocol.Cursor) (protocol.Cursor, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, ErrClosed
	}
	highWatermark, err := s.lastReserved(ctx)
	if err != nil {
		return 0, err
	}
	if cursor > highWatermark {
		return 0, &eventlog.CursorError{Requested: cursor, Latest: highWatermark}
	}
	logLast, err := s.events.LastSequence(ctx)
	if err != nil {
		return 0, err
	}
	return min(highWatermark, logLast), nil
}

func (s *Store) LastSequence(ctx context.Context) (protocol.Cursor, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, ErrClosed
	}
	return s.lastReserved(ctx)
}

func (s *Store) Close(ctx context.Context) error {
	s.readers.Lock()
	defer s.readers.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return errors.Join(s.events.Close(ctx), s.sqlite.Close())
}

func (s *Store) CloseAll(ctx context.Context) error {
	return errors.Join(s.Close(ctx), s.content.Close(ctx))
}

func (s *Store) reserve(ctx context.Context, event protocol.Event) error {
	status := "reserved"
	if !eventlog.ShouldPersist(event.Kind) {
		// No log append follows streaming noise. Reserve its sequence in the
		// final state atomically instead of committing a second UPDATE.
		status = "abandoned"
	}
	return s.sqlite.Transaction(ctx, func(tx *sql.Tx) error {
		var last protocol.Cursor
		if err := tx.QueryRowContext(
			ctx, "SELECT COALESCE(MAX(sequence), 0) FROM event_reservations",
		).Scan(&last); err != nil {
			return fmt.Errorf("read event sequence high watermark: %w", err)
		}
		if event.Sequence <= last {
			return fmt.Errorf(
				"%w: sequence=%d high_watermark=%d",
				ErrSequenceReserved, event.Sequence, last,
			)
		}
		_, err := tx.ExecContext(
			ctx,
			`INSERT INTO event_reservations(sequence, event_id, status, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?)`,
			event.Sequence, event.ID, status, timestamp(event.CreatedAt), timestamp(time.Now()),
		)
		if err != nil {
			return fmt.Errorf("reserve event sequence %d: %w", event.Sequence, err)
		}
		return nil
	})
}

func (s *Store) reservation(
	ctx context.Context,
	sequence protocol.Cursor,
) (status, eventID string, err error) {
	err = s.sqlite.DB().QueryRowContext(
		ctx,
		"SELECT status, event_id FROM event_reservations WHERE sequence = ?",
		sequence,
	).Scan(&status, &eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read event reservation %d: %w", sequence, err)
	}
	return status, eventID, nil
}

func (s *Store) markReservation(
	ctx context.Context,
	sequence protocol.Cursor,
	status string,
) error {
	_, err := s.sqlite.DB().ExecContext(
		ctx,
		"UPDATE event_reservations SET status = ?, updated_at = ? WHERE sequence = ?",
		status, timestamp(time.Now()), sequence,
	)
	return err
}

func (s *Store) commitProjection(
	ctx context.Context,
	event protocol.Event,
	evidence eventlog.Evidence,
) error {
	return s.sqlite.Transaction(ctx, func(tx *sql.Tx) error {
		return s.commitProjectionTx(ctx, tx, event, evidence)
	})
}

// commitProjectionBatch commits the projections of one durable batch in a
// single transaction; the log records are already fsynced at this point.
func (s *Store) commitProjectionBatch(
	ctx context.Context,
	events []protocol.Event,
	evidences []eventlog.Evidence,
) error {
	return s.sqlite.Transaction(ctx, func(tx *sql.Tx) error {
		for index := range events {
			if err := s.commitProjectionTx(
				ctx, tx, events[index], evidences[index],
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) commitProjectionTx(
	ctx context.Context,
	tx *sql.Tx,
	event protocol.Event,
	evidence eventlog.Evidence,
) error {
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO event_index(
		 sequence, event_id, thread_id, turn_id, item_id, kind,
		 log_offset, log_length, sha256, created_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(sequence) DO UPDATE SET
		 event_id=excluded.event_id, thread_id=excluded.thread_id,
		 turn_id=excluded.turn_id, item_id=excluded.item_id, kind=excluded.kind,
		 log_offset=excluded.log_offset, log_length=excluded.log_length,
		 sha256=excluded.sha256, created_at=excluded.created_at`,
		event.Sequence, event.ID, nullString(event.ThreadID), nullString(event.TurnID),
		nullString(event.ItemID), event.Kind, evidence.Offset, evidence.Length,
		evidence.SHA256, timestamp(event.CreatedAt),
	); err != nil {
		return fmt.Errorf("project event index %d: %w", event.Sequence, err)
	}
	result, err := tx.ExecContext(
		ctx,
		`UPDATE event_reservations
		 SET status = 'committed', updated_at = ?
		 WHERE sequence = ? AND event_id = ?`,
		timestamp(time.Now()), event.Sequence, event.ID,
	)
	if err != nil {
		return fmt.Errorf("commit event reservation %d: %w", event.Sequence, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("event reservation %d disappeared during commit", event.Sequence)
	}
	if err := projectAgentGraphTx(ctx, tx, event); err != nil {
		return fmt.Errorf("project agent graph %d: %w", event.Sequence, err)
	}
	return projectSessionSearchTx(ctx, tx, event)
}

func (s *Store) lastReserved(ctx context.Context) (protocol.Cursor, error) {
	var last protocol.Cursor
	if err := s.sqlite.DB().QueryRowContext(
		ctx, "SELECT COALESCE(MAX(sequence), 0) FROM event_reservations",
	).Scan(&last); err != nil {
		return 0, fmt.Errorf("read last reserved event sequence: %w", err)
	}
	return last, nil
}

func (s *Store) reconcile(ctx context.Context) error {
	records, err := s.events.ReplayRecords(ctx, 0)
	if err != nil {
		return err
	}
	projections, err := s.committedProjections(ctx)
	if err != nil {
		return err
	}
	seen := make(map[protocol.Cursor]struct{}, len(records))
	for _, record := range records {
		seen[record.Event.Sequence] = struct{}{}
		// Index and agent projections commit in the same transaction. A
		// matching committed index needs verification, not another write.
		if projected, ok := projections[record.Event.Sequence]; ok &&
			projected.matches(record) {
			continue
		}
		status, eventID, err := s.reservation(ctx, record.Event.Sequence)
		if err != nil {
			return err
		}
		if status != "" && eventID != string(record.Event.ID) {
			return fmt.Errorf(
				"%w: reservation %d identifies %s, log identifies %s",
				ErrProjection, record.Event.Sequence, eventID, record.Event.ID,
			)
		}
		if status == "" {
			if _, err := s.sqlite.DB().ExecContext(
				ctx,
				`INSERT INTO event_reservations(sequence, event_id, status, created_at, updated_at)
				 VALUES (?, ?, 'reserved', ?, ?)`,
				record.Event.Sequence, record.Event.ID, timestamp(record.Event.CreatedAt),
				timestamp(time.Now()),
			); err != nil {
				return fmt.Errorf("repair event reservation %d: %w", record.Event.Sequence, err)
			}
		}
		if err := s.commitProjection(ctx, record.Event, record.Evidence); err != nil {
			return fmt.Errorf("repair event projection %d: %w", record.Event.Sequence, err)
		}
	}

	rows, err := s.sqlite.DB().QueryContext(
		ctx,
		"SELECT sequence, status FROM event_reservations WHERE status != 'abandoned'",
	)
	if err != nil {
		return fmt.Errorf("read reservations during recovery: %w", err)
	}
	var interrupted []protocol.Cursor
	for rows.Next() {
		var sequence protocol.Cursor
		var status string
		if err := rows.Scan(&sequence, &status); err != nil {
			_ = rows.Close()
			return err
		}
		if _, exists := seen[sequence]; exists {
			continue
		}
		if status == "committed" {
			_ = rows.Close()
			return fmt.Errorf(
				"%w: committed reservation %d has no durable event",
				ErrProjection, sequence,
			)
		}
		interrupted = append(interrupted, sequence)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, sequence := range interrupted {
		if err := s.markReservation(ctx, sequence, "abandoned"); err != nil {
			return fmt.Errorf("abandon interrupted event reservation %d: %w", sequence, err)
		}
	}
	return s.reconcileSessionSearch(ctx, records)
}

type committedEventProjection struct {
	eventID   protocol.EventID
	threadID  protocol.ThreadID
	turnID    protocol.TurnID
	itemID    protocol.ItemID
	kind      protocol.EventKind
	offset    int64
	length    int64
	digest    string
	createdAt string
}

func (p committedEventProjection) matches(record eventlog.Record) bool {
	event, evidence := record.Event, record.Evidence
	return p.eventID == event.ID &&
		p.threadID == event.ThreadID && p.turnID == event.TurnID &&
		p.itemID == event.ItemID && p.kind == event.Kind &&
		p.offset == evidence.Offset && p.length == evidence.Length &&
		p.digest == evidence.SHA256 && p.createdAt == timestamp(event.CreatedAt)
}

func (s *Store) committedProjections(
	ctx context.Context,
) (map[protocol.Cursor]committedEventProjection, error) {
	rows, err := s.sqlite.DB().QueryContext(ctx, `
		SELECT i.sequence, i.event_id, COALESCE(i.thread_id, ''),
		       COALESCE(i.turn_id, ''), COALESCE(i.item_id, ''),
		       i.kind, i.log_offset, i.log_length, i.sha256, i.created_at
		FROM event_index i JOIN event_reservations r
		  ON r.sequence = i.sequence AND r.event_id = i.event_id
		WHERE r.status = 'committed'`)
	if err != nil {
		return nil, fmt.Errorf("read committed event projections: %w", err)
	}
	defer rows.Close()
	projections := make(map[protocol.Cursor]committedEventProjection)
	for rows.Next() {
		var sequence protocol.Cursor
		var projection committedEventProjection
		if err := rows.Scan(
			&sequence, &projection.eventID, &projection.threadID,
			&projection.turnID, &projection.itemID, &projection.kind,
			&projection.offset, &projection.length, &projection.digest,
			&projection.createdAt,
		); err != nil {
			return nil, err
		}
		projections[sequence] = projection
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return projections, nil
}

func nullString[T ~string](value T) any {
	if value == "" {
		return nil
	}
	return string(value)
}

func timestamp(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
