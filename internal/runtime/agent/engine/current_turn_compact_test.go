package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestMidTurnDegradesLatestBatchWhenNoClosedGroup(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	route := mustTestRouteWithContext(t, 1024)
	engine.options.Route = route
	engine.options.Routes, _ = model.NewRouteSet(route, nil, false)
	engine.options.MaxOutputTokens = 128
	engine.options.Context.Window.AutoTokens = 500
	body := strings.Repeat("latest-body ", 400)
	encoded, err := json.Marshal(tool.ModelResult("read", tool.Result{Content: body}))
	if err != nil {
		t.Fatal(err)
	}
	history := []provider.Message{
		messageWithText(provider.RoleUser, "inspect the latest output", 1),
		toolCallMessage(1, "call-a", "read", `{"path":"a.go"}`),
		toolCallMessage(1, "call-b", "read", `{"path":"b.go"}`),
		toolResultMessage(1, "call-a", string(encoded)),
		toolResultMessage(1, "call-b", string(encoded)),
	}
	// Two assistant messages would split the latest batch. Keep both calls
	// on the last assistant message so they are one unconsumed batch.
	history[1].Blocks = append(history[1].Blocks, history[2].Blocks...)
	history = append(history[:2], history[3:]...)
	snapshot := agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot()
	window, err := engine.runCompactGate(
		t.Context(), &history, snapshot, 128, CompactionPhaseMidTurn, true,
		func(State, Event) error { return nil }, 0, engine.contextViewProject(nil),
	)
	if err != nil {
		t.Fatalf("latest-batch turn failed: %v", err)
	}
	if window.hardLimit != 0 && window.total > window.hardLimit {
		t.Fatalf("degraded latest batch still overflows: %+v", window)
	}
	if !currentTurnKeepsUser(history, "inspect the latest output") {
		t.Fatalf("user request missing: %+v", history)
	}
	var handles int
	for _, message := range history {
		for _, block := range message.Blocks {
			if block.Type != provider.ContentToolResult || block.ToolResult == nil {
				continue
			}
			var value tool.Result
			if err := json.Unmarshal([]byte(block.ToolResult.Content), &value); err != nil {
				t.Fatalf("latest result is not a projection: %v", err)
			}
			if value.Handle == "" || !value.Truncated {
				t.Fatalf("latest result stayed raw: %+v", value)
			}
			handles++
		}
	}
	if handles != 2 {
		t.Fatalf("handles = %d, want 2", handles)
	}
}

func TestMidTurnBoundsLatestPatchArguments(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	route := mustTestRouteWithContext(t, 1024)
	engine.options.Route = route
	engine.options.Routes, _ = model.NewRouteSet(route, nil, false)
	engine.options.MaxOutputTokens = 128
	arguments, err := json.Marshal(map[string]string{
		"path":    "parser.go",
		"content": strings.Repeat("patch-body ", 400),
	})
	if err != nil {
		t.Fatal(err)
	}
	history := []provider.Message{
		messageWithText(provider.RoleUser, "apply the remaining patch", 1),
		toolCallMessage(1, "write-1", "file_write", string(arguments)),
		toolResultMessage(1, "write-1", `{"ok":true}`),
	}
	snapshot := agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot()
	window, err := engine.runCompactGate(
		t.Context(), &history, snapshot, 128, CompactionPhaseMidTurn, true,
		func(State, Event) error { return nil }, 0, engine.contextViewProject(nil),
	)
	if err != nil {
		t.Fatalf("argument-pressure turn failed: %v", err)
	}
	if window.hardLimit != 0 && window.total > window.hardLimit {
		t.Fatalf("bounded arguments still overflow: %+v", window)
	}
	if !currentTurnKeepsUser(history, "apply the remaining patch") {
		t.Fatalf("user request missing: %+v", history)
	}
	if !strings.Contains(history[1].Blocks[0].ToolCall.Arguments, `"path":"parser.go"`) ||
		strings.Contains(history[1].Blocks[0].ToolCall.Arguments, "patch-body") {
		t.Fatalf("arguments = %s", history[1].Blocks[0].ToolCall.Arguments)
	}
}

