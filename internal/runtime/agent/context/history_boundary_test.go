package agentcontext

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func TestRecentToolResultStartKeepsLastN(t *testing.T) {
	history := []provider.Message{
		{
			Role: provider.RoleTool,
			Turn: 1,
			Blocks: []provider.ContentBlock{{
				Type:       provider.ContentToolResult,
				ToolResult: &provider.ToolResult{CallID: "old", Content: "old"},
			}},
		},
		textTurn(provider.RoleUser, "two", 2),
		{
			Role: provider.RoleTool,
			Turn: 2,
			Blocks: []provider.ContentBlock{{
				Type:       provider.ContentToolResult,
				ToolResult: &provider.ToolResult{CallID: "keep", Content: "keep"},
			}},
		},
		textTurn(provider.RoleUser, "three", 3),
	}
	if start := RecentToolResultStart(history, 0); start != len(history) {
		t.Fatalf("keep 0 start = %d", start)
	}
	if start := UnconsumedToolResultStart(history); start != len(history) {
		t.Fatalf("trailing user consumes results, start = %d", start)
	}
	if start := RecentToolResultStart(history, 1); start != 2 {
		t.Fatalf("keep 1 start = %d", start)
	}
	if start := RecentToolResultStart(history, 2); start != 0 {
		t.Fatalf("keep 2 start = %d", start)
	}
}

func TestUnconsumedToolResultStartKeepsOpenRoundOnly(t *testing.T) {
	history := []provider.Message{
		textTurn(provider.RoleUser, "go", 1),
		{
			Role: provider.RoleAssistant, Turn: 1,
			Blocks: []provider.ContentBlock{{
				Type:     provider.ContentToolCall,
				ToolCall: &provider.ToolCall{ID: "a", Name: "file_read"},
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
				ToolCall: &provider.ToolCall{ID: "b", Name: "file_read"},
			}},
		},
		{
			Role: provider.RoleTool, Turn: 1,
			Blocks: []provider.ContentBlock{{
				Type:       provider.ContentToolResult,
				ToolResult: &provider.ToolResult{CallID: "b", Content: "keep"},
			}},
		},
		{
			Role: provider.RoleTool, Turn: 1,
			Blocks: []provider.ContentBlock{{
				Type:       provider.ContentToolResult,
				ToolResult: &provider.ToolResult{CallID: "c", Content: "keep-parallel"},
			}},
		},
	}
	if start := UnconsumedToolResultStart(history); start != 4 {
		t.Fatalf("open round start = %d, want 4", start)
	}
	if start := WorkingSetGCStart(history, 0); start != 4 {
		t.Fatalf("keep 0 gc start = %d, want 4", start)
	}
	if start := WorkingSetGCStart(history, 3); start != 2 {
		t.Fatalf("keep 3 gc start = %d, want 2", start)
	}
}

func textTurn(role provider.Role, text string, turn uint64) provider.Message {
	message := provider.TextMessage(role, text)
	message.Turn = turn
	return message
}
