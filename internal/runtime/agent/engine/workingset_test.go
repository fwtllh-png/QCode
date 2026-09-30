package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
)

func planFixture() interact.Plan {
	return interact.Plan{
		Steps:         []interact.PlanStep{{Title: "step one", Status: interact.StepPending}},
		CriticalFiles: []string{"design.md"},
	}
}

// stubRepoContext renders one line per working-set entry, which is enough to see
// what the engine handed it and when.
type stubRepoContext struct {
	turns    []uint64
	entries  [][]agentcontext.WorkingSetEntry
	evidence []agentcontext.EvidenceSnapshot
	receipts []promptcontext.Receipt
}

func (e *Engine) turnContextMessages(
	ctx context.Context,
) ([]provider.Message, []promptcontext.Receipt) {
	scope := e.executionScope()
	scope.spec.Policy = e.options.Security.CloneSampling()
	catalog, err := e.options.Tools.Snapshot()
	if err != nil {
		return nil, nil
	}
	_, advertised, err := e.toolDefinitionsFromSnapshot(catalog, TurnRequest{})
	if err != nil {
		return nil, nil
	}
	history := cloneMessages(e.history)
	stable, delta, receipts, _, err := e.projectWorldState(
		ctx, history, catalog, advertised,
	)
	if err != nil {
		return nil, nil
	}
	e.history = append(history, cloneMessages(delta)...)
	messages := append(cloneMessages(stable), cloneMessages(e.history)...)
	return messages, receipts
}

func (s *stubRepoContext) Build(
	_ context.Context, state promptcontext.TurnState,
) promptcontext.TurnContext {
	s.turns = append(s.turns, state.Turn)
	s.entries = append(s.entries, state.WorkingSet)
	s.evidence = append(s.evidence, state.Evidence)
	paths := make([]string, 0, len(state.WorkingSet))
	selections := make([]promptcontext.Selection, 0, len(state.WorkingSet))
	for _, entry := range state.WorkingSet {
		paths = append(paths, entry.Path)
		selections = append(selections, promptcontext.Selection{
			Path: entry.Path, Kind: "file", Reasons: []string{"read"}, Included: true,
		})
	}
	message := provider.TextMessage(
		provider.RoleSystem,
		"[working_set] "+strings.Join(paths, " "),
	)
	receipts := append([]promptcontext.Receipt(nil), s.receipts...)
	if len(receipts) == 0 {
		size := len(message.Text())
		receipt := promptcontext.Receipt{
			Kind:          promptcontext.PartitionWorkingSetLedger,
			Digest:        "set:" + strings.Join(paths, ","),
			OriginalBytes: size, RetainedBytes: size,
		}
		receipts = append(receipts, receipt)
	}
	return promptcontext.TurnContext{
		Messages: []provider.Message{message},
		Receipts: receipts, Selections: selections,
	}
}

func TestObservedPathsAreStoredWorkspaceRelative(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, nil)
	engine.options.Workspace = t.TempDir()
	engine.turn = 3

	engine.observePath(agentcontext.SourceEdited, filepath.Join(engine.options.Workspace, "internal", "a.go"))
	engine.observePath(agentcontext.SourceRead, filepath.Join("internal", "b.go"))
	engine.observePath(agentcontext.SourceRead, filepath.Join(engine.options.Workspace, "..", "outside.go"))
	engine.observePath(agentcontext.SourceRead, engine.options.Workspace)
	engine.observePath(agentcontext.SourceRead, "")

	entries := engine.WorkingSetEntries(3, 10)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want only the two paths inside the workspace", entries)
	}
	if entries[0].Path != "internal/a.go" || entries[1].Path != "internal/b.go" {
		t.Fatalf("paths = %+v, want workspace-relative slash paths", entries)
	}
}

func TestReadObservationsFollowTheGuardMetadataKey(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, nil)
	engine.options.Workspace = t.TempDir()
	engine.turn = 1

	// The engine learns about reads from typed outcome facts.
	path := filepath.Join(engine.options.Workspace, "read.go")
	engine.observePath(agentcontext.SourceRead, turnkernel.ObservedFileRead(tool.Result{
		Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{
			WorkspaceRead: &tool.WorkspaceReadFact{Path: path},
		}},
	}))
	if paths := engine.ReadPaths(1); len(paths) != 1 || paths[0] != "read.go" {
		t.Fatalf("read paths = %v", paths)
	}
	if turnkernel.ObservedFileRead(tool.Result{}) != "" {
		t.Fatal("a result without typed read facts must report no read")
	}
}

