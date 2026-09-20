package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestNarrativeReasoningEffortPrefersOff(t *testing.T) {
	capabilities := model.Capabilities{
		Reasoning: true, ReasoningEfforts: []string{"off", "low"},
	}
	if effort := agentcontext.NarrativeReasoningEffort(capabilities); effort != "off" {
		t.Fatalf("narrative reasoning effort = %q", effort)
	}
}

func TestNarrativeGenerationUsesSummaryRouteWithoutTools(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{
				Type: provider.EventReasoningDelta,
				Text: "identify narrative-only claims",
			},
			{
				Type: provider.EventReasoningSignature,
				Text: "opaque-signature",
			},
			{Type: provider.EventReplayState},
			{
				Type: provider.EventTextDelta,
				Text: `{"technical_concepts":[],"files_and_code":[],"errors_and_fixes":[],"pending_jobs":[],"current_work":[],"next_steps":[],"critical_context":[],"decisions":[],"rationale":[],"preferences":[{"text":"Prefer deterministic state.","source_message_ids":["SOURCE"]}],"unresolved":[]}`,
			},
			{Type: provider.EventUsage, Usage: &provider.Usage{
				InputTokens: 40, OutputTokens: 12,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn},
		}},
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.SemanticNarrative = "post_turn"
	routeDigest, err := engine.SummaryRouteDigest()
	if err != nil {
		t.Fatal(err)
	}
	truth := engine.buildTruthCapsule(agentcontext.Summary{Goal: "continue"}, nil)
	authority, err := truth.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	input, err := agentcontext.BuildNarrativeInput(
		"thread-1",
		"window-1",
		authority,
		routeDigest,
		[]provider.Message{
			messageWithText(provider.RoleUser, "prefer deterministic state", 1),
		},
		engine.options.Context.NarrativeLimits,
		time.Now().UTC(),
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	for index := range runtime.streams[0].(*providerfixture.SliceStream).Events {
		event := &runtime.streams[0].(*providerfixture.SliceStream).Events[index]
		if event.Type == provider.EventTextDelta {
			event.Text =
				`{"technical_concepts":[],"files_and_code":[],"errors_and_fixes":[],"pending_jobs":[],"current_work":[],"next_steps":[],"critical_context":[],"decisions":[],"rationale":[],"preferences":[{"text":"Prefer deterministic state.","source_message_ids":["` +
					input.Excerpts[0].MessageID + `"]}],"unresolved":[]}`
		}
	}
	result, err := engine.GenerateNarrative(
		t.Context(),
		truth,
		input,
		2,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Fallback || len(result.Artifact.Body.Items) != 1 ||
		result.Usage.InputTokens != 40 ||
		len(runtime.requests) != 1 ||
		runtime.requests[0].Purpose != "summary" ||
		runtime.requests[0].MaxOutputTokens !=
			engine.options.Route.Model().Limits.MaxOutputTokens ||
		len(runtime.requests[0].Tools) != 0 ||
		runtime.requests[0].NativeSearch ||
		!strings.Contains(
			runtime.requests[0].Messages[0].Text(),
			"files_and_code",
		) ||
		!strings.Contains(
			runtime.requests[0].Messages[0].Text(),
			"next_steps",
		) ||
		!strings.Contains(
			runtime.requests[0].Messages[0].Text(),
			"purpose=deliverable",
		) {
		t.Fatalf("result=%+v request=%+v", result, runtime.requests)
	}
}

func TestNarrativeGenerationHonorsOperatorOutputCeiling(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn},
		}},
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.SemanticNarrative = "post_turn"
	engine.options.Context.NarrativeLimits.MaxOutputBytes = 512 * 4
	truth, input := mustNarrativeRequest(t, engine)
	runtime.streams[0].(*providerfixture.SliceStream).Events[0].Text =
		narrativePreferenceJSON(input.Excerpts[0].MessageID)
	result, err := engine.GenerateNarrative(t.Context(), truth, input, 2, "")
	// 2048 output bytes are a ceil(2048/3) = 683-token ceiling at
	// dense-script density.
	if err != nil || result.Fallback ||
		len(runtime.requests) != 1 ||
		runtime.requests[0].MaxOutputTokens != 683 {
		t.Fatalf("result=%+v request=%+v err=%v", result, runtime.requests, err)
	}
}

