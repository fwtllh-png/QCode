//go:build stress
// +build stress

package engine

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// TestStressConcurrentEngineCloneEmpty verifies that Engine.CloneEmpty does not
// race or deadlock when called concurrently from many goroutines.
func TestStressConcurrentEngineCloneEmpty(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))

	const numGoroutines = 50
	var wg sync.WaitGroup
	var clones atomic.Int64
	var errors atomic.Int64

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			clone, err := engine.CloneEmpty()
			if err != nil {
				errors.Add(1)
				return
			}
			clones.Add(1)
			_ = clone
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("clones=%d errors=%d", clones.Load(), errors.Load())
	case <-time.After(10 * time.Second):
		t.Error("BUG: stress concurrent CloneEmpty deadlocked (timeout after 10s)")
	}
}

// TestStressConcurrentEngineOptionsRead verifies that OptionsSeed and other
// metadata reads do not race when called concurrently.
func TestStressConcurrentEngineOptionsRead(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))

	const numGoroutines = 100
	var wg sync.WaitGroup
	var reads atomic.Int64

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = engine.OptionsSeed()
				reads.Add(1)
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
		t.Error("BUG: stress concurrent OptionsSeed deadlocked")
	}
}

// TestStressConcurrentPolicyChanges verifies that SetPermission and
// SetGranular are safe under concurrent calls.
func TestStressConcurrentPolicyChanges(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))

	const numGoroutines = 50
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				switch (idx + j) % 2 {
				case 0:
					engine.SetPermission(policy.PermissionBypass)
				case 1:
					engine.SetGranular(policy.Granular{})
				}
			}
		}(i)
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
		t.Error("BUG: stress concurrent policy changes deadlocked")
	}
}

// TestStressConcurrentEngineFork verifies that Engine.Fork does not deadlock
// under concurrent access.
func TestStressConcurrentEngineFork(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))

	const numGoroutines = 20
	var wg sync.WaitGroup
	var forks atomic.Int64
	var errors atomic.Int64

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			forked, err := engine.Fork()
			if err != nil {
				errors.Add(1)
				return
			}
			forks.Add(1)
			_ = forked
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("forks=%d errors=%d", forks.Load(), errors.Load())
	case <-time.After(10 * time.Second):
		t.Error("BUG: stress concurrent Fork deadlocked")
	}
}

// TestStressConcurrentHistoryAndCompaction verifies that history reads and
// compaction do not deadlock under concurrent access.
func TestStressConcurrentHistoryAndCompaction(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))

	// Seed some history.
	engine.history = []provider.Message{
		provider.TextMessage(provider.RoleUser, "old request"),
		provider.TextMessage(provider.RoleAssistant, "old answer"),
		provider.TextMessage(provider.RoleUser, "current request"),
	}

	const numGoroutines = 30
	var wg sync.WaitGroup
	var reads atomic.Int64

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = engine.History()
				reads.Add(1)
			}
		}()
	}

	for i := 0; i < numGoroutines/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = engine.Compact()
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("history reads=%d", reads.Load())
	case <-time.After(10 * time.Second):
		t.Error("BUG: stress concurrent History/Compact deadlocked")
	}
}

// TestStressEphemeralCoordinatorRuntime verifies the ephemeral coordinator
// runtime used by engine tests is safe for concurrent stress use.
func TestStressEphemeralCoordinatorRuntime(t *testing.T) {
	runtime := turnkernel.NewEphemeralCoordinatorRuntime()
	if runtime == nil {
		t.Fatal("ephemeral coordinator runtime is nil")
	}

	// Exercise the runtime concurrently to verify thread safety.
	const numGoroutines = 50
	var wg sync.WaitGroup
	var errors atomic.Int64

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			coordinator, err := runtime.Restore(t.Context(), "stress-turn")
			if err != nil {
				errors.Add(1)
				return
			}
			if coordinator.Coordinator == nil {
				errors.Add(1)
				return
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
		t.Logf("coordinator errors=%d", errors.Load())
	case <-time.After(10 * time.Second):
		t.Error("BUG: stress coordinator runtime deadlocked")
	}
}
