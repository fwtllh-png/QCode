//go:build stress
// +build stress

package mcp

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStressConcurrentPoolCatalogReads verifies that Catalog(), ResourceCatalog(),
// ResourceTemplateCatalog(), and PromptCatalog() are safe under concurrent reads.
func TestStressConcurrentPoolCatalogReads(t *testing.T) {
	pool := NewPool(nil)

	const numGoroutines = 100
	var wg sync.WaitGroup
	var reads atomic.Int64

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = pool.Catalog()
				_ = pool.ResourceCatalog()
				_ = pool.ResourceTemplateCatalog()
				_ = pool.PromptCatalog()
				_ = pool.ServerNames()
				_ = pool.Hash()
				reads.Add(5)
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("total reads=%d", reads.Load())
	case <-time.After(10 * time.Second):
		t.Error("BUG: stress concurrent catalog reads deadlocked")
	}
}

// TestStressConcurrentPoolHealthSnapshots verifies that HealthSnapshots is
// safe under concurrent reads.
func TestStressConcurrentPoolHealthSnapshots(t *testing.T) {
	pool := NewPool(nil)

	const numGoroutines = 50
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = pool.HealthSnapshots()
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success.
	case <-time.After(10 * time.Second):
		t.Error("BUG: stress concurrent HealthSnapshots deadlocked")
	}
}

// TestStressConcurrentPoolSubscriptions verifies that health subscriptions are
// safe under concurrent access.
func TestStressConcurrentPoolSubscriptions(t *testing.T) {
	pool := NewPool(nil)

	const numGoroutines = 50
	var wg sync.WaitGroup
	var subscribed atomic.Int64
	var unsubscribed atomic.Int64

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unsubHealth := pool.SubscribeHealth(func(change HealthChange) {
				// No-op observer.
			})
			subscribed.Add(1)
			unsubHealth()
			unsubscribed.Add(1)
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("subscribed=%d unsubscribed=%d", subscribed.Load(), unsubscribed.Load())
	case <-time.After(10 * time.Second):
		t.Error("BUG: stress concurrent subscriptions deadlocked")
	}
}

// TestStressConcurrentPoolConnectionLookup verifies that Connection() is safe
// under concurrent reads.
func TestStressConcurrentPoolConnectionLookup(t *testing.T) {
	pool := NewPool(nil)

	const numGoroutines = 50
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = pool.Connection("nonexistent")
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success.
	case <-time.After(10 * time.Second):
		t.Error("BUG: stress concurrent Connection lookup deadlocked")
	}
}

// TestStressConcurrentPoolInvalidate verifies that Invalidate is safe when
// called concurrently with reads.
func TestStressConcurrentPoolInvalidate(t *testing.T) {
	pool := NewPool(nil)

	const numGoroutines = 50
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool.Invalidate()
			_ = pool.Hash()
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success.
	case <-time.After(10 * time.Second):
		t.Error("BUG: stress concurrent Invalidate deadlocked")
	}
}

// TestStressConcurrentPoolProbeOpen verifies that ProbeOpen is safe under
// concurrent access.
func TestStressConcurrentPoolProbeOpen(t *testing.T) {
	pool := NewPool(nil)

	const numGoroutines = 20
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pool.ProbeOpen(t.Context())
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success.
	case <-time.After(10 * time.Second):
		t.Error("BUG: stress concurrent ProbeOpen deadlocked")
	}
}

// TestStressNilPoolIsSafe verifies that all Pool methods are nil-safe under
// concurrent access. Each method call is wrapped in a recover to catch
// nil-pointer panics in production code paths.
func TestStressNilPoolIsSafe(t *testing.T) {
	var pool *Pool

	const numGoroutines = 50
	var wg sync.WaitGroup
	var panics atomic.Int64

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Wrap each call in a recover to safely detect nil panics.
			safeCall := func(fn func()) {
				defer func() {
					if r := recover(); r != nil {
						panics.Add(1)
					}
				}()
				fn()
			}
			safeCall(func() { pool.Invalidate() })
			safeCall(func() { _ = pool.Catalog() })
			safeCall(func() { _ = pool.ResourceCatalog() })
			safeCall(func() { _ = pool.ResourceTemplateCatalog() })
			safeCall(func() { _ = pool.PromptCatalog() })
			safeCall(func() { _ = pool.ServerNames() })
			safeCall(func() { _ = pool.HealthSnapshots() })
			safeCall(func() { _ = pool.Hash() })
			safeCall(func() { _ = pool.ProbeOpen(t.Context()) })
			safeCall(func() { _, _ = pool.Connection("any") })
			safeCall(func() { _ = pool.SubscribeHealth(nil) })
			safeCall(func() { _ = pool.SubscribeHealth(func(HealthChange) {}) })
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		if panics.Load() > 0 {
			t.Errorf("nil Pool panicked %d times — Pool methods are not nil-safe", panics.Load())
		}
	case <-time.After(10 * time.Second):
		t.Error("BUG: stress nil pool operations deadlocked")
	}
}