func TestMidTurnBoundsExecCommandKeepsTruncatedCommand(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	descriptor := echoDescriptor()
	descriptor.Name = "exec_command"
	descriptor.Description = "run a command"
	descriptor.IdentityKeys = []string{"command", "cwd"}
	if err := registry.Register(&countingCatalogExecutor{descriptor: descriptor}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{}, registry)
	route := mustTestRouteWithContext(t, 1024)
	engine.options.Route = route
	engine.options.Routes, _ = model.NewRouteSet(route, nil, false)
	engine.options.MaxOutputTokens = 128
	command := strings.Repeat("go test ./parser ", 400)
	arguments, err := json.Marshal(map[string]any{
		"command": command, "cwd": ".", "timeout_ms": 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	history := []provider.Message{
		messageWithText(provider.RoleUser, "run the parser tests", 1),
		toolCallMessage(1, "exec-1", "exec_command", string(arguments)),
		toolResultMessage(1, "exec-1", `{"ok":false}`),
	}
	snapshot := agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot()
	window, err := engine.runCompactGate(
		t.Context(), &history, snapshot, 128, CompactionPhaseMidTurn, true,
		func(State, Event) error { return nil }, 0, engine.contextViewProject(nil),
	)
	if err != nil {
		t.Fatalf("command-pressure turn failed: %v", err)
	}
	if window.hardLimit != 0 && window.total > window.hardLimit {
		t.Fatalf("bounded command still overflows: %+v", window)
	}
	got := history[1].Blocks[0].ToolCall.Arguments
	if !strings.Contains(got, `"cwd":"."`) ||
		!strings.Contains(got, "go test ./parser") ||
		strings.Contains(got, "timeout_ms") ||
		!strings.Contains(got, "...") {
		t.Fatalf("arguments = %s", got)
	}
}

func TestMidTurnStillFailsWhenUserRequestIsIrreducible(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.Window.AutoTokens = 300
	engine.options.SummaryMaxBytes = 100
	history := []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("goal ", 4000), 1),
	}
	before := cloneMessages(history)
	snapshot := agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot()
	window, err := engine.runCompactGate(
		t.Context(), &history, snapshot, 128, CompactionPhaseMidTurn, true,
		func(State, Event) error { return nil }, 0, engine.contextViewProject(nil),
	)
	if protocol.CodeOf(err) != protocol.CodeResourceExhausted ||
		window.hardLimit == 0 || window.total <= window.hardLimit {
		t.Fatalf("window=%+v error=%v, want resource_exhausted", window, err)
	}
	if history[0].Text() != before[0].Text() {
		t.Fatalf("irreducible user request was rewritten: %q", history[0].Text())
	}
}

func TestMidTurnSummarizesHugeClosedRoundCommentary(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	route := mustTestRouteWithContext(t, 1024)
	engine.options.Route = route
	engine.options.Routes, _ = model.NewRouteSet(route, nil, false)
	engine.options.MaxOutputTokens = 128
	call := toolCallMessage(1, "search-1", "search_text", `{"query":"cancel"}`)
	call.Blocks = append([]provider.ContentBlock{{
		Type: provider.ContentText,
		Text: strings.Repeat("excluded cache and only cancel settlement remains ", 200),
	}}, call.Blocks...)
	history := []provider.Message{
		messageWithText(provider.RoleUser, "why is settlement failing", 1),
		call,
		toolResultMessage(1, "search-1", `{"hits":[]}`),
	}
	snapshot := agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot()
	window, err := engine.runCompactGate(
		t.Context(), &history, snapshot, 128, CompactionPhaseMidTurn, true,
		func(State, Event) error { return nil }, 0, engine.contextViewProject(nil),
	)
	if err != nil {
		t.Fatalf("commentary-pressure turn failed: %v", err)
	}
	if window.hardLimit != 0 && window.total > window.hardLimit {
		t.Fatalf("summarized commentary still overflows: %+v", window)
	}
	if !currentTurnKeepsUser(history, "why is settlement failing") {
		t.Fatalf("user request missing: %+v", history)
	}
	if !strings.Contains(history[1].Text(), agentcontext.PriorDecisionProjectionPrefix) ||
		!strings.Contains(history[1].Text(), "source=search-1") ||
		!strings.Contains(history[1].Text(), "excluded cache") {
		t.Fatalf("closed-round commentary = %q", history[1].Text())
	}
}

func currentTurnKeepsUser(history []provider.Message, want string) bool {
	for _, message := range history {
		if agentcontext.IsWorldStateMessage(message) {
			continue
		}
		if message.Role == provider.RoleUser && strings.Contains(message.Text(), want) {
			return true
		}
	}
	return false
}

func historyTexts(history []provider.Message) string {
	var builder strings.Builder
	for _, message := range history {
		builder.WriteString(message.Text())
		for _, block := range message.Blocks {
			if block.ToolCall != nil {
				builder.WriteString(block.ToolCall.Arguments)
			}
			if block.ToolResult != nil {
				builder.WriteString(block.ToolResult.Content)
			}
		}
	}
	return builder.String()
}
