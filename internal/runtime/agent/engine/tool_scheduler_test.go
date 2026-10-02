package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func TestToolSchedulerAdmitsWaitersInFIFOOrder(t *testing.T) {
	sched := newToolScheduler(1)
	release, err := sched.Admit(t.Context(), tool.ParallelConcurrent)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan int, 5)
	var wg sync.WaitGroup
	for index := range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := sched.Admit(t.Context(), tool.ParallelSerial)
			if err != nil {
				return
			}
			acquired <- index
			release()
		}()
		for sched.Waiting() != index+1 {
			time.Sleep(time.Millisecond)
		}
	}
	release()
	for want := range 5 {
		select {
		case got := <-acquired:
			if got != want {
				t.Fatalf("FIFO order[%d] = %d", want, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("waiter %d starved", want)
		}
	}
	wg.Wait()
	if sched.Active() != 0 || sched.Waiting() != 0 {
		t.Fatalf("scheduler leaked active=%d waiting=%d", sched.Active(), sched.Waiting())
	}
}

func TestToolSchedulerConcurrentBound(t *testing.T) {
	sched := newToolScheduler(2)
	var inside atomic.Int32
	var maxInside atomic.Int32

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := sched.Admit(context.Background(), tool.ParallelConcurrent)
			if err != nil {
				t.Errorf("admit: %v", err)
				return
			}
			cur := inside.Add(1)
			for {
				prev := maxInside.Load()
				if cur <= prev || maxInside.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			inside.Add(-1)
			release()
		}()
	}
	wg.Wait()
	if maxInside.Load() > 2 {
		t.Fatalf("max concurrent inside = %d, want <= 2", maxInside.Load())
	}
}

func TestToolSchedulerCancelDuringAdmit(t *testing.T) {
	sched := newToolScheduler(1)
	release, err := sched.Admit(context.Background(), tool.ParallelConcurrent)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sched.Admit(ctx, tool.ParallelConcurrent)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancel error")
		}
	case <-time.After(time.Second):
		t.Fatal("admit did not unblock on cancel")
	}
	release()
	if sched.Active() != 0 || sched.Waiting() != 0 {
		t.Fatalf("scheduler leaked active=%d waiting=%d", sched.Active(), sched.Waiting())
	}
}
