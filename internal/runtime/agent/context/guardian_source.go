package agentcontext

import (
	"context"
	"errors"

	"github.com/fwtllh-png/QCode/internal/security/guardian"
)

// GuardianSource owns current durable user authority and the serialization
// boundary between updating it and starting a reviewed operation.
type GuardianSource interface {
	Capture(context.Context, AuthorizationScope) (GuardianAuthorization, error)
	WithCurrent(context.Context, AuthorizationScope, guardian.AuthorizationSnapshot, func() error) error
}

type GuardianEventFence interface {
	AuthorizationEventStore
	WithAuthorizationEvents(context.Context, func() error) error
}

// DurableGuardianSource resolves parent scopes from trusted Runtime metadata.
// A nil parent resolver supports root scopes only.
type DurableGuardianSource struct {
	Store  GuardianEventFence
	Parent func(context.Context, AuthorizationScope) (*AuthorizationScope, error)
	// Delegated reads the durable agent graph, not merely a parent-thread
	// relationship: checkpoint forks also have parents and retain local users.
	Delegated func(context.Context, AuthorizationScope) (bool, error)
}

func (s DurableGuardianSource) Capture(ctx context.Context, scope AuthorizationScope) (GuardianAuthorization, error) {
	return s.capture(ctx, scope, make(map[string]bool))
}

func (s DurableGuardianSource) capture(ctx context.Context, scope AuthorizationScope, seen map[string]bool) (GuardianAuthorization, error) {
	if seen[scope.ThreadID] {
		return GuardianAuthorization{}, errors.New("Guardian parent authority contains a cycle")
	}
	seen[scope.ThreadID] = true
	if s.Delegated != nil {
		delegated, err := s.Delegated(ctx, scope)
		if err != nil {
			return GuardianAuthorization{}, err
		}
		scope.Child = scope.Child || delegated
	}
	var parent *GuardianAuthorization
	if s.Parent != nil {
		parentScope, err := s.Parent(ctx, scope)
		if err != nil {
			return GuardianAuthorization{}, err
		}
		if parentScope != nil {
			value, err := s.capture(ctx, *parentScope, seen)
			if err != nil {
				return GuardianAuthorization{}, err
			}
			parent = &value
		}
	}
	return CaptureGuardianAuthorization(ctx, s.Store, scope, parent)
}

func (s DurableGuardianSource) WithCurrent(ctx context.Context, scope AuthorizationScope, expected guardian.AuthorizationSnapshot, start func() error) error {
	if s.Store == nil {
		return errors.New("Guardian durable event fence is unavailable")
	}
	return s.Store.WithAuthorizationEvents(ctx, func() error {
		current, err := s.Capture(ctx, scope)
		if err != nil {
			return err
		}
		if current.Snapshot().Digest() != expected.Digest() {
			return errors.New("Guardian user authorization changed before process start")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return start()
	})
}
