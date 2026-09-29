package egress_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type bareBackend struct{ closed *bool }

func (bareBackend) Capability() sandbox.Capability { return sandbox.Capability{Backend: "bare"} }

func (bareBackend) Prepare(_ context.Context, command sandbox.Command) (sandbox.Command, error) {
	return command, nil
}

func (b bareBackend) Close() error {
	if b.closed != nil {
		*b.closed = true
	}
	return nil
}

type fixedOpener struct{ calls int }

func (o *fixedOpener) OpenProcessSession([]egress.Target) (egress.ProcessSession, error) {
	o.calls++
	return nil, errors.New("fixture session")
}

type opaqueWrapper struct{ sandbox.Backend }

func TestSessionBackendExposesCapabilitiesWithoutUnwrapping(t *testing.T) {
	closed := false
	bound, err := sandbox.BindPolicy(bareBackend{closed: &closed}, sandbox.Options{
		WorkspaceRoot: t.TempDir(), SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	opener := &fixedOpener{}
	composed, err := egress.NewSessionBackend(bound, opener)
	if err != nil {
		t.Fatal(err)
	}
	policy, ok := sandbox.BackendPolicy(composed)
	want, _ := sandbox.BackendPolicy(bound)
	if !ok || policy.ID != want.ID {
		t.Fatalf("composed policy = %q, want %q", policy.ID, want.ID)
	}
	found, ok := egress.LookupProcessSessionOpener(composed)
	if !ok {
		t.Fatal("composed backend lost its session allocator")
	}
	_, _ = found.OpenProcessSession(nil)
	if opener.calls != 1 {
		t.Fatalf("session allocator calls = %d", opener.calls)
	}
	if err := composed.Close(); err != nil || !closed {
		t.Fatalf("close err=%v closed=%v", err, closed)
	}

	wrapped := opaqueWrapper{Backend: composed}
	if _, ok := sandbox.BackendPolicy(wrapped); ok {
		t.Fatal("BackendPolicy unwrapped an opaque wrapper")
	}
	if _, ok := egress.LookupProcessSessionOpener(wrapped); ok {
		t.Fatal("LookupProcessSessionOpener unwrapped an opaque wrapper")
	}
	if _, err := egress.NewSessionBackend(bound, nil); err == nil {
		t.Fatal("session backend without an allocator was composed")
	}
}