func TestNarrativeRetriesCompatibleQuota429ThenSucceeds(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&errorStream{err: protocol.NewProblem(
			protocol.CodeUnavailable,
			`provider returned HTTP 429: {"error":{"message":"Allocated quota exceeded, please increase your quota limit.","type":"invalid_request_error","code":"insufficient_quota"}}`,
			true,
			&provider.Failure{
				Code: provider.FailureRateLimit, HTTPStatus: 429,
				Message:      "Allocated quota exceeded, please increase your quota limit.",
				RetryAfterMS: 1,
			},
		)},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta},
			{Type: provider.EventUsage, Usage: &provider.Usage{
				InputTokens: 8, OutputTokens: 3,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn},
		}},
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.SemanticNarrative = "post_turn"
	engine.options.Context.NarrativeTimeout = time.Second
	truth, input := mustNarrativeRequest(t, engine)
	runtime.streams[1].(*providerfixture.SliceStream).Events[0].Text =
		narrativePreferenceJSON(input.Excerpts[0].MessageID)
	result, err := engine.GenerateNarrative(t.Context(), truth, input, 2, "")
	if err != nil || result.Fallback || result.Attempt != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(runtime.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(runtime.requests))
	}
}

func TestNarrativeDoesNotRetryHardQuota(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&errorStream{err: protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"billing hard limit",
			false,
			&provider.Failure{
				Code: provider.FailureQuota, Message: "billing hard limit",
			},
		)},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: `{}`},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn},
		}},
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.SemanticNarrative = "post_turn"
	truth, input := mustNarrativeRequest(t, engine)
	_, err := engine.GenerateNarrative(t.Context(), truth, input, 2, "")
	if err == nil || !strings.Contains(err.Error(), "billing hard limit") {
		t.Fatalf("err = %v", err)
	}
	if len(runtime.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(runtime.requests))
	}
}

