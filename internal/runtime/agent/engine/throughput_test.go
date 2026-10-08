package engine

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	providerratelimit "github.com/fwtllh-png/QCode/internal/adapter/provider/ratelimit"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/contextview"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type throughputScriptedProvider struct {
	scriptedProvider
	limits         providerratelimit.Controller
	observeStatus  int
	observeHeader  http.Header
	pendingObserve bool
}

func (p *throughputScriptedProvider) Stream(
	ctx context.Context,
	request provider.ModelRequest,
) (provider.Stream, error) {
	stream, err := p.scriptedProvider.Stream(ctx, request)
	if p.observeHeader != nil {
		p.pendingObserve = true
	}
	return stream, err
}

func (p *throughputScriptedProvider) applyObservedHeaders(route model.ReadyRoute) {
	if !p.pendingObserve || p.observeHeader == nil {
		return
	}
	_ = p.limits.Observe(
		providerratelimit.Key(route),
		0,
		p.observeStatus,
		p.observeHeader,
		nil,
	)
	p.pendingObserve = false
}

func (p *throughputScriptedProvider) DecideThroughput(
	route model.ReadyRoute,
	required uint64,
	operatorLimit uint64,
) providerratelimit.Decision {
	p.applyObservedHeaders(route)
	return p.limits.Decide(
		providerratelimit.Key(route),
		required,
		operatorLimit,
		time.Now(),
	)
}

func (p *throughputScriptedProvider) ReserveThroughput(
	route model.ReadyRoute,
	tokens uint64,
) {
	p.limits.Reserve(providerratelimit.Key(route), tokens, time.Now())
}

func TestOversizedWorkingSetIsRefusedBeforeProviderProbe(t *testing.T) {
	runtime := &throughputScriptedProvider{
		scriptedProvider: scriptedProvider{
			streams: []provider.Stream{textStream("should not run")},
		},
	}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.TokensPerMinute = 1

	_, err := engine.Run(t.Context(), "hello", nil)
	problem := protocol.ProblemOf(err)
	if problem == nil ||
		problem.Code != protocol.CodeResourceExhausted ||
		problem.Details == nil ||
		problem.Details.Reason != protocol.ProblemReasonProviderThroughput ||
		problem.Details.ResourceID != providerratelimit.ReasonExceedsBurst ||
		problem.Retryable {
		t.Fatalf("throughput refusal = %#v", err)
	}
	if len(runtime.requests) != 0 {
		t.Fatalf("provider requests = %d, want 0", len(runtime.requests))
	}
}

func TestRateLimitHeaderBurstAbortsRetryBeforeSecondProbe(t *testing.T) {
	runtime := &throughputScriptedProvider{
		scriptedProvider: scriptedProvider{
			streams: []provider.Stream{
				&errorStream{err: protocol.NewProblem(
					protocol.CodeUnavailable,
					"rate limited",
					true,
					&provider.Failure{
						Code:         provider.FailureRateLimit,
						Message:      "rate limited",
						RetryAfterMS: 1,
					},
				)},
				textStream("should not run"),
			},
		},
		observeStatus: http.StatusTooManyRequests,
		observeHeader: func() http.Header {
			header := make(http.Header)
			header.Set("X-RateLimit-Limit-Tokens", "100")
			header.Set("X-RateLimit-Remaining-Tokens", "0")
			return header
		}(),
	}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))

	_, err := engine.Run(t.Context(), "hello", nil)
	problem := protocol.ProblemOf(err)
	if problem == nil ||
		problem.Code != protocol.CodeResourceExhausted ||
		problem.Details == nil ||
		problem.Details.Reason != protocol.ProblemReasonProviderThroughput ||
		problem.Details.ResourceID != providerratelimit.ReasonExceedsBurst {
		t.Fatalf("header burst abort = %#v", err)
	}
	if len(runtime.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(runtime.requests))
	}
}

type scriptedThroughputGovernor struct {
	scriptedProvider
	decisions []providerratelimit.Decision
	required  []uint64
	reserved  []uint64
}

func (p *scriptedThroughputGovernor) DecideThroughput(
	_ model.ReadyRoute,
	required uint64,
	_ uint64,
) providerratelimit.Decision {
	p.required = append(p.required, required)
	if len(p.decisions) == 0 {
		return providerratelimit.Decision{
			Status: providerratelimit.StatusRefuse,
			Reason: providerratelimit.ReasonExceedsBurst,
		}
	}
	decision := p.decisions[0]
	p.decisions = p.decisions[1:]
	decision.Required = required
	return decision
}

