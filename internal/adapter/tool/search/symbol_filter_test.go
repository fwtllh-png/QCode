package search

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestSearchSymbolFiltersBeyondPreviousPrefetchWindow(t *testing.T) {
	files := map[string]string{}
	for n := range 8 {
		files[fmt.Sprintf("a/%d.ts", n)] = "function Serve() {}\n"
	}
	for n := range 3 {
		files[fmt.Sprintf("z_%%/%d.ts", n)] = "export function Serve() {}\n"
	}
	files["zXY/decoy.ts"] = "function Serve() {}\n"
	registry := indexedRegistry(t, files)
	for _, input := range []map[string]any{
		{"query": "Serve", "path_prefix": "z_%/", "max_results": 1},
		{"query": "Serve", "exported_only": true, "max_results": 1},
		{"query": "Serve", "path_prefix": "z_%/", "exported_only": true, "max_results": 1},
	} {
		result := execute(t, registry, KindSymbol, input)
		var payload struct {
			Matches   []symbolMatch `json:"matches"`
			Total     int           `json:"total"`
			Truncated bool          `json:"truncated"`
		}
		if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Matches) != 1 || payload.Matches[0].File != "z_%/0.ts" ||
			payload.Total != 3 || !payload.Truncated || !result.Truncated ||
			result.Metadata["matches"] != 3 || result.Metadata["returned"] != 1 {
			t.Fatalf("input=%v result=%+v", input, result)
		}
	}
	// Exact-name searches also report the total beyond the former limit*4 page.
	result := execute(t, registry, KindDefinition, map[string]any{"name": "Serve", "max_results": 1})
	if !result.Truncated || result.Metadata["matches"] != len(files) {
		t.Fatalf("definition=%+v", result)
	}
	result = execute(t, registry, KindSymbol, map[string]any{
		"query": "Serve", "path_prefix": "z_%/0.ts", "max_results": 1,
	})
	if result.Truncated || result.Metadata["matches"] != 1 || len(decodeSymbols(t, result.Content)) != 1 {
		t.Fatalf("exactly full filtered page=%+v", result)
	}
}
