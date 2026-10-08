package prompt

import (
	"strings"
	"testing"

	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func TestContextSelectionHintUsesExactTurnsReasonsAndLegalArguments(t *testing.T) {
	result := agentcontext.ProjectionResult{Omissions: []agentcontext.ProjectionOmission{
		{Source: agentcontext.ProjectionSource{Turn: 1}, Reason: agentcontext.OmittedTurnLimit, Retrieval: &agentcontext.TurnRetrieval{Turn: 1}},
		{Source: agentcontext.ProjectionSource{Turn: 3}, Reason: agentcontext.OmittedTokenLimit, Retrieval: &agentcontext.TurnRetrieval{Turn: 3}},
	}}
	hint, err := ContextSelectionHint(result, 0)
	if err != nil || hint == nil {
		t.Fatalf("hint=%+v err=%v", hint, err)
	}
	text := hint.Text()
	for _, expected := range []string{"turn=1 reason=recent_tail_turns", "turn=3 reason=history_token_ceiling", `turn_history {"turn":1}`, `turn_history {"turn":3}`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %q in %s", expected, text)
		}
	}
	if strings.Contains(text, "preferred_turn") || strings.Contains(text, "1-3") {
		t.Fatalf("invented recovery parameter or range: %s", text)
	}
	if empty, err := ContextSelectionHint(agentcontext.ProjectionResult{}, 256); empty != nil || err != nil {
		t.Fatal("empty selection produced a hint")
	}
}

func TestContextSelectionHintBoundsSparseOmissionDirectory(t *testing.T) {
	var result agentcontext.ProjectionResult
	for turn := uint64(1); turn < 100; turn += 2 {
		result.Omissions = append(result.Omissions, agentcontext.ProjectionOmission{
			Source: agentcontext.ProjectionSource{Turn: turn}, Reason: agentcontext.OmittedCapacity,
			Retrieval: &agentcontext.TurnRetrieval{Turn: turn},
		})
	}
	const budget = 256 // Explicit test input, not a runtime limit.
	hint, err := ContextSelectionHint(result, budget)
	if err != nil || hint == nil || len(hint.Text()) > budget ||
		!strings.Contains(hint.Text(), "additional omission groups") || !strings.Contains(hint.Text(), `{"turn":99}`) {
		t.Fatalf("unbounded or misleading hint: %+v err=%v", hint, err)
	}
	if len(result.Omissions) != 50 {
		t.Fatal("rendering discarded the full omission metadata")
	}
	if _, err := ContextSelectionHint(result, 1); err == nil {
		t.Fatal("unrenderable configured budget was ignored")
	}
}
