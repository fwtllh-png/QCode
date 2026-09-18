package history

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestSearchSnippetLiteralUnicodeAndWindow(t *testing.T) {
	for _, test := range []struct {
		text, query string
		want        bool
	}{
		{"中文搜索结果", "文", true},
		{"ÉCLAIR 的修复", "éclair", true},
		{"路径 a%b_c.go", "%b_", true},
		{"路径 axbyc.go", "%b_", false},
		{"no result", "missing", false},
		{"İABC", "abc", true},
	} {
		snippet, ok := SearchSnippet(test.text, test.query)
		if ok != test.want || ok && snippet != test.text {
			t.Errorf("SearchSnippet(%q, %q) = %q, %v", test.text, test.query, snippet, ok)
		}
	}
	text := strings.Repeat("界", 300) + "Needle" + strings.Repeat("界", 300)
	snippet, ok := SearchSnippet(text, "needle")
	if !ok || !strings.Contains(snippet, "Needle") ||
		!strings.HasPrefix(snippet, "...") || !strings.HasSuffix(snippet, "...") ||
		utf8.RuneCountInString(snippet) != 246 {
		t.Fatalf("snippet = %q, %v", snippet, ok)
	}
}

func TestSearchFieldsUseVisiblePromptAndLastMatchingReceiptField(t *testing.T) {
	event := protocol.Event{ThreadID: "thread", TurnID: "turn",
		Data: &protocol.TurnStartedData{Prompt: "internal only", DisplayPrompt: "用户请求"}}
	fields := SearchFields(event)
	if _, _, ok := MatchSearchFields(fields, "internal"); ok {
		t.Fatal("hidden prompt was searchable")
	}
	if kind, _, ok := MatchSearchFields(fields, "请求"); !ok || kind != "user_request" {
		t.Fatalf("visible prompt match = %q, %v", kind, ok)
	}
	event.Data = &protocol.ExecutionReceiptData{
		Changes: []protocol.ReceiptChange{{Path: "中文.go"}},
		EditorContext: []protocol.EditorContextReceipt{{
			Path: "中文.ts", Symbol: &protocol.EditorSymbol{Name: "中文Symbol"},
		}},
	}
	kind, snippet, ok := MatchSearchFields(SearchFields(event), "中文")
	if !ok || kind != "symbol" || snippet != "中文Symbol" {
		t.Fatalf("receipt match = %q, %q, %v", kind, snippet, ok)
	}
}
