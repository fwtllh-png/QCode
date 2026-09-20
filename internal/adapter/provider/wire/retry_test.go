package wire

import (
	"context"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestRetryPolicyRateLimitAttemptBudget(t *testing.T) {
	policy := RetryPolicy{MaxRetries: 2, RateLimitMaxRetries: 1}
	err := rateLimitError(1000)
	if _, ok := policy.Decide(err, false, 0, false); !ok {
		t.Fatal("first rate limit retry should be allowed")
	}
	policy.RateLimitRetries = 1
	if _, ok := policy.Decide(err, false, 2, false); ok {
		t.Fatal("rate limit attempt budget should be exhausted")
	}
}

func TestRetryPolicySharedBudgetDoesNotChangeSampleRetryNumber(t *testing.T) {
	policy := RetryPolicy{
		RateLimitMaxRetries:    5,
		RateLimitRetries:       0,
		SharedRateLimitRetries: 3,
	}
	retry, ok := policy.Decide(rateLimitError(1), false, 0, false)
	if !ok || retry.Retry != 1 {
		t.Fatalf("sample retry must stay per-sample, got %+v", retry)
	}
	policy.SharedRateLimitRetries = 5
	if _, ok := policy.Decide(rateLimitError(1), false, 0, false); ok {
		t.Fatal("shared pot should exhaust the sample")
	}
}

func TestRetryPolicyRateLimitWaitBudgetUsesFullProviderDelay(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	policy := RetryPolicy{
		MaxRetries: 2, MaxDelay: 2 * time.Minute,
		RateLimitMaxWait: 2 * time.Minute, Now: func() time.Time { return now },
	}
	retry, ok := policy.Decide(rateLimitError(uint64(time.Hour/time.Millisecond)), false, 0, false)
	if ok {
		t.Fatalf("hour retry-after should exhaust a 2m wait budget: %+v", retry)
	}
	policy.RateLimitMaxWait = 0
	retry, ok = policy.Decide(rateLimitError(uint64(time.Hour/time.Millisecond)), false, 0, false)
	if !ok || retry.EffectiveDelay != time.Hour {
		t.Fatalf("unlimited wait should honor provider delay: %+v", retry)
	}
}

func TestRetryPolicyRateLimitWaitIncludesRouteCooldown(t *testing.T) {
	policy := RetryPolicy{
		MaxDelay: 2 * time.Minute, RouteCooldown: 5 * time.Second,
	}
	retry, ok := policy.Decide(rateLimitError(1000), false, 0, false)
	if !ok || retry.EffectiveDelay != 5*time.Second {
		t.Fatalf("cooldown should raise needed wait: %+v", retry)
	}
	policy.RateLimitMaxWait = 3 * time.Second
	if _, ok := policy.Decide(rateLimitError(1000), false, 0, false); ok {
		t.Fatal("cooldown beyond wait budget should exhaust")
	}
}

func TestRetryPolicyTimeoutBudgetSurvivesRateLimitRetries(t *testing.T) {
	policy := RetryPolicy{MaxRetries: 1}
	timeout := protocol.NewFault(
		protocol.CodeDeadlineExceeded,
		"provider request failed during response_headers",
		true,
		protocol.FaultMetadata{
			Origin: protocol.FaultOriginProvider,
			Stage:  protocol.FaultStageResponseHeaders,
		},
		context.DeadlineExceeded,
	)
	policy.RateLimitRetries = 3
	retry, ok := policy.Decide(timeout, false, 0, false)
	if !ok || retry.Retry != 4 {
		t.Fatalf("timeout after rate limits should keep transient budget: %+v allowed=%t", retry, ok)
	}
	if _, ok := policy.Decide(timeout, false, 1, false); ok {
		t.Fatal("transient timeout budget should still exhaust")
	}
}

func TestRetryPolicyTransientFailuresStillUseAttemptAndDelayCaps(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	policy := RetryPolicy{
		MaxRetries: 2, MaxDelay: time.Second,
		Now: func() time.Time { return now },
	}
	err := protocol.NewProblem(
		protocol.CodeUnavailable,
		"server unavailable",
		true,
		&provider.Failure{Code: provider.FailureServer, Message: "server unavailable"},
	)
	if _, ok := policy.Decide(err, false, 2, false); ok {
		t.Fatal("transient attempt budget should still apply")
	}
	err = protocol.NewProblem(
		protocol.CodeUnavailable,
		"server unavailable",
		true,
		&provider.Failure{
			Code: provider.FailureServer, Message: "server unavailable",
			RetryAfterMS: uint64(time.Hour / time.Millisecond),
		},
	)
	retry, ok := policy.Decide(err, false, 0, false)
	if !ok || retry.EffectiveDelay != time.Second {
		t.Fatalf("transient retry should keep max delay cap: %+v", retry)
	}
}

func TestRetryPolicyZeroMaxRetriesDoesNotRetryTransient(t *testing.T) {
	policy := RetryPolicy{}
	err := protocol.NewProblem(
		protocol.CodeUnavailable,
		"server unavailable",
		true,
		&provider.Failure{Code: provider.FailureServer, Message: "server unavailable"},
	)
	if _, ok := policy.Decide(err, false, 0, false); ok {
		t.Fatal("explicit zero transient budget should not retry")
	}
	if _, ok := policy.Decide(
		failure(provider.FailureContextWindowExceeded),
		false, 0, true,
	); ok {
		t.Fatal("explicit zero should not retry a folded context window")
	}
	if _, ok := policy.Decide(failure(provider.FailureEmptyResponse), false, 0, false); !ok {
		t.Fatal("empty response still gets one recovery")
	}
	if _, ok := policy.Decide(failure(provider.FailureEmptyResponse), false, 1, false); ok {
		t.Fatal("empty response recovery should exhaust after one retry")
	}
	if _, ok := policy.Decide(rateLimitError(1000), false, 0, false); !ok {
		t.Fatal("rate-limit recoveries with Retry-After must not use the transient zero budget")
	}
	if _, ok := policy.Decide(rateLimitError(0), false, 0, false); ok {
		t.Fatal("bare 429 without a wait signal should inherit the transient zero budget")
	}
}

func TestRetryPolicyBareRateLimitInheritsTransientAttemptBudget(t *testing.T) {
	policy := RetryPolicy{MaxRetries: 2}
	err := rateLimitError(0)
	if _, ok := policy.Decide(err, false, 0, false); !ok {
		t.Fatal("first bare 429 should inherit provider_retry_limit")
	}
	policy.RateLimitRetries = 2
	if _, ok := policy.Decide(err, false, 4, false); ok {
		t.Fatal("bare 429 should exhaust the inherited transient budget")
	}
	policy.RateLimitRetries = 0
	policy.RouteCooldown = time.Second
	policy.RateLimitMaxWait = 10 * time.Second
	if _, ok := policy.Decide(err, false, 0, false); !ok {
		t.Fatal("route cooldown is a wait signal and should keep count unbounded")
	}
	policy.RateLimitRetries = 3
	if _, ok := policy.Decide(err, false, 0, false); !ok {
		t.Fatal("cooldown-backed 429 must not inherit the transient count")
	}
	policy.RouteCooldown = 0
	policy.RateLimitMaxRetries = 1
	policy.RateLimitRetries = 1
	if _, ok := policy.Decide(err, false, 0, false); ok {
		t.Fatal("explicit rate_limit_retry_limit still bounds a bare 429")
	}
}

func TestRetryBackoffJitterIsSeededAndBounded(t *testing.T) {
	first := retryBackoff(4, "session-a")
	repeat := retryBackoff(4, "session-a")
	if first != repeat {
		t.Fatalf("same seed changed: %s vs %s", first, repeat)
	}
	base := 160 * time.Millisecond
	if first < base || first > base+base/5 {
		t.Fatalf("jitter left the 20 percent span: %s", first)
	}
	decorrelated := false
	for retries := uint32(3); retries < 8; retries++ {
		left := retryBackoff(retries, "session-a")
		right := retryBackoff(retries, "session-b")
		span := retryBackoffBase(retries) / 5
		baseDelay := retryBackoffBase(retries)
		if left < baseDelay || left > baseDelay+span ||
			right < baseDelay || right > baseDelay+span {
			t.Fatalf("jitter left the 20 percent span: retries=%d left=%s right=%s", retries, left, right)
		}
		if left != right {
			decorrelated = true
		}
	}
	if !decorrelated {
		t.Fatal("different seeds stayed synchronized across retry counts")
	}
	providerDelay, ok := (RetryPolicy{MaxRetries: 2, JitterSeed: "session-a"}).Decide(
		rateLimitError(1500), false, 0, false,
	)
	if !ok || providerDelay.EffectiveDelay != 1500*time.Millisecond {
		t.Fatalf("provider retry-after should not receive jitter: %+v", providerDelay)
	}
}

func failure(code provider.FailureCode) error {
	return protocol.NewProblem(
		protocol.CodeUnavailable,
		string(code),
		true,
		&provider.Failure{Code: code, Message: string(code)},
	)
}

func rateLimitError(retryAfterMS uint64) error {
	return protocol.NewProblem(
		protocol.CodeUnavailable,
		"rate limited",
		true,
		&provider.Failure{
			Code: provider.FailureRateLimit, Message: "rate limited",
			RetryAfterMS: retryAfterMS,
		},
	)
}
