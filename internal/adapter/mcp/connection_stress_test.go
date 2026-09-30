//go:build stress
// +build stress

package mcp

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStressConnectionCloseIsIdempotent verifies that Connection.Close is safe
// when called multiple times concurrently.
func TestStressConnectionCloseIsIdempotent(t *testing.T) {
	// Use a mock transport that records close calls.
	transport := &stressMockTransport{}
	conn, err := NewConnection("stress", transport, time.Second, time.Second)
	if err != nil {
		// Connection creation may fail without initialization; skip if so.
		t.Skipf("connection creation skipped: %v", err)
		return
	}

	const numGoroutines = 50
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = conn.Close(t.Context())
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("transport close calls=%d", transport.closeCount.Load())
		if transport.closeCount.Load() > 1 {
			t.Errorf("transport.Close called %d times, want at most 1", transport.closeCount.Load())
		}
	case <-time.After(10 * time.Second):
		t.Error("BUG: stress concurrent Connection.Close deadlocked")
	}
}

type stressMockTransport struct {
	closeCount atomic.Int64
}

func (t *stressMockTransport) Request(ctx context.Context, method string, params any, result any) error {
	return nil
}

func (t *stressMockTransport) Notify(ctx context.Context, method string, params any) error {
	return nil
}

func (t *stressMockTransport) Close(ctx context.Context) error {
	t.closeCount.Add(1)
	return nil
}

func (t *stressMockTransport) StderrTail() string {
	return ""
}

var _ Transport = (*stressMockTransport)(nil)
