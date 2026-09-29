package ratelimit

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type routeState struct {
	nextRequest time.Time
	lastRequest time.Time
	penalty     time.Duration
	commits     []tokenCommit
	tokenLimit  uint64
	remaining   *uint64
	remainingAt time.Time
	resetAt     time.Time
}

// Controller coordinates static pacing and Provider-driven cooldowns for all
// requests sharing a Provider route.
type Controller struct {
	mu     sync.Mutex
	routes map[string]*routeState
}

func Key(route model.ReadyRoute) string {
	credential := route.Credential()
	return strings.Join([]string{
		route.ProviderID(), route.Endpoint(),
		credential.Kind, credential.Name, route.Model().ID,
	}, "\x00")
}

func (c *Controller) Wait(
	ctx context.Context,
	key string,
	requestsPerSecond float64,
) (time.Duration, error) {
	var waited time.Duration
	for {
		c.mu.Lock()
		state := c.state(key)
		interval := max(requestInterval(requestsPerSecond), state.penalty)
		now := time.Now()
		if !now.Before(state.nextRequest) {
			state.nextRequest = now.Add(interval)
			state.lastRequest = now
			c.mu.Unlock()
			return waited, nil
		}
		delay := state.nextRequest.Sub(now)
		c.mu.Unlock()
		started := time.Now()
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
			waited += time.Since(started)
		case <-ctx.Done():
			timer.Stop()
			waited += time.Since(started)
			return waited, ctx.Err()
		}
	}
}

