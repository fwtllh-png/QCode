package config

import (
	"fmt"
	"testing"
)

func TestNarrativeZeroLimitsAndModeCombinations(t *testing.T) {
	for _, digest := range []string{"ledger", "ledger+narrative"} {
		for _, mode := range []string{"off", "post_turn"} {
			path := writeConfig(t, fmt.Sprintf(`[context.view]
digest = %q
narrative_mode = %q
[context.compact]
semantic_narrative_max_input_tokens = 0
semantic_narrative_max_items = 0
semantic_narrative_item_max_bytes = 0
`, digest, mode))
			snapshot, err := Load(LoadOptions{Path: path, LookupEnv: envLookup(nil)})
			if err != nil {
				t.Fatalf("%s/%s: %v", digest, mode, err)
			}
			for _, field := range []string{fieldCompactSemanticNarrativeMaxInputTokens, fieldCompactSemanticNarrativeMaxItems, fieldCompactSemanticNarrativeItemMaxBytes} {
				if snapshot.Provenance[field] != SourceFile {
					t.Fatalf("zero lost provenance for %s", field)
				}
			}
		}
	}
	for _, limit := range []struct {
		env     string
		maximum int
	}{
		{"QCODE_COMPACT_SEMANTIC_NARRATIVE_MAX_INPUT_TOKENS", 64 << 20},
		{"QCODE_COMPACT_SEMANTIC_NARRATIVE_MAX_ITEMS", 1024},
		{"QCODE_COMPACT_SEMANTIC_NARRATIVE_ITEM_MAX_BYTES", 64 << 10},
	} {
		for _, value := range []int{-1, 0, 1, limit.maximum, limit.maximum + 1} {
			_, err := Load(LoadOptions{LookupEnv: envLookup(map[string]string{limit.env: fmt.Sprint(value)})})
			if (err == nil) != (value >= 0 && value <= limit.maximum) {
				t.Fatalf("%s=%d: %v", limit.env, value, err)
			}
		}
	}
}
