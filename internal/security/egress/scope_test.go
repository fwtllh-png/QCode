package egress_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/egress"
)

func TestCallScopePermissions(t *testing.T) {
	gate := &egress.Gate{Enforce: true, UseCallScope: true}
	target := egress.Target{Host: "203.0.113.10", Protocol: "https", Methods: []string{"GET"}}
	gate.AllowTarget(target)
	assertAccess := func(ctx context.Context, target egress.Target, allowed bool) {
		t.Helper()
		_, err := gate.Authorize(ctx, target, "test")
		if (allowed && err != nil) || (!allowed && !errors.Is(err, egress.ErrDenied)) {
			t.Fatalf("allowed=%t target=%+v err=%v", allowed, target, err)
		}
	}
	t.Run("scope overrides fixed grants and closes", func(t *testing.T) {
		ctx, closeScope := egress.WithScope(t.Context())
		defer closeScope()
		assertAccess(ctx, target, false)
		egress.AllowInScope(ctx, target)
		assertAccess(ctx, target, true)
		post := target
		post.Methods = []string{"POST"}
		assertAccess(ctx, post, false)
		closeScope()
		egress.AllowInScope(ctx, target)
		assertAccess(ctx, target, false)
		assertAccess(t.Context(), target, true)
	})
	t.Run("independent concurrent calls", func(t *testing.T) {
		first, closeFirst := egress.WithScope(t.Context())
		defer closeFirst()
		second, closeSecond := egress.WithScope(t.Context())
		defer closeSecond()
		egress.AllowInScope(first, target)
		assertAccess(second, target, false)
		egress.AllowInScope(second, target)
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			closeFirst()
			egress.AllowInScope(first, target)
		}()
		go func() {
			defer wait.Done()
			_, err := gate.Authorize(second, target, "test")
			if err != nil {
				t.Errorf("other call revoked permission: %v", err)
			}
		}()
		wait.Wait()
		assertAccess(first, target, false)
		assertAccess(second, target, true)
	})
	t.Run("cancellation and nested scope", func(t *testing.T) {
		parent, cancel := context.WithCancel(t.Context())
		ctx, closeScope := egress.WithScope(parent)
		defer closeScope()
		egress.AllowInScope(ctx, target)
		nested, closeNested := egress.WithScope(ctx)
		defer closeNested()
		assertAccess(nested, target, false)
		cancel()
		assertAccess(ctx, target, false)
	})
	t.Run("private permission and malformed target", func(t *testing.T) {
		ctx, closeScope := egress.WithScope(t.Context())
		defer closeScope()
		private := target
		private.Host = "127.0.0.1"
		egress.AllowInScope(ctx, private)
		assertAccess(ctx, private, false)
		private.AllowPrivate = true
		egress.AllowInScope(ctx, private)
		assertAccess(ctx, private, true)
		invalid := target
		invalid.Host = "host/path"
		egress.AllowInScope(ctx, invalid)
		assertAccess(ctx, invalid, false)
	})
	t.Run("provider and process gates ignore web scope", func(t *testing.T) {
		other := &egress.Gate{Enforce: true}
		other.AllowTarget(target)
		ctx, closeScope := egress.WithScope(t.Context())
		closeScope()
		if _, err := other.Authorize(ctx, target, "provider"); err != nil {
			t.Fatal(err)
		}
	})
}
