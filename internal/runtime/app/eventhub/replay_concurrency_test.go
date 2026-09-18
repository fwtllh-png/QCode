package eventhub_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/app/eventhub"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type gatedReplayStore struct {
	eventhub.Store
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *gatedReplayStore) wait(ctx context.Context) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *gatedReplayStore) Replay(ctx context.Context, cursor protocol.Cursor) ([]protocol.Event, error) {
	if err := s.wait(ctx); err != nil {
		return nil, err
	}
	return s.Store.Replay(ctx, cursor)
}

func (s *gatedReplayStore) EventByID(ctx context.Context, id protocol.EventID) (protocol.Event, bool, error) {
	return s.Store.(eventhub.IdentityStore).EventByID(ctx, id)
}

type gatedFencedStore struct{ *gatedReplayStore }

func (s *gatedFencedStore) ReplayThrough(ctx context.Context, cursor, through protocol.Cursor, limit int) ([]protocol.Event, bool, error) {
	if err := s.wait(ctx); err != nil {
		return nil, false, err
	}
	return s.Store.(eventhub.FencedStore).ReplayThrough(ctx, cursor, through, limit)
}

type gatedLimitedStore struct{ *gatedReplayStore }

func (s *gatedLimitedStore) ReplayLimit(ctx context.Context, cursor protocol.Cursor, limit int) ([]protocol.Event, bool, error) {
	events, err := s.Replay(ctx, cursor)
	if len(events) > limit {
		return events[:limit], true, err
	}
	return events, false, err
}

func replayTestHub(t *testing.T, mode string, buffer int, runtimeCtx context.Context) (*eventhub.Hub, *gatedReplayStore, *atomic.Int32) {
	t.Helper()
	gate := &gatedReplayStore{
		Store: app.NewMemoryEventStore(32), entered: make(chan struct{}), release: make(chan struct{}),
	}
	var store eventhub.Store = gate
	switch mode {
	case "fenced":
		store = &gatedFencedStore{gate}
	case "limited":
		store = &gatedLimitedStore{gate}
	}
	dropped := &atomic.Int32{}
	hub := eventhub.New(eventhub.Config{
		Store: store, Buffer: buffer, Context: runtimeCtx,
		Closed: app.ErrClosed, CursorAhead: app.ErrCursorAhead,
		ReplayOverflow: func(cursor protocol.Cursor, limit int) error {
			return &app.ReplayLimitError{Requested: cursor, Limit: limit}
		},
		OnDropped: func() { dropped.Add(1) },
	})
	t.Cleanup(func() { _ = hub.Close(context.Background()) })
	return hub, gate, dropped
}

var replayTestMeta = protocol.EventMeta{
	OperationID: "op", ThreadID: "thread", TurnID: "turn", ItemID: "item",
}

func publishDuringReplay(t *testing.T, hub *eventhub.Hub) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- hub.Publish(replayTestMeta, &protocol.TurnCompletedData{}, func(protocol.Event) error { return nil })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("publish waited for blocked replay")
	}
}

func awaitReplayGate(t *testing.T, gate *gatedReplayStore) {
	t.Helper()
	select {
	case <-gate.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("replay did not reach gate")
	}
}

func TestHubReplayFenceAndLiveHandoff(t *testing.T) {
	for _, mode := range []string{"fenced", "limited", "legacy"} {
		for _, subscribe := range []bool{false, true} {
			name := mode + "/replay"
			if subscribe {
				name = mode + "/subscribe"
			}
			t.Run(name, func(t *testing.T) {
				hub, gate, _ := replayTestHub(t, mode, 4, t.Context())
				release := sync.OnceFunc(func() { close(gate.release) })
				defer release()
				publishDuringReplay(t, hub)
				publishDuringReplay(t, hub)
				type result struct {
					channel <-chan protocol.Event
					page    []protocol.Event
					more    bool
					err     error
				}
				done := make(chan result, 1)
				go func() {
					var r result
					if subscribe {
						r.channel, r.err = hub.Events(t.Context(), 0, 2)
					} else {
						r.page, r.more, r.err = hub.Replay(t.Context(), 0, 2)
					}
					done <- r
				}()
				awaitReplayGate(t, gate)
				publishDuringReplay(t, hub)
				publishDuringReplay(t, hub)
				release()
				r := <-done
				if r.err != nil || r.more {
					t.Fatalf("more=%v err=%v", r.more, r.err)
				}
				if subscribe {
					publishDuringReplay(t, hub)
					for sequence := protocol.Cursor(1); sequence <= 5; sequence++ {
						select {
						case event := <-r.channel:
							if event.Sequence != sequence {
								t.Fatalf("sequence=%d want=%d", event.Sequence, sequence)
							}
						case <-time.After(3 * time.Second):
							t.Fatalf("missing sequence %d", sequence)
						}
					}
					select {
					case event := <-r.channel:
						t.Fatalf("duplicate event %d", event.Sequence)
					default:
					}
				} else if len(r.page) != 2 || r.page[0].Sequence != 1 || r.page[1].Sequence != 2 {
					t.Fatalf("replay crossed fence: %v", r.page)
				}
				page, more, err := hub.Replay(t.Context(), 0, 1)
				if err != nil || !more || len(page) != 1 {
					t.Fatalf("limited replay: len=%d more=%v err=%v", len(page), more, err)
				}
				if _, err := hub.Events(t.Context(), 0, 1); err == nil {
					t.Fatal("history overflow accepted")
				}
			})
		}
	}
}

