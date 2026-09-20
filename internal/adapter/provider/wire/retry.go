package wire

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"strconv"
	"syscall"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const RetryPolicyRevision = "provider-retry/v5"

type RetryPolicy struct {
	MaxRetries          int
	MaxDelay            time.Duration
	RateLimitMaxRetries int
	RateLimitMaxWait    time.Duration
	RateLimitRetries    uint32
	RateLimitWaited     time.Duration
	// SharedRateLimitRetries / SharedRateLimitWaited are the session pot.
	// They gate whether another wait is allowed. The sample Retry number still
	// comes from RateLimitRetries so turn-kernel monotonicity stays per sample.
	SharedRateLimitRetries uint32
	SharedRateLimitWaited  time.Duration
	RouteCooldown          time.Duration
	// JitterSeed decorrelates locally derived backoff across sessions. The
	// same seed and retry count stay reproducible; Provider Retry-After is
	// never jittered.
	JitterSeed string
	Now        func() time.Time
}

type RetryDecision struct {
	Attempt        int                `json:"attempt"`
	Retry          uint32             `json:"retry"`
	Code           protocol.ErrorCode `json:"code"`
	Category       string             `json:"category"`
	Failure        provider.Failure   `json:"failure"`
	EffectiveDelay time.Duration      `json:"effective_delay"`
	RetryAt        time.Time          `json:"retry_at"`
	PolicyRevision string             `json:"policy_revision"`
}

func (p RetryPolicy) Decide(
	err error,
	meaningful bool,
	retries uint32,
	contextChanged bool,
) (RetryDecision, bool) {
	failure := ClassifyFailure(err, meaningful)
	limit := max(p.MaxRetries, 0)
	eligible := false
	attempt := int(retries)
	switch failure.Code {
	case provider.FailureRateLimit:
		eligible = true
		attempt = int(p.rateLimitBudgetRetries())
		limit = p.rateLimitAttemptLimit(err, failure)
		if limit == 0 && !rateLimitHasWaitSignal(err, failure, p.RouteCooldown) {
			eligible = false
		}
	case provider.FailureServer,
		provider.FailureTransport,
		provider.FailureStreamClosed,
		provider.FailureTimeout:
		eligible = limit > 0
	case provider.FailureContextWindowExceeded:
		eligible = contextChanged && limit > 0
	case provider.FailureEmptyResponse:
		eligible = true
		limit = 1
	case provider.FailureUnknown:
		eligible = protocol.IsRetryable(err) && limit > 0
	}
	if !eligible {
		return RetryDecision{}, false
	}
	decision := protocol.DecideRecovery(
		RecoveryFault(err, failure),
		protocol.RecoveryContext{
			Owner:       protocol.FaultRetryOwnerEngine,
			Idempotent:  true,
			Progress:    meaningful,
			Attempt:     attempt,
			MaxAttempts: limit,
		},
	)
	if decision.Action != protocol.RecoveryRetry &&
		decision.Action != protocol.RecoveryWait {
		return RetryDecision{}, false
	}
	backoffRetries := retries
	if failure.Code == provider.FailureRateLimit {
		backoffRetries = p.rateLimitBudgetRetries()
	}
	delayMS := failure.RetryAfterMS
	if delayMS == 0 {
		delayMS = uint64(retryBackoff(backoffRetries, p.JitterSeed) / time.Millisecond)
	}
	needed := time.Duration(delayMS) * time.Millisecond
	if p.RouteCooldown > needed {
		needed = p.RouteCooldown
	}
	providerSpecified := failure.RetryAfterMS > 0 || p.RouteCooldown > 0
	if !providerSpecified && p.MaxDelay > 0 && needed > p.MaxDelay {
		needed = p.MaxDelay
	}
	if failure.Code == provider.FailureRateLimit &&
		p.RateLimitMaxWait > 0 &&
		p.rateLimitBudgetWaited()+needed > p.RateLimitMaxWait {
		return RetryDecision{}, false
	}
	if failure.Code != provider.FailureRateLimit &&
		p.MaxDelay > 0 && needed > p.MaxDelay {
		needed = p.MaxDelay
	}
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	return RetryDecision{
		Attempt: int(p.RateLimitRetries + retries + 1), Retry: p.RateLimitRetries + retries + 1,
		Code: protocol.CodeOf(err), Category: FailureCategory(err, failure.Code),
		Failure: failure, EffectiveDelay: needed, RetryAt: now.Add(needed),
		PolicyRevision: RetryPolicyRevision,
	}, true
}

