package tool

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// Isolator starts a command-scoped isolated workspace for declared write trees.
type Isolator interface {
	Begin(ctx context.Context, id string, trees []string) (IsolatedWorkspace, error)
	// BeginShadow starts an isolated workspace whose Settle summarizes the
	// planned changes without applying any of them: shadow verification
	// discards writes instead of settling them into the parent.
	BeginShadow(ctx context.Context, id string, trees []string) (IsolatedWorkspace, error)
}

// IsolatedWorkspace is one isolated execution root. Settle writes approved
// tree changes into the parent through Broker/Journal.
type IsolatedWorkspace interface {
	Root() string
	PrepareBackend(parent sandbox.Backend) (sandbox.Backend, func() error, error)
	Settle(ctx context.Context) ([]WorkspaceChange, error)
	Close() error
}

type isolatorKey struct{}

func WithIsolator(ctx context.Context, isolator Isolator) context.Context {
	if ctx == nil || isolator == nil {
		return ctx
	}
	return context.WithValue(ctx, isolatorKey{}, isolator)
}

func IsolatorFrom(ctx context.Context) Isolator {
	if ctx == nil {
		return nil
	}
	isolator, _ := ctx.Value(isolatorKey{}).(Isolator)
	return isolator
}
