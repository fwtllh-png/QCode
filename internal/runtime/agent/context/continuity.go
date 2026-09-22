package agentcontext

import (
	"fmt"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

const ContinuitySource = "runtime.continuity"

// ContinuityInput is the sourced working state the next sample must still
// see after the visible tail clips older investigation turns: the latest
// completed user-facing conclusion and tool-located sites.
type ContinuityInput struct {
	Conclusion    string
	Sites         []string
	SourceTurns   []uint64
	Next          string
	PreferredTurn uint64
}

func LastAssistantConclusion(messages []provider.Message) string {
	var text string
	for _, message := range messages {
		if IsWorldStateMessage(message) || message.Role != provider.RoleAssistant {
			continue
		}
		var parts []string
		for _, block := range message.Blocks {
			if block.Type != provider.ContentText {
				continue
			}
			if value := strings.TrimSpace(block.Text); value != "" {
				parts = append(parts, value)
			}
		}
		if joined := strings.TrimSpace(strings.Join(parts, "\n")); joined != "" {
			text = joined
		}
	}
	return text
}

func ContinuitySites(facts []EvidenceFact) []string {
	var sites []string
	seen := make(map[string]struct{}, len(facts))
	for _, fact := range facts {
		site := FormatContinuitySite(fact)
		if site == "" {
			continue
		}
		if _, exists := seen[site]; exists {
			continue
		}
		seen[site] = struct{}{}
		sites = append(sites, site)
	}
	return sites
}

func FormatContinuitySite(fact EvidenceFact) string {
	if fact.Line <= 0 {
		return ""
	}
	path := strings.TrimSpace(fact.Path)
	if path == "" {
		return ""
	}
	site := fmt.Sprintf("%s:%d", path, fact.Line)
	if symbol := strings.TrimSpace(fact.Symbol); symbol != "" {
		site += " " + symbol
	}
	return site
}

func UniqueTurns(turns []uint64) []uint64 {
	if len(turns) == 0 {
		return nil
	}
	seen := make(map[uint64]struct{}, len(turns))
	var result []uint64
	for _, turn := range turns {
		if turn == 0 {
			continue
		}
		if _, exists := seen[turn]; exists {
			continue
		}
		seen[turn] = struct{}{}
		result = append(result, turn)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func FormatContinuityHint(input ContinuityInput) string {
	return FormatContinuityHintBudgeted(input, 0)
}

func FormatContinuityHintBudgeted(input ContinuityInput, budget int) string {
	sites := append([]string(nil), input.Sites...)
	omitted := 0
	conclusion := strings.TrimSpace(input.Conclusion)
	for {
		hint := formatContinuityHint(ContinuityInput{
			Conclusion:    conclusion,
			Sites:         sites,
			SourceTurns:   input.SourceTurns,
			Next:          input.Next,
			PreferredTurn: input.PreferredTurn,
		}, omitted)
		if budget <= 0 || len(hint) <= budget || (len(sites) == 0 && conclusion == "") {
			return hint
		}
		if len(sites) > 0 {
			sites = sites[:len(sites)-1]
			omitted++
			continue
		}
		if conclusion != "" {
			conclusion = TruncateUTF8(conclusion, max(0, len(conclusion)-16))
			continue
		}
		return hint
	}
}

func formatContinuityHint(input ContinuityInput, omitted int) string {
	conclusion := strings.TrimSpace(input.Conclusion)
	next := strings.TrimSpace(input.Next)
	if conclusion == "" && len(input.Sites) == 0 && omitted == 0 {
		return ""
	}
	var parts []string
	if turns := UniqueTurns(input.SourceTurns); len(turns) > 0 {
		parts = append(parts, "Confirmed continuity from turn "+formatTurnList(turns)+".")
	} else {
		parts = append(parts, "Confirmed continuity.")
	}
	if conclusion != "" {
		parts = append(parts, "Conclusion: "+conclusion)
		if !strings.HasSuffix(conclusion, ".") {
			parts[len(parts)-1] += "."
		}
	}
	if len(input.Sites) > 0 || omitted > 0 {
		if len(input.Sites) > 0 {
			parts = append(parts, "Located sites: "+strings.Join(input.Sites, ", ")+".")
		}
		if omitted > 0 {
			parts = append(parts, fmt.Sprintf("(%d more located sites omitted).", omitted))
		}
	}
	parts = append(parts,
		"Do not call "+TurnHistoryToolName+" or search the repository to restore this analysis. Read only uncovered windows needed for the current request.",
	)
	if next != "" {
		parts = append(parts, "Next open work: "+next+".")
	}
	if input.PreferredTurn > 0 {
		parts = append(parts, fmt.Sprintf(
			"If a listed site is insufficient, call %s with preferred_turn=%d; the first page ends with that turn's findings index.",
			TurnHistoryToolName, input.PreferredTurn,
		))
	}
	return strings.Join(parts, " ")
}

func ContinuityRetrievalEntity(input ContinuityInput) (TruthEntity, bool) {
	return ContinuityRetrievalEntityBudgeted(input, 0)
}

func ContinuityRetrievalEntityBudgeted(
	input ContinuityInput, budget int,
) (TruthEntity, bool) {
	hint := FormatContinuityHintBudgeted(input, budget)
	if hint == "" {
		return TruthEntity{}, false
	}
	entity := NewTruthEntity(EntityFact, "continuity", hint, ContinuitySource)
	entity.normalizeLifecycle()
	return entity, true
}

func SessionStateContinuityHint(capsule TruthCapsule) string {
	for _, entity := range capsule.Entities {
		if entity.Kind == EntityFact && entity.Source == ContinuitySource {
			return entity.Value
		}
	}
	return ""
}

func formatTurnList(turns []uint64) string {
	parts := make([]string, 0, len(turns))
	for _, turn := range turns {
		parts = append(parts, fmt.Sprintf("%d", turn))
	}
	return strings.Join(parts, ", ")
}
