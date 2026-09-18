package eventhub

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type Store interface {
	Append(context.Context, protocol.Event) error
	Replay(context.Context, protocol.Cursor) ([]protocol.Event, error)
	LastSequence(context.Context) (protocol.Cursor, error)
	Close(context.Context) error
}
type IdentityStore interface {
	EventByID(context.Context, protocol.EventID) (protocol.Event, bool, error)
}
type LimitedStore interface {
	ReplayLimit(context.Context, protocol.Cursor, int) ([]protocol.Event, bool, error)
}

// FencedStore reads (cursor, through] without following concurrent appends.
// A nonpositive limit returns the entire range.
type FencedStore interface {
	ReplayThrough(context.Context, protocol.Cursor, protocol.Cursor, int) ([]protocol.Event, bool, error)
}

var ErrSubscriptionOverflow = errors.New("event subscription buffer overflow")

type Config struct {
	Store          Store
	Buffer         int
	Context        context.Context
	Closed         error
	CursorAhead    error
	ReplayOverflow func(protocol.Cursor, int) error
	OnPublished    func()
	OnDropped      func()
	OnEvent        func(protocol.Event)
}
type Snapshot struct {
	LastSequence protocol.Cursor
	Subscribers  int
}
type subscription struct {
	events chan protocol.Event
	done   chan struct{}
	once   sync.Once
	floor  protocol.Cursor
	err    error
}

func newSubscription(capacity int) *subscription {
	return &subscription{
		events: make(chan protocol.Event, capacity),
		done:   make(chan struct{}),
	}
}

func (s *subscription) close(err error) {
	s.once.Do(func() {
		s.err = err
		close(s.events)
		close(s.done)
	})
}

type Hub struct {
	config      Config
	publishMu   sync.Mutex
	mu          sync.Mutex
	last        protocol.Cursor
	next        uint64
	subscribers map[uint64]*subscription
	pending     map[protocol.EventID]struct{}
	closed      bool
}

func New(config Config) *Hub {
	h := &Hub{
		config:      config,
		subscribers: make(map[uint64]*subscription),
		pending:     make(map[protocol.EventID]struct{}),
	}
	if last, err := config.Store.LastSequence(context.Background()); err == nil {
		h.last = last
	}
	return h
}
func (h *Hub) Events(ctx context.Context, cursor protocol.Cursor, limit int) (<-chan protocol.Event, error) {
	h.publishMu.Lock()
	through, err := h.replayFence(ctx, cursor)
	if err != nil {
		h.publishMu.Unlock()
		return nil, err
	}
	h.mu.Lock()
	subscriber := newSubscription(max(h.config.Buffer, 1))
	subscriber.floor = through
	h.next++
	id := h.next
	h.subscribers[id] = subscriber
	h.mu.Unlock()
	h.publishMu.Unlock()

	replayCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		var err error
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-h.config.Context.Done():
			err = h.config.Context.Err()
		case <-subscriber.done:
			err = subscriber.err
		}
		cancel(err)
		h.remove(id, err)
	}()
	replay, more, err := h.replay(replayCtx, cursor, through, limit)
	if err != nil {
		if cause := context.Cause(replayCtx); cause != nil {
			err = cause
		}
		h.remove(id, err)
		return nil, err
	}
	if more {
		err := h.config.ReplayOverflow(cursor, limit)
		h.remove(id, err)
		return nil, err
	}
	events := subscriber.events
	if len(replay) > 0 {
		events = make(chan protocol.Event, len(replay)+max(h.config.Buffer, 1))
		for _, event := range replay {
			events <- event
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.subscribers[id]; !exists {
		return nil, subscriber.err
	}
	if err := errors.Join(ctx.Err(), h.config.Context.Err()); err != nil {
		delete(h.subscribers, id)
		subscriber.close(err)
		return nil, err
	}
	// Fanout and removal use mu, so this handoff has no forwarding goroutine
	// or gap between the frozen history and the buffered live suffix.
	if len(replay) > 0 {
		for range len(subscriber.events) {
			events <- <-subscriber.events
		}
		subscriber.events = events
	}
	return events, nil
}
func (h *Hub) Replay(ctx context.Context, cursor protocol.Cursor, limit int) ([]protocol.Event, bool, error) {
	h.publishMu.Lock()
	through, err := h.replayFence(ctx, cursor)
	h.publishMu.Unlock()
	if err != nil {
		return nil, false, err
	}
	return h.replay(ctx, cursor, through, limit)
}

// Caller holds publishMu to pair this boundary with subscription registration.
// Other Runtime writers can advance the shared store without updating h.last.
func (h *Hub) replayFence(ctx context.Context, cursor protocol.Cursor) (protocol.Cursor, error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return 0, h.config.Closed
	}
	h.mu.Unlock()
	stored, err := h.config.Store.LastSequence(ctx)
	if err != nil {
		return 0, err
	}
	h.mu.Lock()
	h.last = max(h.last, stored)
	through := h.last
	h.mu.Unlock()
	if cursor > through {
		return 0, h.config.CursorAhead
	}
	return through, nil
}
func (h *Hub) replay(ctx context.Context, cursor, through protocol.Cursor, limit int) ([]protocol.Event, bool, error) {
	if store, ok := h.config.Store.(FencedStore); ok {
		return store.ReplayThrough(ctx, cursor, through, limit)
	}
	var page []protocol.Event
	var err error
	if limit > 0 && limit < math.MaxInt {
		if store, ok := h.config.Store.(LimitedStore); ok {
			// One extra event determines "more" within the fence even when
			// the legacy store counts concurrent appends in its own result.
			page, _, err = store.ReplayLimit(ctx, cursor, limit+1)
		} else {
			page, err = h.config.Store.Replay(ctx, cursor)
		}
	} else {
		page, err = h.config.Store.Replay(ctx, cursor)
	}
	for index, event := range page {
		if event.Sequence > through {
			page = page[:index]
			break
		}
	}
	if limit > 0 && len(page) > limit {
		return page[:limit], true, err
	}
	return page, false, err
}
func (h *Hub) Publish(meta protocol.EventMeta, data protocol.EventData, project func(protocol.Event) error) error {
	return h.publish(meta, "", time.Time{}, data, project)
}
func (h *Hub) PublishStable(meta protocol.EventMeta, id protocol.EventID, data protocol.EventData, project func(protocol.Event) error) error {
	return h.publish(meta, id, time.Now(), data, project)
}
func (h *Hub) publish(meta protocol.EventMeta, id protocol.EventID, at time.Time, data protocol.EventData, project func(protocol.Event) error) error {
	h.publishMu.Lock()
	defer h.publishMu.Unlock()
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return h.config.Closed
	}
	last := h.last
	h.mu.Unlock()
	identity, identityCapable := h.config.Store.(IdentityStore)
	stable := id != ""
	if stable && !identityCapable {
		return errors.New("stable event requires identity store")
	}
	if stable {
		if event, exists, err := identity.EventByID(context.Background(), id); err != nil {
			return err
		} else if exists {
			h.mu.Lock()
			h.last = max(h.last, event.Sequence)
			h.mu.Unlock()
			_, announce := h.pending[event.ID]
			return h.projectAndAnnounce(event, true, announce, project)
		}
	}
	for attempt := 0; ; attempt++ {
		meta.Sequence = last + 1
		var event protocol.Event
		var err error
		if id == "" {
			event, err = protocol.NewEvent(meta, data)
		} else {
			event, err = protocol.NewEventWithIdentity(meta, id, at, data)
		}
		if err != nil {
			return err
		}
		if err = h.config.Store.Append(context.Background(), event); err == nil {
			h.mu.Lock()
			h.last = event.Sequence
			h.mu.Unlock()
			return h.projectAndAnnounce(event, stable, true, project)
		}
		if stable {
			if existing, exists, lookupErr := identity.EventByID(context.Background(), id); lookupErr != nil {
				return errors.Join(err, lookupErr)
			} else if exists {
				h.mu.Lock()
				h.last = max(h.last, existing.Sequence)
				h.mu.Unlock()
				return h.projectAndAnnounce(existing, true, true, project)
			}
		}
		storedLast, sequenceErr := h.config.Store.LastSequence(
			context.Background(),
		)
		if sequenceErr != nil || attempt >= 3 {
			return err
		}
		last = max(last, storedLast)
		h.mu.Lock()
		h.last = max(h.last, storedLast)
		h.mu.Unlock()
	}
}

