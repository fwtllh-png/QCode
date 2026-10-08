package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type narrativeFunctionProvider struct {
	scriptedProvider
	summary func(context.Context, provider.ModelRequest) (provider.Stream, error)
}

type narrativeUsageFailureStream struct{ sent bool }

func (s *narrativeUsageFailureStream) Close() error { return nil }
func (s *narrativeUsageFailureStream) Recv() (provider.StreamEvent, error) {
	if !s.sent {
		s.sent = true
		return provider.StreamEvent{Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 3, OutputTokens: 1}}, nil
	}
	return provider.StreamEvent{}, protocol.NewProblem(protocol.CodeUnavailable, "temporary failure", true, &provider.Failure{Code: provider.FailureRateLimit, HTTPStatus: 429, RetryAfterMS: 1})
}

func TestNarrativeP3RetriesRetainEveryAttemptUsage(t *testing.T) {
	calls := 0
	p := &narrativeFunctionProvider{}
	p.summary = func(_ context.Context, r provider.ModelRequest) (provider.Stream, error) {
		calls++
		if calls == 1 {
			return &narrativeUsageFailureStream{}, nil
		}
		return coveredNarrativeStream(narrativeInputOf(t, r)), nil
	}
	e := newEngine(t, p, tool.NewRegistry(nil, nil))
	e.options.Context.SemanticNarrative = "post_turn"
	e.options.Context.Digest = "ledger+narrative"
	truth, input := mustNarrativeRequest(t, e)
	result, err := e.generateNarrativeForTest(t.Context(), truth, input, 2, "")
	if err != nil || len(result.Calls) != 2 || result.Usage.Total() != 14 {
		t.Fatalf("retry accounting: %+v %v", result, err)
	}
	if result.Calls[0].Attempt == result.Calls[1].Attempt || result.Calls[0].Usage.Total() != 4 {
		t.Fatal("failed physical attempt lost identity or usage")
	}
	if usage, _ := e.Usage(); usage.Total() != 14 {
		t.Fatalf("session usage=%+v", usage)
	}
}

func TestNarrativeP3ReadyCandidateWaitsForSafeBoundary(t *testing.T) {
	p := &narrativeFunctionProvider{scriptedProvider: scriptedProvider{streams: []provider.Stream{textStream("continue")}}, summary: func(_ context.Context, r provider.ModelRequest) (provider.Stream, error) {
		return coveredNarrativeStream(narrativeInputOf(t, r)), nil
	}}
	e := newEngine(t, p, tool.NewRegistry(nil, nil))
	e.options.Context.SemanticNarrative = "post_turn"
	e.options.Context.Digest = "ledger+narrative"
	seedOmittedHistory(e)
	prepared := e.PreparePostTurnNarrative("thread-1", "turn-3")
	e.mu.Lock()
	done := make(chan NarrativeGenerationResult, 1)
	go func() { result, _ := prepared.Run(t.Context()); done <- result }()
	select {
	case result := <-done:
		if result.Receipt == nil || result.Receipt.FallbackReason != "awaiting_safe_boundary" {
			t.Error("candidate did not wait for boundary")
		}
	case <-time.After(3 * time.Second):
		e.mu.Unlock()
		t.Fatal("generation waited for foreground lock")
	}
	if e.context.Compaction().Digest != nil {
		t.Error("candidate changed frozen context")
	}
	e.history = append(e.history, messageWithText(provider.RoleUser, "unrelated new turn", 4), messageWithText(provider.RoleAssistant, "done", 4))
	e.turn = 4
	e.mu.Unlock()
	if _, err := e.Run(t.Context(), "continue", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joinMessageText(p.requests[0].Messages), "保留原始问题编号和定义") {
		t.Fatal("valid earlier candidate was not installed at boundary")
	}
}

