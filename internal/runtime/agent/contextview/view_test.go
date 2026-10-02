package contextview

import (
	"fmt"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"reflect"
	"strings"
	"testing"
)

func TestProjectContextViewKeepsOnlyRecentTurns(t *testing.T) {
	history := []provider.Message{
		textTurn(provider.RoleUser, "one", 1),
		textTurn(provider.RoleAssistant, "first", 1),
		textTurn(provider.RoleUser, "two", 2),
		textTurn(provider.RoleAssistant, "second", 2),
		textTurn(provider.RoleUser, "three", 3),
		textTurn(provider.RoleAssistant, "third", 3),
		textTurn(provider.RoleUser, "four", 4),
	}
	viewed := ProjectContextView(history, 2)
	if len(viewed) != 3 ||
		viewed[0].Text() != "three" ||
		viewed[2].Text() != "four" {
		t.Fatalf("view = %+v", viewed)
	}
	if history[0].Text() != "one" {
		t.Fatal("durable history was modified")
	}
}

func TestProjectContextViewZeroTurnsUsesPublicDefault(t *testing.T) {
	history := []provider.Message{
		textTurn(provider.RoleUser, "one", 1),
		textTurn(provider.RoleUser, "two", 2),
		textTurn(provider.RoleUser, "three", 3),
	}
	viewed := ProjectContextView(history, 0)
	if len(viewed) != 2 || viewed[0].Text() != "two" {
		t.Fatalf("default view = %+v", viewed)
	}
}

func TestSafeTailStartDoesNotSplitToolPairs(t *testing.T) {
	history := []provider.Message{
		textTurn(provider.RoleUser, "old", 1),
		{
			Role: provider.RoleAssistant, Turn: 2,
			Blocks: []provider.ContentBlock{{
				Type:     provider.ContentToolCall,
				ToolCall: &provider.ToolCall{ID: "call-1", Name: "file_read"},
			}},
		},
		{
			Role: provider.RoleTool, Turn: 3,
			Blocks: []provider.ContentBlock{{
				Type:       provider.ContentToolResult,
				ToolResult: &provider.ToolResult{CallID: "call-1", Content: "body"},
			}},
		},
		textTurn(provider.RoleUser, "now", 3),
	}
	start := agentcontext.SafeTailStart(history, 2)
	if start != 1 {
		t.Fatalf("start = %d, want tool-pair-safe 1", start)
	}
	viewed := ProjectContextView(history, 2)
	if !agentcontext.ToolPairsClosed(viewed) || len(viewed) != 3 {
		t.Fatalf("view = %+v", viewed)
	}
}

func TestOldestVisibleTailFoldDropsOldestClosedGroup(t *testing.T) {
	history := []provider.Message{
		textTurn(provider.RoleUser, "one", 1),
		textTurn(provider.RoleAssistant, "first", 1),
		textTurn(provider.RoleUser, "two", 2),
		textTurn(provider.RoleAssistant, "second", 2),
		textTurn(provider.RoleUser, "three", 3),
	}
	start, ok := OldestVisibleTailFold(history, 2, 0, false)
	if !ok || start != 4 {
		t.Fatalf("fold start = %d ok=%t, want 4", start, ok)
	}
	viewed := ProjectContextViewFrom(history, start)
	if len(viewed) != 1 || viewed[0].Text() != "three" {
		t.Fatalf("folded view = %+v", viewed)
	}
	if history[0].Text() != "one" {
		t.Fatal("durable history was modified")
	}
}

func TestOldestVisibleTailFoldKeepsCurrentUserRequest(t *testing.T) {
	history := []provider.Message{
		textTurn(provider.RoleUser, strings.Repeat("request ", 4000), 1),
		textTurn(provider.RoleAssistant, "ack", 1),
	}
	if _, ok := OldestVisibleTailFold(history, 2, 0, true); ok {
		t.Fatal("single-turn fold hid the current request")
	}
}

func TestOldestVisibleTailFoldDoesNotSplitToolPairs(t *testing.T) {
	history := []provider.Message{
		textTurn(provider.RoleUser, "old", 1),
		{
			Role: provider.RoleAssistant, Turn: 2,
			Blocks: []provider.ContentBlock{{
				Type:     provider.ContentToolCall,
				ToolCall: &provider.ToolCall{ID: "call-1", Name: "file_read"},
			}},
		},
		{
			Role: provider.RoleTool, Turn: 2,
			Blocks: []provider.ContentBlock{{
				Type:       provider.ContentToolResult,
				ToolResult: &provider.ToolResult{CallID: "call-1", Content: "body"},
			}},
		},
		textTurn(provider.RoleUser, "now", 3),
	}
	start, ok := OldestVisibleTailFold(history, 2, 0, false)
	if !ok {
		t.Fatal("expected a safe fold")
	}
	viewed := ProjectContextViewFrom(history, start)
	if !agentcontext.ToolPairsClosed(viewed) {
		t.Fatalf("folded view split a tool pair: %+v", viewed)
	}
}

