package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type HealthState string

const (
	HealthStarting HealthState = "starting"
	HealthHealthy  HealthState = "healthy"
	HealthDegraded HealthState = "degraded"
	HealthOpen     HealthState = "open"
	HealthHalfOpen HealthState = "half_open"
)

var (
	ErrServerUnavailable error = &availabilityError{
		message: "MCP server unavailable", category: ErrorCategoryUnavailable,
	}
	ErrCircuitOpen error = &availabilityError{
		message: "MCP circuit breaker is open", category: ErrorCategoryCircuitOpen,
	}
)

const (
	ErrorCategoryUnavailable = "mcp_unavailable"
	ErrorCategoryCircuitOpen = "mcp_circuit_open"
)

type availabilityError struct {
	message  string
	category string
}

func (e *availabilityError) Error() string { return e.message }

// RecoverableCategory lets the tool result boundary classify MCP availability
// failures without importing the MCP implementation. It does not authorize a retry.
func (e *availabilityError) RecoverableCategory() string { return e.category }

func ErrorCategory(err error) string {
	switch {
	case errors.Is(err, ErrCircuitOpen):
		return ErrorCategoryCircuitOpen
	case errors.Is(err, ErrServerUnavailable):
		return ErrorCategoryUnavailable
	default:
		return ""
	}
}

type HealthSnapshot struct {
	Server              string      `json:"server"`
	State               HealthState `json:"state"`
	ConsecutiveFailures int         `json:"consecutive_failures"`
	LastError           string      `json:"last_error,omitempty"`
	ChangedAt           time.Time   `json:"changed_at"`
	RetryAt             time.Time   `json:"retry_at,omitempty"`
}

type HealthChange struct {
	Previous HealthSnapshot `json:"previous"`
	Current  HealthSnapshot `json:"current"`
}

type healthTracker struct {
	mu        sync.Mutex
	snapshot  HealthSnapshot
	threshold int
	cooldown  time.Duration
	now       func() time.Time
	onChange  func(HealthChange)
}

func newHealthTracker(
	server string,
	config CircuitBreakerConfig,
	onChange func(HealthChange),
) *healthTracker {
	now := time.Now
	if config.FailureThreshold <= 0 {
		config.FailureThreshold = 3
	}
	if config.Cooldown <= 0 {
		config.Cooldown = 5 * time.Second
	}
	return &healthTracker{
		snapshot: HealthSnapshot{
			Server: server, State: HealthStarting, ChangedAt: now().UTC(),
		},
		threshold: config.FailureThreshold,
		cooldown:  config.Cooldown,
		now:       now,
		onChange:  onChange,
	}
}

func (h *healthTracker) Snapshot() HealthSnapshot {
	if h == nil {
		return HealthSnapshot{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshot
}

func (h *healthTracker) Healthy() {
	h.transition(HealthHealthy, 0, "", time.Time{})
}

func (h *healthTracker) Failure(err error) {
	if h == nil || err == nil || ignoredHealthFailure(err) {
		return
	}
	h.mu.Lock()
	failures := h.snapshot.ConsecutiveFailures + 1
	state := HealthDegraded
	var retryAt time.Time
	if failures >= h.threshold {
		state = HealthOpen
		retryAt = h.now().UTC().Add(h.cooldown)
	}
	change, changed := h.transitionLocked(state, failures, err.Error(), retryAt)
	h.mu.Unlock()
	h.emit(change, changed)
}

func (h *healthTracker) Open(err error) {
	if h == nil {
		return
	}
	message := ""
	if err != nil {
		message = err.Error()
	}
	h.mu.Lock()
	failures := h.snapshot.ConsecutiveFailures + 1
	if failures < h.threshold {
		failures = h.threshold
	}
	change, changed := h.transitionLocked(
		HealthOpen, failures, message, h.now().UTC().Add(h.cooldown),
	)
	h.mu.Unlock()
	h.emit(change, changed)
}

func (h *healthTracker) BeforeBusinessCall(
	ctx context.Context,
	probe func(context.Context) error,
) error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	switch h.snapshot.State {
	case HealthHealthy, HealthDegraded:
		h.mu.Unlock()
		return nil
	case HealthStarting:
		server := h.snapshot.Server
		h.mu.Unlock()
		return fmt.Errorf("%w: %s is starting", ErrServerUnavailable, server)
	case HealthHalfOpen:
		server := h.snapshot.Server
		h.mu.Unlock()
		return fmt.Errorf("%w: %s is probing", ErrCircuitOpen, server)
	case HealthOpen:
		if h.now().Before(h.snapshot.RetryAt) {
			snapshot := h.snapshot
			h.mu.Unlock()
			return fmt.Errorf(
				"%w: %s retry after %s",
				ErrCircuitOpen, snapshot.Server, snapshot.RetryAt.Format(time.RFC3339Nano),
			)
		}
		change, changed := h.transitionLocked(
			HealthHalfOpen,
			h.snapshot.ConsecutiveFailures,
			h.snapshot.LastError,
			h.snapshot.RetryAt,
		)
		h.mu.Unlock()
		h.emit(change, changed)
	default:
		h.mu.Unlock()
		return fmt.Errorf("%w: invalid health state", ErrServerUnavailable)
	}
	if probe == nil {
		err := errors.New("MCP half-open probe is unavailable")
		h.Open(err)
		return fmt.Errorf("%w: %v", ErrCircuitOpen, err)
	}
	if err := probe(ctx); err != nil {
		h.Open(err)
		return fmt.Errorf("%w: half-open probe failed: %v", ErrCircuitOpen, err)
	}
	h.Healthy()
	return nil
}

func (h *healthTracker) transition(
	state HealthState,
	failures int,
	lastError string,
	retryAt time.Time,
) {
	if h == nil {
		return
	}
	h.mu.Lock()
	change, changed := h.transitionLocked(state, failures, lastError, retryAt)
	h.mu.Unlock()
	h.emit(change, changed)
}

func (h *healthTracker) transitionLocked(
	state HealthState,
	failures int,
	lastError string,
	retryAt time.Time,
) (HealthChange, bool) {
	previous := h.snapshot
	next := previous
	next.State = state
	next.ConsecutiveFailures = failures
	next.LastError = lastError
	next.RetryAt = retryAt
	changed := previous.State != next.State ||
		previous.ConsecutiveFailures != next.ConsecutiveFailures ||
		previous.LastError != next.LastError ||
		!previous.RetryAt.Equal(next.RetryAt)
	if changed {
		next.ChangedAt = h.now().UTC()
		h.snapshot = next
	}
	return HealthChange{Previous: previous, Current: next}, changed
}

func (h *healthTracker) emit(change HealthChange, changed bool) {
	if changed && h.onChange != nil {
		h.onChange(change)
	}
}

func ignoredHealthFailure(err error) bool {
	return errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}
