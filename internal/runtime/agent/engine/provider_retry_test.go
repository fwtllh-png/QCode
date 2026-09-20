package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/trace"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestProviderRetryMatrix(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	engine := &Engine{options: Options{ProviderConfig: ProviderConfig{MaxRetries: 2, MaxRetryDelay: 2 * time.Minute}, TelemetryConfig: TelemetryConfig{Observability: trace.Runtime{Clock: func() time.Time { return now }}}}}
	failure := func(code provider.FailureCode, retryAfter uint64) error {
		return protocol.NewProblem(
			protocol.CodeUnavailable,
			string(code),
			true,
			&provider.Failure{
				Code: code, Message: string(code),
				RetryAfterMS: retryAfter,
			},
		)
	}
	tests := []struct {
		name           string
		err            error
		meaningful     bool
		retries        uint32
		contextChanged bool
		want           bool
	}{
		{"auth", failure(provider.FailureAuth, 0), false, 0, false, false},
		{"quota", failure(provider.FailureQuota, 0), false, 0, false, false},
		{"invalid", failure(provider.FailureInvalidRequest, 0), false, 0, false, false},
		{"unsupported", failure(provider.FailureUnsupportedContent, 0), false, 0, false, false},
		{"context unchanged", failure(provider.FailureContextWindowExceeded, 0), false, 0, false, false},
		{"context changed", failure(provider.FailureContextWindowExceeded, 0), false, 0, true, true},
		{"rate", failure(provider.FailureRateLimit, 1200), false, 0, false, true},
		{"server", failure(provider.FailureServer, 0), false, 0, false, true},
		{"stream before output", failure(provider.FailureStreamClosed, 0), false, 0, false, true},
		{"stream after output", failure(provider.FailureStreamClosed, 0), true, 0, false, false},
		{"timeout", context.DeadlineExceeded, false, 0, false, true},
		{"empty once", failure(provider.FailureEmptyResponse, 0), false, 0, false, true},
		{"empty exhausted", failure(provider.FailureEmptyResponse, 0), false, 1, false, false},
		{"malformed", failure(provider.FailureMalformedResponse, 0), false, 0, false, false},
		{"aborted", context.Canceled, false, 0, false, false},
		{"server exhausted", failure(provider.FailureServer, 0), false, 2, false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			retry, ok := engine.providerRetry(
				test.err,
				test.meaningful,
				test.retries,
				test.contextChanged,
				rateLimitBudget{},
				"sample-matrix",
			)
			if ok != test.want {
				t.Fatalf("retryable = %t, want %t: %+v", ok, test.want, retry)
			}
			if !ok {
				return
			}
			if retry.Retry != test.retries+1 ||
				retry.PolicyRevision != providerRetryPolicyRevision ||
				retry.Failure.Code == "" {
				t.Fatalf("retry = %+v", retry)
			}
			if test.name == "rate" &&
				(retry.EffectiveDelay != 1200*time.Millisecond ||
					!retry.RetryAt.Equal(now.Add(1200*time.Millisecond))) {
				t.Fatalf("rate retry = %+v", retry)
			}
		})
	}
}

func TestProviderRetryZeroMaxRetriesHonorsExplicitNoRetry(t *testing.T) {
	engine := &Engine{options: Options{ProviderConfig: ProviderConfig{MaxRetryDelay: time.Second}}}
	server := protocol.NewProblem(
		protocol.CodeUnavailable,
		"server unavailable",
		true,
		&provider.Failure{Code: provider.FailureServer, Message: "server unavailable"},
	)
	if retry, ok := engine.providerRetry(server, false, 0, false, rateLimitBudget{}, "sample-zero"); ok {
		t.Fatalf("zero transient budget retried: %+v", retry)
	}
	empty := protocol.NewProblem(
		protocol.CodeUnavailable,
		"empty",
		true,
		&provider.Failure{Code: provider.FailureEmptyResponse, Message: "empty"},
	)
	if _, ok := engine.providerRetry(empty, false, 0, false, rateLimitBudget{}, "sample-zero"); !ok {
		t.Fatal("empty response should still retry once")
	}
	rate := protocol.NewProblem(
		protocol.CodeUnavailable,
		"rate limited",
		true,
		&provider.Failure{Code: provider.FailureRateLimit, Message: "rate limited", RetryAfterMS: 1},
	)
	if _, ok := engine.providerRetry(rate, false, 0, false, rateLimitBudget{}, "sample-zero"); !ok {
		t.Fatal("rate-limit recoveries should ignore the transient zero budget")
	}
}

