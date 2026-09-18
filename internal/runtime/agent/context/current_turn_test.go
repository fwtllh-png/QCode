package agentcontext

import (
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func TestCurrentTurnWorkingSetCutsKeepUserAndClosedPairs(t *testing.T) {
	history := []provider.Message{
		textTurn(provider.RoleUser, "fix it", 1),
		{
			Role: provider.RoleAssistant, Turn: 1,
			Blocks: []provider.ContentBlock{{
				Type:     provider.ContentToolCall,
				ToolCall: &provider.ToolCall{ID: "a", Name: "file_read", Arguments: `{"path":"a.go"}`},
			}},
		},
		{
			Role: provider.RoleTool, Turn: 1,
			Blocks: []provider.ContentBlock{{
				Type:       provider.ContentToolResult,
				ToolResult: &provider.ToolResult{CallID: "a", Content: "old"},
			}},
		},
		{
			Role: provider.RoleAssistant, Turn: 1,
			Blocks: []provider.ContentBlock{{
				Type:     provider.ContentToolCall,
				ToolCall: &provider.ToolCall{ID: "b", Name: "file_read", Arguments: `{"path":"b.go"}`},
			}},
		},
		{
			Role: provider.RoleTool, Turn: 1,
			Blocks: []provider.ContentBlock{{
				Type:       provider.ContentToolResult,
				ToolResult: &provider.ToolResult{CallID: "b", Content: "new"},
			}},
		},
	}
	if CurrentTurnUserIndex(history) != 0 {
		t.Fatalf("user index = %d", CurrentTurnUserIndex(history))
	}
	cuts := CurrentTurnWorkingSetCuts(history)
	if len(cuts) != 1 || cuts[0] != 3 {
		t.Fatalf("cuts = %v, want [3]", cuts)
	}
}

func TestCurrentTurnWorkingSetCutsIgnoreUserOnlyHistory(t *testing.T) {
	history := []provider.Message{
		textTurn(provider.RoleUser, strings.Repeat("goal ", 40), 1),
		textTurn(provider.RoleAssistant, "ack", 1),
	}
	if cuts := CurrentTurnWorkingSetCuts(history); len(cuts) != 0 {
		t.Fatalf("plain turn cuts = %v", cuts)
	}
}

func TestCollapseCurrentTurnWorldKeepsLatestPatch(t *testing.T) {
	first := worldMessage(
		WorldSection{
			ID: "working_set", Present: true,
			Message: ptrMessage(provider.TextMessage(provider.RoleSystem, "old.go")),
		},
		WorldEntry{ID: "working_set", Digest: "set:old.go", Revision: 1, Present: true},
		WorldPatch,
	)
	first.Turn = 4
	second := worldMessage(
		WorldSection{
			ID: "working_set", Present: true,
			Message: ptrMessage(provider.TextMessage(provider.RoleSystem, "new.go")),
		},
		WorldEntry{ID: "working_set", Digest: "set:new.go", Revision: 2, Present: true},
		WorldPatch,
	)
	second.Turn = 4
	history := []provider.Message{
		first,
		textTurn(provider.RoleUser, "edit", 4),
		second,
	}
	collapsed, changed := CollapseCurrentTurnWorld(history)
	if !changed || len(collapsed) != 2 || collapsed[1].Text() != "new.go" {
		t.Fatalf("collapsed = %+v changed=%t", collapsed, changed)
	}
}

func TestBoundToolCallArgumentsKeepsPathIdentity(t *testing.T) {
	history := []provider.Message{{
		Role: provider.RoleAssistant, Turn: 1,
		Blocks: []provider.ContentBlock{{
			Type: provider.ContentToolCall,
			ToolCall: &provider.ToolCall{
				ID:   "closed",
				Name: "file_write",
				Arguments: `{"path":"main.go","content":"` +
					strings.Repeat("x", 80) + `"}`,
			},
		}},
	}, {
		Role: provider.RoleAssistant, Turn: 1,
		Blocks: []provider.ContentBlock{{
			Type: provider.ContentToolCall,
			ToolCall: &provider.ToolCall{
				ID:        "latest",
				Name:      "file_write",
				Arguments: `{"path":"keep.go","content":"body"}`,
			},
		}},
	}}
	if BoundToolCallArguments(history, false) != 1 {
		t.Fatalf("closed arguments were not bounded: %+v", history)
	}
	if history[0].Blocks[0].ToolCall.Arguments != `{"path":"main.go"}` {
		t.Fatalf("closed args = %s", history[0].Blocks[0].ToolCall.Arguments)
	}
	if history[1].Blocks[0].ToolCall.Arguments != `{"path":"keep.go","content":"body"}` {
		t.Fatalf("latest args = %s", history[1].Blocks[0].ToolCall.Arguments)
	}
	if BoundToolCallArguments(history, true) != 1 {
		t.Fatalf("latest arguments were not bounded: %+v", history[1])
	}
	if history[1].Blocks[0].ToolCall.Arguments != `{"path":"keep.go"}` {
		t.Fatalf("latest args after include = %s", history[1].Blocks[0].ToolCall.Arguments)
	}
}

func TestStripConsumedReasoningKeepsLatestAssistant(t *testing.T) {
	history := []provider.Message{{
		Role: provider.RoleAssistant, Turn: 1,
		Blocks: []provider.ContentBlock{
			{Type: provider.ContentReasoning, Text: "old thought"},
			{Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{ID: "a", Name: "file_read"}},
		},
	}, {
		Role: provider.RoleAssistant, Turn: 1,
		Blocks: []provider.ContentBlock{
			{Type: provider.ContentReasoning, Text: "new thought"},
			{Type: provider.ContentText, Text: "done"},
		},
	}}
	if StripConsumedReasoning(history, true) != 1 {
		t.Fatalf("removed consumed reasoning: %+v", history)
	}
	if len(history[0].Blocks) != 1 || history[0].Blocks[0].Type != provider.ContentToolCall {
		t.Fatalf("consumed assistant = %+v", history[0])
	}
	if history[1].Blocks[0].Type != provider.ContentReasoning {
		t.Fatalf("latest reasoning dropped: %+v", history[1])
	}
}

func TestSummarizeClosedAssistantTextSkipsShortLatestDecision(t *testing.T) {
	history := []provider.Message{{
		Role: provider.RoleAssistant, Turn: 1,
		Blocks: []provider.ContentBlock{
			{Type: provider.ContentText, Text: "已排除缓存问题，下一步只修改取消结算路径"},
			{Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{
				ID: "search-1", Name: "search_text", Arguments: `{}`,
			}},
		},
	}, {
		Role: provider.RoleTool, Turn: 1,
		Blocks: []provider.ContentBlock{{
			Type:       provider.ContentToolResult,
			ToolResult: &provider.ToolResult{CallID: "search-1", Content: "hits"},
		}},
	}}
	if SummarizeClosedAssistantText(history, false, 512) != 0 {
		t.Fatalf("short latest decision was rewritten: %+v", history[0])
	}
	if history[0].Text() != "已排除缓存问题，下一步只修改取消结算路径" {
		t.Fatalf("text = %q", history[0].Text())
	}
}

func TestSummarizeClosedAssistantTextRewritesOlderLongCommentary(t *testing.T) {
	long := strings.Repeat("excluded cache and retrying settlement ", 40)
	history := []provider.Message{{
		Role: provider.RoleAssistant, Turn: 1,
		Blocks: []provider.ContentBlock{
			{Type: provider.ContentText, Text: long},
			{Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{
				ID: "old", Name: "search_text", Arguments: `{}`,
			}},
		},
	}, {
		Role: provider.RoleTool, Turn: 1,
		Blocks: []provider.ContentBlock{{
			Type:       provider.ContentToolResult,
			ToolResult: &provider.ToolResult{CallID: "old", Content: "hits"},
		}},
	}, {
		Role: provider.RoleAssistant, Turn: 1,
		Blocks: []provider.ContentBlock{
			{Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{
				ID: "latest", Name: "file_read", Arguments: `{}`,
			}},
		},
	}, {
		Role: provider.RoleTool, Turn: 1,
		Blocks: []provider.ContentBlock{{
			Type:       provider.ContentToolResult,
			ToolResult: &provider.ToolResult{CallID: "latest", Content: "ok"},
		}},
	}}
	if SummarizeClosedAssistantText(history, false, 80) != 1 {
		t.Fatalf("older commentary was not summarized: %+v", history[0])
	}
	if !strings.HasPrefix(history[0].Text(), PriorDecisionProjectionPrefix) ||
		!strings.Contains(history[0].Text(), "source=old") ||
		!strings.Contains(history[0].Text(), "excluded cache") ||
		len(history[0].Text()) > 83 {
		t.Fatalf("summary = %q", history[0].Text())
	}
	if history[2].Blocks[0].ToolCall.ID != "latest" {
		t.Fatalf("latest call changed: %+v", history[2])
	}
}

func ptrMessage(message provider.Message) *provider.Message {
	return &message
}
