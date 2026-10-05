package wire

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionCloseWaitCanCancelWithoutInterruptingCleanup(t *testing.T) {
	session := &Session{resourceBundle: resourceBundle{resources: NewResourceStack()}}
	failure := errors.New("resource close failed")
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	if err := session.RegisterResource("blocking", func(context.Context) error {
		calls.Add(1)
		close(started)
		<-release
		return failure
	}); err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	first := make(chan error, 1)
	go func() { first <- session.Close(context.Background()) }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	waiter := make(chan error, 1)
	go func() { waiter <- session.Close(canceled) }()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Close ignored the waiter's canceled context")
	}
	select {
	case err := <-first:
		t.Fatalf("waiter interrupted cleanup: %v", err)
	default:
	}
	unblock()
	select {
	case err := <-first:
		if !errors.Is(err, failure) {
			t.Fatalf("first Close = %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := session.Close(canceled); !errors.Is(err, failure) {
		t.Fatalf("completed Close lost its error: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("resource closed %d times", calls.Load())
	}
}