func TestProviderRetryHonorsKnownRateLimitDelay(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	engine := &Engine{options: Options{ProviderConfig: ProviderConfig{MaxRetries: 1, MaxRetryDelay: 5 * time.Second}, TelemetryConfig: TelemetryConfig{Observability: trace.Runtime{Clock: func() time.Time { return now }}}}}
	err := protocol.NewProblem(
		protocol.CodeUnavailable,
		"rate limited",
		true,
		&provider.Failure{
			Code: provider.FailureRateLimit, Message: "rate limited",
			RetryAfterMS: uint64(time.Hour / time.Millisecond),
		},
	)
	retry, ok := engine.providerRetry(err, false, 0, false, rateLimitBudget{}, "sample-rate")
	if !ok ||
		retry.EffectiveDelay != time.Hour ||
		!retry.RetryAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("retry = %+v", retry)
	}
}

func TestProviderRetryExhaustsWhenRateLimitWaitExceedsBudget(t *testing.T) {
	engine := &Engine{options: Options{ProviderConfig: ProviderConfig{
		MaxRetries: 1, MaxRetryDelay: 5 * time.Second, RateLimitMaxWait: 2 * time.Minute,
	}}}
	err := protocol.NewProblem(
		protocol.CodeUnavailable,
		"rate limited",
		true,
		&provider.Failure{
			Code: provider.FailureRateLimit, Message: "rate limited",
			RetryAfterMS: uint64(time.Hour / time.Millisecond),
		},
	)
	if retry, ok := engine.providerRetry(err, false, 0, false, rateLimitBudget{}, "sample-wait"); ok {
		t.Fatalf("retry = %+v, want exhausted wait budget", retry)
	}
}

func TestProviderRetryUsesDeterministicBackoffWithoutRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	engine := &Engine{options: Options{ProviderConfig: ProviderConfig{MaxRetries: 2, MaxRetryDelay: time.Minute}, TelemetryConfig: TelemetryConfig{Observability: trace.Runtime{Clock: func() time.Time { return now }}}}}
	err := protocol.NewProblem(
		protocol.CodeUnavailable,
		"server unavailable",
		true,
		&provider.Failure{
			Code: provider.FailureServer, Message: "server unavailable",
		},
	)
	engine.options.SessionID = "session-backoff"
	first, ok := engine.providerRetry(err, false, 0, false, rateLimitBudget{}, "sample-backoff")
	if !ok || first.EffectiveDelay < 10*time.Millisecond ||
		first.EffectiveDelay > 12*time.Millisecond {
		t.Fatalf("first retry = %+v", first)
	}
	repeat, ok := engine.providerRetry(err, false, 0, false, rateLimitBudget{}, "sample-backoff")
	if !ok || repeat.EffectiveDelay != first.EffectiveDelay {
		t.Fatalf("seeded backoff changed: first=%+v repeat=%+v", first, repeat)
	}
	second, ok := engine.providerRetry(err, false, 1, false, rateLimitBudget{}, "sample-backoff")
	if !ok || second.EffectiveDelay < 20*time.Millisecond ||
		second.EffectiveDelay > 24*time.Millisecond {
		t.Fatalf("second retry = %+v", second)
	}
}

