package engine

import (
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestCompactGateKeepsAdmittedToolResultsAppendOnly(t *testing.T) {
	results := tool.NewResultStore(32 << 10)
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, results))
	oldContent := "HEAD-" + strings.Repeat("old-result ", 400) + "-TAIL"
	newContent := "HEAD-" + strings.Repeat("new-result ", 400) + "-TAIL"
	history := []provider.Message{
		messageWithText(provider.RoleUser, "read files", 1),
		toolCallMessage(1, "call-old", "file_read", `{"path":"old.txt"}`),
		toolResultMessage(1, "call-old", string(mustJSON(t, tool.Result{Content: oldContent}))),
		messageWithText(provider.RoleAssistant, "next file", 1),
		toolCallMessage(1, "call-new", "file_read", `{"path":"new.txt"}`),
		toolResultMessage(1, "call-new", string(mustJSON(t, tool.Result{Content: newContent}))),
	}
	original := cloneMessages(history)
	if _, err := engine.runCompactGate(
		t.Context(),
		&history,
		agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot(),
		128,
		CompactionPhaseMidTurn,
		true,
		func(State, Event) error { return nil },
		0,
		engine.projectSelectedHistoryForTest(nil),
	); err != nil {
		t.Fatal(err)
	}
	if history[2].Blocks[0].ToolResult.Content !=
		original[2].Blocks[0].ToolResult.Content ||
		history[5].Blocks[0].ToolResult.Content !=
			original[5].Blocks[0].ToolResult.Content {
		t.Fatalf("admitted tool results were rewritten: %+v", history)
	}
}

func TestContextViewFillsNewestTailUntilResidual(t *testing.T) {
	for _, contextTokens := range []uint64{2048, 8192} {
		engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
		engine.options.Route = mustTestRouteWithContext(t, contextTokens)
		engine.options.MaxOutputTokens = contextTokens / 4
		hard := engine.contextCapacity().HardInputTokens
		chunk := strings.Repeat("tok ", int(hard/2)+32)
		history := []provider.Message{
			messageWithText(provider.RoleUser, chunk, 1),
			messageWithText(provider.RoleAssistant, chunk, 1),
			messageWithText(provider.RoleUser, "current request", 2),
		}
		original := history[0].Text()
		viewed := engine.projectSelectedHistoryForTest(nil)(history)
		if len(viewed) == 0 || strings.Contains(viewed[0].Text(), "tok ") {
			t.Fatalf(
				"context=%d residual view kept the oldest group: %+v",
				contextTokens, viewed,
			)
		}
		if viewed[len(viewed)-1].Text() != "current request" {
			t.Fatalf("context=%d lost the current request: %+v", contextTokens, viewed)
		}
		if history[0].Text() != original {
			t.Fatal("durable history was clipped")
		}
	}
}

func TestContextViewOperatorTailCeilingClipsBeforeHardInput(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.RecentTailMaxTokens = 32
	history := []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("old ", 40), 1),
		messageWithText(provider.RoleAssistant, strings.Repeat("ans ", 40), 1),
		messageWithText(provider.RoleUser, "current request", 2),
	}
	viewed := engine.projectSelectedHistoryForTest(nil)(history)
	if len(viewed) != 1 || viewed[0].Text() != "current request" {
		t.Fatalf("operator ceiling view = %+v", viewed)
	}
}

func TestContextViewResidualDropsOlderTurnWithoutRewritingToolResult(t *testing.T) {
	results := tool.NewResultStore(32 << 10)
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, results))
	engine.options.Context.RecentTailMaxTokens = 48
	oldContent := "HEAD-" + strings.Repeat("old-result ", 80) + "-TAIL"
	encoded := string(mustJSON(t, tool.Result{Content: oldContent}))
	history := []provider.Message{
		messageWithText(provider.RoleUser, "read old", 1),
		toolCallMessage(1, "call-old", "file_read", `{"path":"old.txt"}`),
		toolResultMessage(1, "call-old", encoded),
		messageWithText(provider.RoleUser, "current request", 2),
	}
	if _, err := engine.runCompactGate(
		t.Context(),
		&history,
		agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot(),
		0,
		CompactionPhaseMidTurn,
		true,
		func(State, Event) error { return nil },
		0,
		engine.projectSelectedHistoryForTest(nil),
	); err != nil {
		t.Fatal(err)
	}
	if history[2].Blocks[0].ToolResult.Content != encoded {
		t.Fatalf("view residual rewrote durable tool result: %+v", history[2])
	}
	viewed := engine.projectSelectedHistoryForTest(nil)(history)
	if len(viewed) == 0 || viewed[len(viewed)-1].Text() != "current request" {
		t.Fatalf("view = %+v", viewed)
	}
}

