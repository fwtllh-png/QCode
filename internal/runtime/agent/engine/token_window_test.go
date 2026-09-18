package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"reflect"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	contextview "github.com/fwtllh-png/QCode/internal/runtime/agent/contextview"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type finishProcessTool struct{}

func (finishProcessTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "exec_command", Description: "finish the current command",
		Visibility: tool.VisibleModel, Capability: tool.CapabilityProcess,
		AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelSerial,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{},
			"additionalProperties": false,
		},
	}
}

func (finishProcessTool) Execute(
	context.Context,
	json.RawMessage,
) (tool.Result, error) {
	return tool.Result{Content: "command complete"}, nil
}

func TestTokenWindowIncludesStableDynamicToolsAndOutputReserve(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	input := agentcontext.NewMessageLedger(agentcontext.LedgerInput{
		Stable:  []provider.Message{provider.TextMessage(provider.RoleSystem, strings.Repeat("s", 400))},
		History: []provider.Message{provider.TextMessage(provider.RoleUser, strings.Repeat("h", 400))},
		Dynamic: []provider.Message{provider.TextMessage(provider.RoleSystem, strings.Repeat("d", 400))},
		Definitions: []provider.ToolDefinition{{
			Name: "lookup", Description: strings.Repeat("schema", 100),
		}},
	}).Snapshot()
	total, err := engine.measureTokenWindow(input, 128, 0)
	if err != nil {
		t.Fatal(err)
	}
	engine.options.Context.Window.Scope = compactScopeBodyAfterPrefix
	body, err := engine.measureTokenWindow(input, 128, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total.total != total.estimated+128 || total.active != body.active ||
		body.accounting.PendingTokens != body.estimated {
		t.Fatalf("total=%+v body=%+v", total, body)
	}
}

func TestTokenWindowUsesObservedBaselineForPendingDelta(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	attachTestScope(t, engine)
	first := protocol.SampleContextData{
		ContextDigest: "sha256:first", EstimatedTokens: 100,
	}
	engine.prepareTokenWindow(&first, 20)
	engine.observeTokenWindow(&first, 150, 50)
	second := protocol.SampleContextData{
		ContextDigest: "sha256:second", EstimatedTokens: 200,
	}
	projected := engine.prepareTokenWindow(&second, 20)
	if projected.FullActiveTokens != 250 || projected.PrefillTokens != 150 ||
		projected.BodyTokens != 100 || projected.PendingTokens != 100 ||
		!projected.Observed {
		t.Fatalf("observed projection=%+v", projected)
	}
	actualNextInput := uint64(245)
	errorRate := float64(absDiff(projected.FullActiveTokens, actualNextInput)) /
		float64(actualNextInput)
	if errorRate > 0.05 {
		t.Fatalf("compaction trigger error=%f projection=%+v", errorRate, projected)
	}
}

func TestEconomicWindowCountsCachedPrefixAgainstTokenBudget(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	attachTestScope(t, engine)
	observed := protocol.SampleContextData{
		ContextDigest: "sha256:observed", EstimatedTokens: 100,
	}
	engine.prepareTokenWindow(&observed, 20)
	engine.observeTokenWindow(&observed, 150, 100)
	engine.options.Context.Window.Scope = compactScopeBodyAfterPrefix
	input := agentcontext.NewMessageLedger(agentcontext.LedgerInput{
		History: []provider.Message{provider.TextMessage(
			provider.RoleUser, strings.Repeat("budgeted ", 160),
		)},
	}).Snapshot()
	window, err := engine.measureTokenWindow(input, 20, 120)
	if err != nil {
		t.Fatal(err)
	}
	if window.active != window.accounting.FullActiveTokens ||
		window.compactLimit > 120 {
		t.Fatalf("economic window = %+v", window)
	}
}

func TestCompactionAndReplacementAdvanceTokenWindow(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	first := engine.context.Window()
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("old ", 300), 1),
		messageWithText(provider.RoleAssistant, "old answer", 1),
		messageWithText(provider.RoleUser, "current", 2),
	}
	if receipt := engine.CompactForced(); receipt == nil {
		t.Fatal("forced compaction produced no receipt")
	}
	window := engine.context.Window()
	if window.ID == first.ID || window.Number != first.Number+1 ||
		window.PrefillObserved {
		t.Fatalf("compacted window=%+v first=%+v", window, first)
	}
	compacted := window
	engine.ReplaceHistory([]provider.Message{
		messageWithText(provider.RoleUser, "replacement", 3),
	})
	window = engine.context.Window()
	if window.ID == compacted.ID ||
		window.Number != compacted.Number+1 {
		t.Fatalf("replacement window=%+v compacted=%+v", window, compacted)
	}
}

