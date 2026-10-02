package agentcontext

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestReconstructThreadAppliesPostCompactEvents(t *testing.T) {
	base, err := EncodeCompactedHistory([]provider.Message{
		provider.TextMessage(provider.RoleUser, "old"),
		provider.TextMessage(provider.RoleAssistant, "summary"),
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []protocol.Event{
		{
			Kind: protocol.EventThreadCompacted, ThreadID: "thread-a", Sequence: 1,
			Data: &protocol.ThreadCompactedData{
				Summary: "c1", ReplacementHistory: base,
				WindowNumber: 2, FirstWindowID: "w0", WindowID: "w1",
			},
		},
		{
			Kind: protocol.EventTurnStarted, ThreadID: "thread-a", TurnID: "turn-new", Sequence: 2,
			Data: &protocol.TurnStartedData{Provider: "p", Model: "m", Prompt: "new question"},
		},
		{
			Kind: protocol.EventToolStart, ThreadID: "thread-a", TurnID: "turn-new", Sequence: 3,
			Data: &protocol.ToolStartData{
				Tool: "echo", CallID: "c1", Arguments: []byte(`{"value":1}`),
			},
		},
		{
			Kind: protocol.EventToolResult, ThreadID: "thread-a", TurnID: "turn-new", Sequence: 4,
			Data: &protocol.ToolResultData{Tool: "echo", CallID: "c1", Output: `{"ok":true}`},
		},
		{
			Kind: protocol.EventTurnCompleted, ThreadID: "thread-a", TurnID: "turn-new", Sequence: 5,
			Data: &protocol.TurnCompletedData{Text: "done"},
		},
		{
			Kind: protocol.EventTurnStarted, ThreadID: "thread-b", Sequence: 6,
			Data: &protocol.TurnStartedData{Provider: "p", Model: "m", Prompt: "other thread"},
		},
	}
	recon, err := ReconstructThread(events, "thread-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(recon.History) != 6 {
		t.Fatalf("history len=%d want 6: %+v", len(recon.History), recon.History)
	}
	if recon.History[2].Text() != "new question" {
		t.Fatalf("prompt = %q", recon.History[2].Text())
	}
	if recon.History[3].Role != provider.RoleAssistant ||
		recon.History[3].Blocks[0].ToolCall == nil ||
		recon.History[3].Blocks[0].ToolCall.ID != "c1" {
		t.Fatalf("tool call = %+v", recon.History[3])
	}
	if recon.History[4].Role != provider.RoleTool {
		t.Fatalf("tool role = %s", recon.History[4].Role)
	}
	if recon.History[5].Text() != "done" {
		t.Fatalf("assistant = %q", recon.History[5].Text())
	}
	if recon.Window.Current != "w1" || recon.Window.Number != 2 {
		t.Fatalf("window = %+v", recon.Window)
	}
}

func TestReconstructThreadUsesCheckpointRestoreAndForkAsHistoryBaselines(
	t *testing.T,
) {
	baseline, err := EncodeCompactedHistory([]provider.Message{
		provider.TextMessage(provider.RoleUser, "checkpoint prompt"),
		provider.TextMessage(provider.RoleAssistant, "checkpoint result"),
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []protocol.Event{
		{
			Kind: protocol.EventTurnStarted, ThreadID: "thread-a",
			TurnID: "later", Sequence: 1,
			Data: &protocol.TurnStartedData{
				Provider: "p", Model: "m", Prompt: "discard me",
			},
		},
		{
			Kind: protocol.EventTurnCompleted, ThreadID: "thread-a",
			TurnID: "later", Sequence: 2,
			Data: &protocol.TurnCompletedData{Text: "discarded"},
		},
		{
			Kind: protocol.EventCheckpointRestored, ThreadID: "thread-a",
			TurnID: "checkpoint-turn", Sequence: 3,
			Data: &protocol.CheckpointRestoredData{
				CheckpointID:       "checkpoint-1",
				SourceThreadID:     "thread-a",
				SourceTurnID:       "checkpoint-turn",
				SourceCursor:       9,
				ReplacementHistory: baseline,
			},
		},
		{
			Kind: protocol.EventCheckpointForked, ThreadID: "thread-a",
			TurnID: "checkpoint-turn", Sequence: 4,
			Data: &protocol.CheckpointForkedData{
				CheckpointID:       "checkpoint-1",
				NewThreadID:        "thread-child",
				Title:              "Child",
				SourceCursor:       9,
				ReplacementHistory: baseline,
			},
		},
	}
	restored, err := ReconstructThread(events, "thread-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.History) != 2 ||
		restored.History[0].Text() != "checkpoint prompt" ||
		restored.History[1].Text() != "checkpoint result" {
		t.Fatalf("restored history = %+v", restored.History)
	}
	forked, err := ReconstructThread(events, "thread-child")
	if err != nil {
		t.Fatal(err)
	}
	if len(forked.History) != 2 ||
		forked.History[1].Text() != "checkpoint result" {
		t.Fatalf("Fork history = %+v", forked.History)
	}
}

func TestReconstructThreadCommitsOnlyCompletedPairedToolHistory(t *testing.T) {
	events := []protocol.Event{
		{
			Kind: protocol.EventTurnStarted, ThreadID: "thread-a", TurnID: "failed", Sequence: 1,
			Data: &protocol.TurnStartedData{Provider: "p", Model: "m", Prompt: "failed prompt"},
		},
		{
			Kind: protocol.EventToolStart, ThreadID: "thread-a", TurnID: "failed", Sequence: 2,
			Data: &protocol.ToolStartData{Tool: "echo", CallID: "failed-call", Arguments: []byte(`{}`)},
		},
		{
			Kind: protocol.EventTurnFailed, ThreadID: "thread-a", TurnID: "failed", Sequence: 3,
			Data: &protocol.TurnFailedData{Code: "internal", Message: "failed"},
		},
		{
			Kind: protocol.EventTurnStarted, ThreadID: "thread-a", TurnID: "completed", Sequence: 4,
			Data: &protocol.TurnStartedData{Provider: "p", Model: "m", Prompt: "completed prompt"},
		},
		{
			Kind: protocol.EventToolStart, ThreadID: "thread-a", TurnID: "completed", Sequence: 5,
			Data: &protocol.ToolStartData{
				Tool: "first", CallID: "call-1", Arguments: []byte(`{"first":true}`),
			},
		},
		{
			Kind: protocol.EventToolStart, ThreadID: "thread-a", TurnID: "completed", Sequence: 6,
			Data: &protocol.ToolStartData{
				Tool: "second", CallID: "call-2", Arguments: []byte(`{"second":true}`),
			},
		},
		{
			Kind: protocol.EventToolResult, ThreadID: "thread-a", TurnID: "completed", Sequence: 7,
			Data: &protocol.ToolResultData{Tool: "first", CallID: "call-1", Output: "one"},
		},
		{
			Kind: protocol.EventToolResult, ThreadID: "thread-a", TurnID: "completed", Sequence: 8,
			Data: &protocol.ToolResultData{Tool: "second", CallID: "call-2", Output: "two"},
		},
		{
			Kind: protocol.EventToolResult, ThreadID: "thread-a", TurnID: "completed", Sequence: 9,
			Data: &protocol.ToolResultData{Tool: "orphan", CallID: "orphan", Output: "drop"},
		},
		{
			Kind: protocol.EventTurnCompleted, ThreadID: "thread-a", TurnID: "completed", Sequence: 10,
			Data: &protocol.TurnCompletedData{Text: "done"},
		},
		{
			Kind: protocol.EventTurnStarted, ThreadID: "thread-a", TurnID: "incomplete", Sequence: 11,
			Data: &protocol.TurnStartedData{Provider: "p", Model: "m", Prompt: "incomplete prompt"},
		},
	}
	recon, err := ReconstructThread(events, "thread-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(recon.History) != 6 {
		t.Fatalf("history len=%d want 6: %+v", len(recon.History), recon.History)
	}
	if recon.History[0].Text() != "completed prompt" ||
		recon.History[5].Text() != "done" {
		t.Fatalf("completed boundary was not preserved: %+v", recon.History)
	}
	calls := make(map[string]bool)
	results := make(map[string]bool)
	for _, message := range recon.History {
		if id := replayToolCallID(message); id != "" {
			calls[id] = true
		}
		if id := replayToolResultID(message); id != "" {
			results[id] = true
		}
	}
	if !calls["call-1"] || !calls["call-2"] ||
		!results["call-1"] || !results["call-2"] ||
		calls["failed-call"] || results["orphan"] {
		t.Fatalf("tool pairs calls=%v results=%v", calls, results)
	}
}

func TestReconstructThreadRetainsOnlyPairedUserInterruptedHistory(t *testing.T) {
	events := []protocol.Event{
		{
			Kind: protocol.EventTurnStarted, ThreadID: "thread-a", TurnID: "interrupted", Sequence: 1,
			Data: &protocol.TurnStartedData{Provider: "p", Model: "m", Prompt: "inspect web"},
		},
		{
			Kind: protocol.EventToolStart, ThreadID: "thread-a", TurnID: "interrupted", Sequence: 2,
			Data: &protocol.ToolStartData{Tool: "read", CallID: "paired", Arguments: []byte(`{}`)},
		},
		{
			Kind: protocol.EventToolResult, ThreadID: "thread-a", TurnID: "interrupted", Sequence: 3,
			Data: &protocol.ToolResultData{Tool: "read", CallID: "paired", Output: "result"},
		},
		{
			Kind: protocol.EventToolStart, ThreadID: "thread-a", TurnID: "interrupted", Sequence: 4,
			Data: &protocol.ToolStartData{Tool: "read", CallID: "orphan", Arguments: []byte(`{}`)},
		},
		{
			Kind: protocol.EventTurnCanceled, ThreadID: "thread-a", TurnID: "interrupted", Sequence: 5,
			Data: &protocol.TurnCanceledData{Reason: protocol.CancelReasonUserInterrupted},
		},
		{
			Kind: protocol.EventTurnStarted, ThreadID: "thread-a", TurnID: "shutdown", Sequence: 6,
			Data: &protocol.TurnStartedData{Provider: "p", Model: "m", Prompt: "discard me"},
		},
		{
			Kind: protocol.EventTurnCanceled, ThreadID: "thread-a", TurnID: "shutdown", Sequence: 7,
			Data: &protocol.TurnCanceledData{Reason: protocol.CancelReasonShutdown},
		},
	}
	recon, err := ReconstructThread(events, "thread-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(recon.History) != 3 {
		t.Fatalf("history len=%d want 3: %+v", len(recon.History), recon.History)
	}
	if recon.History[0].Text() != "inspect web" ||
		replayToolCallID(recon.History[1]) != "paired" ||
		replayToolResultID(recon.History[2]) != "paired" {
		t.Fatalf("interrupted history=%+v", recon.History)
	}
}

func TestReconstructThreadRevertDropsOnlyTargetTurn(t *testing.T) {
	events := []protocol.Event{
		{
			Kind: protocol.EventTurnStarted, ThreadID: "thread-a", TurnID: "turn-1", Sequence: 1,
			Data: &protocol.TurnStartedData{Provider: "p", Model: "m", Prompt: "first"},
		},
		{
			Kind: protocol.EventTurnCompleted, ThreadID: "thread-a", TurnID: "turn-1", Sequence: 2,
			Data: &protocol.TurnCompletedData{Text: "first done"},
		},
		{
			Kind: protocol.EventTurnStarted, ThreadID: "thread-a", TurnID: "turn-2", Sequence: 3,
			Data: &protocol.TurnStartedData{Provider: "p", Model: "m", Prompt: "second"},
		},
		{
			Kind: protocol.EventToolResult, ThreadID: "thread-a", TurnID: "turn-2", Sequence: 4,
			Data: &protocol.ToolResultData{Tool: "edit", CallID: "c1", Output: `{"ok":true}`},
		},
		{
			Kind: protocol.EventTurnCompleted, ThreadID: "thread-a", TurnID: "turn-2", Sequence: 5,
			Data: &protocol.TurnCompletedData{Text: "second done"},
		},
		{
			Kind: protocol.EventTurnReverted, ThreadID: "thread-a", TurnID: "turn-2", Sequence: 6,
			Data: &protocol.TurnRevertedData{TargetTurnID: "turn-2", Restored: []string{"a.go"}},
		},
	}
	recon, err := ReconstructThread(events, "thread-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(recon.History) != 2 {
		t.Fatalf("history len=%d want 2: %+v", len(recon.History), recon.History)
	}
	if recon.History[0].Text() != "first" || recon.History[1].Text() != "first done" {
		t.Fatalf("reverted history = %+v", recon.History)
	}
}

func TestReconstructThreadRevertKeepsCompactedWindow(t *testing.T) {
	base, err := EncodeCompactedHistory([]provider.Message{
		provider.TextMessage(provider.RoleSystem, "earlier summary"),
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []protocol.Event{
		{
			Kind: protocol.EventThreadCompacted, ThreadID: "thread-a", Sequence: 1,
			Data: &protocol.ThreadCompactedData{
				Summary: "c1", ReplacementHistory: base,
				WindowNumber: 2, FirstWindowID: "w0", WindowID: "w1",
			},
		},
		{
			Kind: protocol.EventTurnStarted, ThreadID: "thread-a", TurnID: "turn-9", Sequence: 2,
			Data: &protocol.TurnStartedData{Provider: "p", Model: "m", Prompt: "after compact"},
		},
		{
			Kind: protocol.EventTurnReverted, ThreadID: "thread-a", TurnID: "turn-9", Sequence: 3,
			Data: &protocol.TurnRevertedData{TargetTurnID: "turn-9"},
		},
	}
	recon, err := ReconstructThread(events, "thread-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(recon.History) != 1 || recon.History[0].Text() != "earlier summary" {
		t.Fatalf("history after revert = %+v", recon.History)
	}
}
