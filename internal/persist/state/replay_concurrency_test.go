package state

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// Pause at the first replay cancellation checkpoint outside the write lock.
// Log's own tests independently block ReadAt to check the lower lock boundary.
type replayCheckpointContext struct {
	context.Context
	store   *Store
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (c *replayCheckpointContext) Err() error {
	if c.store.mu.TryLock() {
		c.store.mu.Unlock()
		c.once.Do(func() {
			close(c.entered)
			<-c.release
		})
	}
	return c.Context.Err()
}

func TestStoreAppendCompletesDuringPausedReplay(t *testing.T) {
	for _, mode := range []string{"replay", "limit", "identity", "session"} {
		t.Run(mode, func(t *testing.T) {
			store, err := Open(t.Context(), Options{DataDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
			first := testEvent(t, 1)
			if err := store.Append(t.Context(), first); err != nil {
				t.Fatal(err)
			}
			ctx := &replayCheckpointContext{
				Context: context.Background(), store: store,
				entered: make(chan struct{}), release: make(chan struct{}),
			}
			release := sync.OnceFunc(func() { close(ctx.release) })
			defer release()
			done := make(chan error, 1)
			go func() {
				var err error
				switch mode {
				case "replay":
					_, err = store.Replay(ctx, 0)
				case "limit":
					_, _, err = store.ReplayLimit(ctx, 0, 1)
				case "identity":
					_, _, err = store.EventByID(ctx, first.ID)
				case "session":
					_, _, err = store.ReplaySessionBefore(ctx, "", []protocol.ThreadID{first.ThreadID}, 0, 1, 1)
				}
				done <- err
			}()
			select {
			case <-ctx.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("replay held the store write lock at every read checkpoint")
			}
			appended := make(chan error, 1)
			second := testEvent(t, 2)
			go func() { appended <- store.Append(t.Context(), second) }()
			select {
			case err := <-appended:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("append waited for paused replay")
			}
			release()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
