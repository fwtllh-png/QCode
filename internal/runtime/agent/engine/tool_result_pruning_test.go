package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func TestCompactGateDoesNotRewriteOlderAdmittedToolResults(t *testing.T) {
	results := tool.NewResultStore(32 << 10)
	engine := newEngine(
		t,
		&scriptedProvider{},
		tool.NewRegistry(nil, results),
	)
	content := "HEAD-" + strings.Repeat("model-visible-result ", 80) + "-TAIL"
	encoded, err := json.Marshal(tool.Result{Content: content})
	if err != nil {
		t.Fatal(err)
	}
	history := []provider.Message{
		messageWithText(provider.RoleUser, "inspect the output", 1),
		toolCallMessage(1, "call-consumed", "file_read", `{"path":"old.txt"}`),
		toolResultMessage(1, "call-consumed", string(encoded)),
		messageWithText(provider.RoleUser, "inspect the latest", 2),
		toolCallMessage(2, "call-latest", "file_read", `{"path":"latest.txt"}`),
		toolResultMessage(2, "call-latest", string(encoded)),
	}
	original := cloneMessages(history)
	var receipt *CompactionReceipt
	if _, err := engine.runCompactGate(
		t.Context(),
		&history,
		agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot(),
		128,
		CompactionPhaseMidTurn,
		true,
		func(_ State, event Event) error {
			receipt = event.Compaction
			return nil
		},
		0,
		engine.contextViewProject(nil),
	); err != nil {
		t.Fatal(err)
	}
	if receipt != nil {
		t.Fatalf("gc should not emit a replacement receipt: %+v", receipt)
	}
	if len(history) != len(original) ||
		messageToolResultID(history[2]) != "call-consumed" ||
		history[5].Blocks[0].ToolResult.Content !=
			original[5].Blocks[0].ToolResult.Content {
		t.Fatalf("tool pairing changed: %+v", history)
	}
	var projected tool.Result
	if err := json.Unmarshal(
		[]byte(history[2].Blocks[0].ToolResult.Content),
		&projected,
	); err != nil {
		t.Fatal(err)
	}
	if projected.Handle != "" || projected.Content != content {
		t.Fatalf("older turn result was rewritten: %+v", projected)
	}
	if history[2].Blocks[0].ToolResult.Content !=
		original[2].Blocks[0].ToolResult.Content {
		t.Fatal("older admitted tool result bytes changed")
	}
}