func TestHubReplayCancellationAndOverflowReleaseSubscription(t *testing.T) {
	for _, mode := range []string{"caller", "runtime", "close", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			runtimeCtx, cancelRuntime := context.WithCancel(t.Context())
			defer cancelRuntime()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			hub, gate, dropped := replayTestHub(t, "fenced", 1, runtimeCtx)
			release := sync.OnceFunc(func() { close(gate.release) })
			defer release()
			done := make(chan error, 1)
			go func() { _, err := hub.Events(ctx, 0, 0); done <- err }()
			awaitReplayGate(t, gate)
			want := context.Canceled
			switch mode {
			case "caller":
				cancel()
			case "runtime":
				cancelRuntime()
			case "close":
				want = app.ErrClosed
				closed := make(chan error, 1)
				go func() { closed <- hub.Close(t.Context()) }()
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("close waited for blocked replay")
				}
			case "overflow":
				want = eventhub.ErrSubscriptionOverflow
				publishDuringReplay(t, hub)
				publishDuringReplay(t, hub)
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("error=%v want=%v", err, want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("replay was not canceled")
			}
			if count := hub.Snapshot().Subscribers; count != 0 {
				t.Fatalf("retained %d subscribers", count)
			}
			wantDropped := int32(0)
			if mode == "overflow" {
				wantDropped = 1
			}
			if dropped.Load() != wantDropped {
				t.Fatalf("dropped=%d want=%d", dropped.Load(), wantDropped)
			}
		})
	}
}

func TestHubStableRetryDoesNotDuplicateFrozenHistory(t *testing.T) {
	hub, gate, _ := replayTestHub(t, "fenced", 2, t.Context())
	release := sync.OnceFunc(func() { close(gate.release) })
	defer release()
	err := hub.PublishStable(replayTestMeta, "stable", &protocol.TurnCompletedData{}, func(protocol.Event) error {
		return errors.New("projection unavailable")
	})
	if err == nil {
		t.Fatal("expected failed projection")
	}
	type result struct {
		events <-chan protocol.Event
		err    error
	}
	done := make(chan result, 1)
	go func() {
		events, err := hub.Events(t.Context(), 0, 0)
		done <- result{events, err}
	}()
	awaitReplayGate(t, gate)
	publishDuringReplay(t, hub)
	retried := make(chan error, 1)
	go func() {
		retried <- hub.PublishStable(replayTestMeta, "stable", &protocol.TurnCompletedData{}, func(protocol.Event) error { return nil })
	}()
	select {
	case err := <-retried:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stable retry waited for replay")
	}
	release()
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	for _, want := range []protocol.Cursor{1, 2} {
		if event := <-r.events; event.Sequence != want {
			t.Fatalf("sequence=%d want=%d", event.Sequence, want)
		}
	}
	select {
	case event := <-r.events:
		t.Fatalf("duplicate stable event: %s", event.ID)
	default:
	}
}

func TestHubReplayUsesSharedStoreWatermark(t *testing.T) {
	for _, mode := range []string{"fenced", "limited", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			hub, gate, _ := replayTestHub(t, mode, 2, t.Context())
			close(gate.release)
			appendExternal := func(sequence protocol.Cursor) {
				t.Helper()
				meta := replayTestMeta
				meta.Sequence = sequence
				event, err := protocol.NewEvent(meta, &protocol.TurnCompletedData{})
				if err != nil {
					t.Fatal(err)
				}
				if err := gate.Store.Append(t.Context(), event); err != nil {
					t.Fatal(err)
				}
			}
			appendExternal(1)
			page, more, err := hub.Replay(t.Context(), 0, 1)
			if err != nil || more || len(page) != 1 || page[0].Sequence != 1 {
				t.Fatalf("shared store replay: page=%v more=%v err=%v", page, more, err)
			}
			appendExternal(2)
			events, err := hub.Events(t.Context(), 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			publishDuringReplay(t, hub)
			for _, sequence := range []protocol.Cursor{2, 3} {
				select {
				case event := <-events:
					if event.Sequence != sequence {
						t.Fatalf("sequence=%d want=%d", event.Sequence, sequence)
					}
				case <-time.After(3 * time.Second):
					t.Fatalf("missing sequence %d", sequence)
				}
			}
		})
	}
}
