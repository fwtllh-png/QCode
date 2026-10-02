package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type contextLifecycleGolden struct {
	SchemaVersion int                        `json:"schema_version"`
	Scenarios     []contextLifecycleScenario `json:"scenarios"`
}

type contextLifecycleScenario struct {
	Name     string                   `json:"name"`
	Thread   string                   `json:"thread"`
	Window   contextLifecycleWindow   `json:"window"`
	Messages []contextLifecycleRecord `json:"messages"`
}

type contextLifecycleWindow struct {
	Number  uint64 `json:"number"`
	First   string `json:"first,omitempty"`
	Current string `json:"current,omitempty"`
}

type contextLifecycleRecord struct {
	Index      int    `json:"index"`
	Role       string `json:"role"`
	Turn       uint64 `json:"turn,omitempty"`
	Text       string `json:"text,omitempty"`
	CallID     string `json:"call_id,omitempty"`
	ResultID   string `json:"result_id,omitempty"`
	ResultFail bool   `json:"result_error,omitempty"`
	Digest     string `json:"content_digest"`
}

func TestContextEngineeringCE0LifecycleGolden(t *testing.T) {
	compacted := encodeGoldenHistory(t, []provider.Message{
		provider.TextMessage(provider.RoleUser, "pre-compact question"),
		provider.TextMessage(provider.RoleAssistant, "deterministic compact summary"),
	})
	forked := encodeGoldenHistory(t, []provider.Message{
		provider.TextMessage(provider.RoleUser, "fork source question"),
		provider.TextMessage(provider.RoleAssistant, "fork source answer"),
	})
	scenarios := []struct {
		name   string
		thread string
		events []protocol.Event
	}{
		{
			name: "restart_filters_failed_and_unpaired_turns", thread: "restart",
			events: []protocol.Event{
				turnStarted("restart", "failed", 1, "discard failed"),
				toolStarted("restart", "failed", 2, "failed-call"),
				{
					Kind: protocol.EventTurnFailed, ThreadID: "restart",
					TurnID: "failed", Sequence: 3,
					Data: &protocol.TurnFailedData{Code: "internal", Message: "failed"},
				},
				turnStarted("restart", "complete", 4, "keep completed"),
				toolStarted("restart", "complete", 5, "paired-call"),
				toolResult("restart", "complete", 6, "paired-call", false),
				{
					Kind: protocol.EventTurnCompleted, ThreadID: "restart",
					TurnID: "complete", Sequence: 7,
					Data: &protocol.TurnCompletedData{Text: "completed answer"},
				},
				turnStarted("restart", "pending", 8, "discard pending"),
			},
		},
		{
			name: "rollback_drops_only_target_turn", thread: "rollback",
			events: []protocol.Event{
				turnStarted("rollback", "turn-1", 1, "keep first"),
				{
					Kind: protocol.EventTurnCompleted, ThreadID: "rollback",
					TurnID: "turn-1", Sequence: 2,
					Data: &protocol.TurnCompletedData{Text: "first answer"},
				},
				turnStarted("rollback", "turn-2", 3, "remove second"),
				{
					Kind: protocol.EventTurnCompleted, ThreadID: "rollback",
					TurnID: "turn-2", Sequence: 4,
					Data: &protocol.TurnCompletedData{Text: "second answer"},
				},
				{
					Kind: protocol.EventTurnReverted, ThreadID: "rollback",
					TurnID: "turn-2", Sequence: 5,
					Data: &protocol.TurnRevertedData{TargetTurnID: "turn-2"},
				},
			},
		},
		{
			name: "fork_uses_replacement_history", thread: "child",
			events: []protocol.Event{{
				Kind: protocol.EventThreadForked, ThreadID: "parent",
				TurnID: "source-turn", Sequence: 1,
				Data: &protocol.ThreadForkedData{
					NewThreadID: "child", SourceCursor: 7,
					ReplacementHistory: forked,
					WindowNumber:       1, FirstWindowID: "fork-window",
					WindowID: "fork-window",
				},
			}},
		},
		{
			name: "compaction_installs_window_then_appends", thread: "compact",
			events: []protocol.Event{
				{
					Kind: protocol.EventThreadCompacted, ThreadID: "compact", Sequence: 1,
					Data: &protocol.ThreadCompactedData{
						Summary:            "deterministic compact summary",
						ReplacementHistory: compacted,
						WindowNumber:       3, FirstWindowID: "window-1",
						PreviousWindowID: "window-2", WindowID: "window-3",
					},
				},
				turnStarted("compact", "turn-3", 2, "after compact"),
				{
					Kind: protocol.EventTurnCompleted, ThreadID: "compact",
					TurnID: "turn-3", Sequence: 3,
					Data: &protocol.TurnCompletedData{Text: "post-compact answer"},
				},
			},
		},
	}
	actual := contextLifecycleGolden{SchemaVersion: 1}
	for _, scenario := range scenarios {
		reconstructed, err := agentcontext.ReconstructThread(scenario.events, protocol.ThreadID(scenario.thread))
		if err != nil {
			t.Fatalf("%s: %v", scenario.name, err)
		}
		record := contextLifecycleScenario{
			Name: scenario.name, Thread: scenario.thread,
			Window: contextLifecycleWindow{
				Number:  reconstructed.Window.Number,
				First:   reconstructed.Window.FirstID,
				Current: reconstructed.Window.Current,
			},
		}
		for index, message := range reconstructed.History {
			item := contextLifecycleRecord{
				Index: index, Role: string(message.Role), Turn: message.Turn,
				Text: message.Text(), Digest: provider.MessageContentDigest(message),
			}
			for _, block := range message.Blocks {
				if block.ToolCall != nil {
					item.CallID = block.ToolCall.ID
				}
				if block.ToolResult != nil {
					item.ResultID = block.ToolResult.CallID
					item.ResultFail = block.ToolResult.IsError
				}
			}
			record.Messages = append(record.Messages, item)
		}
		actual.Scenarios = append(actual.Scenarios, record)
	}
	encoded, err := json.MarshalIndent(actual, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	goldenPath := filepath.Join("testdata", "context_engineering_ce0.golden.json")
	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read lifecycle golden: %v\n\ngot:\n%s", err, encoded)
	}
	if !bytes.Equal(encoded, bytes.TrimSpace(golden)) {
		t.Fatalf("context lifecycle baseline drifted\nwant:\n%s\n\ngot:\n%s", golden, encoded)
	}
}