func TestContextViewProjectorHidesTurnsOutsideRecentTail(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.RecentTailTurns = 2
	history := []provider.Message{
		messageWithText(provider.RoleUser, "turn one "+strings.Repeat("x", 200), 1),
		messageWithText(provider.RoleAssistant, "answer one", 1),
		messageWithText(provider.RoleUser, "turn two", 2),
		messageWithText(provider.RoleAssistant, "answer two", 2),
		messageWithText(provider.RoleUser, "turn three", 3),
	}
	viewed := engine.projectSelectedHistoryForTest(nil)(history)
	if len(viewed) != 3 ||
		!strings.Contains(viewed[0].Text(), "turn two") ||
		viewed[2].Text() != "turn three" {
		t.Fatalf("projected view = %+v", viewed)
	}
	if !strings.Contains(history[0].Text(), "turn one") {
		t.Fatal("durable history was clipped")
	}
}

func TestAdmitFoldsOldestVisibleTailWithoutReplacingHistory(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.Window.AutoTokens = 80
	history := []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("old ", 200), 1),
		messageWithText(provider.RoleAssistant, strings.Repeat("ans ", 200), 1),
		messageWithText(provider.RoleUser, "current", 2),
	}
	original := cloneMessages(history)
	var receipt *CompactionReceipt
	window, err := engine.runCompactGate(
		t.Context(),
		&history,
		agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot(),
		128,
		CompactionPhasePreSampling,
		false,
		func(_ State, event Event) error {
			receipt = event.Compaction
			return nil
		},
		0,
		engine.projectSelectedHistoryForTest(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if receipt == nil ||
		receipt.TruncationReason != "visible_tail_fold" ||
		receipt.RetainedBytes >= receipt.OriginalBytes {
		t.Fatalf("fold receipt = %+v", receipt)
	}
	if history[0].Text() != original[0].Text() || len(history) != len(original) {
		t.Fatalf("durable history changed: %+v", history)
	}
	viewed := engine.projectSelectedHistoryForTest(nil)(history)
	if len(viewed) == 0 || strings.Contains(viewed[0].Text(), "old ") {
		t.Fatalf("folded view still has the oldest group: %+v", viewed)
	}
	if window.hardLimit != 0 && window.total > window.hardLimit {
		t.Fatalf("admitted overflowing window=%+v", window)
	}
}

func TestOverflowFoldDoesNotRetryAfterOneVisibleFold(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	history := []provider.Message{
		messageWithText(provider.RoleUser, "one", 1),
		messageWithText(provider.RoleAssistant, "first", 1),
		messageWithText(provider.RoleUser, "two", 2),
		messageWithText(provider.RoleAssistant, "second", 2),
	}
	snapshot := agentcontext.NewMessageLedger(agentcontext.LedgerInput{
		History: history,
	}).Snapshot()
	overflow := protocol.NewProblem(
		protocol.CodeInvalidArgument,
		"context is too large",
		false,
		&provider.Failure{
			Code:    provider.FailureContextWindowExceeded,
			Message: "context is too large",
		},
	)
	changed, err := engine.recoverContextOverflow(
		overflow, false, &history, snapshot, 128,
		func(State, Event) error { return nil }, engine.projectSelectedHistoryForTest(nil),
	)
	if err != nil || !changed {
		t.Fatalf("first overflow fold changed=%t err=%v", changed, err)
	}
	before := cloneMessages(history)
	changed, err = engine.recoverContextOverflow(
		overflow, false, &history, snapshot, 128,
		func(State, Event) error { return nil }, engine.projectSelectedHistoryForTest(nil),
	)
	if err != nil || changed {
		t.Fatalf("second overflow fold changed=%t err=%v", changed, err)
	}
	if history[0].Text() != before[0].Text() {
		t.Fatal("overflow fold replaced history")
	}
}

func TestCompactGateDoesNotStageNarrativeOrReplaceUnderHardLimit(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.SemanticNarrative = "inline"
	if err := normalizeEngineOptions(&engine.options); err == nil {
		t.Fatal("inline semantic narrative was accepted")
	}
	engine.options.Context.SemanticNarrative = "post_turn"
	if err := normalizeEngineOptions(&engine.options); err != nil {
		t.Fatal(err)
	}
	history := []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("first context ", 200), 1),
		messageWithText(provider.RoleAssistant, "first answer", 1),
		messageWithText(provider.RoleUser, "current request", 2),
	}
	original := cloneMessages(history)
	var receipt *CompactionReceipt
	_, err := engine.runCompactGate(
		t.Context(),
		&history,
		agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot(),
		0,
		CompactionPhasePreSampling,
		true,
		func(_ State, event Event) error {
			receipt = event.Compaction
			return nil
		},
		0,
		engine.projectSelectedHistoryForTest(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if receipt != nil {
		t.Fatalf("unexpected compaction receipt: %+v", receipt)
	}
	if engine.compactionState().State != nil {
		t.Fatalf("prepared compaction leaked: %+v", engine.compactionState().State)
	}
	if len(history) != len(original) || history[0].Text() != original[0].Text() {
		t.Fatalf("durable history changed: %+v", history)
	}
}

func (e *Engine) projectSelectedHistoryForTest(
	next agentcontext.HistoryProjector,
) agentcontext.HistoryProjector {
	return func(history []provider.Message) []provider.Message {
		return agentcontext.ProjectHistory(
			e.contextProjection(history).Messages,
			next,
		)
	}
}
