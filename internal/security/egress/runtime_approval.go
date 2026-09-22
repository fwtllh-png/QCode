package egress

import (
	"context"
	"errors"
	"sync"
)

// RuntimeApprover decides whether an ungranted target may be added to the
// current execution before the proxy connects. A nil error grants this
// execution only. The approver must not dial the target.
type RuntimeApprover func(ctx context.Context, target Target) error

type runtimeApproverKey struct{}

func WithRuntimeApprover(ctx context.Context, approver RuntimeApprover) context.Context {
	if ctx == nil || approver == nil {
		return ctx
	}
	return context.WithValue(ctx, runtimeApproverKey{}, approver)
}

func RuntimeApproverFrom(ctx context.Context) RuntimeApprover {
	if ctx == nil {
		return nil
	}
	approver, _ := ctx.Value(runtimeApproverKey{}).(RuntimeApprover)
	return approver
}

func (g *Gate) SetRuntimeApprover(approver RuntimeApprover) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.approver = approver
	g.mu.Unlock()
}

func (g *Gate) runtimeApprover(ctx context.Context) RuntimeApprover {
	if approver := RuntimeApproverFrom(ctx); approver != nil {
		return approver
	}
	if g == nil {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.approver
}

type askWait struct {
	done chan struct{}
	err  error
}

type askState struct {
	mu      sync.Mutex
	pending map[string]*askWait
	decided map[string]error
}

func (s *askState) ask(ctx context.Context, request Target, approver RuntimeApprover) error {
	if s == nil {
		return deniedTarget(request, reasonTargetNotGranted)
	}
	origin := key(request)
	s.mu.Lock()
	if err, ok := s.decided[origin]; ok {
		s.mu.Unlock()
		return err
	}
	if wait, ok := s.pending[origin]; ok {
		s.mu.Unlock()
		return waitAsk(ctx, wait)
	}
	wait := &askWait{done: make(chan struct{})}
	if s.pending == nil {
		s.pending = map[string]*askWait{}
	}
	s.pending[origin] = wait
	s.mu.Unlock()

	var err error
	if approver == nil {
		err = deniedTarget(request, reasonTargetNotGranted)
	} else {
		err = approver(ctx, request)
	}

	s.mu.Lock()
	if s.decided == nil {
		s.decided = map[string]error{}
	}
	s.decided[origin] = err
	delete(s.pending, origin)
	wait.err = err
	close(wait.done)
	s.mu.Unlock()
	return err
}

func waitAsk(ctx context.Context, wait *askWait) error {
	if wait == nil {
		return deniedTarget(Target{}, reasonTargetNotGranted)
	}
	select {
	case <-wait.done:
		return wait.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

var errNoRuntimeApprover = errors.New("runtime approver is not bound")

func (g *Gate) discover(ctx context.Context, request Target) error {
	if g == nil {
		return errNoRuntimeApprover
	}
	approver := g.runtimeApprover(ctx)
	if approver == nil {
		return errNoRuntimeApprover
	}
	if g.UseCallScope {
		scope, _ := ctx.Value(scopeKey{}).(*callScope)
		if scope == nil {
			return deniedTarget(request, reasonTargetNotGranted)
		}
		return scope.asks.ask(ctx, request, approver)
	}
	return g.asks.ask(ctx, request, approver)
}

func settledDenied(request Target, err error) *DeniedError {
	var denied *DeniedError
	if errors.As(err, &denied) && denied != nil {
		clone := *denied
		clone.ApprovalSettled = true
		if clone.Host == "" {
			clone.Host = request.Host
			clone.Protocol = request.Protocol
			clone.Port = request.Port
			clone.Method = firstMethod(request.Methods)
		}
		return &clone
	}
	denied = deniedTarget(request, reasonTargetNotGranted)
	denied.ApprovalSettled = true
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		denied.Reason = "approval wait ended before connect"
	}
	return denied
}
