package tool

import (
	"strings"
	"testing"
)

func TestResolvedIdentityKeysPrefersDeclaration(t *testing.T) {
	keys := ResolvedIdentityKeys(Descriptor{
		IdentityKeys: []string{"command", "cwd"},
		InputSchema: map[string]any{
			"type":       "object",
			"required":   []string{"command"},
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
		},
	})
	if len(keys) != 2 || keys[0] != "command" || keys[1] != "cwd" {
		t.Fatalf("declared keys = %v", keys)
	}
}

func TestResolvedIdentityKeysUnionsWhitelistAndRequiredScalars(t *testing.T) {
	keys := ResolvedIdentityKeys(Descriptor{
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"ticket", "command", "payload"},
			"properties": map[string]any{
				"ticket":  map[string]any{"type": "string"},
				"command": map[string]any{"type": "string"},
				"payload": map[string]any{"type": "object"},
			},
		},
	})
	joined := strings.Join(keys, ",")
	if !strings.Contains(joined, "ticket") ||
		!strings.Contains(joined, "path") ||
		strings.Contains(joined, "command") ||
		strings.Contains(joined, "payload") {
		t.Fatalf("undeclared keys = %v", keys)
	}
}

func TestCompactIdentityArgumentsTruncatesDeclaredCommand(t *testing.T) {
	command := strings.Repeat("x", 200)
	got := CompactIdentityArguments(
		`{"command":"`+command+`","cwd":"src"}`,
		[]string{"command", "cwd"},
		160,
	)
	if !strings.Contains(got, `"cwd":"src"`) ||
		strings.Count(got, "x") != 160 ||
		!strings.Contains(got, "...") {
		t.Fatalf("compacted = %s", got)
	}
}

func TestCompactIdentityArgumentsDropsUndeclaredLargeFields(t *testing.T) {
	got := CompactIdentityArguments(
		`{"command":"rm -rf /","path":"main.go","content":"body"}`,
		nil,
		160,
	)
	if got != `{"path":"main.go"}` {
		t.Fatalf("compacted = %s", got)
	}
}
