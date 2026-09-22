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

func TestPostTurnPromotesCitedOpenWorkIntoSessionState(t *testing.T) {
	runtime := &sourceEchoNarrativeProvider{
		scriptedProvider: scriptedProvider{streams: []provider.Stream{
			textStream("continue after clip"),
		}},
	}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.SemanticNarrative = "post_turn"
	engine.options.Workspace = t.TempDir()
	if err := engine.ApplyPlan(interact.Plan{
		Objective: "teach the parser about trailing commas",
		Steps: []interact.PlanStep{{
			Title: "update the lexer", Status: interact.StepInProgress,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	seedOmittedHistory(engine)
	result, err := engine.RunPostTurnNarrative(
		t.Context(), "thread-1", "turn-3",
	)
	if err != nil || result.Fallback {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := engine.Run(t.Context(), "continue now", nil); err != nil {
		t.Fatal(err)
	}
	joined := joinMessageText(runtime.requests[len(runtime.requests)-1].Messages)
	if !strings.Contains(joined, "Write the parser implementation.") {
		t.Fatalf("promoted open work missing from session state: %s", joined)
	}
	if countWorldSection(engine.History(), promptcontext.PartitionSessionState) == 0 {
		t.Fatal("session state missing after promotion")
	}
}

func TestClosedTurnCheckpointIsAppendOnlyInDynamic(t *testing.T) {
	runtime := &sourceEchoNarrativeProvider{
		scriptedProvider: scriptedProvider{streams: []provider.Stream{
			textStream("first"), textStream("second"),
		}},
	}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.SemanticNarrative = "post_turn"
	engine.options.Workspace = t.TempDir()
	seedOmittedHistory(engine)
	if _, err := engine.RunPostTurnNarrative(
		t.Context(), "thread-1", "turn-3",
	); err != nil {
		t.Fatal(err)
	}
	first := engine.closedTurnCheckpointMessages()
	if len(first) != 1 ||
		!strings.Contains(first[0].Text(), agentcontext.CheckpointMarkerStart) {
		t.Fatalf("first checkpoints = %+v", first)
	}
	frozen := first[0].Text()
	if _, err := engine.RunPostTurnNarrative(
		t.Context(), "thread-1", "turn-3",
	); err != nil {
		t.Fatal(err)
	}
	second := engine.closedTurnCheckpointMessages()
	if len(second) != 1 || second[0].Text() != frozen {
		t.Fatalf("checkpoint rewritten: first=%q second=%+v", frozen, second)
	}
	if _, err := engine.Run(t.Context(), "continue now", nil); err != nil {
		t.Fatal(err)
	}
	joined := joinMessageText(runtime.requests[len(runtime.requests)-1].Messages)
	if !strings.Contains(joined, agentcontext.CheckpointMarkerStart) {
		t.Fatalf("sample missing dynamic checkpoint: %s", joined)
	}
}

func TestCanceledTurnCheckpointKeepsNextPlanAndReadPaths(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.turn = 4
	if err := engine.ApplyPlan(interact.Plan{
		Steps: []interact.PlanStep{
			{Title: "audit accept()", Status: interact.StepDone},
			{Title: "fix overflow in accept()", Status: interact.StepPending},
		},
	}); err != nil {
		t.Fatal(err)
	}
	engine.observePath(agentcontext.SourceRead, "multi_paxos_node.h")
	engine.sealClosedTurnMemory(agentcontext.CheckpointCanceled, nil, "")
	messages := engine.closedTurnCheckpointMessages()
	if len(messages) != 1 {
		t.Fatalf("checkpoints = %+v", messages)
	}
	text := messages[0].Text()
	if !strings.Contains(text, "next: fix overflow in accept()") ||
		!strings.Contains(text, "multi_paxos_node.h") {
		t.Fatalf("canceled checkpoint = %s", text)
	}
}

func TestFailedTurnCheckpointKeepsNextPlan(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.turn = 4
	if err := engine.ApplyPlan(interact.Plan{
		Steps: []interact.PlanStep{{
			Title: "finish parser tests", Status: interact.StepInProgress,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	engine.sealClosedTurnMemory(
		agentcontext.CheckpointFailed, nil, "provider timeout",
	)
	messages := engine.closedTurnCheckpointMessages()
	if len(messages) != 1 {
		t.Fatalf("checkpoints = %+v", messages)
	}
	if !strings.Contains(messages[0].Text(), "next: finish parser tests") {
		t.Fatalf("failed checkpoint lost open work: %s", messages[0].Text())
	}
	if !strings.Contains(messages[0].Text(), "provider timeout") {
		t.Fatalf("failed checkpoint = %s", messages[0].Text())
	}
}

func TestClosedTurnCheckpointsBackfillOmittedTurnsWithoutGuessing(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{textStream("ok")}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Workspace = t.TempDir()
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, "audit", 1),
		messageWithText(provider.RoleAssistant, "five P2s: missing overflow test", 1),
		messageWithText(provider.RoleUser, "continue", 2),
		messageWithText(provider.RoleAssistant, "working", 2),
		messageWithText(provider.RoleUser, "next", 3),
		messageWithText(provider.RoleAssistant, "still working", 3),
	}
	engine.turn = 3
	if _, err := engine.Run(t.Context(), "where are the five P2s", nil); err != nil {
		t.Fatal(err)
	}
	joined := joinMessageText(runtime.requests[len(runtime.requests)-1].Messages)
	if !strings.Contains(joined, "turn_history") ||
		!strings.Contains(joined, "preferred_turn=2") {
		t.Fatalf("restored session missing retrieval hint: %s", joined)
	}
	if strings.Contains(joined, "missing overflow test") {
		t.Fatalf("backfill leaked turn-1 P2 text: %s", joined)
	}
	var sawTurnOne, sawTurnTwo, sawTurnThree bool
	for _, checkpoint := range engine.closedTurnCheckpointMessages() {
		if strings.Contains(checkpoint.Text(), "missing overflow test") {
			t.Fatalf("backfill invented P2 text: %s", checkpoint.Text())
		}
		if checkpoint.Turn <= 3 && !strings.Contains(checkpoint.Text(), `"status":"unknown"`) {
			t.Fatalf("backfill invented terminal success: %s", checkpoint.Text())
		}
		switch checkpoint.Turn {
		case 1:
			sawTurnOne = strings.Contains(checkpoint.Text(), turnhistory.Name)
		case 2:
			sawTurnTwo = true
		case 3:
			sawTurnThree = true
		}
	}
	if !sawTurnOne || !sawTurnTwo || !sawTurnThree {
		t.Fatalf("backfill = %+v", engine.closedTurnCheckpointMessages())
	}
}

func TestTurnHistoryFirstPageIncludesSealedFindings(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Workspace = t.TempDir()
	engine.turn = 7
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, "how should we fix the lock", 7),
		messageWithText(provider.RoleAssistant, "hasGlobalLock is the root cause", 7),
	}
	engine.context.Evidence().Observe(agentcontext.EvidenceFact{
		Kind:   agentcontext.KindDefinition,
		Path:   "eds_metaserver.cc",
		Line:   88,
		Symbol: "hasGlobalLock",
		Tool:   "search_definition",
		Turn:   7,
	})
	engine.sealClosedTurnMemory(agentcontext.CheckpointCompleted, nil, "")
	_, _, executor, err := engine.options.Tools.Resolve(turnhistory.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(t.Context(), []byte(`{"turn":7}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "[turn 7 findings]") ||
		!strings.Contains(result.Content, "hasGlobalLock is the root cause") ||
		!strings.Contains(result.Content, "eds_metaserver.cc:88 hasGlobalLock") {
		t.Fatalf("turn_history = %q", result.Content)
	}
	visible := engine.closedTurnCheckpointMessages()
	if len(visible) != 1 || strings.Contains(visible[0].Text(), "hasGlobalLock is the root cause") {
		t.Fatalf("visible checkpoint leaked findings: %+v", visible)
	}
}

func TestTurnHistoryReadsDurableTurnAfterViewClip(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{textStream("ok")}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, "P2: missing overflow test", 1),
		messageWithText(provider.RoleAssistant, "noted the five P2s", 1),
		messageWithText(provider.RoleUser, "continue", 2),
		messageWithText(provider.RoleAssistant, "working", 2),
		messageWithText(provider.RoleUser, "next", 3),
	}
	engine.turn = 3
	_, _, executor, err := engine.options.Tools.Resolve(turnhistory.Name)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(t.Context(), []byte(`{"turn":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "P2: missing overflow test") {
		t.Fatalf("turn_history = %q", result.Content)
	}
}

func TestContextSnapshotRestoresWriteOnceCheckpoints(t *testing.T) {
	workspace := t.TempDir()
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Workspace = workspace
	engine.options.WorkspaceIdentity = "workspace:test"
	engine.turn = 2
	engine.sealClosedTurnMemory(agentcontext.CheckpointCompleted, nil, "")
	snapshot, err := engine.ExportContextSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.TurnCheckpoints) != 1 {
		t.Fatalf("snapshot checkpoints = %+v", snapshot.TurnCheckpoints)
	}
	engine.checkpointMu.Lock()
	engine.turnCheckpoints = nil
	engine.checkpointMu.Unlock()
	if _, err := engine.RestoreContextSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	got := engine.closedTurnCheckpointMessages()
	if len(got) != 1 || got[0].Text() != snapshot.TurnCheckpoints[0].Text {
		t.Fatalf("restored = %+v want %q", got, snapshot.TurnCheckpoints[0].Text)
	}
}