func mustNarrativeRequest(
	t *testing.T,
	engine *Engine,
) (agentcontext.TruthCapsule, agentcontext.NarrativeInputArtifact) {
	t.Helper()
	routeDigest, err := engine.SummaryRouteDigest()
	if err != nil {
		t.Fatal(err)
	}
	truth := engine.buildTruthCapsule(agentcontext.Summary{Goal: "continue"}, nil)
	authority, err := truth.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	input, err := agentcontext.BuildNarrativeInput(
		"thread-1",
		"window-1",
		authority,
		routeDigest,
		[]provider.Message{
			messageWithText(provider.RoleUser, "prefer deterministic state", 1),
		},
		engine.options.Context.NarrativeLimits,
		time.Now().UTC(),
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	return truth, input
}

func narrativePreferenceJSON(source string) string {
	return `{"technical_concepts":[],"files_and_code":[],"errors_and_fixes":[],"pending_jobs":[],"current_work":[],"next_steps":[],"critical_context":[],"decisions":[],"rationale":[],"preferences":[{"text":"Prefer deterministic state.","source_message_ids":["` +
		source + `"]}],"unresolved":[]}`
}

func TestNarrativeOffDoesNotReplaceHistoryOnSamplePath(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.SemanticNarrative = "off"
	engine.options.Context.Window.AutoTokens = 300
	scope := attachTestScope(t, engine)
	scope.spec.Identity = TurnIdentity{
		SessionID: "session-1", ThreadID: "thread-1",
		TurnID: "turn-2", ProfileRevision: 1,
	}
	scope.state.kernel = newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		telemetry.NewMetrics(),
	)
	history := []provider.Message{
		messageWithText(
			provider.RoleUser,
			"retain deterministic facts "+strings.Repeat("old ", 200),
			1,
		),
		messageWithText(
			provider.RoleAssistant,
			strings.Repeat("answer ", 200),
			1,
		),
		messageWithText(provider.RoleUser, "continue", 2),
	}
	input := agentcontext.NewMessageLedger(
		agentcontext.LedgerInput{History: history},
	).Snapshot()
	if _, err := engine.runCompactGate(
		t.Context(),
		&history,
		input,
		128,
		CompactionPhaseMidTurn,
		true,
		func(State, Event) error { return nil },
		0,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(history[0].Text(), agentcontext.TruthMarkerStart) {
		t.Fatalf("sample path replaced history: %q", history[0].Text())
	}
}

func TestTerminalTailBudgetForcesDeterministicReplacement(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.SemanticNarrative = "off"
	engine.options.Context.Window.AutoTokens = 1 << 20
	engine.options.Context.RecentTailTurns = 2
	engine.options.Context.RecentTailMaxTokens = 128
	scope := attachTestScope(t, engine)
	scope.spec.Identity = TurnIdentity{
		SessionID: "session-1", ThreadID: "thread-1",
		TurnID: "turn-1", ProfileRevision: 1,
	}
	scope.state.kernel = newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		telemetry.NewMetrics(),
	)
	history := []provider.Message{
		messageWithText(
			provider.RoleUser,
			"a large request "+strings.Repeat("context ", 400),
			1,
		),
		messageWithText(provider.RoleAssistant, "completed answer", 1),
	}
	originalBytes := agentcontext.HistoryBytes(history)
	if err := engine.runTerminalCompactGate(
		&history,
		true,
		func(State, Event) error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	if agentcontext.HistoryBytes(history) >= originalBytes ||
		!strings.Contains(history[0].Text(), agentcontext.TruthMarkerStart) {
		t.Fatalf(
			"original_bytes=%d retained_bytes=%d history=%v",
			originalBytes,
			agentcontext.HistoryBytes(history),
			history,
		)
	}
}

func TestPostTurnNarrativeTimeoutDoesNotBlockNextSample(t *testing.T) {
	runtime := &hangingSummaryProvider{
		started: make(chan struct{}),
		scriptedProvider: scriptedProvider{streams: []provider.Stream{
			textStream("next sample"),
		}},
	}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.SemanticNarrative = "post_turn"
	engine.options.Context.NarrativeTimeout = 40 * time.Millisecond
	engine.options.Workspace = t.TempDir()
	seedOmittedHistory(engine)
	if err := engine.ApplyPlan(interact.Plan{
		Objective: "teach the parser about trailing commas",
		Steps: []interact.PlanStep{{
			Title: "update the lexer", Status: interact.StepInProgress,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	before := joinMessageText(engine.History())
	done := make(chan NarrativeGenerationResult, 1)
	go func() {
		result, err := engine.RunPostTurnNarrative(
			t.Context(), "thread-1", "turn-1",
		)
		if err != nil {
			t.Error(err)
		}
		done <- result
	}()
	select {
	case <-runtime.started:
	case <-time.After(time.Second):
		t.Fatal("narrative provider was not entered")
	}
	started := time.Now()
	if _, err := engine.Run(t.Context(), "continue now", nil); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("next sample waited %s for narrative", time.Since(started))
	}
	result := <-done
	if !result.Fallback || result.FailureReason == "" {
		t.Fatalf("timeout result = %+v", result)
	}
	after := joinMessageText(engine.History())
	if !strings.Contains(after, "I prefer deterministic state") ||
		after == "" ||
		!strings.Contains(before, "I prefer deterministic state") {
		t.Fatalf("timeout replaced history: %s", after)
	}
	joined := joinMessageText(runtime.requests[len(runtime.requests)-1].Messages)
	if !strings.Contains(joined, "teach the parser about trailing commas") {
		t.Fatalf("ledger digest missing after timeout: %s", joined)
	}
}

func TestPostTurnNarrativeSkipsCanceledTurnWithoutProviderCall(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("should not run"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.SemanticNarrative = "post_turn"
	engine.options.Workspace = t.TempDir()
	seedOmittedHistory(engine)
	engine.sealClosedTurnMemory(
		agentcontext.CheckpointCanceled, nil, "canceled",
	)
	result, err := engine.RunPostTurnNarrative(
		t.Context(), "thread-1", "turn-3",
	)
	if err != nil || result.Receipt != nil || result.Usage.Total() != 0 {
		t.Fatalf("canceled post-turn = %+v err=%v", result, err)
	}
	for _, request := range runtime.requests {
		if request.Purpose == "summary" {
			t.Fatalf("canceled turn called narrative provider: %+v", request)
		}
	}
}

func TestPostTurnNarrativeSuccessIsPartitionNotReplacement(t *testing.T) {
	runtime := &sourceEchoNarrativeProvider{
		scriptedProvider: scriptedProvider{streams: []provider.Stream{
			textStream("continue"),
		}},
	}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.SemanticNarrative = "post_turn"
	engine.options.Workspace = t.TempDir()
	seedOmittedHistory(engine)
	if err := engine.ApplyPlan(interact.Plan{
		Objective: "teach the parser about trailing commas",
		Steps: []interact.PlanStep{{
			Title: "update the lexer", Status: interact.StepInProgress,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := engine.RunPostTurnNarrative(
		t.Context(), "thread-1", "turn-1",
	)
	if err != nil || result.Fallback || result.Receipt == nil ||
		!result.Receipt.NarrativeIncluded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !strings.Contains(
		joinMessageText(engine.History()),
		"I prefer deterministic state",
	) {
		t.Fatalf("success replaced history: %q", engine.History()[0].Text())
	}
	if _, err := engine.Run(t.Context(), "continue now", nil); err != nil {
		t.Fatal(err)
	}
	joined := joinMessageText(runtime.requests[len(runtime.requests)-1].Messages)
	if !strings.Contains(joined, "Prefer deterministic state.") ||
		!strings.Contains(joined, "teach the parser about trailing commas") {
		t.Fatalf("next sample missing digest partition: %s", joined)
	}
}

func TestManualCompactPassesFocusToNarrative(t *testing.T) {
	runtime := &sourceEchoNarrativeProvider{}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.SemanticNarrative = "post_turn"
	engine.options.Workspace = t.TempDir()
	engine.history = []provider.Message{
		messageWithText(
			provider.RoleUser,
			"I prefer deterministic state "+strings.Repeat("old ", 200),
			1,
		),
		messageWithText(
			provider.RoleAssistant,
			strings.Repeat("answer ", 200),
			1,
		),
		messageWithText(provider.RoleUser, "continue", 2),
		messageWithText(provider.RoleAssistant, "ready", 2),
		messageWithText(provider.RoleUser, "keep going", 3),
	}
	beforeBytes := agentcontext.HistoryBytes(engine.history)
	result, err := engine.CompactForcedDurable(
		t.Context(),
		"thread-1",
		"turn-1",
		"parser trailing commas",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Receipt == nil ||
		agentcontext.HistoryBytes(engine.history) >= beforeBytes ||
		!strings.Contains(engine.history[0].Text(), agentcontext.TruthMarkerStart) {
		t.Fatalf("manual compact = %+v history=%q", result, engine.history[0].Text())
	}
	if runtime.lastPayload() == "" ||
		!strings.Contains(runtime.lastPayload(), "parser trailing commas") {
		t.Fatalf("focus missing from narrative request: %q", runtime.lastPayload())
	}
}

func TestCompactForcedDurableAppliesHistoryWithoutRebaseCommit(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.SemanticNarrative = "post_turn"
	engine.options.Workspace = t.TempDir()
	engine.options.WorkspaceIdentity = "workspace:test"
	engine.history = []provider.Message{
		messageWithText(
			provider.RoleUser,
			"old context "+strings.Repeat("detail ", 300),
			1,
		),
		messageWithText(
			provider.RoleAssistant,
			strings.Repeat("answer ", 300),
			1,
		),
		messageWithText(provider.RoleUser, "continue", 2),
	}
	beforeBytes := agentcontext.HistoryBytes(engine.history)

	result, err := engine.CompactForcedDurable(
		t.Context(),
		"thread-1",
		"turn-1",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Receipt == nil ||
		agentcontext.HistoryBytes(engine.history) >= beforeBytes ||
		!strings.Contains(engine.history[0].Text(), agentcontext.TruthMarkerStart) {
		t.Fatalf(
			"result=%+v history_bytes=%d/%d history=%q",
			result,
			agentcontext.HistoryBytes(engine.history),
			beforeBytes,
			engine.history[0].Text(),
		)
	}
}

func seedOmittedHistory(engine *Engine) {
	engine.history = []provider.Message{
		messageWithText(
			provider.RoleUser,
			"I prefer deterministic state "+strings.Repeat("old ", 20),
			1,
		),
		messageWithText(provider.RoleAssistant, "first answer", 1),
		messageWithText(provider.RoleUser, "second", 2),
		messageWithText(provider.RoleAssistant, "second answer", 2),
		messageWithText(provider.RoleUser, "third", 3),
		messageWithText(provider.RoleAssistant, "third answer", 3),
	}
	engine.turn = 3
}

type hangingSummaryProvider struct {
	started chan struct{}
	scriptedProvider
}

func (p *hangingSummaryProvider) Stream(
	ctx context.Context,
	request provider.ModelRequest,
) (provider.Stream, error) {
	if request.Purpose == "summary" {
		select {
		case <-p.started:
		default:
			close(p.started)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.scriptedProvider.Stream(ctx, request)
}

type sourceEchoNarrativeProvider struct {
	scriptedProvider
	payloads []string
}

func (p *sourceEchoNarrativeProvider) lastPayload() string {
	if len(p.payloads) == 0 {
		return ""
	}
	return p.payloads[len(p.payloads)-1]
}

func (p *sourceEchoNarrativeProvider) Stream(
	ctx context.Context,
	request provider.ModelRequest,
) (provider.Stream, error) {
	if request.Purpose != "summary" {
		return p.scriptedProvider.Stream(ctx, request)
	}
	if len(request.Messages) < 2 {
		return nil, errors.New("unexpected narrative request")
	}
	p.payloads = append(p.payloads, request.Messages[1].Text())
	var payload struct {
		Input agentcontext.NarrativeInputArtifact `json:"input"`
	}
	if err := json.Unmarshal(
		[]byte(request.Messages[1].Text()),
		&payload,
	); err != nil {
		return nil, err
	}
	if len(payload.Input.Excerpts) == 0 {
		return nil, errors.New("narrative request has no excerpts")
	}
	source := payload.Input.Excerpts[0].MessageID
	for _, excerpt := range payload.Input.Excerpts {
		if excerpt.Role == provider.RoleTool {
			source = excerpt.MessageID
			break
		}
	}
	body, _ := json.Marshal(map[string]any{
		"technical_concepts": []any{},
		"files_and_code": []map[string]any{{
			"text": "parser.go defines Parse.",
			"source_message_ids": []string{
				source,
			},
		}},
		"errors_and_fixes": []any{},
		"pending_jobs":     []any{},
		"current_work": []map[string]any{{
			"text": "Implement parser.go.",
			"source_message_ids": []string{
				source,
			},
		}},
		"next_steps": []map[string]any{{
			"text": "Write the parser implementation.",
			"source_message_ids": []string{
				source,
			},
		}},
		"critical_context": []any{},
		"decisions":        []any{},
		"rationale":        []any{},
		"preferences": []map[string]any{{
			"text": "Prefer deterministic state.",
			"source_message_ids": []string{
				payload.Input.Excerpts[0].MessageID,
			},
		}},
		"unresolved": []any{},
	})
	return &providerfixture.SliceStream{Events: []provider.StreamEvent{
		{Type: provider.EventTextDelta, Text: string(body)},
		{Type: provider.EventUsage, Usage: &provider.Usage{
			InputTokens: 9, OutputTokens: 3,
		}},
		{Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn},
	}}, nil
}

func TestPreparePostTurnNarrativeJoinIsBounded(t *testing.T) {
	runtime := &hangingSummaryProvider{
		started: make(chan struct{}),
		scriptedProvider: scriptedProvider{streams: []provider.Stream{
			textStream("next sample"), textStream("next sample again"),
		}},
	}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.SemanticNarrative = "post_turn"
	engine.options.Context.NarrativeTimeout = 40 * time.Millisecond
	engine.options.Workspace = t.TempDir()
	seedOmittedHistory(engine)

	prepared := engine.PreparePostTurnNarrative("thread-1", "turn-1")
	if prepared == nil {
		t.Fatal("prepared narrative is nil for a completed turn")
	}
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		if _, err := prepared.Run(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-runtime.started:
	case <-time.After(time.Second):
		t.Fatal("narrative provider was not entered")
	}
	// Execute joins the hanging narrative and waits for its settlement; the
	// wait is bounded by the narrative timeout that unblocks the provider.
	started := time.Now()
	if _, err := engine.Run(t.Context(), "continue now", nil); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("next turn waited %s for hanging narrative", waited)
	}
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("narrative did not settle within its timeout")
	}
	// The pending slot is released after settlement, so a follow-up turn
	// starts without waiting.
	started = time.Now()
	if _, err := engine.Run(t.Context(), "continue again", nil); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("follow-up turn waited %s after narrative settled", waited)
	}
}

func TestPreparePostTurnNarrativeSnapshotExcludesLaterTurns(t *testing.T) {
	runtime := &sourceEchoNarrativeProvider{
		scriptedProvider: scriptedProvider{streams: []provider.Stream{
			textStream("first"), textStream("second"), textStream("third"),
		}},
	}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.SemanticNarrative = "post_turn"
	engine.options.Context.NarrativeTimeout = 40 * time.Millisecond
	engine.options.Workspace = t.TempDir()
	seedOmittedHistory(engine)

	prepared := engine.PreparePostTurnNarrative("thread-1", "turn-1")
	if prepared == nil {
		t.Fatal("prepared narrative is nil for a completed turn")
	}
	// A follow-up turn joins the pending narrative and then appends fresh
	// history; the settled input must still be the prepared snapshot.
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		if _, err := prepared.Run(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	if _, err := engine.Run(t.Context(), "brand new request text", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("narrative did not settle")
	}
	payload := runtime.lastPayload()
	if !strings.Contains(payload, "I prefer deterministic state") {
		t.Fatalf("snapshot lost the omitted history: %s", payload)
	}
	if strings.Contains(payload, "brand new request text") {
		t.Fatalf("narrative snapshot captured a later turn: %s", payload)
	}
}