func encodeGoldenHistory(t *testing.T, messages []provider.Message) []protocol.CompactedMessage {
	t.Helper()
	encoded, err := agentcontext.EncodeCompactedHistory(messages)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func turnStarted(
	thread, turn string,
	sequence protocol.Cursor,
	prompt string,
) protocol.Event {
	return protocol.Event{
		Kind: protocol.EventTurnStarted, ThreadID: protocol.ThreadID(thread),
		TurnID: protocol.TurnID(turn), Sequence: sequence,
		Data: &protocol.TurnStartedData{Provider: "fixture", Model: "fixture", Prompt: prompt},
	}
}

func toolStarted(
	thread, turn string,
	sequence protocol.Cursor,
	callID string,
) protocol.Event {
	return protocol.Event{
		Kind: protocol.EventToolStart, ThreadID: protocol.ThreadID(thread),
		TurnID: protocol.TurnID(turn), Sequence: sequence,
		Data: &protocol.ToolStartData{
			Tool: "read", CallID: callID, Arguments: []byte(`{"path":"fixture"}`),
		},
	}
}

func toolResult(
	thread, turn string,
	sequence protocol.Cursor,
	callID string,
	isError bool,
) protocol.Event {
	return protocol.Event{
		Kind: protocol.EventToolResult, ThreadID: protocol.ThreadID(thread),
		TurnID: protocol.TurnID(turn), Sequence: sequence,
		Data: &protocol.ToolResultData{
			Tool: "read", CallID: callID, Output: "fixture output", IsError: isError,
		},
	}
}