func TestBareRateLimitInheritsTransientAttemptBudget(t *testing.T) {
	engine := &Engine{options: Options{ProviderConfig: ProviderConfig{
		MaxRetries: 2, MaxRetryDelay: time.Second,
	}}}
	err := protocol.NewProblem(
		protocol.CodeUnavailable,
		"rate limited",
		true,
		&provider.Failure{Code: provider.FailureRateLimit, Message: "rate limited"},
	)
	if _, ok := engine.providerRetry(err, false, 0, false, rateLimitBudget{}, "sample-bare"); !ok {
		t.Fatal("first bare 429 should inherit provider_retry_limit")
	}
	if retry, ok := engine.providerRetry(
		err, false, 0, false, rateLimitBudget{retries: 2}, "sample-bare",
	); ok {
		t.Fatalf("bare 429 should exhaust inherited budget: %+v", retry)
	}
	if _, ok := engine.providerRetry(
		err, false, 0, false,
		rateLimitBudget{retries: 2, cooldown: time.Second},
		"sample-bare",
	); !ok {
		t.Fatal("cooldown should keep a bare 429 count unbounded")
	}
}

func TestRateLimitRetryContinuesBeyondTransientFailureBudget(t *testing.T) {
	engine := &Engine{options: Options{ProviderConfig: ProviderConfig{
		MaxRetries: 2, MaxRetryDelay: time.Second,
	}}}
	err := protocol.NewProblem(
		protocol.CodeUnavailable,
		"rate limited",
		true,
		&provider.Failure{
			Code: provider.FailureRateLimit, Message: "rate limited",
			RetryAfterMS: 1000,
		},
	)
	for retries := range uint32(5) {
		retry, ok := engine.providerRetry(
			err,
			false,
			retries,
			false,
			rateLimitBudget{},
			"sample-rate-continue",
		)
		if !ok || retry.EffectiveDelay != time.Second {
			t.Fatalf(
				"retry %d = %+v, allowed=%t",
				retries,
				retry,
				ok,
			)
		}
	}
}

