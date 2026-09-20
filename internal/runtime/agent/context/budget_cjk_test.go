package agentcontext

import (
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

// CJK sessions used to overflow their measured context: the old estimate
// divided every rune by four, undercounting Chinese by roughly 4x while the
// window measurement compared against provider-scale numbers. These tests
// lock the dense-script baseline the calibrated estimator starts from.
func TestEstimateMessageTokensCountsDenseScriptPerRune(t *testing.T) {
	chinese := strings.Repeat("配置文件内容", 40) // 240 runes
	messages := []provider.Message{
		provider.TextMessage(provider.RoleUser, chinese),
		{Role: provider.RoleAssistant, Blocks: []provider.ContentBlock{
			{Type: provider.ContentText, Text: chinese},
			{Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{
				ID: "call_1", Name: "file_read", Arguments: `{"path":"配置/文件.go"}`,
			}},
		}},
		{Role: provider.RoleTool, Blocks: []provider.ContentBlock{{
			Type:       provider.ContentToolResult,
			ToolResult: &provider.ToolResult{CallID: "call_1", Content: chinese},
		}}},
	}
	// Text appears three times (240 dense runes each = 720 tokens) plus the
	// concatenated call name and arguments ({"path":"配置/文件.go"}: four
	// dense runes plus 24 ASCII characters = 10 tokens). Every CJK rune
	// counts as one token instead of a quarter.
	if got := EstimateMessageTokens(messages); got != 730 {
		t.Fatalf("dense-script estimate = %d, want 730", got)
	}
}

func TestEstimateMessageTokensKeepsLatinBaseline(t *testing.T) {
	message := provider.TextMessage(provider.RoleUser, strings.Repeat("a", 400))
	if got := EstimateMessageTokens([]provider.Message{message}); got != 100 {
		t.Fatalf("latin estimate = %d, want 100", got)
	}
}