func TestStatelessDefaultKeepsLargeOlderTurnsOutOfTheProjection(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, nil)
	engine.options.Route = reasoningRoute(t)
	history := []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("first context ", 5000), 1),
		messageWithText(provider.RoleAssistant, "first answer", 1),
		messageWithText(provider.RoleUser, strings.Repeat("second context ", 5000), 2),
		messageWithText(provider.RoleAssistant, "second answer", 2),
		messageWithText(provider.RoleUser, "current request", 3),
	}
	var receipt *CompactionReceipt
	window, err := engine.runCompactGate(
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
		engine.contextViewProject(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if receipt != nil {
		t.Fatalf("history under the hard limit was replaced: %+v", receipt)
	}
	if !strings.Contains(history[0].Text(), "first context") {
		t.Fatalf("durable older turn was rewritten: %q", history[0].Text())
	}
	viewed := engine.contextViewProject(nil)(history)
	if len(viewed) != 3 || !strings.Contains(viewed[0].Text(), "second context") {
		t.Fatalf("projected tail = %+v", viewed)
	}
	if window.active == 0 {
		t.Fatal("projected window was empty")
	}
}

func TestToolResultPruningSkipsMalformedAndRetrievalResults(t *testing.T) {
	results := tool.NewResultStore(32 << 10)
	engine := newEngine(
		t,
		&scriptedProvider{},
		tool.NewRegistry(nil, results),
	)
	history := []provider.Message{
		toolCallMessage(1, "call-malformed", "file_read", `{}`),
		toolResultMessage(1, "call-malformed", "not-json"),
		toolCallMessage(1, "call-retrieval", "result_get", `{}`),
		toolResultMessage(
			1,
			"call-retrieval",
			string(mustJSON(t, tool.Result{
				Content: strings.Repeat("retrieved", 2000),
			})),
		),
	}
	before := cloneMessages(history)
	stats, _, err := engine.pruneToolResultSurfaces(
		&history,
		agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot(),
		128,
		true,
		false,
		0,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stats.results != 0 ||
		agentcontext.HistoryBytes(history) != agentcontext.HistoryBytes(before) {
		t.Fatalf("stats=%+v history=%+v", stats, history)
	}
}

func TestCompactGateKeepsConsumedSameTurnResultsAppendOnly(t *testing.T) {
	results := tool.NewResultStore(32 << 10)
	engine := newEngine(
		t,
		&scriptedProvider{},
		tool.NewRegistry(nil, results),
	)
	engine.options.Context.Window.AutoTokens = 3500
	large := strings.Repeat("result ", 160)
	encoded, err := json.Marshal(tool.Result{Content: large})
	if err != nil {
		t.Fatal(err)
	}
	history := []provider.Message{
		toolCallMessage(1, "old-1", "file_read", `{}`),
		toolResultMessage(1, "old-1", string(encoded)),
		toolCallMessage(1, "old-2", "file_read", `{}`),
		toolResultMessage(1, "old-2", string(encoded)),
		toolCallMessage(1, "latest", "file_read", `{}`),
		toolResultMessage(1, "latest", string(encoded)),
	}
	latest := history[len(history)-1].Blocks[0].ToolResult.Content
	before := cloneMessages(history)
	var receipt *CompactionReceipt
	_, err = engine.runCompactGate(
		t.Context(),
		&history,
		agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot(),
		0,
		CompactionPhaseMidTurn,
		true,
		func(_ State, event Event) error {
			receipt = event.Compaction
			return nil
		},
		0,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if receipt != nil {
		t.Fatalf("gc should not emit a replacement receipt: %+v", receipt)
	}
	if history[len(history)-1].Blocks[0].ToolResult.Content != latest {
		t.Fatalf("latest result was rewritten: %+v", history)
	}
	var first, second tool.Result
	if err := json.Unmarshal([]byte(history[1].Blocks[0].ToolResult.Content), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(history[3].Blocks[0].ToolResult.Content), &second); err != nil {
		t.Fatal(err)
	}
	if first.Handle != "" || second.Handle != "" ||
		first.Content != large || second.Content != large {
		t.Fatalf("consumed same-turn results were rewritten: first=%+v second=%+v", first, second)
	}
	if agentcontext.HistoryBytes(history) != agentcontext.HistoryBytes(before) {
		t.Fatalf("consumed results changed size: before=%d after=%d",
			agentcontext.HistoryBytes(before), agentcontext.HistoryBytes(history))
	}
}

func TestCompactGateKeepsOlderTurnToolResultWhenViewClipsIt(t *testing.T) {
	results := tool.NewResultStore(32 << 10)
	engine := newEngine(
		t,
		&scriptedProvider{},
		tool.NewRegistry(nil, results),
	)
	engine.options.Context.RecentTailTurns = 1
	large := strings.Repeat("result ", 160)
	encoded, err := json.Marshal(tool.Result{Content: large})
	if err != nil {
		t.Fatal(err)
	}
	history := []provider.Message{
		toolCallMessage(1, "old", "file_read", `{}`),
		toolResultMessage(1, "old", string(encoded)),
		messageWithText(provider.RoleUser, "continue", 2),
		toolCallMessage(2, "latest", "file_read", `{}`),
		toolResultMessage(2, "latest", string(encoded)),
	}
	latest := history[len(history)-1].Blocks[0].ToolResult.Content
	if _, err := engine.runCompactGate(
		t.Context(),
		&history,
		agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot(),
		0,
		CompactionPhaseMidTurn,
		true,
		func(State, Event) error { return nil },
		0,
		engine.contextViewProject(nil),
	); err != nil {
		t.Fatal(err)
	}
	if history[len(history)-1].Blocks[0].ToolResult.Content != latest {
		t.Fatalf("latest result was rewritten: %+v", history)
	}
	var projected tool.Result
	if err := json.Unmarshal(
		[]byte(history[1].Blocks[0].ToolResult.Content),
		&projected,
	); err != nil {
		t.Fatal(err)
	}
	if projected.Handle != "" || projected.Content != large {
		t.Fatalf("older admitted result was rewritten: %+v", projected)
	}
}

func TestToolPairIdentityEquivalenceRejectsRewrittenCalls(t *testing.T) {
	before := []provider.Message{
		toolCallMessage(1, "call-stable", "file_read", `{}`),
		toolResultMessage(1, "call-stable", "result"),
	}
	after := cloneMessages(before)
	after[1].Blocks[0].ToolResult.CallID = "call-drifted"
	if agentcontext.ToolPairIdentityEquivalent(before, after) {
		t.Fatal("rewritten Tool Call/Result identity was accepted")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
