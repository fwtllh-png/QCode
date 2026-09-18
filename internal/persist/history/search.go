package history

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// SearchReader is an optional event-store projection. It returns the newest
// matching event per session, restricted to the supplied thread ownership.
type SearchReader interface {
	SearchSessionEvents(context.Context, map[protocol.ThreadID]string, string) ([]protocol.SessionSearchMatch, error)
}

type SearchField struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// SearchFields preserves the field order used by history search: the last
// matching field in an event wins. Only user-visible prompt text is indexed.
func SearchFields(event protocol.Event) []SearchField {
	if event.ThreadID == "" || event.TurnID == "" {
		return nil
	}
	switch data := event.Data.(type) {
	case *protocol.TurnStartedData:
		prompt := data.DisplayPrompt
		if prompt == "" {
			prompt = data.Prompt
		}
		return []SearchField{{Kind: "user_request", Text: prompt}}
	case *protocol.TurnCompletedData:
		return []SearchField{{Kind: "agent_output", Text: data.Text}}
	case *protocol.ExecutionReceiptData:
		fields := make([]SearchField, 0, len(data.Changes)+len(data.EditorContext))
		for _, change := range data.Changes {
			fields = append(fields, SearchField{Kind: "path", Text: change.Path})
		}
		for _, reference := range data.EditorContext {
			fields = append(fields, SearchField{Kind: "path", Text: reference.Path})
			if reference.Symbol != nil {
				fields = append(fields, SearchField{Kind: "symbol", Text: reference.Symbol.Name})
			}
		}
		return fields
	default:
		return nil
	}
}

func MatchSearchFields(fields []SearchField, query string) (kind, snippet string, found bool) {
	for index := len(fields) - 1; index >= 0; index-- {
		if snippet, ok := SearchSnippet(fields[index].Text, query); ok {
			return fields[index].Kind, snippet, true
		}
	}
	return "", "", false
}

// SearchSnippet retains the existing 240-rune presentation window. Matching
// uses literal Unicode lowercase substrings, including one-character queries.
func SearchSnippet(value, query string) (string, bool) {
	const limit = 240
	lower := strings.ToLower(value)
	index := strings.Index(lower, strings.ToLower(query))
	if index < 0 {
		return "", false
	}
	// Lowercasing can change UTF-8 byte lengths; slice the original by runes.
	index = utf8.RuneCountInString(lower[:index])
	valueRunes := []rune(value)
	start := max(0, index-limit/3)
	end := min(len(valueRunes), start+limit)
	start = max(0, end-limit)
	snippet := strings.TrimSpace(string(valueRunes[start:end]))
	if start > 0 {
		snippet = "..." + snippet
	}
	if end < len(valueRunes) {
		snippet += "..."
	}
	return snippet, true
}
