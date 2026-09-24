package usage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// Scope names what a Rollup covers. An empty field means "not narrowed to one",
// so a session rollup has SessionID set and the other two empty.
type Scope struct {
	SessionID string
	ThreadID  protocol.ThreadID
	TurnID    protocol.TurnID
}

// Rollup is one scope's totals, folded from the per-model aggregate rows.
//
// It keeps PricedCalls and UnpricedCalls for the same reason Aggregate does:
// CostMicrounits totals only the calls whose model had a price, so one unpriced
// call anywhere in the scope makes it a floor rather than the amount. Read it
// through Cost, which says which of the two it is.
type Rollup struct {
	Scope Scope
	// Activity is absent for billing filters that cannot scope execution events.
	Activity *Activity
	// Turns counts distinct turns, so a scope's spend can be divided by the work
	// it paid for rather than by how many provider calls that work needed.
	Turns uint64
	Calls uint64

	InputTokens     uint64
	OutputTokens    uint64
	ReasoningTokens uint64
	CachedTokens    uint64

	CostMicrounits uint64
	PricedCalls    uint64
	UnpricedCalls  uint64

	First time.Time
	Last  time.Time
}

// Empty reports that nothing in this scope has been billed yet. A caller should
// say so rather than print a row of zeros, which reads as "this was free".
func (r Rollup) Empty() bool {
	return r.Calls == 0
}

// TotalTokens is input plus output. Cached tokens are part of the input and
// reasoning tokens are part of the output (see provider.Usage), so adding those
// two would count them twice.
func (r Rollup) TotalTokens() uint64 {
	return r.InputTokens + r.OutputTokens
}

// CostKnown reports that every call in this scope had a price, which is what
// makes CostMicrounits the amount rather than a lower bound.
func (r Rollup) CostKnown() bool {
	return r.PricedCalls > 0 && r.UnpricedCalls == 0
}

// CachedShare is the fraction of input tokens that came from the provider's
// prompt cache. It is the number that says whether caching is earning anything.
func (r Rollup) CachedShare() float64 {
	if r.InputTokens == 0 {
		return 0
	}
	return float64(r.CachedTokens) / float64(r.InputTokens)
}

// Cost renders this scope's spend without letting an unknown price read as free.
func (r Rollup) Cost() string {
	return FormatCost(r.CostMicrounits, r.PricedCalls, r.UnpricedCalls)
}

// QueryRollup folds every matching aggregate row into one total for the scope
// the filter names.
func (r *Repository) QueryRollup(ctx context.Context, filter Query) (Rollup, error) {
	// Pagination applies only to detail rows. A scope total must include every
	// matching call, even when its accompanying detail response is bounded.
	filter.Limit = 0
	aggregates, err := r.QueryAggregates(ctx, filter)
	if err != nil {
		return Rollup{}, err
	}
	rollup := Fold(Scope{
		SessionID: filter.SessionID, ThreadID: filter.ThreadID, TurnID: filter.TurnID,
	}, aggregates)
	rollup.Activity, err = r.queryActivity(ctx, filter)
	return rollup, err
}

// Fold sums aggregate rows into one rollup.
//
// The folding happens here rather than in a second SQL statement so that every
// surface reads the same numbers as the per-model rows it can also show. Two
// queries would drift apart, and a total that disagrees with the rows under it
// is worse than no total.
func Fold(scope Scope, aggregates []Aggregate) Rollup {
	rollup := Rollup{Scope: scope}
	turns := make(map[protocol.TurnID]struct{}, len(aggregates))
	for _, aggregate := range aggregates {
		if aggregate.TurnID != "" {
			turns[aggregate.TurnID] = struct{}{}
		}
		rollup.Calls += aggregate.Calls
		rollup.InputTokens += aggregate.InputTokens
		rollup.OutputTokens += aggregate.OutputTokens
		rollup.ReasoningTokens += aggregate.ReasoningTokens
		rollup.CachedTokens += aggregate.CachedTokens
		rollup.CostMicrounits += aggregate.CostMicrounits
		rollup.PricedCalls += aggregate.PricedCalls
		rollup.UnpricedCalls += aggregate.UnpricedCalls
		if rollup.First.IsZero() || aggregate.FirstAt.Before(rollup.First) {
			rollup.First = aggregate.FirstAt
		}
		if aggregate.LastAt.After(rollup.Last) {
			rollup.Last = aggregate.LastAt
		}
	}
	rollup.Turns = uint64(len(turns))
	return rollup
}

// FormatCost renders a cost that may be wholly or partly unpriced.
//
// The three answers are deliberately different strings. A priced zero is
// "$0.00" because a free model with a known price really did cost nothing. An
// unpriced call is "unknown" because nobody knows what it cost. A mix is a floor
// marked with a trailing plus. Rendering the second as the first is the mistake
// this function exists to prevent, which is why it lives next to the counts that
// make the distinction possible rather than in each surface that prints money.
//
// Amounts are USD: the wiring refuses any other currency for a known price, so
// the symbol is a fact rather than an assumption.
func FormatCost(microunits, priced, unpriced uint64) string {
	if priced == 0 {
		if unpriced == 0 {
			return "n/a"
		}
		return "unknown"
	}
	amount := formatAmount(microunits)
	if unpriced == 0 {
		return amount
	}
	calls := "calls"
	if unpriced == 1 {
		calls = "call"
	}
	return fmt.Sprintf("%s+ (%d %s unpriced)", amount, unpriced, calls)
}

// formatAmount prints microunits as dollars, exactly.
//
// The arithmetic is integer because a microunit is the unit the amount is stored
// in: dividing into a float and rounding to a fixed width would quietly report
// $0.00015 as $0.0001, and a cost that reads lower than it is defeats the point
// of reporting it. Trailing zeros come off down to cents, so a session total of a
// dollar and a half reads as $1.50 rather than $1.500000.
func formatAmount(microunits uint64) string {
	text := fmt.Sprintf("%d.%06d", microunits/1_000_000, microunits%1_000_000)
	decimals := func() int { return len(text) - strings.IndexByte(text, '.') - 1 }
	for decimals() > 2 && strings.HasSuffix(text, "0") {
		text = strings.TrimSuffix(text, "0")
	}
	return "$" + text
}