func TestDiagnosticsAndPlanFeedTheWorkingSet(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, nil)
	engine.options.Workspace = t.TempDir()
	engine.turn = 2

	engine.recordTurnDiagnostics([]verify.DiagnosticReceipt{{Path: "broken.go", Status: "failed"}})
	engine.observePaths(agentcontext.SourcePlan, []string{"design.md"})

	entries := engine.WorkingSetEntries(2, 10)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Path != "design.md" || !entries[0].Critical {
		t.Fatalf("entries = %+v, want the plan's critical file first", entries)
	}
	if entries[1].Path != "broken.go" || entries[1].Sources[0] != agentcontext.SourceDiagnostic {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestTheWorkingSetOutlivesTheTurnAndTheTurnDiffDoesNot(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, nil)
	scope := attachTestScope(t, engine)
	engine.options.Workspace = t.TempDir()

	engine.turn = 1
	scope.state.diff.Record(turnkernel.TurnDiffEntry{Path: "a.go", Kind: "modified"})
	engine.observePath(agentcontext.SourceEdited, "a.go")

	engine.turn = 2
	scope.state.diff.Reset()
	if len(engine.TurnDiff()) != 0 {
		t.Fatal("the turn diff must not survive its turn")
	}
	entries := engine.WorkingSetEntries(2, 10)
	if len(entries) != 1 || entries[0].Path != "a.go" || entries[0].LastTurn != 1 {
		t.Fatalf("entries = %+v, want the previous turn's edit still known", entries)
	}
}

func TestForkInheritsTheWorkingSetWithoutSharingIt(t *testing.T) {
	parent := newEngine(t, &scriptedProvider{}, nil)
	parent.options.Workspace = t.TempDir()
	parent.turn = 1
	parent.observePath(agentcontext.SourceEdited, "shared.go")

	child, err := parent.Fork()
	if err != nil {
		t.Fatal(err)
	}
	child.turn = 2
	child.observePath(agentcontext.SourceRead, "child.go")
	parent.observePath(agentcontext.SourceRead, "parent.go")

	if got := len(child.WorkingSetEntries(2, 10)); got != 2 {
		t.Fatalf("child entries = %d, want the inherited edit plus its own read", got)
	}
	for _, entry := range child.WorkingSetEntries(2, 10) {
		if entry.Path == "parent.go" {
			t.Fatal("the fork must not see what the parent did afterwards")
		}
	}
}

func TestWorldStateFullIsRetainedInDurableHistory(t *testing.T) {
	stub := &stubRepoContext{}
	engine := newEngine(t, &scriptedProvider{}, nil)
	engine.options.Workspace = t.TempDir()
	engine.options.RepoContext = stub
	engine.options.StaticContext = []provider.Message{
		provider.TextMessage(provider.RoleSystem, "stable prefix"),
	}
	engine.turn = 4
	engine.observePath(agentcontext.SourceEdited, "a.go")
	engine.ApplyPlan(planFixture())

	engine.history = []provider.Message{messageWithText(provider.RoleUser, "do it", 4)}
	messages, _ := engine.turnContextMessages(t.Context())
	if messages[0].Text() != "stable prefix" || messages[1].Role != provider.RoleUser {
		t.Fatalf("messages = %+v, want stable prefix before durable history", messages)
	}
	var working, plan bool
	for _, message := range engine.history {
		working = working || strings.Contains(message.Text(), "a.go")
		plan = plan || strings.Contains(message.Text(), "step one")
	}
	if !working || !plan {
		t.Fatalf("durable world history = %+v", engine.history)
	}
}

func TestFrozenToolCatalogPrecedesChangingHistory(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("echo-1", "echo", `{"text":"one"}`),
		textStream("done"),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	engine.options.RepoContext = &stubRepoContext{}
	if _, err := engine.Run(t.Context(), "inspect", nil); err != nil {
		t.Fatal(err)
	}
	if len(runtime.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(runtime.requests))
	}
	for _, request := range runtime.requests {
		catalog, world, user := -1, -1, -1
		worldCount := 0
		for index, message := range request.Messages {
			if strings.Contains(message.Text(), "[tool_catalog]") {
				catalog = index
			}
			if strings.Contains(message.Text(), "[working_set]") {
				world, worldCount = index, worldCount+1
			}
			if message.Role == provider.RoleUser && message.Text() == "inspect" {
				user = index
			}
		}
		if catalog < 0 || world < 0 || user < 0 ||
			user >= catalog || catalog >= world || worldCount != 1 {
			t.Fatalf(
				"catalog/world/user = %d/%d/%d count=%d messages=%+v",
				catalog, world, user, worldCount, request.Messages,
			)
		}
	}
}

