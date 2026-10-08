package agentcontext

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func TestRenderNarrativeDigestIsOptionalPartition(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	input, err := BuildNarrativeInput(
		"thread-1",
		"window-1",
		"sha256:authority",
		"sha256:route",
		[]provider.Message{
			messageAt(provider.RoleUser, "I prefer deterministic ledgers", 1),
		},
		NarrativeLimits{},
		now,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"technical_concepts": []any{},
		"files_and_code":     []any{},
		"errors_and_fixes":   []any{},
		"pending_jobs":       []any{},
		"current_work":       []any{},
		"next_steps":         []any{},
		"critical_context":   []any{},
		"decisions":          []any{},
		"rationale":          []any{},
		"preferences": []map[string]any{{
			"text":               "Prefer deterministic state.",
			"source_message_ids": []string{input.Excerpts[0].MessageID},
		}},
		"unresolved": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := ValidateNarrativeJSON(
		raw, input, NarrativeLimits{}, 2, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	text, err := RenderNarrativeDigest(artifact, 0)
	if err != nil ||
		!strings.Contains(text, "non-authoritative") ||
		!strings.Contains(text, "Prefer deterministic state.") {
		t.Fatalf("digest = %q err=%v", text, err)
	}
	if omitted, err := RenderNarrativeDigest(artifact, 8); err != nil || omitted != "" {
		t.Fatalf("oversized digest = %q err=%v", omitted, err)
	}
}