func (c *Controller) Remaining(key string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.routes == nil {
		return 0
	}
	state := c.routes[key]
	if state == nil {
		return 0
	}
	remaining := time.Until(state.nextRequest)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (c *Controller) Observe(
	key string,
	requestsPerSecond float64,
	status int,
	header http.Header,
	err error,
) error {
	now := time.Now()
	metadata := Metadata(header, now)
	if failure, ok := errors.AsType[*provider.Failure](err); ok &&
		failure.Code != provider.FailureRateLimit &&
		status == http.StatusTooManyRequests {
		return attach(err, metadata, status)
	}
	c.mu.Lock()
	state := c.state(key)
	applyTokenHeaders(state, header, now)
	switch {
	case status == http.StatusTooManyRequests:
		base := max(
			time.Since(state.lastRequest),
			requestInterval(requestsPerSecond),
		)
		delay := retryDelay(metadata)
		if delay <= 0 {
			if base <= 0 {
				base = time.Nanosecond
			}
			delay = base
			if state.penalty > 0 {
				delay = max(delay, saturatingDouble(state.penalty))
			}
		}
		state.penalty = delay
		state.nextRequest = later(state.nextRequest, time.Now().Add(delay))
		// Local pacing lives only in route state. RetryAfterMS is reserved
		// for provider headers and must not impersonate a Retry-After.
	case status == http.StatusSwitchingProtocols ||
		status >= 200 && status < 300:
		if delay := retryDelay(metadata); delay > 0 {
			state.penalty = delay
			state.nextRequest = later(state.nextRequest, time.Now().Add(delay))
		} else {
			state.penalty = 0
			state.nextRequest = earlier(
				state.nextRequest,
				time.Now().Add(requestInterval(requestsPerSecond)),
			)
			if requestsPerSecond <= 0 && !hasTokenAdmissionState(state) {
				delete(c.routes, key)
			}
		}
	}
	c.mu.Unlock()
	return attach(err, metadata, status)
}

// Metadata normalizes standard and legacy Provider rate-limit headers.
func Metadata(header http.Header, now time.Time) *protocol.RateLimitMetadata {
	retryDelay, hasRetryAfter := retryAfter(header.Get("Retry-After"), now)
	metadata := &protocol.RateLimitMetadata{
		Limit:     firstHeader(header, "RateLimit-Limit", "X-RateLimit-Limit"),
		Remaining: firstHeader(header, "RateLimit-Remaining", "X-RateLimit-Remaining"),
		Reset:     firstHeader(header, "RateLimit-Reset", "X-RateLimit-Reset"),
	}
	if !hasRetryAfter && exhausted(metadata.Remaining) {
		retryDelay, hasRetryAfter = resetDelay(header, now)
	}
	if hasRetryAfter {
		metadata.RetryAfterMS = uint64(retryDelay / time.Millisecond)
	}
	if metadata.Limit == "" && metadata.Remaining == "" &&
		metadata.Reset == "" && metadata.RetryAfterMS == 0 {
		return nil
	}
	return metadata
}

func hasTokenAdmissionState(state *routeState) bool {
	return state.tokenLimit > 0 ||
		state.remaining != nil ||
		len(state.commits) > 0 ||
		!state.resetAt.IsZero()
}

func (c *Controller) state(key string) *routeState {
	if c.routes == nil {
		c.routes = make(map[string]*routeState)
	}
	state := c.routes[key]
	if state == nil {
		state = &routeState{lastRequest: time.Now()}
		c.routes[key] = state
	}
	return state
}

func requestInterval(requestsPerSecond float64) time.Duration {
	if requestsPerSecond <= 0 {
		return 0
	}
	return time.Duration(float64(time.Second) / requestsPerSecond)
}

func retryDelay(metadata *protocol.RateLimitMetadata) time.Duration {
	if metadata == nil || metadata.RetryAfterMS == 0 {
		return 0
	}
	return time.Duration(metadata.RetryAfterMS) * time.Millisecond
}

func saturatingDouble(value time.Duration) time.Duration {
	if value > time.Duration(1<<63-1)/2 {
		return time.Duration(1<<63 - 1)
	}
	return value * 2
}

func later(left, right time.Time) time.Time {
	if left.After(right) {
		return left
	}
	return right
}

func earlier(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}

func attach(
	err error,
	metadata *protocol.RateLimitMetadata,
	status int,
) error {
	if err == nil || metadata == nil {
		return err
	}
	problem, hasProblem := errors.AsType[*protocol.Problem](err)
	if hasProblem {
		problem.RateLimit = metadata
	}
	if failure, ok := errors.AsType[*provider.Failure](err); ok {
		failure.RetryAfterMS = metadata.RetryAfterMS
	} else if hasProblem && status == http.StatusTooManyRequests {
		replacement := protocol.NewProblem(
			problem.Code,
			problem.Message,
			problem.Retryable,
			&provider.Failure{
				Code: provider.FailureRateLimit, Message: problem.Message,
				HTTPStatus: status, RetryAfterMS: metadata.RetryAfterMS,
			},
		)
		replacement.HTTPStatus, replacement.RateLimit =
			problem.HTTPStatus, metadata
		replacement.Details, replacement.Fault =
			problem.Details, problem.Fault
		return replacement
	}
	return err
}

func firstHeader(header http.Header, names ...string) string {
	for _, name := range names {
		if value := header.Get(name); value != "" {
			return value
		}
	}
	return ""
}

func exhausted(value string) bool {
	remaining, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	return err == nil && remaining <= 0
}

func resetDelay(header http.Header, now time.Time) (time.Duration, bool) {
	if value := strings.TrimSpace(header.Get("RateLimit-Reset")); value != "" {
		return numericResetDelay(value, now, false)
	}
	if value := strings.TrimSpace(header.Get("X-RateLimit-Reset")); value != "" {
		return numericResetDelay(value, now, true)
	}
	return 0, false
}

func numericResetDelay(
	value string,
	now time.Time,
	epochAllowed bool,
) (time.Duration, bool) {
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || number < 0 {
		return 0, false
	}
	if !epochAllowed {
		return time.Duration(number * float64(time.Second)), true
	}
	// X-RateLimit-Reset is sent as delta seconds, epoch seconds, or epoch
	// milliseconds depending on the provider. The unit is the reading that
	// lands nearest to now; a reset already passed means no wait.
	nowSeconds := float64(now.UnixNano()) / float64(time.Second)
	delay := number
	for _, candidate := range []float64{number - nowSeconds, number/1000 - nowSeconds} {
		if math.Abs(candidate) < math.Abs(delay) {
			delay = candidate
		}
	}
	return time.Duration(max(delay, 0) * float64(time.Second)), true
}

func retryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(at.Sub(now), 0), true
}
