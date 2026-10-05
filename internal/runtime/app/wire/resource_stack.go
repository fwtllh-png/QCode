package wire

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type closeResource struct {
	name  string
	close func(context.Context) error
}

// ResourceStack closes registered resources once, in reverse registration
// order. Registration is construction-only; Close permanently seals the stack.
type ResourceStack struct {
	mu      sync.Mutex
	entries []closeResource
	names   map[string]struct{}
	closed  bool
	done    chan struct{}
	err     error
}

func NewResourceStack() *ResourceStack {
	return &ResourceStack{
		names: make(map[string]struct{}),
		done:  make(chan struct{}),
	}
}

func (s *ResourceStack) Add(
	name string,
	close func(context.Context) error,
) error {
	if s == nil {
		return errors.New("resource stack is required")
	}
	if name == "" {
		return errors.New("resource name is required")
	}
	if close == nil {
		return fmt.Errorf("resource %q close function is required", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("resource stack is closed: %s", name)
	}
	if s.names == nil {
		s.names = make(map[string]struct{})
	}
	if s.done == nil {
		s.done = make(chan struct{})
	}
	if _, exists := s.names[name]; exists {
		return fmt.Errorf("resource %q is already registered", name)
	}
	s.entries = append(s.entries, closeResource{name: name, close: close})
	s.names[name] = struct{}{}
	return nil
}

func (s *ResourceStack) Detach(name string) bool {
	if s == nil || name == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if _, exists := s.names[name]; !exists {
		return false
	}
	for index := len(s.entries) - 1; index >= 0; index-- {
		if s.entries[index].name != name {
			continue
		}
		s.entries = append(s.entries[:index], s.entries[index+1:]...)
		delete(s.names, name)
		return true
	}
	return false
}

func (s *ResourceStack) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.done == nil {
		s.done = make(chan struct{})
	}
	if s.closed {
		// A completed close always returns its cached result, even when this
		// caller's context is already canceled. Only an in-flight wait cancels.
		select {
		case <-s.done:
			err := s.err
			s.mu.Unlock()
			return err
		default:
		}
		done := s.done
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.Lock()
			err := s.err
			s.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.closed = true
	entries := append([]closeResource(nil), s.entries...)
	s.entries = nil
	clear(s.names)
	s.mu.Unlock()

	closeErrors := make([]error, 0)
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if err := entry.close(ctx); err != nil {
			closeErrors = append(
				closeErrors,
				fmt.Errorf("close resource %q: %w", entry.name, err),
			)
		}
	}
	result := errors.Join(closeErrors...)

	s.mu.Lock()
	s.err = result
	close(s.done)
	s.mu.Unlock()
	return result
}