func TestUnchangedToolCatalogEmitsNoPatch(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{}, registry)
	attachTestScope(t, engine)
	first, _ := engine.turnContextMessages(t.Context())
	before := len(engine.history)
	first[0].Blocks[0].Text = "mutated caller copy"
	second, _ := engine.turnContextMessages(t.Context())
	if len(engine.history) != before || second[0].Text() == "mutated caller copy" {
		t.Fatalf("history=%+v second=%+v", engine.history, second)
	}
}

func TestTurnContextRebuildsWithinTheSameTurn(t *testing.T) {
	stub := &stubRepoContext{}
	engine := newEngine(t, &scriptedProvider{}, nil)
	engine.options.Workspace = t.TempDir()
	engine.options.RepoContext = stub
	engine.turn = 1

	if _, _ = engine.turnContextMessages(t.Context()); len(stub.entries[0]) != 0 {
		t.Fatalf("first sample entries = %+v, want none yet", stub.entries[0])
	}
	// A file read during the turn is in the working set for the very next sample,
	// which is what makes the tail worth rebuilding instead of caching per turn.
	engine.observePath(agentcontext.SourceRead, "found.go")
	messages, _ := engine.turnContextMessages(t.Context())
	if len(stub.entries) != 2 || len(stub.entries[1]) != 1 {
		t.Fatalf("second sample entries = %+v", stub.entries)
	}
	found := false
	for _, message := range messages {
		found = found || strings.Contains(message.Text(), "found.go")
	}
	if !found {
		t.Fatalf("second sample messages = %+v", messages)
	}
	if stub.turns[0] != 1 || stub.turns[1] != 1 {
		t.Fatalf("turns = %v, want both samples in turn 1", stub.turns)
	}
	before := len(engine.history)
	_, _ = engine.turnContextMessages(t.Context())
	if len(engine.history) != before {
		t.Fatalf("unchanged world state appended history: %+v", engine.history[before:])
	}
}

func TestTurnContextReceiptsJoinTheContextReceipts(t *testing.T) {
	stub := &stubRepoContext{receipts: []promptcontext.Receipt{{
		Kind: promptcontext.PartitionRepoMap, OriginalBytes: 12, RetainedBytes: 12,
		Digest:    "repo-map",
		Truncated: true, TruncationReason: "byte_budget",
	}}}
	engine := newEngine(t, &scriptedProvider{}, nil)
	engine.options.RepoContext = stub
	engine.options.StaticContextReceipts = []promptcontext.Receipt{
		{Kind: promptcontext.PartitionBase, RetainedBytes: 3},
	}

	_, receipts := engine.turnContextMessages(t.Context())
	engine.recordTurnContextReceipts(receipts)

	kinds := map[string]bool{}
	for _, receipt := range engine.ContextReceipts() {
		kinds[receipt.Kind] = true
	}
	if !kinds[promptcontext.PartitionBase] || !kinds[promptcontext.PartitionRepoMap] {
		t.Fatalf("receipt kinds = %v, want both the frozen and the volatile ones", kinds)
	}
	// Re-rendering replaces rather than appends, or a long turn would grow a
	// receipt list one entry per sample.
	engine.recordTurnContextReceipts(receipts)
	count := 0
	for _, receipt := range engine.ContextReceipts() {
		if receipt.Kind == promptcontext.PartitionRepoMap {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("repo map receipts = %d, want one", count)
	}
	engine.observePath(agentcontext.SourceRead, "internal/value.go")
	_, _ = engine.turnContextMessages(t.Context())
	selections := engine.ContextSelections()
	if len(selections) != 1 || selections[0].Path != "internal/value.go" ||
		selections[0].Reasons[0] != "read" {
		t.Fatalf("context selections = %+v", selections)
	}
}

func TestTurnContextIsOptional(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, nil)
	messages, receipts := engine.turnContextMessages(t.Context())
	if len(messages) == 0 || len(receipts) == 0 {
		t.Fatalf("messages = %+v, receipts = %+v, want the policy world baseline", messages, receipts)
	}
}