func (p *scriptedThroughputGovernor) ReserveThroughput(
	_ model.ReadyRoute,
	tokens uint64,
) {
	p.reserved = append(p.reserved, tokens)
}

func TestAdmitThroughputFoldsWhenRequiredExceedsBurst(t *testing.T) {
	runtime := &scriptedThroughputGovernor{
		decisions: []providerratelimit.Decision{
			{
				Status: providerratelimit.StatusRefuse,
				Reason: providerratelimit.ReasonExceedsBurst,
				Limit:  300,
			},
			{
				Status: providerratelimit.StatusAdmit,
				Reason: providerratelimit.ReasonAdmitted,
				Source: providerratelimit.SourceOperator,
				Limit:  300,
			},
		},
	}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	folded := false
	err := engine.admitProviderThroughput(
		t.Context(),
		engine.activeRoute(),
		800,
		nil,
		func() (uint64, bool, error) {
			folded = true
			return 200, true, nil
		},
	)
	if err != nil || !folded {
		t.Fatalf("admit after fold err=%v folded=%t", err, folded)
	}
	if len(runtime.required) != 2 ||
		runtime.required[0] != 800 ||
		runtime.required[1] != 200 {
		t.Fatalf("required probes = %v", runtime.required)
	}
}

func TestAdmitThroughputStillRefusesWhenFoldCannotShrinkBurst(t *testing.T) {
	runtime := &scriptedThroughputGovernor{
		decisions: []providerratelimit.Decision{{
			Status: providerratelimit.StatusRefuse,
			Reason: providerratelimit.ReasonExceedsBurst,
			Limit:  1,
		}},
	}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	err := engine.admitProviderThroughput(
		t.Context(),
		engine.activeRoute(),
		800,
		nil,
		func() (uint64, bool, error) { return 0, false, nil },
	)
	problem := protocol.ProblemOf(err)
	if problem == nil ||
		problem.Details == nil ||
		problem.Details.ResourceID != providerratelimit.ReasonExceedsBurst {
		t.Fatalf("refusal = %#v", err)
	}
}

func TestAdmitThroughputDoesNotFoldWhenWaitFitsBudget(t *testing.T) {
	runtime := &scriptedThroughputGovernor{
		decisions: []providerratelimit.Decision{
			{
				Status: providerratelimit.StatusWait,
				Reason: providerratelimit.ReasonWaitForWindow,
				Wait:   10 * time.Millisecond,
				Limit:  300,
			},
			{
				Status: providerratelimit.StatusAdmit,
				Reason: providerratelimit.ReasonAdmitted,
				Source: providerratelimit.SourceOperator,
				Limit:  300,
			},
		},
	}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.RateLimitMaxWait = time.Second
	folded := false
	err := engine.admitProviderThroughput(
		t.Context(),
		engine.activeRoute(),
		200,
		nil,
		func() (uint64, bool, error) {
			folded = true
			return 80, true, nil
		},
	)
	if err != nil || folded {
		t.Fatalf("short wait folded=%t err=%v", folded, err)
	}
}

func TestAdmitThroughputFoldsWhenWaitExceedsBudget(t *testing.T) {
	runtime := &scriptedThroughputGovernor{
		decisions: []providerratelimit.Decision{
			{
				Status: providerratelimit.StatusWait,
				Reason: providerratelimit.ReasonWaitForWindow,
				Wait:   time.Hour,
				Limit:  300,
			},
			{
				Status: providerratelimit.StatusAdmit,
				Reason: providerratelimit.ReasonAdmitted,
				Source: providerratelimit.SourceOperator,
				Limit:  300,
			},
		},
	}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.RateLimitMaxWait = time.Second
	folded := false
	err := engine.admitProviderThroughput(
		t.Context(),
		engine.activeRoute(),
		200,
		nil,
		func() (uint64, bool, error) {
			folded = true
			return 80, true, nil
		},
	)
	if err != nil || !folded {
		t.Fatalf("wait-budget fold err=%v folded=%t", err, folded)
	}
}

func TestFoldWorkingSetForThroughputDoesNotReplaceHistory(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	history := []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("old ", 400), 1),
		messageWithText(provider.RoleAssistant, strings.Repeat("ans ", 400), 1),
		messageWithText(provider.RoleUser, "continue", 2),
	}
	original := history[0].Text()
	var receipt *CompactionReceipt
	next, ok, err := engine.foldWorkingSetForThroughput(
		&history,
		engine.projectSelectedHistoryForTest(nil),
		agentcontext.NewMessageLedger(agentcontext.LedgerInput{
			History: history,
		}).Snapshot(),
		128,
		CompactionPhasePreSampling,
		func(_ State, event Event) error {
			receipt = event.Compaction
			return nil
		},
	)
	if err != nil || !ok || next == 0 ||
		receipt == nil ||
		receipt.TruncationReason != "throughput_tail_fold" ||
		receipt.Mode != "view" {
		t.Fatalf("throughput fold next=%d ok=%t receipt=%+v err=%v", next, ok, receipt, err)
	}
	if history[0].Text() != original {
		t.Fatal("throughput fold replaced durable history")
	}
	viewed := engine.projectSelectedHistoryForTest(nil)(history)
	if len(viewed) == 0 || strings.Contains(viewed[0].Text(), "old ") {
		t.Fatalf("folded view still has the oldest group: %+v", viewed)
	}
}

