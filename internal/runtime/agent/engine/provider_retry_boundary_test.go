package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestModelFailureRetryAcrossTransportBoundaries(t *testing.T) {
	for _, opened := range []bool{false, true} {
		boundary := "before_open"
		if opened {
			boundary = "after_open"
		}
		for _, outcome := range []string{"recovered", "exhausted", "event_failure", "wait_canceled"} {
			t.Run(boundary+"/"+outcome, func(t *testing.T) {
				failure := func(code provider.FailureCode) error {
					return protocol.NewProblem(protocol.CodeUnavailable, string(code), true,
						&provider.Failure{Code: code, Message: string(code), RetryAfterMS: 1})
				}
				runtime := &retryBoundaryProvider{opened: opened, failures: []error{
					failure(provider.FailureRateLimit), failure(provider.FailureRateLimit),
					failure(provider.FailureServer),
				}}
				if outcome == "exhausted" {
					runtime.failures = append(runtime.failures, failure(provider.FailureServer))
				}
				engine := newEngine(t, runtime, nil)
				engine.options.MaxRetries = 1
				engine.options.SharedRateLimit = NewSharedRateLimit(1)
				scope := attachTestScope(t, engine)
				scope.spec.Request = TurnRequest{Prompt: "review"}
				catalog, err := engine.options.Tools.Snapshot()
				if err != nil {
					t.Fatal(err)
				}
				scope.spec.Catalog = catalog
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				eventFailure := errors.New("retry event failed")
				var retries []ProviderRetry
				var statuses []string
				history := []provider.Message{provider.TextMessage(provider.RoleUser, "review")}
				blocks, _, _, _, err := engine.modelStep(
					ctx, &history, provider.Usage{}, "sample-retry", "normal", modelRetryState{}, false, false,
					nil, nil, nil, nil, nil, nil, nil,
					func(_ State, event Event) error {
						if event.ModelExecution != nil {
							statuses = append(statuses, event.ModelExecution.Status)
						}
						if event.ProviderRetry == nil {
							return nil
						}
						retries = append(retries, *event.ProviderRetry)
						execution := event.ModelExecution
						if execution == nil || execution.Status != protocol.ProviderAttemptRetryWait ||
							execution.RateLimitRetries != uint32(len(retries)-1) ||
							execution.RateLimitWaited != time.Duration(len(retries)-1)*time.Millisecond {
							t.Fatalf("retry execution = %+v", execution)
						}
						sharedRetries, sharedWaited := engine.options.SharedRateLimit.Load()
						if sharedRetries != uint32(min(len(retries), 2)) ||
							sharedWaited != time.Duration(sharedRetries)*time.Millisecond {
							t.Fatalf("shared retry budget = %d, %v", sharedRetries, sharedWaited)
						}
						if len(engine.options.SharedRateLimit.token) != 1 {
							t.Fatal("retry event published before releasing the sample lease")
						}
						switch outcome {
						case "event_failure":
							return eventFailure
						case "wait_canceled":
							cancel()
						}
						return nil
					},
				)
				wantAttempts, wantRetries := 4, 3
				switch outcome {
				case "recovered":
					if err != nil || len(blocks) != 1 || blocks[0].Text != "recovered" {
						t.Fatalf("blocks=%+v error=%v", blocks, err)
					}
				case "exhausted":
					if err == nil {
						t.Fatal("transient retry budget was not exhausted")
					}
				case "event_failure", "wait_canceled":
					wantAttempts, wantRetries = 1, 1
					wantErr := eventFailure
					if outcome == "wait_canceled" {
						wantErr = context.Canceled
					}
					if !errors.Is(err, wantErr) {
						t.Fatalf("error = %v, want %v", err, wantErr)
					}
				}
				if len(runtime.requests) != wantAttempts || len(retries) != wantRetries {
					t.Fatalf("attempts=%d retries=%d", len(runtime.requests), len(retries))
				}
				var wantStatuses []string
				for i := range wantAttempts {
					status := protocol.ProviderAttemptFailed
					if outcome == "recovered" && i == wantAttempts-1 {
						status = protocol.ProviderAttemptCompleted
					}
					wantStatuses = append(wantStatuses, protocol.ProviderAttemptStarted, status)
					if i < wantRetries {
						wantStatuses = append(wantStatuses, protocol.ProviderAttemptRetryWait)
					}
				}
				if !reflect.DeepEqual(statuses, wantStatuses) {
					t.Fatalf("statuses = %v, want %v", statuses, wantStatuses)
				}
				for i, retry := range retries {
					wantCode, wantRetry := provider.FailureRateLimit, uint32(i+1)
					if i == 2 {
						wantCode = provider.FailureServer
					}
					if retry.Failure.Code != wantCode || retry.Retry != wantRetry {
						t.Fatalf("retry %d = %+v", i, retry)
					}
				}
				if len(engine.options.SharedRateLimit.token) != 1 {
					t.Fatal("sample lease leaked")
				}
			})
		}
	}
}

