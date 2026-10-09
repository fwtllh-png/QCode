package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func retryFailure(code provider.FailureCode, delay time.Duration) error {
	return protocol.NewProblem(protocol.CodeUnavailable, string(code), true,
		&provider.Failure{Code: code, Message: string(code), RetryAfterMS: uint64(delay / time.Millisecond)})
}

func TestProviderRetryBudgetSurvivesRestart(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		before     []provider.FailureCode
		after      []provider.FailureCode
		rateLimit  int
		waitBudget time.Duration
		completed  bool
	}{
		{"rate_limits_then_server", []provider.FailureCode{provider.FailureRateLimit, provider.FailureRateLimit}, []provider.FailureCode{provider.FailureServer}, 3, time.Second, true},
		{"server_then_rate_limit", []provider.FailureCode{provider.FailureServer}, []provider.FailureCode{provider.FailureRateLimit, provider.FailureRateLimit}, 2, time.Second, true},
		{"rate_limit_count_exhausted", []provider.FailureCode{provider.FailureRateLimit, provider.FailureRateLimit}, []provider.FailureCode{provider.FailureRateLimit}, 2, time.Second, false},
		{"rate_limit_wait_exhausted", []provider.FailureCode{provider.FailureRateLimit, provider.FailureRateLimit}, []provider.FailureCode{provider.FailureRateLimit}, 0, 2 * time.Millisecond, false},
		{"server_count_exhausted", []provider.FailureCode{provider.FailureServer}, []provider.FailureCode{provider.FailureServer}, 3, time.Second, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			runtime, engine, echo, store := restartTestEngines(t, "workspace-restart", func(e *Engine, p *scriptedProvider) {
				e.options.MaxRetries = 1
				e.options.RateLimitMaxRetries = scenario.rateLimit
				e.options.RateLimitMaxWait = scenario.waitBudget
				streams := []provider.Stream{p.streams[0]}
				for _, code := range scenario.before {
					streams = append(streams, &errorStream{err: retryFailure(code, time.Millisecond)})
				}
				p.streams = append(streams, p.streams[1])
			})
			engine.options.MaxRetries = 1
			engine.options.RateLimitMaxRetries = scenario.rateLimit
			engine.options.RateLimitMaxWait = scenario.waitBudget
			runtime.streams = nil
			for _, code := range scenario.after {
				runtime.streams = append(runtime.streams, &errorStream{err: retryFailure(code, time.Millisecond)})
			}
			runtime.streams = append(runtime.streams, textStream("recovered"))
			result, err := engine.RunForTurn(t.Context(), "turn-restart", "inspect the parser", nil)
			if scenario.completed {
				if err != nil || result.State != Completed || len(runtime.requests) != len(scenario.after)+1 {
					t.Fatalf("state=%s requests=%d error=%v", result.State, len(runtime.requests), err)
				}
			} else if err == nil || result.State != Failed || len(runtime.requests) != 1 {
				t.Fatalf("restart refunded a budget: state=%s requests=%d error=%v", result.State, len(runtime.requests), err)
			}
			if echo.calls.Load() != 1 {
				t.Fatal("restart replayed an already completed tool")
			}
			facts, err := store.LoadDomainFacts(t.Context(), "turn-restart")
			if err != nil {
				t.Fatal(err)
			}
			for _, fact := range facts {
				for _, sample := range fact.State.SampleLedger {
					budget := sample.RetryBudget
					if sample.ProviderRetries != budget.TransientRetries+budget.RateLimitRetries ||
						budget.RateLimitWaited != time.Duration(budget.RateLimitRetries)*time.Millisecond {
						t.Fatalf("unclassified durable budget: %+v", sample)
					}
				}
			}
		})
	}
}

func TestProviderRetryRestartDuringWaitKeepsDeadlineAndReservation(t *testing.T) {
	store := turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil)
	coordinators, err := turnkernel.NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	blobs := newMapBlobStore()
	registry := tool.NewRegistry(nil, nil)
	echo := &echoTool{}
	if err := registry.Register(echo); err != nil {
		t.Fatal(err)
	}
	providerA := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("done", "echo", `{"text":"probe"}`),
		&errorStream{err: retryFailure(provider.FailureRateLimit, 120*time.Millisecond)},
	}}
	first := newEngine(t, providerA, registry)
	first.options.TurnCoordinatorRuntime, first.options.TurnContinuations = coordinators, blobs
	first.options.RateLimitMaxWait = 120 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	scheduled, release := make(chan ProviderRetry, 1), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := first.RunForTurn(ctx, "retry-wait-restart", "inspect", func(event Event) error {
			if event.ProviderRetry != nil {
				scheduled <- *event.ProviderRetry
				<-release
			}
			return nil
		})
		done <- err
	}()
	t.Cleanup(func() { cancel(); close(release); <-done })
	var retry ProviderRetry
	select {
	case retry = <-scheduled:
	case <-time.After(5 * time.Second):
		t.Fatal("retry was not scheduled")
	}
	if err := coordinators.Release(t.Context(), "retry-wait-restart"); err != nil {
		t.Fatal(err)
	}
	secondRuntime, err := turnkernel.NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	secondProvider := &timedRetryProvider{failure: retryFailure(provider.FailureRateLimit, time.Millisecond)}
	second := newEngine(t, secondProvider, registry)
	second.options.TurnCoordinatorRuntime, second.options.TurnContinuations = secondRuntime, blobs
	second.options.RateLimitMaxWait = first.options.RateLimitMaxWait
	_, err = second.RunForTurn(t.Context(), "retry-wait-restart", "inspect", nil)
	if err == nil || secondProvider.calls != 1 || secondProvider.started.Before(retry.RetryAt) {
		t.Fatalf("retry deadline or reservation lost: calls=%d started=%v deadline=%v error=%v", secondProvider.calls, secondProvider.started, retry.RetryAt, err)
	}
	if echo.calls.Load() != 1 {
		t.Fatal("completed tool replayed")
	}
}

type timedRetryProvider struct {
	failure error
	started time.Time
	calls   int
}

func (p *timedRetryProvider) Stream(context.Context, provider.ModelRequest) (provider.Stream, error) {
	p.calls++
	p.started = time.Now()
	return nil, p.failure
}

func TestProviderFailureReasonsPreserveClassification(t *testing.T) {
	for code, reason := range map[provider.FailureCode]string{
		provider.FailureAuth:                  protocol.ProblemReasonProviderAuth,
		provider.FailureQuota:                 protocol.ProblemReasonProviderQuota,
		provider.FailureInvalidRequest:        protocol.ProblemReasonProviderRequest,
		provider.FailureMalformedResponse:     protocol.ProblemReasonProviderResponse,
		provider.FailureUnsupportedContent:    protocol.ProblemReasonProviderContent,
		provider.FailureContextWindowExceeded: protocol.ProblemReasonContextWindow,
		provider.FailureServer:                protocol.ProblemReasonProviderRetry,
	} {
		t.Run(string(code), func(t *testing.T) {
			original := &provider.Failure{Code: code, Message: string(code)}
			err := exhaustedProviderRetry(original)
			problem := protocol.ProblemOf(err)
			if problem.Fault.Reason != reason || problem.Fault.RecoveryAction == "" || !errors.Is(err, original) {
				t.Fatalf("classification lost: %+v", problem)
			}
			if code != provider.FailureServer && problem.Retryable {
				t.Fatal("hard failure became automatically retryable")
			}
		})
	}
}