func (h *Hub) projectAndAnnounce(event protocol.Event, trackPending, announce bool, project func(protocol.Event) error) error {
	if err := project(event); err != nil {
		if trackPending && announce && event.ID != "" {
			h.pending[event.ID] = struct{}{}
		}
		return err
	}
	if !announce {
		return nil
	}
	delete(h.pending, event.ID)
	if h.config.OnEvent != nil {
		h.config.OnEvent(event)
	}
	h.fanout(event)
	return nil
}

func (h *Hub) fanout(event protocol.Event) {
	h.mu.Lock()
	dropped := 0
	for id, subscriber := range h.subscribers {
		if event.Sequence <= subscriber.floor {
			continue
		}
		select {
		case subscriber.events <- event:
		default:
			delete(h.subscribers, id)
			subscriber.close(ErrSubscriptionOverflow)
			dropped++
		}
	}
	h.mu.Unlock()
	if h.config.OnPublished != nil {
		h.config.OnPublished()
	}
	if h.config.OnDropped != nil {
		for range dropped {
			h.config.OnDropped()
		}
	}
}
func (h *Hub) Snapshot() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return Snapshot{LastSequence: h.last, Subscribers: len(h.subscribers)}
}
func (h *Hub) Restore(sequence protocol.Cursor) {
	h.mu.Lock()
	h.last = max(h.last, sequence)
	h.mu.Unlock()
}
func (h *Hub) Close(ctx context.Context) error {
	h.publishMu.Lock()
	h.mu.Lock()
	h.closed = true
	for id, subscriber := range h.subscribers {
		delete(h.subscribers, id)
		subscriber.close(h.config.Closed)
	}
	h.mu.Unlock()
	h.publishMu.Unlock()
	return h.config.Store.Close(ctx)
}
func (h *Hub) remove(id uint64, err error) {
	h.mu.Lock()
	if subscriber, exists := h.subscribers[id]; exists {
		delete(h.subscribers, id)
		subscriber.close(err)
	}
	h.mu.Unlock()
}
