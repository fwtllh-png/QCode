package egress

import (
	"context"
	"sync"
)

type scopeKey struct{}

// callScope owns only dynamic grants. Configured Gate grants cannot authorize a
// scoped call; Guard must approve its declared backend endpoints as resources.
type callScope struct {
	mu     sync.Mutex
	grants *Gate
	asks   askState
}

// WithScope starts an independent network authorization lifetime. Close removes
// all grants and keeps the context fail-closed, including retained child contexts.
func WithScope(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	scope := &callScope{grants: &Gate{}}
	return context.WithValue(ctx, scopeKey{}, scope), func() {
		cancel()
		scope.mu.Lock()
		scope.grants = nil
		scope.mu.Unlock()
	}
}

// AllowInScope never falls back to a shared Gate if the caller has no live scope.
func AllowInScope(ctx context.Context, target Target) {
	scope, _ := ctx.Value(scopeKey{}).(*callScope)
	if scope == nil || ctx.Err() != nil {
		return
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.grants != nil {
		scope.grants.AllowTarget(target)
	}
}

// AdoptScope copies the grants Guard approved for the current call onto g so
// they outlive the call. It serves long-lived clients such as the browser
// session, whose page traffic continues after the approving call returns.
// Without a live scope it adds nothing.
func (g *Gate) AdoptScope(ctx context.Context) {
	if g == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	scope, _ := ctx.Value(scopeKey{}).(*callScope)
	if scope == nil {
		return
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.grants == nil {
		return
	}
	scope.grants.mu.RLock()
	defer scope.grants.mu.RUnlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.allowed == nil {
		g.allowed = make(map[string]targetGrant, len(scope.grants.allowed))
	}
	for origin, adopted := range scope.grants.allowed {
		grant := g.allowed[origin]
		grant.allMethods = grant.allMethods || adopted.allMethods
		grant.allPrivate = grant.allPrivate || adopted.allPrivate
		for method, private := range adopted.methods {
			if grant.methods == nil {
				grant.methods = make(map[string]bool, len(adopted.methods))
			}
			grant.methods[method] = grant.methods[method] || private
		}
		g.allowed[origin] = grant
	}
}

func scopedPermissions(ctx context.Context, target Target) (scoped, allowed, private bool) {
	scope, _ := ctx.Value(scopeKey{}).(*callScope)
	if scope == nil {
		return false, false, false
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.grants == nil || ctx.Err() != nil {
		return true, false, false
	}
	scope.grants.mu.RLock()
	defer scope.grants.mu.RUnlock()
	grant := scope.grants.allowed[key(target)]
	allowed, private = grant.permissions(target.Methods)
	return true, allowed, private
}
