package prompt

import (
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/repository"
)

func receiptFor(t *testing.T, receipts []Receipt, kind string) Receipt {
	t.Helper()
	for _, receipt := range receipts {
		if receipt.Kind == kind {
			return receipt
		}
	}
	t.Fatalf("no %q receipt in %+v", kind, receipts)
	return Receipt{}
}

func readyMap() repository.Map {
	return repository.Map{
		Status: repoindex.StatusReady, FileCount: 12, SymbolCount: 40,
		Build: []string{"go.mod"}, Entries: []string{"cmd/app/main.go"},
		Directories: []repository.Directory{
			{Path: "internal/store", Files: 8, Symbols: 30, Languages: []string{"go"}},
		},
		OmittedDirectories: 3,
		Outlines: []repository.Outline{{
			Path: "internal/store/store.go",
			Symbols: []repoindex.Symbol{
				{Path: "internal/store/store.go", Name: "Store", Kind: "type", Line: 10},
				{Path: "internal/store/store.go", Name: "Get", Kind: "method", Line: 22, Container: "Store"},
			},
			Truncated: true,
		}},
	}
}

func TestAssembleTurnRendersBothSectionsAsSystemMessages(t *testing.T) {
	assembled := AssembleTurn(TurnOptions{
		Turn: 7, RepoMap: readyMap(),
		WorkingSet: []agentcontext.WorkingSetEntry{
			{Path: "internal/store/store.go", Sources: []agentcontext.WorkingSetSource{agentcontext.SourceEdited, agentcontext.SourceRead}, LastTurn: 7},
			{Path: "docs/plan.md", Sources: []agentcontext.WorkingSetSource{agentcontext.SourcePinned}, LastTurn: 2, Critical: true},
		},
		Evidence: agentcontext.EvidenceSnapshot{Facts: []agentcontext.EvidenceFact{{
			Kind: agentcontext.KindDefinition, Path: "internal/store/store.go",
			Line: 10, Symbol: "Store", Tool: "search_definition", Turn: 7,
		}}},
	})
	if len(assembled.Messages) != 3 {
		t.Fatalf("messages = %d, want repo map, working set, and evidence", len(assembled.Messages))
	}
	for _, message := range assembled.Messages {
		if message.Role != provider.RoleSystem {
			t.Fatalf("role = %q, want system so adapters can hoist it instead of breaking role alternation", message.Role)
		}
	}
	mapText := assembled.Messages[0].Text()
	for _, want := range []string{
		"[repo_map turn=7 index=ready]", "12 indexed files, 40 declarations",
		"build: go.mod", "entry: cmd/app/main.go",
		"internal/store — 8 files, 30 declarations, go",
		"(3 more directories not listed)",
		"10 type Store", "22 method Get (in Store)", "(more declarations not listed)",
	} {
		if !strings.Contains(mapText, want) {
			t.Fatalf("repo map missing %q:\n%s", want, mapText)
		}
	}
	setText := assembled.Messages[1].Text()
	for _, want := range []string{
		"[working_set turn=7]",
		"internal/store/store.go — edited, read (turn 7)",
		"docs/plan.md — pinned (turn 2) critical",
	} {
		if !strings.Contains(setText, want) {
			t.Fatalf("working set missing %q:\n%s", want, setText)
		}
	}
	if strings.Contains(setText, "read what you need") ||
		strings.Contains(setText, "does not contain that read") ||
		strings.Contains(setText, "file may have changed") {
		t.Fatalf("working set still authorizes tail-missing re-read:\n%s", setText)
	}
	if !strings.Contains(setText, "prior text is no longer in this sample") ||
		!strings.Contains(setText, "A dirty git status or git_diff is not a reason to file_read") {
		t.Fatalf("working set missing resume constraint:\n%s", setText)
	}
	if strings.Contains(setText, "critical\n  internal/store") {
		t.Fatalf("working set order changed:\n%s", setText)
	}
	if len(assembled.Selections) != 2 ||
		assembled.Selections[0].Reasons[0] != "edited" ||
		len(assembled.Selections[0].Evidence) != 1 ||
		assembled.Selections[0].Evidence[0].Symbol != "Store" ||
		!assembled.Selections[0].Included ||
		assembled.Selections[0].Truncated {
		t.Fatalf("selections = %+v", assembled.Selections)
	}
}

func TestAssembleTurnSaysWhyTheMapIsMissing(t *testing.T) {
	assembled := AssembleTurn(TurnOptions{
		Turn:    3,
		RepoMap: repository.Map{Status: repoindex.StatusDegraded, Detail: "database is locked"},
	})
	if len(assembled.Messages) != 1 {
		t.Fatalf("messages = %d, want only the degraded map", len(assembled.Messages))
	}
	text := assembled.Messages[0].Text()
	for _, want := range []string{
		"index=degraded", "database is locked", "may still exist", "search_text",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("degraded map missing %q:\n%s", want, text)
		}
	}

	mute := AssembleTurn(TurnOptions{
		Turn:    3,
		RepoMap: repository.Map{Status: repoindex.StatusDisabled},
	})
	if !strings.Contains(mute.Messages[0].Text(), "no detail was reported") {
		t.Fatalf("text = %q", mute.Messages[0].Text())
	}
}