func TestModelFailureCancellationPreservesTransportResult(t *testing.T) {
	for _, boundary := range []string{"before_open", "after_open", "after_output"} {
		t.Run(boundary, func(t *testing.T) {
			runtime := &retryBoundaryProvider{
				opened: boundary != "before_open",
				failures: []error{protocol.NewProblem(protocol.CodeUnavailable, "rate limited", true,
					&provider.Failure{Code: provider.FailureRateLimit, Message: "rate limited", RetryAfterMS: 1})},
			}
			if boundary == "after_output" {
				runtime.events = []provider.StreamEvent{
					{Type: provider.EventTextDelta, Text: "partial"},
					{Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 7, OutputTokens: 2}},
				}
			}
			engine := newEngine(t, runtime, nil)
			engine.options.SharedRateLimit = NewSharedRateLimit(1)
			scope := attachTestScope(t, engine)
			scope.spec.Request = TurnRequest{Prompt: "review"}
			catalog, err := engine.options.Tools.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			scope.spec.Catalog = catalog
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			history := []provider.Message{provider.TextMessage(provider.RoleUser, "review")}
			blocks, calls, usage, _, err := engine.modelStep(
				ctx, &history, provider.Usage{}, "sample-cancel", "normal", modelRetryState{}, false, false,
				nil, nil, nil, nil, nil, nil, nil,
				func(_ State, event Event) error {
					if event.ProviderRetry != nil {
						t.Fatal("canceled attempt scheduled a retry")
					}
					if execution := event.ModelExecution; execution != nil &&
						(execution.Status == protocol.ProviderAttemptFailed || execution.Status == protocol.ProviderAttemptIncomplete) {
						cancel()
					}
					return nil
				},
			)
			if boundary == "before_open" {
				problem := protocol.ProblemOf(err)
				if problem == nil || problem.Fault == nil || problem.Fault.Reason != protocol.ProblemReasonProviderRateLimited {
					t.Fatalf("unopened failure = %v", err)
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Fatalf("opened failure = %v, want canceled", err)
			}
			if boundary == "after_output" {
				if len(blocks) != 1 || blocks[0].Text != "partial" || usage.InputTokens != 7 || usage.OutputTokens != 2 {
					t.Fatalf("cancellation lost output: blocks=%+v usage=%+v", blocks, usage)
				}
			} else if len(blocks) != 0 || usage.Total() != 0 {
				t.Fatalf("unexpected output: blocks=%+v usage=%+v", blocks, usage)
			}
			if len(calls) != 0 || len(runtime.requests) != 1 || len(engine.options.SharedRateLimit.token) != 1 {
				t.Fatalf("calls=%+v requests=%d available slots=%d", calls, len(runtime.requests), len(engine.options.SharedRateLimit.token))
			}
			retries, waited := engine.options.SharedRateLimit.Load()
			if boundary == "before_open" {
				if retries != 1 || waited != time.Millisecond {
					t.Fatalf("unopened cooldown = %d, %v", retries, waited)
				}
			} else if retries != 0 || waited != 0 {
				t.Fatalf("canceled stream recorded a cooldown: %d, %v", retries, waited)
			}
		})
	}
}

type retryBoundaryProvider struct {
	opened   bool
	failures []error
	events   []provider.StreamEvent
	requests []provider.ModelRequest
}

func (p *retryBoundaryProvider) Stream(_ context.Context, request provider.ModelRequest) (provider.Stream, error) {
	p.requests = append(p.requests, request)
	if len(p.failures) == 0 {
		return textStream("recovered"), nil
	}
	err := p.failures[0]
	p.failures = p.failures[1:]
	if !p.opened {
		return nil, err
	}
	return &eventErrorStream{events: p.events, err: err}, nil
}
