package ratelimit

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	StatusAdmit  = "admit"
	StatusWait   = "wait"
	StatusRefuse = "refuse"

	SourceUnknown  = "unknown"
	SourceOperator = "operator_config"
	SourceHeader   = "provider_header"

	ReasonUnknown           = "throughput_unknown"
	ReasonAdmitted          = "admitted"
	ReasonWaitForWindow     = "wait_for_window"
	ReasonExceedsBurst      = "exceeds_route_burst"
	ReasonRemainingTooLow   = "remaining_below_required"
	ReasonWaitExceedsBudget = "wait_exceeds_budget"

	// TokenWindow is the rolling period implied by tokens_per_minute.
	TokenWindow = time.Minute
)

type Decision struct {
	Status    string
	Wait      time.Duration
	Required  uint64
	Available uint64
	Limit     uint64
	Reason    string
	Source    string
}

type tokenCommit struct {
	tokens uint64
	at     time.Time
}

func (c *Controller) Decide(
	key string,
	required uint64,
	operatorLimit uint64,
	now time.Time,
) Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.decideLocked(key, required, operatorLimit, now)
}

func (c *Controller) TryReserve(
	key string,
	required uint64,
	operatorLimit uint64,
	now time.Time,
) Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	decision := c.decideLocked(key, required, operatorLimit, now)
	if decision.Status == StatusAdmit && decision.Source != SourceUnknown {
		c.reserveLocked(key, required, now)
	}
	return decision
}

func (c *Controller) Reserve(key string, tokens uint64, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reserveLocked(key, tokens, now)
}

func (c *Controller) decideLocked(
	key string,
	required uint64,
	operatorLimit uint64,
	now time.Time,
) Decision {
	state := c.routes[key]
	limit, source := resolveTokenLimit(operatorLimit, state)
	available, known := tokenAvailable(state, limit, now)
	if known && source == SourceUnknown {
		source = SourceHeader
	}
	decision := Decision{
		Required: required, Available: available, Limit: limit, Source: source,
	}
	if !known {
		decision.Status = StatusAdmit
		decision.Reason = ReasonUnknown
		decision.Source = SourceUnknown
		return decision
	}
	if limit > 0 && required > limit {
		decision.Status = StatusRefuse
		decision.Reason = ReasonExceedsBurst
		return decision
	}
	if required <= available {
		decision.Status = StatusAdmit
		decision.Reason = ReasonAdmitted
		return decision
	}
	wait := tokenWait(state, required, limit, now)
	decision.Status = StatusWait
	decision.Wait = wait
	decision.Reason = ReasonWaitForWindow
	if wait <= 0 {
		decision.Status = StatusRefuse
		decision.Reason = ReasonRemainingTooLow
	}
	return decision
}

func (c *Controller) reserveLocked(key string, tokens uint64, now time.Time) {
	if tokens == 0 {
		return
	}
	state := c.state(key)
	state.commits = pruneCommits(state.commits, now)
	state.commits = append(state.commits, tokenCommit{tokens: tokens, at: now})
	if state.remaining != nil {
		if *state.remaining > tokens {
			remaining := *state.remaining - tokens
			state.remaining = &remaining
		} else {
			zero := uint64(0)
			state.remaining = &zero
		}
	}
}

func resolveTokenLimit(operatorLimit uint64, state *routeState) (uint64, string) {
	if operatorLimit > 0 {
		return operatorLimit, SourceOperator
	}
	if state != nil && state.tokenLimit > 0 {
		return state.tokenLimit, SourceHeader
	}
	return 0, SourceUnknown
}

func tokenAvailable(state *routeState, limit uint64, now time.Time) (uint64, bool) {
	if state != nil && state.remaining != nil {
		available := *state.remaining
		if limit > 0 {
			used := committedTokens(state, now)
			if used < limit {
				rolling := limit - used
				if rolling < available {
					available = rolling
				}
			} else {
				available = 0
			}
		}
		return available, true
	}
	if limit == 0 {
		return 0, false
	}
	used := committedTokens(state, now)
	if used >= limit {
		return 0, true
	}
	return limit - used, true
}

func committedTokens(state *routeState, now time.Time) uint64 {
	if state == nil {
		return 0
	}
	state.commits = pruneCommits(state.commits, now)
	var used uint64
	for _, commit := range state.commits {
		used += commit.tokens
	}
	return used
}

func pruneCommits(commits []tokenCommit, now time.Time) []tokenCommit {
	cutoff := now.Add(-TokenWindow)
	kept := commits[:0]
	for _, commit := range commits {
		if commit.at.After(cutoff) {
			kept = append(kept, commit)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func tokenWait(
	state *routeState,
	required uint64,
	limit uint64,
	now time.Time,
) time.Duration {
	if state != nil && state.resetAt.After(now) {
		return state.resetAt.Sub(now)
	}
	if state == nil || limit == 0 || required == 0 {
		return TokenWindow
	}
	cutoff := now.Add(-TokenWindow)
	used := committedTokens(state, now)
	if used+required <= limit {
		return 0
	}
	need := used + required - limit
	var released uint64
	earliest := time.Time{}
	for _, commit := range state.commits {
		if commit.at.Before(cutoff) {
			continue
		}
		released += commit.tokens
		expire := commit.at.Add(TokenWindow)
		if earliest.IsZero() || expire.Before(earliest) {
			earliest = expire
		}
		if released >= need {
			return max(expire.Sub(now), 0)
		}
	}
	if earliest.After(now) {
		return earliest.Sub(now)
	}
	return TokenWindow
}

func applyTokenHeaders(state *routeState, header http.Header, now time.Time) {
	if state == nil {
		return
	}
	if limit, ok := parseTokenCount(header, tokenLimitHeaders...); ok {
		state.tokenLimit = limit
	}
	if remaining, ok := parseTokenCount(header, tokenRemainingHeaders...); ok {
		value := remaining
		state.remaining = &value
		state.remainingAt = now
	}
	if delay, ok := tokenResetDelay(header, now); ok {
		state.resetAt = now.Add(delay)
	}
}

func parseTokenCount(header http.Header, names ...string) (uint64, bool) {
	value := firstHeader(header, names...)
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseUint(strings.TrimSpace(strings.Split(value, ";")[0]), 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func tokenResetDelay(header http.Header, now time.Time) (time.Duration, bool) {
	if value := firstHeader(header, tokenResetHeaders...); value != "" {
		if delay, ok := numericResetDelay(value, now, true); ok {
			return delay, true
		}
	}
	return 0, false
}

var (
	tokenLimitHeaders = []string{
		"X-RateLimit-Limit-Tokens",
	}
	tokenRemainingHeaders = []string{
		"X-RateLimit-Remaining-Tokens",
	}
	tokenResetHeaders = []string{
		"X-RateLimit-Reset-Tokens",
	}
)