func TestEngineRetriesHeaderTimeoutAfterRateLimitRetries(t *testing.T) {
	rateLimited := func() provider.Stream {
		return &errorStream{err: protocol.NewProblem(
			protocol.CodeUnavailable,
			"rate limited",
			true,
			&provider.Failure{
				Code: provider.FailureRateLimit, Message: "rate limited",
				RetryAfterMS: 1,
			},
		)}
	}
	headerTimeout := protocol.NewFault(
		protocol.CodeDeadlineExceeded,
		"provider request failed during response_headers",
		true,
		protocol.FaultMetadata{
			Origin: protocol.FaultOriginProvider,
			Stage:  protocol.FaultStageResponseHeaders,
		},
		context.DeadlineExceeded,
	)
	runtime := &scriptedProvider{streams: []provider.Stream{
		rateLimited(),
		rateLimited(),
		rateLimited(),
		&errorStream{err: headerTimeout},
		textStream("recovered"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.MaxRetries = 1
	engine.options.MaxRetryDelay = time.Second

	result, err := engine.Run(t.Context(), "retry after headers timeout", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "recovered" || len(runtime.requests) != 5 {
		t.Fatalf("result = %+v, requests = %d", result, len(runtime.requests))
	}
}

func TestEngineRetriesRateLimitUntilProviderRecovers(t *testing.T) {
	rateLimited := func() provider.Stream {
		return &errorStream{err: protocol.NewProblem(
			protocol.CodeUnavailable,
			"rate limited",
			true,
			&provider.Failure{
				Code: provider.FailureRateLimit, Message: "rate limited",
				RetryAfterMS: 1,
			},
		)}
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		rateLimited(),
		rateLimited(),
		rateLimited(),
		textStream("recovered"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.MaxRetries = 1
	engine.options.MaxRetryDelay = time.Second

	result, err := engine.Run(t.Context(), "retry rate limit", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "recovered" || len(runtime.requests) != 4 {
		t.Fatalf("result = %+v, requests = %d", result, len(runtime.requests))
	}
}

func TestEngineExhaustsRateLimitWaitBudgetWithoutSecondProbe(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&errorStream{err: protocol.NewProblem(
			protocol.CodeUnavailable,
			"rate limited",
			true,
			&provider.Failure{
				Code: provider.FailureRateLimit, Message: "rate limited",
				RetryAfterMS: uint64(time.Second / time.Millisecond),
			},
		)},
		textStream("should not run"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.RateLimitMaxWait = 50 * time.Millisecond

	var terminal Event
	_, err := engine.Run(t.Context(), "exhaust rate limit wait", func(event Event) error {
		if event.State == Failed {
			terminal = event
		}
		return nil
	})
	problem := protocol.ProblemOf(err)
	if problem == nil ||
		problem.Message != "provider rate limit retry budget exhausted: rate limited" ||
		problem.Fault == nil ||
		problem.Fault.Reason != protocol.ProblemReasonProviderRateLimited ||
		problem.Fault.Disposition != protocol.FaultRetryTurn {
		t.Fatalf("exhausted wait = %#v", err)
	}
	if len(runtime.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(runtime.requests))
	}
	if terminal.Fault == nil ||
		terminal.Fault.Reason != protocol.ProblemReasonProviderRateLimited {
		t.Fatalf("terminal fault = %+v", terminal.Fault)
	}
}

func TestEngineExhaustsRateLimitAttemptBudget(t *testing.T) {
	rateLimited := func() provider.Stream {
		return &errorStream{err: protocol.NewProblem(
			protocol.CodeUnavailable,
			"rate limited",
			true,
			&provider.Failure{
				Code: provider.FailureRateLimit, Message: "rate limited",
				RetryAfterMS: 1,
			},
		)}
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		rateLimited(),
		rateLimited(),
		textStream("should not run"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.RateLimitMaxRetries = 1

	_, err := engine.Run(t.Context(), "exhaust rate limit attempts", nil)
	problem := protocol.ProblemOf(err)
	if problem == nil ||
		problem.Message != "provider rate limit retry budget exhausted: rate limited" {
		t.Fatalf("exhausted attempts = %#v", err)
	}
	if len(runtime.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(runtime.requests))
	}
}

func TestEngineExhaustsBareRateLimitUsingTransientAttemptBudget(t *testing.T) {
	rateLimited := func() provider.Stream {
		return &errorStream{err: protocol.NewProblem(
			protocol.CodeUnavailable,
			"rate limited",
			true,
			&provider.Failure{Code: provider.FailureRateLimit, Message: "rate limited"},
		)}
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		rateLimited(),
		rateLimited(),
		textStream("should not run"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.MaxRetries = 1

	_, err := engine.Run(t.Context(), "exhaust bare rate limit", nil)
	problem := protocol.ProblemOf(err)
	if problem == nil ||
		problem.Message != "provider rate limit retry budget exhausted: rate limited" {
		t.Fatalf("exhausted bare 429 = %#v", err)
	}
	if len(runtime.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2 inherited attempts", len(runtime.requests))
	}
}

func TestSharedRateLimitIsConsumedOnceAcrossEngines(t *testing.T) {
	shared := &SharedRateLimit{}
	rateLimited := func() provider.Stream {
		return &errorStream{err: protocol.NewProblem(
			protocol.CodeUnavailable,
			"rate limited",
			true,
			&provider.Failure{
				Code: provider.FailureRateLimit, Message: "rate limited",
				RetryAfterMS: 1,
			},
		)}
	}
	firstRuntime := &scriptedProvider{streams: []provider.Stream{
		rateLimited(), rateLimited(), textStream("should not run"),
	}}
	first := newEngine(t, firstRuntime, tool.NewRegistry(nil, nil))
	first.options.RateLimitMaxRetries = 1
	first.options.SharedRateLimit = shared
	if _, err := first.Run(t.Context(), "parent rate limit", nil); err == nil {
		t.Fatal("expected first engine to exhaust the shared pot")
	}
	retries, _ := shared.Load()
	if retries == 0 {
		t.Fatal("shared limiter recorded no retries")
	}
	secondRuntime := &scriptedProvider{streams: []provider.Stream{
		rateLimited(), textStream("parent must not get a private retry budget"),
	}}
	second := newEngine(t, secondRuntime, tool.NewRegistry(nil, nil))
	second.options.RateLimitMaxRetries = 1
	second.options.SharedRateLimit = shared
	_, err := second.Run(t.Context(), "child rate limit", nil)
	problem := protocol.ProblemOf(err)
	if problem == nil ||
		problem.Message != "provider rate limit retry budget exhausted: rate limited" {
		t.Fatalf("second engine = %#v", err)
	}
	if len(secondRuntime.requests) != 1 {
		t.Fatalf("second engine requests = %d, want 1 shared exhaustion",
			len(secondRuntime.requests))
	}
}

func TestSharedRateLimitDoesNotReuseRetryNumbersAcrossSamples(t *testing.T) {
	shared := &SharedRateLimit{}
	shared.Record(time.Millisecond)
	shared.Record(time.Millisecond)
	runtime := &scriptedProvider{streams: []provider.Stream{
		&errorStream{err: protocol.NewProblem(
			protocol.CodeUnavailable,
			"rate limited",
			true,
			&provider.Failure{
				Code: provider.FailureRateLimit, Message: "rate limited",
				RetryAfterMS: 1,
			},
		)},
		textStream("recovered after a later sample"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.RateLimitMaxRetries = 5
	engine.options.SharedRateLimit = shared
	var retries []*ProviderRetry
	result, err := engine.Run(t.Context(), "later sample rate limit", func(event Event) error {
		if event.ProviderRetry != nil {
			retries = append(retries, event.ProviderRetry)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("shared pot leaked into sample retry sequence: %v", err)
	}
	if result.Text != "recovered after a later sample" || len(retries) != 1 ||
		retries[0].Retry != 1 {
		t.Fatalf("result=%+v retries=%+v", result, retries)
	}
}

func TestExhaustedProviderRetryBecomesUserRecoverable(t *testing.T) {
	original := protocol.NewProblem(
		protocol.CodeUnavailable,
		"rate limited",
		true,
		&provider.Failure{
			Code: provider.FailureRateLimit, Message: "rate limited",
		},
	)
	original.HTTPStatus = 429
	original.RateLimit = &protocol.RateLimitMetadata{RetryAfterMS: 1000}

	recovered := protocol.ProblemOf(exhaustedProviderRetry(original))
	if recovered.HTTPStatus != 429 ||
		recovered.RateLimit.RetryAfterMS != 1000 ||
		recovered.Fault == nil ||
		recovered.Fault.Disposition != protocol.FaultRetryTurn ||
		recovered.Fault.RecoveryAction == "" {
		t.Fatalf("exhausted retry = %#v", recovered)
	}
	limited := protocol.ProblemOf(exhaustedRateLimitRetry(original))
	if limited.Message != "provider rate limit retry budget exhausted: rate limited" ||
		limited.Fault == nil ||
		limited.Fault.Reason != protocol.ProblemReasonProviderRateLimited ||
		!strings.Contains(limited.Fault.RecoveryAction, "cooldown") ||
		limited.Fault.Disposition != protocol.FaultRetryTurn {
		t.Fatalf("exhausted rate limit = %#v", limited)
	}
}

func TestInvalidProviderRequestRemainsManualButPreservesDraft(t *testing.T) {
	original := protocol.NewProblem(
		protocol.CodeInvalidArgument,
		"invalid provider request",
		false,
		&provider.Failure{
			Code:    provider.FailureInvalidRequest,
			Message: "invalid provider request",
		},
	)
	original.HTTPStatus = 400

	recovered := protocol.ProblemOf(exhaustedProviderRetry(original))
	if recovered.HTTPStatus != 400 ||
		recovered.Retryable ||
		recovered.Fault == nil ||
		recovered.Fault.Disposition != protocol.FaultRetryTurn ||
		recovered.Fault.RetryOwner != protocol.FaultRetryOwnerHost ||
		recovered.Fault.ResumeHint != protocol.FaultResumeRetryTurn ||
		recovered.Fault.RecoveryAction == "" {
		t.Fatalf("invalid request = %#v", recovered)
	}
}

func TestContextOverflowRetriesOnlyAfterVisibleCompaction(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&errorStream{err: protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"context is too large",
			false,
			&provider.Failure{
				Code:    provider.FailureContextWindowExceeded,
				Message: "context is too large",
			},
		)},
		textStream("recovered"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.MaxRetries = 1
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("old request ", 100), 1),
		messageWithText(provider.RoleAssistant, strings.Repeat("old answer ", 100), 1),
		messageWithText(provider.RoleUser, strings.Repeat("new request ", 100), 2),
		messageWithText(provider.RoleAssistant, strings.Repeat("new answer ", 100), 2),
	}
	var compacted bool
	result, err := engine.Run(t.Context(), "recover context", func(event Event) error {
		compacted = compacted || event.Compaction != nil
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "recovered" || !compacted || len(runtime.requests) != 2 {
		t.Fatalf(
			"result=%+v compacted=%t requests=%d",
			result,
			compacted,
			len(runtime.requests),
		)
	}
	before := agentcontext.HistoryBytes(runtime.requests[0].Messages)
	after := agentcontext.HistoryBytes(runtime.requests[1].Messages)
	if after >= before {
		t.Fatalf(
			"context did not shrink: before=%d after=%d bytes",
			before,
			after,
		)
	}
	if !strings.Contains(engine.history[0].Text(), "old request") {
		t.Fatalf("overflow fold replaced durable history: %+v", engine.history)
	}
}

func TestCancellationDuringProviderDelayDoesNotCallProviderAgain(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&errorStream{err: protocol.NewProblem(
			protocol.CodeUnavailable,
			"rate limited",
			true,
			&provider.Failure{
				Code:         provider.FailureRateLimit,
				Message:      "rate limited",
				RetryAfterMS: uint64(time.Hour / time.Millisecond),
			},
		)},
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := engine.Run(ctx, "cancel retry delay", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want deadline exceeded", err)
	}
	if len(runtime.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(runtime.requests))
	}
}

func TestQuotaFailureClosesAttemptWithoutRetryAndKeepsResetReason(t *testing.T) {
	message := "usage quota exhausted; resets at 2026-09-07 19:10:43"
	runtime := &catalogMutationProvider{mutate: func() error {
		return protocol.NewProblem(protocol.CodeResourceExhausted, message, false,
			&provider.Failure{Code: provider.FailureQuota, Message: message, HTTPStatus: 429})
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	var statuses []string
	retries := 0
	_, err := engine.Run(t.Context(), "continue the work", func(event Event) error {
		if event.ProviderRetry != nil {
			retries++
		}
		if event.ModelExecution != nil && event.ModelExecution.Kind == "provider_attempt" {
			statuses = append(statuses, event.ModelExecution.Status)
		}
		return nil
	})
	problem := protocol.ProblemOf(err)
	if problem == nil || problem.Message != message || problem.Retryable ||
		problem.Fault == nil || problem.Fault.Disposition != protocol.FaultResumeTurn {
		t.Fatalf("quota terminal = %+v", problem)
	}
	if len(runtime.requests) != 1 || retries != 0 || len(statuses) != 2 ||
		statuses[0] != protocol.ProviderAttemptStarted || statuses[1] != protocol.ProviderAttemptFailed {
		t.Fatalf("requests=%d retries=%d statuses=%v", len(runtime.requests), retries, statuses)
	}
}