func TestNarrativeP3RestoreCancelsAndInvalidatesWork(t *testing.T) {
	started := make(chan struct{})
	p := &narrativeFunctionProvider{summary: func(ctx context.Context, _ provider.ModelRequest) (provider.Stream, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	e := newEngine(t, p, tool.NewRegistry(nil, nil))
	e.options.Context.SemanticNarrative = "post_turn"
	e.options.Context.Digest = "ledger+narrative"
	e.options.Context.NarrativeTimeout = time.Minute
	e.options.Workspace = t.TempDir()
	seedOmittedHistory(e)
	snapshot, err := e.ExportContextSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	prepared := e.PreparePostTurnNarrative("thread-1", "turn-3")
	done := make(chan NarrativeGenerationResult, 1)
	go func() { result, _ := prepared.Run(t.Context()); done <- result }()
	<-started
	if _, err := e.RestoreContextSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if !result.Fallback {
			t.Fatal("restored epoch accepted old result")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restore did not cancel optional provider work")
	}
	if e.context.Compaction().Digest != nil {
		t.Fatal("restored context contains invalidated narrative")
	}
}

func (p *narrativeFunctionProvider) Stream(ctx context.Context, request provider.ModelRequest) (provider.Stream, error) {
	if request.Purpose == "summary" {
		return p.summary(ctx, request)
	}
	return p.scriptedProvider.Stream(ctx, request)
}
func narrativeInputOf(t *testing.T, request provider.ModelRequest) agentcontext.NarrativeInputArtifact {
	t.Helper()
	var payload struct {
		Input agentcontext.NarrativeInputArtifact `json:"input"`
	}
	if err := json.Unmarshal([]byte(request.Messages[1].Text()), &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Input
}
func coveredNarrativeStream(input agentcontext.NarrativeInputArtifact) provider.Stream {
	ids := make([]string, len(input.Excerpts))
	for i, x := range input.Excerpts {
		ids[i] = x.MessageID
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(narrativePreferenceJSON("unused")), &body)
	body["preferences"] = []map[string]any{{"text": "保留原始问题编号和定义。", "source_message_ids": ids}}
	raw, _ := json.Marshal(body)
	return &providerfixture.SliceStream{Events: []provider.StreamEvent{{Type: provider.EventTextDelta, Text: string(raw)}, {Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 7, OutputTokens: 3}}, {Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn}}}
}

func TestNarrativeP3SplitsCompleteChineseSourceByMeasuredRequest(t *testing.T) {
	p := &narrativeFunctionProvider{}
	e := newEngine(t, p, tool.NewRegistry(nil, nil))
	e.options.Context.SemanticNarrative = "post_turn"
	e.options.Context.Digest = "ledger+narrative"
	body, err := os.ReadFile("../context/testdata/context_continuity_long_report_zh.txt")
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.TrimSpace(string(body)))
	truth, input := mustNarrativeRequest(t, e)
	input, err = agentcontext.BuildNarrativeInput("thread-1", "window-1", input.AuthorityDigest, input.RouteDigest, []provider.Message{messageWithText(provider.RoleAssistant, string(body), 1)}, e.options.Context.NarrativeLimits, time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	first := agentcontext.NarrativeInputPart(input, input.Excerpts[:1])
	request, _, err := agentcontext.PrepareNarrativeRequest(agentcontext.NarrativeGeneratorConfig{Routes: e.options.Routes, TokenEstimator: e.options.TokenEstimator, Limits: e.options.Context.NarrativeLimits}, truth, first)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := e.options.TokenEstimator.Estimate(request.Messages)
	e.options.Context.NarrativeLimits.MaxInputTokens = base + 100
	var excerpts []agentcontext.NarrativeExcerpt
	var deadline time.Time
	calls := 0
	p.summary = func(ctx context.Context, request provider.ModelRequest) (provider.Stream, error) {
		calls++
		current, _ := ctx.Deadline()
		if deadline.IsZero() {
			deadline = current
		} else if !current.Equal(deadline) {
			t.Error("chunk reset the job deadline")
		}
		tokens, _ := e.options.TokenEstimator.Estimate(request.Messages)
		if tokens > e.options.Context.NarrativeLimits.MaxInputTokens {
			t.Errorf("request tokens %d exceed %d", tokens, e.options.Context.NarrativeLimits.MaxInputTokens)
		}
		part := narrativeInputOf(t, request)
		excerpts = append(excerpts, part.Excerpts...)
		return coveredNarrativeStream(part), nil
	}
	result, err := e.generateNarrativeForTest(t.Context(), truth, input, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 || len(result.Calls) != calls || result.Usage.InputTokens != uint64(calls*7) {
		t.Fatalf("calls=%d result=%+v", calls, result)
	}
	var rebuilt strings.Builder
	end := 0
	for _, x := range excerpts {
		if x.Source.Start != end {
			t.Fatal("gap or duplicate range")
		}
		rebuilt.WriteString(x.Text)
		end = x.Source.End
	}
	if rebuilt.String() != string(body) {
		t.Fatal("provider requests lost source text")
	}
	if len(result.Artifact.Coverage) != len(excerpts) {
		t.Fatal("coverage differs from provider inputs")
	}
}

func TestNarrativeP3InvalidOutputKeepsUsageAndDeduplicatesFailure(t *testing.T) {
	calls := 0
	p := &narrativeFunctionProvider{summary: func(context.Context, provider.ModelRequest) (provider.Stream, error) {
		calls++
		return &providerfixture.SliceStream{Events: []provider.StreamEvent{{Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 17, OutputTokens: 5}}, {Type: provider.EventTextDelta, Text: "invalid JSON"}, {Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn}}}, nil
	}}
	e := newEngine(t, p, tool.NewRegistry(nil, nil))
	e.options.Context.SemanticNarrative = "post_turn"
	e.options.Context.Digest = "ledger+narrative"
	seedOmittedHistory(e)
	prepared := e.PreparePostTurnNarrative("thread-1", "turn-3")
	result, err := prepared.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Fallback || result.Usage.Total() != 22 || len(result.Calls) != 1 || result.CostUSD <= 0 {
		t.Fatalf("lost failed usage: %+v", result)
	}
	again, err := prepared.Run(t.Context())
	if err != nil || again.Usage.Total() != 22 || calls != 1 {
		t.Fatal("runner was not idempotent")
	}
	usage, _ := e.Usage()
	if usage.Total() != 22 {
		t.Fatalf("usage=%+v", usage)
	}
	if next := e.PreparePostTurnNarrative("thread-1", "turn-3"); next != nil {
		t.Fatal("same exhausted input was scheduled again")
	}
	if e.context.Compaction().Digest != nil {
		t.Fatal("invalid candidate installed")
	}
}

func TestNarrativeP3ForegroundDoesNotJoinProviderCleanup(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	p := &narrativeFunctionProvider{scriptedProvider: scriptedProvider{streams: []provider.Stream{textStream("new answer")}}, summary: func(ctx context.Context, _ provider.ModelRequest) (provider.Stream, error) {
		close(started)
		<-release
		return nil, ctx.Err()
	}}
	e := newEngine(t, p, tool.NewRegistry(nil, nil))
	e.options.Context.SemanticNarrative = "post_turn"
	e.options.Context.Digest = "ledger+narrative"
	e.options.Context.NarrativeTimeout = time.Minute
	e.options.SharedRateLimit = NewSharedRateLimit(2)
	seedOmittedHistory(e)
	prepared := e.PreparePostTurnNarrative("thread-1", "turn-3")
	done := make(chan struct{})
	go func() { defer close(done); _, _ = prepared.Run(t.Context()) }()
	<-started
	foreground := make(chan error, 1)
	go func() { _, err := e.Run(t.Context(), "continue", nil); foreground <- err }()
	select {
	case err := <-foreground:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(3 * time.Second):
		t.Error("foreground joined unfinished narrative")
	}
	close(release)
	<-done
}

func TestNarrativeP3CandidatePreservesNewPlanAndRejectsEpochChange(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(map[bool]string{false: "new_plan", true: "restore"}[restore], func(t *testing.T) {
			p := &narrativeFunctionProvider{summary: func(_ context.Context, r provider.ModelRequest) (provider.Stream, error) {
				return coveredNarrativeStream(narrativeInputOf(t, r)), nil
			}}
			e := newEngine(t, p, tool.NewRegistry(nil, nil))
			e.options.Context.SemanticNarrative = "post_turn"
			e.options.Context.Digest = "ledger+narrative"
			seedOmittedHistory(e)
			prepared := e.PreparePostTurnNarrative("thread-1", "turn-3")
			if err := e.ApplyPlan(interact.Plan{Objective: "new user correction", Steps: []interact.PlanStep{{Title: "keep new progress", Status: interact.StepDone}}}); err != nil {
				t.Fatal(err)
			}
			if restore {
				e.stateEpoch++
			}
			result, err := prepared.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if restore != result.Fallback {
				t.Fatalf("result=%+v", result)
			}
			if e.currentPlan().Objective != "new user correction" {
				t.Fatal("candidate overwrote new plan")
			}
			if usage, _ := e.Usage(); usage.Total() != 10 {
				t.Fatal("discarded candidate lost usage")
			}
		})
	}
}

type narrativeCommitStore struct {
	withdrawalContextStore
	commits []agentcontext.CurrentContextCommit
	failure error
}

func (s *narrativeCommitStore) CommitCurrentContext(_ context.Context, commit agentcontext.CurrentContextCommit) error {
	if s.failure != nil {
		return s.failure
	}
	s.commits = append(s.commits, commit)
	return commit.Validate()
}

func TestNarrativeP3CommitBeforeInstall(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "failed_commit"}[fail], func(t *testing.T) {
			p := &narrativeFunctionProvider{summary: func(_ context.Context, r provider.ModelRequest) (provider.Stream, error) {
				return coveredNarrativeStream(narrativeInputOf(t, r)), nil
			}}
			e := newEngine(t, p, tool.NewRegistry(nil, nil))
			e.options.Context.SemanticNarrative = "post_turn"
			e.options.Context.Digest = "ledger+narrative"
			e.options.Workspace = t.TempDir()
			seedOmittedHistory(e)
			store := &narrativeCommitStore{}
			if fail {
				store.failure = errors.New("CAS conflict")
			}
			e.options.TurnContexts = store
			result, err := e.runPreparedNarrativeForTest(t.Context(), "thread-1", "turn-3")
			if err != nil {
				t.Fatal(err)
			}
			if fail != result.Fallback || (e.context.Compaction().Digest == nil) != fail {
				t.Fatalf("commit result=%+v", result)
			}
			if !fail {
				if len(store.commits) != 1 || store.commits[0].BaseRevision == nil || store.commits[0].Snapshot.Compaction.Digest == nil {
					t.Fatal("missing durable representation")
				}
			}
		})
	}
}