func TestAssembleTurnStaysSilentWithNothingToReport(t *testing.T) {
	assembled := AssembleTurn(TurnOptions{
		Turn:    1,
		RepoMap: repository.Map{Status: repoindex.StatusReady},
	})
	if len(assembled.Messages) != 0 {
		t.Fatalf("messages = %+v, want none for an empty repository", assembled.Messages)
	}
	// An index that answered and had nothing still leaves a receipt, so the empty
	// map is auditable rather than lost.
	if len(assembled.Receipts) != 1 || assembled.Receipts[0].Kind != PartitionRepoMap {
		t.Fatalf("receipts = %+v", assembled.Receipts)
	}
	if assembled.Receipts[0].RetainedBytes != 0 || assembled.Receipts[0].Truncated {
		t.Fatalf("receipt = %+v, want an empty untruncated section", assembled.Receipts[0])
	}

	// A caller that asked for neither section gets nothing at all, which is how a
	// disabled map or working set is expressed.
	nothing := AssembleTurn(TurnOptions{Turn: 1})
	if len(nothing.Messages) != 0 || len(nothing.Receipts) != 0 {
		t.Fatalf("unrequested sections produced %+v / %+v", nothing.Messages, nothing.Receipts)
	}
}

func TestAssembleTurnReportsBudgetTruncation(t *testing.T) {
	const budget = 256
	assembled := AssembleTurn(TurnOptions{
		Turn: 5, RepoMap: readyMap(),
		Budgets: map[string]Budget{PartitionRepoMap: {MaxBytes: budget}},
	})
	receipt := receiptFor(t, assembled.Receipts, PartitionRepoMap)
	if !receipt.Truncated || receipt.TruncationReason != "byte_budget" {
		t.Fatalf("receipt = %+v, want a byte-budget truncation", receipt)
	}
	if receipt.RetainedBytes == 0 || receipt.OriginalBytes <= budget {
		t.Fatalf("receipt = %+v", receipt)
	}
	// The notice is what tells a model that the missing lines were cut rather
	// than absent, so it has to fit inside the budget, not on top of it.
	text := assembled.Messages[0].Text()
	if !strings.HasSuffix(text, truncationNotice) {
		t.Fatalf("cut section = %q, want it to say it was cut", text)
	}
	if len(text) > budget {
		t.Fatalf("cut section is %d bytes, over its %d budget", len(text), budget)
	}

	limited := AssembleTurn(TurnOptions{
		Turn: 5, RepoMap: readyMap(),
		Budgets: map[string]Budget{PartitionRepoMap: {MaxTokens: 4}},
	})
	if reason := receiptFor(t, limited.Receipts, PartitionRepoMap).TruncationReason; reason != "token_budget" {
		t.Fatalf("reason = %q, want token_budget", reason)
	}
}

func TestWorkingSetSelectionsExplainTestsAndPerEntryTruncation(t *testing.T) {
	assembled := AssembleTurn(TurnOptions{
		Turn: 4,
		WorkingSet: []agentcontext.WorkingSetEntry{
			{
				Path:     "pkg/calc_test.go",
				Sources:  []agentcontext.WorkingSetSource{agentcontext.SourceSearch},
				LastTurn: 4, Score: 5,
			},
			{
				Path:     strings.Repeat("very-long-directory/", 8) + "implementation.go",
				Sources:  []agentcontext.WorkingSetSource{agentcontext.SourceRead},
				LastTurn: 4, Score: 10,
			},
		},
		Evidence: agentcontext.EvidenceSnapshot{Facts: []agentcontext.EvidenceFact{{
			Kind: agentcontext.KindTest, Path: "pkg/calc_test.go",
			Tool: "search_related_tests", Turn: 4,
		}}},
		Budgets: map[string]Budget{
			PartitionWorkingSetLedger: {MaxBytes: 220},
		},
	})
	if len(assembled.Selections) != 2 {
		t.Fatalf("selections = %+v", assembled.Selections)
	}
	if assembled.Selections[0].Kind != "test" ||
		assembled.Selections[0].Evidence[0].Kind != "test" {
		t.Fatalf("test selection = %+v", assembled.Selections[0])
	}
	truncated := 0
	for _, selection := range assembled.Selections {
		if selection.Truncated && selection.TruncationReason == "byte_budget" {
			truncated++
		}
	}
	if truncated == 0 {
		t.Fatalf("no per-entry truncation in %+v", assembled.Selections)
	}
}

func TestAssembleTurnDigestsAreStableAcrossIdenticalTurns(t *testing.T) {
	options := TurnOptions{
		Turn: 4, RepoMap: readyMap(),
		WorkingSet: []agentcontext.WorkingSetEntry{
			{Path: "a.go", Sources: []agentcontext.WorkingSetSource{agentcontext.SourceRead}, LastTurn: 4},
		},
	}
	first := AssembleTurn(options)
	second := AssembleTurn(options)
	for index := range first.Receipts {
		if first.Receipts[index].Digest != second.Receipts[index].Digest {
			t.Fatalf("digest %d differs: %q vs %q", index, first.Receipts[index].Digest, second.Receipts[index].Digest)
		}
	}
	// A different turn number is a different section, so the digest must move.
	options.Turn = 5
	if AssembleTurn(options).Receipts[0].Digest == first.Receipts[0].Digest {
		t.Fatal("digest ignored the turn number")
	}
}
