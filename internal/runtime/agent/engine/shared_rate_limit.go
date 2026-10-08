package engine

import (
	"context"
	"sync"
	"time"
)

// SharedRateLimit is the session-wide provider sample gate. Parent and child
// engines share one instance so concurrent samples stay within the
// operator-declared provider concurrency contract (execution.max_concurrent,
// the same ceiling the provider HTTP client enforces) and honor one shared
// Retry-After cooldown instead of each hammering the provider with a private
// retry pot.
type SharedRateLimit struct {
	init sync.Once
	mu   sync.Mutex

	limit          int
	token          chan struct{}
	retries        uint32
	waited         time.Duration
	cooldownUntil  time.Time
	foreground     int
	changed        chan struct{}
	background     map[uint64]context.CancelFunc
	nextBackground uint64
}

// NewSharedRateLimit derives the gate capacity from the operator-declared
// provider concurrency; values below one keep single-flight.
func NewSharedRateLimit(concurrency int) *SharedRateLimit {
	limiter := &SharedRateLimit{limit: max(1, concurrency)}
	limiter.ensure()
	return limiter
}

func (s *SharedRateLimit) ensure() {
	if s == nil {
		return
	}
	s.init.Do(func() {
		limit := s.limit
		if limit < 1 {
			limit = 1
		}
		s.token = make(chan struct{}, limit)
		for range limit {
			s.token <- struct{}{}
		}
	})
}

func (s *SharedRateLimit) Load() (uint32, time.Duration) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retries, s.waited
}

func (s *SharedRateLimit) Record(delay time.Duration) (uint32, time.Duration) {
	if s == nil {
		return 1, delay
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retries++
	if delay > 0 {
		s.waited += delay
		until := time.Now().Add(delay)
		if until.After(s.cooldownUntil) {
			s.cooldownUntil = until
		}
	}
	return s.retries, s.waited
}

// ObserveSuccess clears the storm pot after a provider sample completes.
func (s *SharedRateLimit) ObserveSuccess() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retries = 0
	s.waited = 0
	s.cooldownUntil = time.Time{}
}

// BeginUserTurn refreshes the wait pot for a new parent user turn. Remaining
// Retry-After is kept so Continue does not immediately re-hit a hot provider.
func (s *SharedRateLimit) BeginUserTurn() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retries = 0
	s.waited = 0
}

// Hot reports an active cooldown or a storm that has already consumed retries.
func (s *SharedRateLimit) Hot() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retries > 0 || time.Now().Before(s.cooldownUntil)
}

// Acquire serializes one provider sample for the session and waits out any
// remaining Retry-After before the caller may send. The returned release must
// run after the attempt, including on error.
func (s *SharedRateLimit) Acquire(ctx context.Context) (func(), error) {
	if s == nil {
		return func() {}, nil
	}
	s.ensure()
	s.mu.Lock()
	s.foreground++
	for _, cancel := range s.background {
		cancel()
	}
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		s.finishForeground()
		return nil, ctx.Err()
	case <-s.token:
	}
	if err := s.waitCooldown(ctx); err != nil {
		s.token <- struct{}{}
		s.finishForeground()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			s.token <- struct{}{}
			s.finishForeground()
		})
	}, nil
}

// AcquireBackground yields to both queued and active foreground work. A new
// foreground request cancels the optional sample before taking its slot.
func (s *SharedRateLimit) AcquireBackground(ctx context.Context) (context.Context, func(), error) {
	if s == nil {
		return ctx, func() {}, ctx.Err()
	}
	s.ensure()
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		s.mu.Lock()
		if s.foreground != 0 || len(s.token) == 0 {
			if s.changed == nil {
				s.changed = make(chan struct{})
			}
			changed := s.changed
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-changed:
				continue
			}
		}
		delay := time.Until(s.cooldownUntil)
		if delay > 0 {
			s.mu.Unlock()
			if err := waitRetryDelay(ctx, delay); err != nil {
				return nil, nil, err
			}
			continue
		}
		<-s.token
		backgroundCtx, cancel := context.WithCancel(ctx)
		s.nextBackground++
		id := s.nextBackground
		if s.background == nil {
			s.background = make(map[uint64]context.CancelFunc)
		}
		s.background[id] = cancel
		s.mu.Unlock()
		var once sync.Once
		return backgroundCtx, func() {
			once.Do(func() {
				cancel()
				s.mu.Lock()
				delete(s.background, id)
				s.token <- struct{}{}
				s.notifyWaiters()
				s.mu.Unlock()
			})
		}, nil
	}
}

// BeginForegroundTurn cancels optional samples without joining their cleanup.
// Provider concurrency remains enforced by Acquire at the actual call boundary.
func (s *SharedRateLimit) BeginForegroundTurn(ctx context.Context) func() {
	if s == nil || ctx.Err() != nil {
		return func() {}
	}
	s.ensure()
	s.mu.Lock()
	s.foreground++
	for _, cancel := range s.background {
		cancel()
	}
	s.mu.Unlock()

	var once sync.Once
	return func() { once.Do(s.finishForeground) }
}

func (s *SharedRateLimit) notifyWaiters() {
	if s.changed == nil {
		return
	}
	close(s.changed)
	s.changed = nil
}

func (s *SharedRateLimit) finishForeground() {
	s.mu.Lock()
	s.foreground--
	s.notifyWaiters()
	s.mu.Unlock()
}

func (s *SharedRateLimit) waitCooldown(ctx context.Context) error {
	s.mu.Lock()
	until := s.cooldownUntil
	s.mu.Unlock()
	delay := time.Until(until)
	if delay <= 0 {
		return nil
	}
	return waitRetryDelay(ctx, delay)
}

type providerSampleLease struct {
	limiter *SharedRateLimit
	release func()
}

func (e *Engine) holdProviderSample(ctx context.Context) (*providerSampleLease, error) {
	lease := &providerSampleLease{limiter: e.options.SharedRateLimit}
	if e.options.SharedRateLimit == nil {
		return lease, nil
	}
	release, err := e.options.SharedRateLimit.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	lease.release = release
	return lease, nil
}

func (l *providerSampleLease) Release() {
	if l == nil || l.release == nil {
		return
	}
	l.release()
	l.release = nil
}

func (l *providerSampleLease) NoteRateLimit(delay time.Duration) {
	if l != nil && l.limiter != nil {
		l.limiter.Record(delay)
	}
	l.Release()
}

func (l *providerSampleLease) Succeeded() {
	if l != nil && l.limiter != nil {
		l.limiter.ObserveSuccess()
	}
	l.Release()
}
