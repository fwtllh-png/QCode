package agentcontext

import (
	"strings"
	"testing"
)

func sample() Summary {
	return Summary{
		Goal:      "make the parser accept trailing commas",
		Todos:     []Todo{{Title: "update the lexer", Status: "in_progress"}},
		DoneTodos: 2,
		Failures: []Failure{
			{Kind: KindVerify, Name: "affected", Reason: "failed: go test ./parser", Turn: 3, Count: 2},
		},
		Changes:       []CompactionChange{{Path: "parser/lex.go", Turn: 3, Read: true}},
		CriticalPaths: []string{"parser/lex.go"},
		Facts:         []CompactionFact{{Line: "definition Lex parser/lex.go:41"}},
		Digest:        []string{"assistant: editing the lexer", "user: also handle arrays"},
	}
}

func TestRenderKeepsSectionsInPriorityOrder(t *testing.T) {
	text, truncated, sections := sample().Render(0)
	if truncated {
		t.Fatalf("unbounded budget truncated: %s", text)
	}
	want := []string{SectionGoals, SectionTodos, SectionFailures, SectionChanges, SectionCritical, SectionFacts, SectionDigest}
	if len(sections) != len(want) {
		t.Fatalf("sections = %v, want %v", sections, want)
	}
	for index, name := range want {
		if sections[index] != name {
			t.Fatalf("sections = %v, want %v", sections, want)
		}
	}
	if !strings.HasPrefix(text, MarkerStart) || !strings.HasSuffix(text, MarkerEnd) {
		t.Fatalf("summary is not wrapped in markers: %s", text)
	}
	for _, fragment := range []string{
		"Goal: make the parser accept trailing commas",
		"2 step(s) already done",
		"update the lexer [in_progress]",
		"verify affected: failed: go test ./parser (turn 3, 2 times)",
		"parser/lex.go (turn 3) — nothing verified it",
		"Critical paths: parser/lex.go",
		"definition Lex parser/lex.go:41",
		"user: also handle arrays",
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("summary missing %q:\n%s", fragment, text)
		}
	}
}

func TestRenderDropsCheapestSectionsFirst(t *testing.T) {
	summary := sample()
	full, _, _ := summary.Render(0)
	// Budget enough for the leading sections but not the tail.
	text, truncated, sections := summary.Render(len(full) - 120)
	if !truncated {
		t.Fatalf("expected truncation at a smaller budget:\n%s", text)
	}
	if !strings.Contains(text, "Goal: make the parser") {
		t.Fatalf("goal was dropped before the digest:\n%s", text)
	}
	if !strings.Contains(text, truncationNotice) {
		t.Fatalf("truncated summary did not admit the cut:\n%s", text)
	}
	for _, name := range sections {
		if name == SectionDigest {
			t.Fatalf("digest survived a budget that forced truncation: %v", sections)
		}
	}
}

// A budget too small for any section still produces the wrapper, so the thread
// keeps a visible trace that history was dropped.
func TestRenderKeepsTheWrapperWhenNothingFits(t *testing.T) {
	text, truncated, sections := sample().Render(10)
	if !truncated || len(sections) != 0 {
		t.Fatalf("truncated=%v sections=%v", truncated, sections)
	}
	if !strings.Contains(text, MarkerStart) || !strings.Contains(text, truncationNotice) {
		t.Fatalf("summary lost its wrapper:\n%s", text)
	}
	if strings.Contains(text, "Goal:") {
		t.Fatalf("a 10 byte budget rendered a section:\n%s", text)
	}
}

func TestRenderReportsNothingForEmptySummary(t *testing.T) {
	text, truncated, sections := Summary{}.Render(1024)
	if text != "" || truncated || sections != nil {
		t.Fatalf("empty summary rendered %q truncated=%v sections=%v", text, truncated, sections)
	}
}

func TestCarryIgnoresOrdinaryText(t *testing.T) {
	if _, ok := Carry("user: nothing to see here"); ok {
		t.Fatal("Carry claimed an ordinary message was a summary")
	}
	if _, ok := Carry(MarkerStart + "\n\n" + MarkerEnd); ok {
		t.Fatal("Carry accepted an empty body")
	}
}

func TestRenderReportsOmittedDigest(t *testing.T) {
	text, truncated, sections := Summary{
		Digest:        []string{"user: newest kept"},
		OmittedDigest: 4,
	}.Render(0)
	if truncated || len(sections) != 1 || sections[0] != SectionDigest {
		t.Fatalf("truncated=%v sections=%v", truncated, sections)
	}
	if !strings.Contains(text, "user: newest kept") ||
		!strings.Contains(text, "(4 more not listed)") {
		t.Fatalf("omitted digest was not visible:\n%s", text)
	}
}

func TestCarriedDigestReadsStructuredSection(t *testing.T) {
	text, _, _ := Summary{
		Digest:        []string{"assistant: newest", "user: older"},
		OmittedDigest: 3,
	}.Render(0)
	lines, omitted, ok := CarriedDigest(text)
	if !ok || omitted != 3 || len(lines) != 2 ||
		lines[0] != "assistant: newest" || lines[1] != "user: older" {
		t.Fatalf("carried=%v omitted=%d ok=%v", lines, omitted, ok)
	}
	if _, _, ok := CarriedDigest("user: nothing to see here"); ok {
		t.Fatal("ordinary text reported a carried digest")
	}
}

func TestDigestDropsOldestEntriesUnderPressure(t *testing.T) {
	summary := Summary{Digest: []string{"newest line", "middle line", "oldest line"}}
	full, _, _ := summary.Render(0)
	// Just short of the whole digest: the notice is charged whether or not it is
	// needed, so the budget has to cover it as well.
	text, truncated, sections := summary.Render(len(full) + len(truncationNotice) - 12)
	if !truncated {
		t.Fatalf("expected the digest to be cut:\n%s", text)
	}
	if len(sections) != 1 || sections[0] != SectionDigest {
		t.Fatalf("sections = %v", sections)
	}
	if !strings.Contains(text, "newest line") {
		t.Fatalf("digest dropped the newest entry:\n%s", text)
	}
	if strings.Contains(text, "oldest line") {
		t.Fatalf("digest kept the oldest entry over the newest:\n%s", text)
	}
}

func TestChangeStateNamesEveryGap(t *testing.T) {
	state := CompactionChange{Path: "a.go", Diagnostics: true}.state()
	for _, fragment := range []string{"nothing verified it", "written without being read first", "diagnostics still failing"} {
		if !strings.Contains(state, fragment) {
			t.Fatalf("state %q missing %q", state, fragment)
		}
	}
	if got := (CompactionChange{Path: "a.go", Read: true, Verified: true}).state(); got != "verified" {
		t.Fatalf("state = %q, want verified", got)
	}
}

func TestSortChangesPutsUnverifiedAndOldestFirst(t *testing.T) {
	changes := []CompactionChange{
		{Path: "z.go", Turn: 9, Verified: true},
		{Path: "b.go", Turn: 4},
		{Path: "a.go", Turn: 4},
	}
	SortChanges(changes)
	if changes[0].Path != "a.go" || changes[1].Path != "b.go" || changes[2].Path != "z.go" {
		t.Fatalf("order = %v", changes)
	}
}