func TestCompactionCutsPreserveConfiguredRecentTurns(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.RecentTailTurns = 2
	engine.options.Context.RecentTailMaxTokens = 1 << 20
	history := []provider.Message{
		messageWithText(provider.RoleSystem, "summary", 0),
		messageWithText(provider.RoleUser, "one", 1),
		messageWithText(provider.RoleAssistant, "done one", 1),
		messageWithText(provider.RoleUser, "two", 2),
		messageWithText(provider.RoleAssistant, "done two", 2),
		messageWithText(provider.RoleUser, "three", 3),
		messageWithText(provider.RoleAssistant, "done three", 3),
	}
	cuts := agentcontext.RetainedTailCuts(
		history,
		false,
		engine.options.Context.RecentTailTurns,
		engine.options.Context.RecentTailMaxTokens,
		estimateMessageTokens,
	)
	if len(cuts) == 0 || cuts[len(cuts)-1] != 3 {
		t.Fatalf("cuts=%v, want latest cut at start of final two turns", cuts)
	}
}

func TestCompactionTailTokenLimitOverridesRecentTurnCount(t *testing.T) {
	history := []provider.Message{
		messageWithText(provider.RoleUser, "old", 1),
		messageWithText(provider.RoleAssistant, "old answer", 1),
		messageWithText(
			provider.RoleUser,
			strings.Repeat("large current context ", 200),
			2,
		),
		messageWithText(provider.RoleAssistant, "current answer", 2),
	}
	cuts := agentcontext.RetainedTailCuts(
		history,
		true,
		2,
		64,
		estimateMessageTokens,
	)
	if len(cuts) == 0 || cuts[len(cuts)-1] != len(history)-1 {
		t.Fatalf(
			"cuts=%v, want tail budget to permit a cut inside recent turns",
			cuts,
		)
	}
}

func TestHeuristicEstimatorAccountsForImageTilesByKind(t *testing.T) {
	imageBytes := encodePNG(t, 512, 512)
	attachment := provider.Attachment{
		Name: "fixture.png", MediaType: "image/png", Data: imageBytes,
	}
	estimator := HeuristicTokenEstimator{}
	imageTokens, err := estimator.EstimateImage(attachment)
	if err != nil {
		t.Fatal(err)
	}
	if imageTokens != 255 {
		t.Fatalf("512x512 image tokens=%d, want 255", imageTokens)
	}
	message := provider.Message{
		Role: provider.RoleUser,
		Blocks: []provider.ContentBlock{{
			Type: provider.ContentImage, Attachment: &attachment,
		}},
	}
	measured, err := agentcontext.NewMessageLedger(agentcontext.LedgerInput{
		History: []provider.Message{message},
	}).Snapshot().Measure("", "", estimator)
	if err != nil {
		t.Fatal(err)
	}
	if measured.ImageTokens != 255 || measured.TextTokens != 0 ||
		measured.EstimatedTokens != 286 {
		t.Fatalf("multimodal attribution=%+v", measured)
	}
	for _, fixture := range []struct {
		width, height int
		actual        uint64
	}{
		{512, 512, 255},
		{1024, 1024, 765},
		{2048, 1024, 1105},
	} {
		value, estimateErr := estimator.EstimateImage(provider.Attachment{
			MediaType: "image/png",
			Data:      encodePNG(t, fixture.width, fixture.height),
		})
		if estimateErr != nil {
			t.Fatal(estimateErr)
		}
		errorRate := float64(absDiff(value, fixture.actual)) /
			float64(fixture.actual)
		if errorRate > 0.10 {
			t.Fatalf(
				"%dx%d multimodal error=%f estimate=%d actual=%d",
				fixture.width, fixture.height, errorRate, value, fixture.actual,
			)
		}
	}
}

func TestBodyScopeStillCompactsBeforeTheHardTotalWindow(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.Window.Scope = compactScopeBodyAfterPrefix
	engine.options.SummaryMaxBytes = 2 << 10
	history := []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("h", 4000), 1),
		messageWithText(provider.RoleUser, "current", 2),
	}
	var receipt *CompactionReceipt
	input := agentcontext.NewMessageLedger(agentcontext.LedgerInput{
		Stable: []provider.Message{
			provider.TextMessage(provider.RoleSystem, strings.Repeat("s", 13_000)),
		},
	}).Snapshot()
	before := cloneMessages(history)
	window, err := engine.runCompactGate(
		t.Context(), &history, input, 128, CompactionPhasePreSampling, false,
		func(_ State, event Event) error {
			receipt = event.Compaction
			return nil
		}, 0, engine.contextViewProject(nil),
	)
	if err != nil && protocol.CodeOf(err) != protocol.CodeResourceExhausted {
		t.Fatalf("window=%+v receipt=%+v error=%v", window, receipt, err)
	}
	if !reflect.DeepEqual(history, before) {
		t.Fatalf("body-scope gate replaced history: %+v", history)
	}
	if err == nil && window.total > window.hardLimit {
		t.Fatalf("admitted overflowing window=%+v", window)
	}
}

