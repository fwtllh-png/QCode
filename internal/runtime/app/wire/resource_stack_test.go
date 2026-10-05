package wire

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestResourceStackClosesOnceInReverseOrder(t *testing.T) {
	stack := NewResourceStack()
	var order []string
	for _, name := range []string{"first", "second", "third"} {
		name := name
		if err := stack.Add(name, func(context.Context) error {
			order = append(order, name)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := stack.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := stack.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"third", "second", "first"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("close order = %v, want %v", order, want)
	}
}

func TestResourceStackContinuesAndJoinsCloseFailures(t *testing.T) {
	stack := NewResourceStack()
	firstErr := errors.New("first failed")
	secondErr := errors.New("second failed")
	var closed atomic.Int32
	for name, closeErr := range map[string]error{
		"first": firstErr, "second": secondErr, "third": nil,
	} {
		name, closeErr := name, closeErr
		if err := stack.Add(name, func(context.Context) error {
			closed.Add(1)
			return closeErr
		}); err != nil {
			t.Fatal(err)
		}
	}
	err := stack.Close(t.Context())
	if !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("joined error = %v", err)
	}
	if closed.Load() != 3 {
		t.Fatalf("closed resources = %d, want 3", closed.Load())
	}
	if !strings.Contains(err.Error(), `resource "first"`) ||
		!strings.Contains(err.Error(), `resource "second"`) {
		t.Fatalf("close error lacks resource identity: %v", err)
	}
}

func TestResourceStackDetachTransfersOwnership(t *testing.T) {
	stack := NewResourceStack()
	var closed atomic.Bool
	if err := stack.Add("detached", func(context.Context) error {
		closed.Store(true)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !stack.Detach("detached") {
		t.Fatal("resource was not detached")
	}
	if stack.Detach("detached") {
		t.Fatal("detached resource remained registered")
	}
	if err := stack.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if closed.Load() {
		t.Fatal("detached resource was closed")
	}
}

func TestResourceStackRejectsInvalidOrLateRegistration(t *testing.T) {
	stack := NewResourceStack()
	if err := stack.Add("", func(context.Context) error { return nil }); err == nil {
		t.Fatal("empty resource name was accepted")
	}
	if err := stack.Add("nil", nil); err == nil {
		t.Fatal("nil close function was accepted")
	}
	if err := stack.Add("one", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := stack.Add("one", func(context.Context) error { return nil }); err == nil {
		t.Fatal("duplicate resource name was accepted")
	}
	if err := stack.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := stack.Add("late", func(context.Context) error { return nil }); err == nil {
		t.Fatal("registration after Close was accepted")
	}
}

func TestResourceStackConcurrentCloseRunsEachCloserOnce(t *testing.T) {
	stack := NewResourceStack()
	var closed atomic.Int32
	if err := stack.Add("shared", func(context.Context) error {
		closed.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := stack.Close(context.Background()); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	group.Wait()
	if closed.Load() != 1 {
		t.Fatalf("close count = %d, want 1", closed.Load())
	}
}

func TestResourceStackCompletedCloseWinsOverCanceledContext(t *testing.T) {
	for _, failure := range []error{nil, errors.New("close failed")} {
		stack := NewResourceStack()
		if err := stack.Add("resource", func(context.Context) error { return failure }); err != nil {
			t.Fatal(err)
		}
		result := stack.Close(t.Context())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		for range 32 {
			if err := stack.Close(ctx); err != result {
				t.Fatalf("completed Close = %v, want cached %v", err, result)
			}
		}
	}
}