func (p RetryPolicy) rateLimitAttemptLimit(
	err error, failure provider.Failure,
) int {
	if p.RateLimitMaxRetries > 0 {
		return p.RateLimitMaxRetries
	}
	if rateLimitHasWaitSignal(err, failure, p.RouteCooldown) {
		return 0
	}
	return max(p.MaxRetries, 0)
}

func rateLimitHasWaitSignal(
	err error, failure provider.Failure, cooldown time.Duration,
) bool {
	if failure.RetryAfterMS > 0 || cooldown > 0 {
		return true
	}
	if problem := protocol.ProblemOf(err); problem != nil &&
		problem.RateLimit != nil && problem.RateLimit.RetryAfterMS > 0 {
		return true
	}
	return false
}

func (p RetryPolicy) rateLimitBudgetRetries() uint32 {
	if p.SharedRateLimitRetries > p.RateLimitRetries {
		return p.SharedRateLimitRetries
	}
	return p.RateLimitRetries
}

func (p RetryPolicy) rateLimitBudgetWaited() time.Duration {
	if p.SharedRateLimitWaited > p.RateLimitWaited {
		return p.SharedRateLimitWaited
	}
	return p.RateLimitWaited
}

func ClassifyFailure(err error, meaningful bool) provider.Failure {
	var classified *provider.Failure
	if errors.As(err, &classified) && classified != nil {
		failure := *classified
		if failure.Message == "" {
			failure.Message = errorText(err)
		}
		return failure
	}
	code := provider.FailureUnknown
	switch {
	case errors.Is(err, context.Canceled):
		code = provider.FailureAborted
	case errors.Is(err, context.DeadlineExceeded):
		code = provider.FailureTimeout
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		code = provider.FailureTransport
	case errors.Is(err, io.ErrUnexpectedEOF):
		if meaningful {
			code = provider.FailureStreamClosed
		} else {
			code = provider.FailureTransport
		}
	case protocol.CodeOf(err) == protocol.CodeUnavailable:
		code = provider.FailureTransport
	}
	return provider.Failure{Code: code, Message: errorText(err)}
}

func FailureCategory(err error, code provider.FailureCode) string {
	switch {
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	case errors.Is(err, syscall.EPIPE):
		return "broken_pipe"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case code != "":
		return string(code)
	default:
		return "provider_unavailable"
	}
}

func RecoveryFault(err error, failure provider.Failure) error {
	problem := protocol.ProblemOf(err)
	code := protocol.CodeUnavailable
	retryable := protocol.IsRetryable(err)
	switch failure.Code {
	case provider.FailureRateLimit,
		provider.FailureServer,
		provider.FailureTransport,
		provider.FailureStreamClosed,
		provider.FailureTimeout,
		provider.FailureContextWindowExceeded,
		provider.FailureEmptyResponse:
		retryable = true
	}
	if failure.Code == provider.FailureTimeout ||
		errors.Is(err, context.DeadlineExceeded) {
		code = protocol.CodeDeadlineExceeded
		retryable = true
	}
	if problem != nil {
		code = problem.Code
	}
	return protocol.NewFault(
		code,
		failure.Message,
		retryable,
		protocol.FaultMetadata{
			Origin:      protocol.FaultOriginProvider,
			Stage:       protocol.FaultStageModelSample,
			RetryOwner:  protocol.FaultRetryOwnerEngine,
			ResumeHint:  protocol.FaultResumeRetryStep,
			Disposition: protocol.FaultRetryStep,
			SideEffects: protocol.SideEffectUnchanged,
		},
		err,
	)
}

func retryBackoff(retries uint32, seed string) time.Duration {
	delay := retryBackoffBase(retries)
	return delay + retryJitter(delay, seed, retries)
}

func retryBackoffBase(retries uint32) time.Duration {
	delay := 10 * time.Millisecond
	for index := uint32(0); index < retries && delay < 30*time.Second; index++ {
		delay *= 2
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return delay
}

func retryJitter(delay time.Duration, seed string, retries uint32) time.Duration {
	span := delay / 5
	if span <= 0 {
		return 0
	}
	sum := sha256.Sum256([]byte(seed + "\x00" + strconv.FormatUint(uint64(retries), 10)))
	return time.Duration(binary.BigEndian.Uint64(sum[:8]) % uint64(span+1))
}

func errorText(err error) string {
	if err == nil {
		return "provider failed"
	}
	return err.Error()
}