func TestCompactGateMeasuresStatelessProviderProjection(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.Window.AutoTokens = 1_800
	decision := "已排除缓存问题，下一步只修改取消结算路径"
	call := toolCallMessage(1, "closed", "file_read", `{}`)
	call.Blocks = append([]provider.ContentBlock{{
		Type: provider.ContentText,
		Text: decision,
	}}, call.Blocks...)
	history := []provider.Message{
		call,
		toolResultMessage(1, "closed", `{"content":"ok"}`),
	}
	originalBytes := agentcontext.HistoryBytes(history)
	var receipt *CompactionReceipt
	project := func(history []provider.Message) []provider.Message {
		projected := contextview.ProjectStatelessHistory(history)
		return projected
	}
	window, err := engine.runCompactGate(
		t.Context(),
		&history,
		agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot(),
		128,
		CompactionPhasePreSampling,
		true,
		func(_ State, event Event) error {
			receipt = event.Compaction
			return nil
		},
		0, project,
	)
	if err != nil {
		t.Fatal(err)
	}
	if receipt != nil || window.active >= window.compactLimit {
		t.Fatalf("window=%+v receipt=%+v", window, receipt)
	}
	projected := project(history)
	if projected[0].Text() != decision {
		t.Fatalf("stateless projection dropped analysis: %+v", projected[0])
	}
	if agentcontext.HistoryBytes(history) != originalBytes ||
		history[0].Blocks[0].Text != decision {
		t.Fatalf("durable history was compacted: %+v", history)
	}
}

func TestTokenWindowEmergencyLimitDoesNotForceFinishOnlyTools(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{textStream("done")}}
	registry := declarationRegistry(t, false)
	if err := registry.Register(finishProcessTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	engine.options.Context.Window.EmergencyTokens = 2_500
	engine.options.StaticContext = []provider.Message{
		provider.TextMessage(provider.RoleSystem, strings.Repeat("x", 6_000)),
	}

	if _, err := engine.Run(t.Context(), "finish", nil); err != nil {
		t.Fatal(err)
	}
	if len(runtime.requests) != 1 {
		t.Fatalf("requests = %+v", runtime.requests)
	}
	if runtime.requests[0].ReasoningEffort != "" {
		t.Fatalf(
			"reasoning effort = %q for a non-reasoning route",
			runtime.requests[0].ReasoningEffort,
		)
	}
	names := make(map[string]bool, len(runtime.requests[0].Tools))
	for _, definition := range runtime.requests[0].Tools {
		names[definition.Name] = true
	}
	if !names["turn_complete"] || !names["result_get"] {
		t.Fatalf("tools = %+v, want the ordinary catalog", names)
	}
}

func TestTokenWindowFinishOnlyExecutesCompletionMutation(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("exec", "exec_command", `{}`),
		textStream("bounded final answer"),
	}}
	registry := declarationRegistry(t, false)
	if err := registry.Register(finishProcessTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	engine.options.Context.Window.EmergencyTokens = 2_500
	engine.options.StaticContext = []provider.Message{
		provider.TextMessage(provider.RoleSystem, strings.Repeat("x", 6_000)),
	}

	result, err := engine.Run(t.Context(), "finish", nil)
	if err != nil {
		t.Fatalf("Run() error = %v requests=%+v", err, runtime.requests)
	}
	if result.Text != "bounded final answer" || len(runtime.requests) != 2 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
	if result.State != Completed {
		t.Fatalf("state = %s", result.State)
	}
}

func TestTokenWindowFinishOnlyReturnsUnadvertisedToolAsRecoverableFailure(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("stale-read", "shell_read", `{}`),
		textStream("tool was unavailable"),
		textStream("bounded final answer"),
	}}
	engine := newEngine(t, runtime, declarationRegistry(t, true))
	engine.options.Context.Window.EmergencyTokens = 2_500
	engine.options.StaticContext = []provider.Message{
		provider.TextMessage(provider.RoleSystem, strings.Repeat("x", 6_000)),
	}

	result, err := engine.Run(t.Context(), "finish", nil)
	if err != nil {
		t.Fatalf("Run() error = %v requests=%+v", err, runtime.requests)
	}
	if result.Text != "bounded final answer" || len(runtime.requests) != 3 ||
		len(result.Tools) != 1 || result.Tools[0].CatalogRevision != 0 {
		t.Fatalf("result=%+v requests=%+v", result, runtime.requests)
	}
}

func encodePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	value := image.NewRGBA(image.Rect(0, 0, width, height))
	value.SetRGBA(0, 0, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, value); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func absDiff(left, right uint64) uint64 {
	if left > right {
		return left - right
	}
	return right - left
}