func TestFillVisibleTailStartDropsOldestGroupToFitTokens(t *testing.T) {
	history := []provider.Message{
		textTurn(provider.RoleUser, strings.Repeat("old ", 40), 1),
		textTurn(provider.RoleAssistant, strings.Repeat("ans ", 40), 1),
		textTurn(provider.RoleUser, "current", 2),
	}
	start := agentcontext.SafeTailStart(history, 2)
	if start != 0 {
		t.Fatalf("turn start = %d, want 0", start)
	}
	full := agentcontext.EstimateMessageTokens(RawTailMessages(history, start))
	filled := FillVisibleTailStart(
		history, 2, start, full/2, true, agentcontext.EstimateMessageTokens,
	)
	viewed := ProjectContextViewFrom(history, filled)
	if len(viewed) != 1 || viewed[0].Text() != "current" {
		t.Fatalf("residual view = %+v", viewed)
	}
	if !strings.Contains(history[0].Text(), "old ") {
		t.Fatal("durable history was modified")
	}
}

func TestFillVisibleTailStartKeepsCurrentUserWhenOverBudget(t *testing.T) {
	history := []provider.Message{
		textTurn(provider.RoleUser, strings.Repeat("request ", 80), 1),
	}
	start := agentcontext.SafeTailStart(history, 2)
	filled := FillVisibleTailStart(
		history, 2, start, 1, true, agentcontext.EstimateMessageTokens,
	)
	viewed := ProjectContextViewFrom(history, filled)
	if len(viewed) != 1 || !strings.Contains(viewed[0].Text(), "request") {
		t.Fatalf("over-budget view hid the current request: %+v", viewed)
	}
}

func TestFillVisibleTailStartDoesNotInventALimit(t *testing.T) {
	history := []provider.Message{
		textTurn(provider.RoleUser, strings.Repeat("old ", 40), 1),
		textTurn(provider.RoleUser, "current", 2),
	}
	start := agentcontext.SafeTailStart(history, 2)
	filled := FillVisibleTailStart(
		history, 2, start, 1, false, agentcontext.EstimateMessageTokens,
	)
	if filled != start {
		t.Fatalf("unlimited fill moved start %d -> %d", start, filled)
	}
}

func TestProjectContextViewKeepsWorldStateOutsideTheTail(t *testing.T) {
	section := provider.TextMessage(provider.RoleSystem, "[working_set] old.go")
	projection, err := agentcontext.ProjectWorld([]agentcontext.WorldSection{{
		ID: "working_set", Digest: "set:old.go", Message: &section,
	}}, agentcontext.WorldBaseline{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	world := projection.Messages[0]
	world.Turn = 1
	history := []provider.Message{
		world,
		textTurn(provider.RoleUser, "one", 1),
		textTurn(provider.RoleUser, "two", 2),
		textTurn(provider.RoleUser, "three", 3),
	}
	viewed := ProjectContextView(history, 2)
	if len(viewed) != 3 || !agentcontext.IsWorldStateMessage(viewed[0]) ||
		viewed[1].Text() != "two" {
		t.Fatalf("view = %+v", viewed)
	}
	start := agentcontext.SafeTailStart(history, 2)
	filled := FillVisibleTailStart(
		history, 2, start, 1, true, agentcontext.EstimateMessageTokens,
	)
	residual := ProjectContextViewFrom(history, filled)
	if len(residual) < 2 || !agentcontext.IsWorldStateMessage(residual[0]) ||
		residual[len(residual)-1].Text() != "three" {
		t.Fatalf("residual view dropped world state: %+v", residual)
	}
}

func textTurn(role provider.Role, text string, turn uint64) provider.Message {
	message := provider.TextMessage(role, text)
	message.Turn = turn
	return message
}

func TestWorldViewRebuildsHistoricalBaselineAndKeepsCurrentPrefix(t *testing.T) {
	for _, versions := range []int{1, 22, 100} {
		t.Run(fmt.Sprint(versions), func(t *testing.T) {
			var history []provider.Message
			baseline := agentcontext.WorldBaseline{}
			for i := 0; i < versions; i++ {
				value := provider.TextMessage(provider.RoleSystem, fmt.Sprintf("state %d", i))
				value.Turn = 1
				projection, err := agentcontext.ProjectWorld([]agentcontext.WorldSection{{
					ID: "session_state", Digest: fmt.Sprint(i), Message: &value,
				}}, baseline, history)
				if err != nil {
					t.Fatal(err)
				}
				history = append(history, projection.Messages...)
				baseline = projection.Baseline
			}
			user := provider.TextMessage(provider.RoleUser, "continue")
			user.Turn = 2
			history = append(history, user)
			original := agentcontext.CloneMessages(history)
			view := ProjectContextViewFrom(history, versions)
			if len(view) != 2 || view[0].Text() != fmt.Sprintf("state %d", versions-1) {
				t.Fatalf("historical versions leaked: %+v", view)
			}
			reply := provider.TextMessage(provider.RoleAssistant, "next")
			reply.Turn = 2
			next := ProjectContextViewFrom(append(agentcontext.CloneMessages(history), reply), versions)
			if !reflect.DeepEqual(view, next[:len(view)]) || !reflect.DeepEqual(history, original) {
				t.Fatal("projection rewrote prefix or durable history")
			}
			removed, err := agentcontext.ProjectWorld(nil, baseline, history)
			if err != nil {
				t.Fatal(err)
			}
			later := provider.TextMessage(provider.RoleUser, "later")
			later.Turn = 3
			removedView := ProjectContextViewFrom(append(append(agentcontext.CloneMessages(history), removed.Messages...), later), len(history))
			if len(removedView) != 1 || removedView[0].Text() != "later" {
				t.Fatalf("removed section resurfaced: %+v", removedView)
			}
		})
	}
}
