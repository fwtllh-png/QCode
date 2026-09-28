package admission

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
)

var (
	ErrTokenBudget = errors.New("token budget exhausted")
	ErrCostBudget  = errors.New("cost budget exhausted")
	ErrDepthBudget = errors.New("depth budget exceeded")
	ErrConcurrency = errors.New("concurrency budget exhausted")
)

type Limits struct {
	MaxTokens      uint64
	MaxCostUSD     float64
	MaxDepth       int
	MaxConcurrency int
}

type Snapshot struct {
	SpentTokens  uint64
	SpentCostUSD float64
	InFlight     int32
	MaxDepthSeen int32
}

// Governor bounds aggregate child-agent spend and concurrent turns.
type Governor struct {
	mu     sync.Mutex
	limits Limits
	spentT uint64
	spentC float64
	flight atomic.Int32
	depth  atomic.Int32
}

func NewGovernor(limits Limits) *Governor {
	return &Governor{limits: limits}
}

// addSaturating sums token counters without wrapping. Overflowing spend reads
// as the maximum, so a malformed or hostile receipt can never budget-bypass by
// wrapping the accumulator back near zero.
func addSaturating(left, right uint64) uint64 {
	total := left + right
	if total < left {
		return math.MaxUint64
	}
	return total
}

func (g *Governor) Limits() Limits { return g.limits }

func (g *Governor) Snapshot() Snapshot {
	return Snapshot{
		SpentTokens:  atomic.LoadUint64(&g.spentT),
		SpentCostUSD: g.cost(),
		InFlight:     g.flight.Load(),
		MaxDepthSeen: g.depth.Load(),
	}
}

func (g *Governor) cost() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.spentC
}

type Lease struct{}

func (g *Governor) Admit(depth int, tokens uint64, cost float64) (Lease, error) {
	if g.limits.MaxDepth > 0 && depth > g.limits.MaxDepth {
		return Lease{}, ErrDepthBudget
	}
	for {
		current := g.flight.Load()
		if g.limits.MaxConcurrency > 0 &&
			int(current) >= g.limits.MaxConcurrency {
			return Lease{}, ErrConcurrency
		}
		if g.flight.CompareAndSwap(current, current+1) {
			break
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	nextTokens := addSaturating(g.spentT, tokens)
	if g.limits.MaxTokens > 0 && nextTokens > g.limits.MaxTokens {
		g.flight.Add(-1)
		return Lease{}, ErrTokenBudget
	}
	if g.limits.MaxCostUSD > 0 && g.spentC+cost > g.limits.MaxCostUSD {
		g.flight.Add(-1)
		return Lease{}, ErrCostBudget
	}
	g.spentT = nextTokens
	g.spentC += cost
	if int32(depth) > g.depth.Load() {
		g.depth.Store(int32(depth))
	}
	return Lease{}, nil
}

func (g *Governor) Release(Lease) {
	g.flight.Add(-1)
}

// Record accounts for spend that already happened and reports any exceeded
// limit so callers can stop admitting more work.
func (g *Governor) Record(tokens uint64, cost float64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.spentT = addSaturating(g.spentT, tokens)
	g.spentC += cost
	if g.limits.MaxTokens > 0 && g.spentT > g.limits.MaxTokens {
		return ErrTokenBudget
	}
	if g.limits.MaxCostUSD > 0 && g.spentC > g.limits.MaxCostUSD {
		return ErrCostBudget
	}
	return nil
}
