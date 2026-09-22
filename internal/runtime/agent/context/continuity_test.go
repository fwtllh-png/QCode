package agentcontext

import (
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func TestLastAssistantConclusionUsesLatestUserFacingText(t *testing.T) {
	got := LastAssistantConclusion([]provider.Message{
		{Role: provider.RoleUser, Blocks: []provider.ContentBlock{{
			Type: provider.ContentText, Text: "how should we fix it",
		}}},
		{Role: provider.RoleAssistant, Blocks: []provider.ContentBlock{
			{Type: provider.ContentReasoning, Text: "restore earlier turns"},
			{Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{Name: "search_text"}},
		}},
		{Role: provider.RoleAssistant, Blocks: []provider.ContentBlock{{
			Type: provider.ContentText, Text: "hasGlobalLock is the root cause",
		}}},
	})
	if got != "hasGlobalLock is the root cause" {
		t.Fatalf("conclusion = %q", got)
	}
}

func TestContinuitySitesKeepSymbolAndSkipFileOnlyHits(t *testing.T) {
	sites := ContinuitySites([]EvidenceFact{
		{Kind: KindDefinition, Path: "eds_metaserver.cc", Line: 88, Symbol: "hasGlobalLock"},
		{Kind: KindTextMatch, Path: "readme.md"},
		{Kind: KindDefinition, Path: "eds_metaserver.cc", Line: 88, Symbol: "hasGlobalLock"},
		{Kind: KindReference, Path: "types.h", Line: 12},
	})
	if len(sites) != 2 ||
		sites[0] != "eds_metaserver.cc:88 hasGlobalLock" ||
		sites[1] != "types.h:12" {
		t.Fatalf("sites = %v", sites)
	}
}

func TestContinuityHintIsMandatoryAndDoesNotInventOldLists(t *testing.T) {
	hint := FormatContinuityHint(ContinuityInput{
		Conclusion:  "hasGlobalLock must be held across the lease",
		Sites:       []string{"eds_metaserver.cc:88 hasGlobalLock"},
		SourceTurns: []uint64{6, 4, 6},
		Next:        "patch the lease path",
	})
	if !strings.Contains(hint, "Confirmed continuity from turn 4, 6.") ||
		!strings.Contains(hint, "Conclusion: hasGlobalLock must be held across the lease.") ||
		!strings.Contains(hint, "Located sites: eds_metaserver.cc:88 hasGlobalLock.") ||
		!strings.Contains(hint, "Do not call "+TurnHistoryToolName+" or search the repository") ||
		!strings.Contains(hint, "Next open work: patch the lease path.") ||
		strings.Contains(hint, "P2:") {
		t.Fatalf("hint = %q", hint)
	}
	entity, ok := ContinuityRetrievalEntity(ContinuityInput{
		Sites: []string{"eds_metaserver.cc:88 hasGlobalLock"},
	})
	if !ok || entity.Retention != RetentionMandatory ||
		entity.Source != ContinuitySource ||
		entity.Kind != EntityFact {
		t.Fatalf("entity = %+v ok=%v", entity, ok)
	}
	capsule := MandatorySessionState(BuildTruthCapsule(TruthProjection{
		Compatibility: Compatibility{SchemaVersion: TruthSchemaVersion},
		ModelID:       "model",
		ContextTokens: 4096,
		Evidence: EvidenceDelta{Facts: []EvidenceFact{{
			Kind: KindDefinition, Path: "parser/lex.go", Line: 41, Symbol: "Lex",
		}}},
		ExtraEntities: []TruthEntity{entity},
	}))
	if SessionStateContinuityHint(capsule) != entity.Value {
		t.Fatalf("capsule hint = %q", SessionStateContinuityHint(capsule))
	}
	var sawEvidenceFact bool
	for _, item := range capsule.Entities {
		if item.Kind == EntityFact && item.Source == "runtime.evidence" {
			sawEvidenceFact = true
		}
	}
	if sawEvidenceFact {
		t.Fatalf("continuity promotion leaked refreshable evidence: %+v", capsule.Entities)
	}
	rendered, err := RenderSessionState(capsule, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.Text, "Located sites: eds_metaserver.cc:88 hasGlobalLock.") ||
		strings.Index(rendered.Text, "Confirmed continuity") >
			strings.Index(rendered.Text, TruthMarkerStart) {
		t.Fatalf("session state = %s", rendered.Text)
	}
}

func TestFormatContinuityHintBudgetOmitsOverflowingSites(t *testing.T) {
	sites := make([]string, 0, 12)
	for index := 0; index < 12; index++ {
		sites = append(sites, "internal/pkg/site_"+string(rune('a'+index))+".go:10 Sym")
	}
	full := FormatContinuityHint(ContinuityInput{
		Conclusion: strings.Repeat("hold the lock ", 8),
		Sites:      sites,
	})
	budget := len(full) - 1
	hint := FormatContinuityHintBudgeted(ContinuityInput{
		Conclusion: strings.Repeat("hold the lock ", 8),
		Sites:      sites,
	}, budget)
	if hint == "" || len(hint) > budget {
		t.Fatalf("budgeted hint = %q len=%d budget=%d", hint, len(hint), budget)
	}
	if !strings.Contains(hint, "more located sites omitted") ||
		!strings.Contains(hint, "Do not call "+TurnHistoryToolName) {
		t.Fatalf("budgeted hint = %q", hint)
	}
}

func TestContinuityHintEmptyWithoutConclusionOrSites(t *testing.T) {
	if got := FormatContinuityHint(ContinuityInput{Next: "keep looking"}); got != "" {
		t.Fatalf("hint = %q", got)
	}
	if _, ok := ContinuityRetrievalEntity(ContinuityInput{}); ok {
		t.Fatal("empty continuity emitted")
	}
}