func TestUnknownThroughputContractDoesNotChangeSamplePath(t *testing.T) {
	runtime := &throughputScriptedProvider{
		scriptedProvider: scriptedProvider{
			streams: []provider.Stream{textStream("ok")},
		},
	}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	if engine.options.TokensPerMinute != 0 {
		t.Fatalf("default TPM = %d", engine.options.TokensPerMinute)
	}

	result, err := engine.Run(t.Context(), "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "ok" || len(runtime.requests) != 1 {
		t.Fatalf("result = %+v, requests = %d", result, len(runtime.requests))
	}
}

func TestThroughputFoldPreservesPreparedModelInput(t *testing.T) {
	for _, test := range []struct {
		name     string
		decision *providerratelimit.Decision
	}{
		{name: "without_fold"},
		{name: "burst", decision: &providerratelimit.Decision{
			Status: providerratelimit.StatusRefuse,
			Reason: providerratelimit.ReasonExceedsBurst,
		}},
		{name: "wait_budget", decision: &providerratelimit.Decision{
			Status: providerratelimit.StatusWait,
			Reason: providerratelimit.ReasonWaitForWindow,
			Wait:   time.Hour,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := &scriptedThroughputGovernor{
				scriptedProvider: scriptedProvider{streams: []provider.Stream{
					&providerfixture.SliceStream{Events: []provider.StreamEvent{
						{Type: provider.EventMessageStart},
						{Type: provider.EventTextDelta, Text: "ok"},
						{Type: provider.EventUsage, Usage: &provider.Usage{OutputTokens: 1}},
						{Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn},
					}},
				}},
			}
			if test.decision != nil {
				runtime.decisions = append(runtime.decisions, *test.decision)
			}
			runtime.decisions = append(runtime.decisions, providerratelimit.Decision{
				Status: providerratelimit.StatusAdmit,
				Source: providerratelimit.SourceOperator,
			})
			engine := newThroughputFoldEngine(t, runtime)
			var projected uint64
			var sampled *protocol.SampleContextData
			var expected agentcontext.MessageSnapshot
			_, err := engine.Execute(t.Context(), TurnRequest{Prompt: "continue"}, func(event Event) error {
				if event.ModelExecution != nil && event.ModelExecution.Status == protocol.ProviderAttemptStarted {
					projected = event.ModelExecution.ProjectedInputTokens
					scope := engine.runningScope()
					scope.mu.Lock()
					snapshot := scope.state.contextLedger.Snapshot()
					scope.mu.Unlock()
					var err error
					expected, _, err = snapshot.Normalize(engine.activeRoute().Model().Capabilities)
					return err
				}
				if event.SampleContext != nil {
					copy := *event.SampleContext
					sampled = &copy
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(runtime.requests) != 1 || sampled == nil || projected == 0 {
				t.Fatalf("requests=%d sampled=%+v projected=%d", len(runtime.requests), sampled, projected)
			}
			if test.decision != nil && (len(runtime.required) != 2 || runtime.required[1] >= runtime.required[0]) {
				t.Fatalf("expected shrinking before admission: %v", runtime.required)
			}
			request := runtime.requests[0]
			if !reflect.DeepEqual(request.Messages, expected.Messages()) {
				t.Error("provider messages differ from the normalized final snapshot")
			}
			for _, message := range request.Messages {
				for _, block := range message.Blocks {
					if block.Type == provider.ContentReasoning {
						t.Error("fold restored reasoning unsupported by the model")
					}
				}
			}
			admitted := runtime.required[len(runtime.required)-1]
			if projected+request.MaxOutputTokens != admitted ||
				!reflect.DeepEqual(runtime.reserved, []uint64{admitted}) {
				t.Errorf("projected=%d output=%d admitted=%d reserved=%v", projected, request.MaxOutputTokens, admitted, runtime.reserved)
			}
			digest, err := expected.Digest()
			if err != nil {
				t.Fatal(err)
			}
			if sampled.ContextDigest != digest || sampled.ContextRevision != expected.Revision() ||
				request.Projection.ContextRevision != sampled.ContextRevision ||
				sampled.MessageCount != len(request.Messages) ||
				sampled.EstimatedTokens != projected || sampled.WindowOutputReserve != request.MaxOutputTokens {
				t.Errorf("sample attribution does not describe the final request: %+v", sampled)
			}
			routeDigest, propertyDigest := contextview.PrefixRequestIdentity(
				request.Route, request.MaxOutputTokens, request.ReasoningEffort, request.NativeSearch,
			)
			measurement, err := expected.MeasureDetailed("test", "", engine.options.TokenEstimator)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := contextview.BuildPrefixManifestFromMeasurement(expected, measurement, routeDigest, propertyDigest)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(engine.prefixManifest, manifest) ||
				sampled.RouteDigest != routeDigest || sampled.RequestPropertyDigest != propertyDigest {
				t.Error("prefix manifest does not describe the final normalized request")
			}
			if !strings.Contains(engine.history[0].Text(), "old ") {
				t.Error("throughput fold changed durable history")
			}
		})
	}
}

func TestThroughputFoldPreparationFailureDoesNotStartProvider(t *testing.T) {
	for _, stage := range []string{"before_fold", "after_fold", "prepare_input"} {
		t.Run(stage, func(t *testing.T) {
			runtime := &scriptedThroughputGovernor{decisions: []providerratelimit.Decision{{
				Status: providerratelimit.StatusRefuse,
				Reason: providerratelimit.ReasonExceedsBurst,
			}}}
			engine := newThroughputFoldEngine(t, runtime)
			estimator := engine.options.TokenEstimator
			failure := errors.New("folded input measurement failed")
			foldReported := false
			engine.options.TokenEstimator = agentcontext.EstimatorFunc(func(messages []provider.Message) (uint64, error) {
				fail := (stage == "before_fold" && len(runtime.required) != 0) ||
					(stage == "after_fold" && engine.viewFold.folded) ||
					(stage == "prepare_input" && foldReported)
				if fail {
					return 0, failure
				}
				return estimator.Estimate(messages)
			})
			started := false
			_, err := engine.Execute(t.Context(), TurnRequest{Prompt: "continue"}, func(event Event) error {
				if event.Compaction != nil && event.Compaction.TruncationReason == "throughput_tail_fold" {
					foldReported = true
				}
				if event.ModelExecution != nil && event.ModelExecution.Status == protocol.ProviderAttemptStarted {
					started = true
				}
				return nil
			})
			if !errors.Is(err, failure) {
				t.Fatalf("error=%v, want measurement failure", err)
			}
			if started || len(runtime.requests) != 0 || len(runtime.reserved) != 0 || len(runtime.required) != 1 {
				t.Fatalf("failed preparation continued: started=%t requests=%d reserved=%v required=%v", started, len(runtime.requests), runtime.reserved, runtime.required)
			}
		})
	}
}

func TestRateLimitRetryPropagatesFoldPreparationFailure(t *testing.T) {
	runtime := &scriptedThroughputGovernor{decisions: []providerratelimit.Decision{{
		Status: providerratelimit.StatusRefuse,
		Reason: providerratelimit.ReasonExceedsBurst,
	}}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	failure := errors.New("retry input preparation failed")
	err := engine.abortOversizedRateLimitRetry(
		t.Context(), engine.activeRoute(), 800,
		ProviderRetry{Failure: provider.Failure{Code: provider.FailureRateLimit}},
		func() (uint64, bool, error) { return 0, false, failure },
	)
	if !errors.Is(err, failure) {
		t.Fatalf("retry error=%v, want preparation failure", err)
	}
	if len(runtime.required) != 1 || len(runtime.reserved) != 0 {
		t.Fatalf("failed retry preparation continued: required=%v reserved=%v", runtime.required, runtime.reserved)
	}
}

func newThroughputFoldEngine(t *testing.T, runtime provider.Provider) *Engine {
	t.Helper()
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Route = mustTestRouteWithContext(t, 16384)
	engine.options.Context.RecentTailTurns = 3
	engine.options.RateLimitMaxWait = time.Second
	engine.turn = 2
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("old ", 100), 1),
		messageWithText(provider.RoleAssistant, strings.Repeat("answer ", 100), 1),
		messageWithText(provider.RoleUser, "second request", 2),
		{Role: provider.RoleAssistant, Turn: 2, Blocks: []provider.ContentBlock{
			{Type: provider.ContentReasoning, Text: "retained reasoning"},
			{Type: provider.ContentText, Text: "second answer"},
		}},
	}
	return engine
}
