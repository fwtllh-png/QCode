package egress

import (
	"context"
	"errors"

	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// Backend composes a policy-bound sandbox backend with the Workspace proxy it
// executes through. Each capability is an explicit field set at construction;
// callers reach them with a direct interface check, never by unwrapping.
type Backend struct {
	backend  sandbox.Backend
	policy   sandbox.Policy
	sessions ProcessSessionOpener
	close    func() error
}

// NewSessionBackend composes a backend that was not built by
// NewManagedBackend, such as a child worktree sandbox, with the parent
// Workspace proxy's Session allocator. Closing it closes only the backend.
func NewSessionBackend(
	backend sandbox.Backend,
	sessions ProcessSessionOpener,
) (*Backend, error) {
	if sessions == nil {
		return nil, errors.New("process session allocator is required")
	}
	return compose(backend, sessions, func() error {
		return sandbox.CloseBackend(backend)
	})
}

func compose(
	backend sandbox.Backend,
	sessions ProcessSessionOpener,
	close func() error,
) (*Backend, error) {
	if backend == nil {
		return nil, errors.New("sandbox backend is required")
	}
	policy, _ := sandbox.BackendPolicy(backend)
	return &Backend{
		backend: backend, policy: policy,
		sessions: sessions, close: close,
	}, nil
}

func (b *Backend) Capability() sandbox.Capability {
	return b.backend.Capability()
}

func (b *Backend) Prepare(ctx context.Context, command sandbox.Command) (sandbox.Command, error) {
	return b.backend.Prepare(ctx, command)
}

func (b *Backend) Policy() sandbox.Policy { return b.policy }

func (b *Backend) OpenProcessSession(targets []Target) (ProcessSession, error) {
	if b == nil || b.sessions == nil {
		return nil, ErrProcessSessionUnsupported
	}
	return b.sessions.OpenProcessSession(targets)
}

func (b *Backend) Close() error {
	if b == nil || b.close == nil {
		return nil
	}
	return b.close()
}
