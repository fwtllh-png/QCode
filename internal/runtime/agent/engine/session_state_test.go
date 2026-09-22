package engine

import (
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	turnhistory "github.com/fwtllh-png/QCode/internal/adapter/tool/turnhistory"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
)

func TestSessionStatePartitionSurvivesProjectedTailWithoutCompact(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("one"), textStream("two"), textStream("three"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Workspace = t.TempDir()
	engine.turn = 1
	engine.context.Evidence().BeginTurn(1)
	if err := engine.ApplyPlan(interact.Plan{
		Objective: "teach the parser about trailing commas",
		Steps: []interact.PlanStep{{
			Title: "update the lexer", Status: interact.StepInProgress,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	engine.observeChangeEvidence(tool.WorkspaceChange{
		Path: "parser/lex.go", Kind: tool.WorkspaceModified,
	})

	var compacted int
	for _, prompt := range []string{"first", "second", "third"} {
		if _, err := engine.Run(t.Context(), prompt, func(event Event) error {
			if event.State == Compacting {
				compacted++
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if compacted != 0 {
		t.Fatalf("compact events = %d, want none", compacted)
	}
	if len(runtime.requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(runtime.requests))
	}
	last := runtime.requests[2].Messages
	joined := joinMessageText(last)
	if !strings.Contains(joined, "teach the parser about trailing commas") ||
		!strings.Contains(joined, "update the lexer") ||
		!strings.Contains(joined, "parser/lex.go") ||
		!strings.Contains(joined, agentcontext.TruthMarkerStart) ||
		!strings.Contains(joined, "turn_history") ||
		!strings.Contains(joined, "Do not search the repository") {
		t.Fatalf("projected sample lost session state: %s", joined)
	}
	if countWorldSection(engine.History(), promptcontext.PartitionSessionState) == 0 {
		t.Fatal("session state never entered durable world history")
	}
}

func TestOmittedTurnHintSurvivesOldSessionWithoutGuessingP2s(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("Turn 1 conclusions listed five P2s: missing overflow test."),
		textStream("still working"),
		textStream("searching the repo"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Workspace = t.TempDir()
	for _, prompt := range []string{"audit the parser", "continue", "where are the five P2s"} {
		if _, err := engine.Run(t.Context(), prompt, nil); err != nil {
			t.Fatal(err)
		}
	}
	last := runtime.requests[len(runtime.requests)-1].Messages
	joined := joinMessageText(last)
	if !strings.Contains(joined, "turn_history") ||
		!strings.Contains(joined, "turn=1") ||
		!strings.Contains(joined, "Do not search the repository") {
		t.Fatalf("omitted-turn hint missing: %s", joined)
	}
	if strings.Contains(joined, "missing overflow test") {
		t.Fatalf("sample invented or leaked turn-1 P2 text: %s", joined)
	}
	var sawTurnOne bool
	for _, checkpoint := range engine.closedTurnCheckpointMessages() {
		text := checkpoint.Text()
		if strings.Contains(text, "missing overflow test") {
			t.Fatalf("backfill invented P2 text: %s", text)
		}
		if checkpoint.Turn == 1 && strings.Contains(text, turnhistory.Name) {
			sawTurnOne = true
		}
	}
	if !sawTurnOne {
		t.Fatalf("closed turn 1 was not backfilled: %+v", engine.closedTurnCheckpointMessages())
	}
}

func TestSessionStateCarriesConfirmedContinuityIntoNextTurn(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("hasGlobalLock must be held across the lease."),
		textStream("I will restore previous analysis"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Workspace = t.TempDir()
	engine.context.Evidence().Observe(agentcontext.EvidenceFact{
		Kind:   agentcontext.KindDefinition,
		Path:   "eds_metaserver.cc",
		Line:   88,
		Symbol: "hasGlobalLock",
		Tool:   "search_definition",
		Turn:   1,
	})
	if _, err := engine.Run(t.Context(), "analyze the lock", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Run(t.Context(), "how should we fix it", nil); err != nil {
		t.Fatal(err)
	}
	joined := joinMessageText(runtime.requests[1].Messages)
	if !strings.Contains(joined, "hasGlobalLock must be held across the lease") ||
		!strings.Contains(joined, "eds_metaserver.cc:88 hasGlobalLock") ||
		!strings.Contains(joined, "Do not call turn_history or search the repository") {
		t.Fatalf("next turn lost continuity: %s", joined)
	}
}

func TestResumeHintSurfacesCompletedPlanAndReadPaths(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("one"), textStream("two"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Workspace = t.TempDir()
	if err := engine.ApplyPlan(interact.Plan{
		Objective: "fix the five P2s",
		Steps: []interact.PlanStep{
			{Title: "audit multi_paxos_node.h", Status: interact.StepDone},
			{Title: "fix overflow in accept()", Status: interact.StepPending},
		},
	}); err != nil {
		t.Fatal(err)
	}
	engine.observePath(agentcontext.SourceRead, "multi_paxos_node.h")
	if _, err := engine.Run(t.Context(), "continue the P2 fixes", nil); err != nil {
		t.Fatal(err)
	}
	first := joinMessageText(runtime.requests[0].Messages)
	if !strings.Contains(first, "Do not repeat completed plan steps") ||
		!strings.Contains(first, "Next open work: fix overflow in accept().") ||
		!strings.Contains(first, "Already-read paths: multi_paxos_node.h.") ||
		!strings.Contains(first, "Absence from the visible tail is not a reason to file_read") ||
		!strings.Contains(first, "A dirty git status or git_diff is not a reason to file_read") ||
		!strings.Contains(first, "After search_text returns line hits") ||
		strings.Contains(first, "file may have changed") ||
		strings.Contains(first, "missing from the visible tail") ||
		strings.Contains(first, "P2:") {
		t.Fatalf("sample missing resume hint: %s", first)
	}
	if err := engine.ApplyPlan(interact.Plan{
		Objective: "fix the five P2s",
		Steps: []interact.PlanStep{
			{Title: "audit multi_paxos_node.h", Status: interact.StepDone},
			{Title: "fix overflow in accept()", Status: interact.StepDone},
			{Title: "fix snapshot restore", Status: interact.StepPending},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Run(t.Context(), "keep going", nil); err != nil {
		t.Fatal(err)
	}
	live := lastWorldSectionText(
		runtime.requests[1].Messages,
		promptcontext.PartitionSessionState,
	)
	if !strings.Contains(live, "Next open work: fix snapshot restore.") ||
		strings.Contains(live, "Next open work: fix overflow in accept().") {
		t.Fatalf("FrozenWorld kept stale resume hint: %s", live)
	}
}

func TestResumeHintKeepsReadsOutsideWorkingSetLimit(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{textStream("ok")}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Workspace = t.TempDir()
	engine.options.WorkingSetLimit = 2
	engine.turn = 1
	if err := engine.ApplyPlan(interact.Plan{
		Objective: "keep already-read paths",
		Steps: []interact.PlanStep{
			{Title: "audit a.go", Status: interact.StepDone},
			{Title: "fix overflow", Status: interact.StepPending},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"late.go", "a.go", "b.go"} {
		engine.observePath(agentcontext.SourceRead, path)
	}
	if got := engine.resumeReadPaths(); len(got) != 3 ||
		got[0] != "a.go" || got[1] != "b.go" || got[2] != "late.go" {
		t.Fatalf("resume paths = %v, want all SourceRead", got)
	}
	entries := engine.WorkingSetEntries(1, engine.options.WorkingSetLimit)
	if len(entries) != 2 {
		t.Fatalf("prompt working set = %+v, want top-2", entries)
	}
	for _, entry := range entries {
		if entry.Path == "late.go" {
			t.Fatalf("prompt working set kept late.go: %+v", entries)
		}
	}
	if _, err := engine.Run(t.Context(), "continue", nil); err != nil {
		t.Fatal(err)
	}
	first := joinMessageText(runtime.requests[0].Messages)
	if !strings.Contains(first, "Already-read paths:") ||
		!strings.Contains(first, "late.go") ||
		!strings.Contains(first, "a.go") ||
		!strings.Contains(first, "b.go") {
		t.Fatalf("resume hint dropped a SourceRead path: %s", first)
	}
}

func TestSessionStatePartitionStaysAbsentWithoutLedgerFacts(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{textStream("done")}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	if _, err := engine.Run(t.Context(), "hello", nil); err != nil {
		t.Fatal(err)
	}
	if countWorldSection(engine.History(), promptcontext.PartitionSessionState) != 0 {
		t.Fatalf("empty ledgers still projected session state: %+v", engine.History())
	}
}

func lastWorldSectionText(messages []provider.Message, id string) string {
	text := ""
	for _, message := range messages {
		entry, _, ok := agentcontext.InspectWorldMessage(message)
		if ok && entry.ID == id {
			text = message.Text()
		}
	}
	return text
}

func joinMessageText(messages []provider.Message) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		if text := message.Text(); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}
