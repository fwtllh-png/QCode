package app

import (
	"context"
	"errors"
	"sync"

	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type EventStore interface {
	Append(context.Context, protocol.Event) error
	Replay(context.Context, protocol.Cursor) ([]protocol.Event, error)
	LastSequence(context.Context) (protocol.Cursor, error)
	Close(context.Context) error
}

type EventIdentityStore interface {
	EventByID(
		context.Context,
		protocol.EventID,
	) (protocol.Event, bool, error)
}

// IndexedEventReplay locates Turn or kind-scoped events without a prefix
// scan of the durable log. Memory stores filter their retained window.
type IndexedEventReplay interface {
	ReplayTurn(context.Context, protocol.TurnID) ([]protocol.Event, error)
	ReplayKind(context.Context, protocol.EventKind) ([]protocol.Event, error)
}

type ContentStore interface {
	Put(context.Context, string, []byte) error
	Get(context.Context, string) ([]byte, error)
	Retain(context.Context, string) error
	Release(context.Context, string) error
	Delete(context.Context, string) error
	Close(context.Context) error
}

var ErrContentNotFound = protocol.NewProblem(
	protocol.CodeInvalidArgument, "content not found", false, nil,
)

type MemoryEventStore struct {
	mu       sync.Mutex
	capacity int
	events   []protocol.Event
	last     protocol.Cursor
	closed   bool
}

func NewMemoryEventStore(capacity int) *MemoryEventStore {
	if capacity <= 0 {
		capacity = 256
	}
	return &MemoryEventStore{capacity: capacity}
}

func (s *MemoryEventStore) Append(ctx context.Context, event protocol.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := event.Validate(); err != nil {
		return protocol.NewProblem(protocol.CodeInvalidArgument, err.Error(), false, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if event.Sequence != s.last+1 {
		return protocol.NewProblem(
			protocol.CodeConflict, "event sequence is not contiguous", false, nil,
		)
	}
	s.events = append(s.events, event)
	s.last = event.Sequence
	if len(s.events) > s.capacity {
		s.events = append([]protocol.Event(nil), s.events[len(s.events)-s.capacity:]...)
	}
	return nil
}

func (s *MemoryEventStore) Replay(ctx context.Context, cursor protocol.Cursor) ([]protocol.Event, error) {
	events, _, err := s.replay(ctx, cursor, nil, 0)
	return events, err
}

// ReplayThrough reads a fixed inclusive fence, including the same eviction
// checks as Replay. A nonpositive limit returns the entire range.
func (s *MemoryEventStore) ReplayThrough(ctx context.Context, cursor, through protocol.Cursor, limit int) ([]protocol.Event, bool, error) {
	return s.replay(ctx, cursor, &through, limit)
}

func (s *MemoryEventStore) replay(ctx context.Context, cursor protocol.Cursor, through *protocol.Cursor, limit int) ([]protocol.Event, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false, ErrClosed
	}
	if cursor > s.last {
		return nil, false, ErrCursorAhead
	}
	if len(s.events) != 0 && cursor+1 < s.events[0].Sequence {
		return nil, false, &CursorGapError{
			Requested: cursor, OldestAvailable: s.events[0].Sequence, Latest: s.last,
		}
	}
	result := make([]protocol.Event, 0, len(s.events))
	for _, event := range s.events {
		if through != nil && event.Sequence > *through {
			break
		}
		if event.Sequence > cursor {
			if limit > 0 && len(result) == limit {
				return result, true, nil
			}
			result = append(result, event)
		}
	}
	return result, false, nil
}

func (s *MemoryEventStore) ReplayTurn(
	ctx context.Context,
	turnID protocol.TurnID,
) ([]protocol.Event, error) {
	if turnID == "" {
		return nil, ctx.Err()
	}
	return s.filterReplay(ctx, func(event protocol.Event) bool {
		return event.TurnID == turnID
	})
}

func (s *MemoryEventStore) ReplayKind(
	ctx context.Context,
	kind protocol.EventKind,
) ([]protocol.Event, error) {
	if kind == "" {
		return nil, ctx.Err()
	}
	return s.filterReplay(ctx, func(event protocol.Event) bool {
		return event.Kind == kind
	})
}

func (s *MemoryEventStore) filterReplay(
	ctx context.Context,
	keep func(protocol.Event) bool,
) ([]protocol.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	result := make([]protocol.Event, 0)
	for _, event := range s.events {
		if keep(event) {
			result = append(result, event)
		}
	}
	return result, nil
}

func (s *MemoryEventStore) EventByID(
	ctx context.Context,
	eventID protocol.EventID,
) (protocol.Event, bool, error) {
	if err := ctx.Err(); err != nil {
		return protocol.Event{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return protocol.Event{}, false, ErrClosed
	}
	for _, event := range s.events {
		if event.ID == eventID {
			return event, true, nil
		}
	}
	return protocol.Event{}, false, nil
}

func (s *MemoryEventStore) LastSequence(ctx context.Context) (protocol.Cursor, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.last, ErrClosed
	}
	return s.last, nil
}

func (s *MemoryEventStore) Close(context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

type MemoryContentStore struct {
	store *contentstore.Memory
}

func NewMemoryContentStore() *MemoryContentStore {
	return &MemoryContentStore{store: contentstore.NewMemory(contentstore.Options{})}
}

func (s *MemoryContentStore) Put(ctx context.Context, id string, content []byte) error {
	if id == "" {
		return protocol.NewProblem(protocol.CodeInvalidArgument, "content id is required", false, nil)
	}
	return contentStoreError(s.store.Put(ctx, id, content))
}

func (s *MemoryContentStore) Get(ctx context.Context, id string) ([]byte, error) {
	value, err := s.store.Get(ctx, id)
	return value, contentStoreError(err)
}

func (s *MemoryContentStore) Retain(ctx context.Context, id string) error {
	return contentStoreError(s.store.Retain(ctx, id))
}

func (s *MemoryContentStore) Release(ctx context.Context, id string) error {
	return contentStoreError(s.store.Release(ctx, id))
}

func (s *MemoryContentStore) Delete(ctx context.Context, id string) error {
	return contentStoreError(s.store.Delete(ctx, id))
}

func (s *MemoryContentStore) Close(ctx context.Context) error {
	return contentStoreError(s.store.Close(ctx))
}

func contentStoreError(err error) error {
	switch {
	case errors.Is(err, contentstore.ErrClosed):
		return ErrClosed
	case errors.Is(err, contentstore.ErrNotFound):
		return ErrContentNotFound
	default:
		return err
	}
}
