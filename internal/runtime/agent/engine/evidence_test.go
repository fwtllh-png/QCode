package engine

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func evidenceEngine(t *testing.T) *Engine {
	t.Helper()
	engine := newEngine(t, &scriptedProvider{}, nil)
	engine.options.Workspace = t.TempDir()
	engine.turn = 1
	engine.context.Evidence().BeginTurn(1)
	return engine
}

func TestSearchHitsBecomeFactsAndTheWeakestWorkingSetSource(t *testing.T) {
	engine := evidenceEngine(t)
	call := provider.ToolCall{Name: "search_definition", Arguments: `{"name":"Verify"}`}
	engine.observeEvidence(call, tool.Result{Outcome: &tool.Outcome{
		Facts: &tool.OutcomeFacts{Evidence: []tool.EvidenceHit{
			{Kind: tool.EvidenceDefinition, Path: "auth/token.go", Line: 12, Symbol: "Verify"},
			// A path outside the workspace is dropped: the ledger points at code the
			// agent can act on.
			{Kind: tool.EvidenceReference, Path: filepath.Join(engine.options.Workspace, "..", "x.go")},
		},
		}}})

	facts := engine.EvidenceSnapshot().Facts
	if len(facts) != 1 {
		t.Fatalf("facts = %+v", facts)
	}
	if facts[0].Kind != agentcontext.KindDefinition || facts[0].Path != "auth/token.go" ||
		facts[0].Line != 12 || facts[0].Symbol != "Verify" ||
		facts[0].Tool != "search_definition" || facts[0].Turn != 1 {
		t.Fatalf("fact = %+v", facts[0])
	}
	entries := engine.WorkingSetEntries(1, 10)
	if len(entries) != 1 || entries[0].Path != "auth/token.go" ||
		entries[0].Sources[0] != agentcontext.SourceSearch {
		t.Fatalf("entries = %+v, want the hit recorded as a search", entries)
	}
}

func TestUnknownEvidenceKindIsIgnored(t *testing.T) {
	engine := evidenceEngine(t)
	engine.observeEvidence(
		provider.ToolCall{Name: "search_text"},
		tool.Result{Metadata: map[string]any{
			tool.MetadataEvidence: []tool.EvidenceHit{{Kind: "guess", Path: "a.go"}},
		}},
	)
	if facts := engine.EvidenceSnapshot().Facts; len(facts) != 0 {
		t.Fatalf("facts = %+v", facts)
	}
}

func TestAnEditAfterAReadIsNotBlind(t *testing.T) {
	engine := evidenceEngine(t)
	read := filepath.Join(engine.options.Workspace, "a.go")
	engine.observePath(agentcontext.SourceRead, read)
	engine.observeChangeEvidence(tool.WorkspaceChange{Path: read, Kind: tool.WorkspaceModified})
	engine.observeChangeEvidence(tool.WorkspaceChange{Path: "b.go", Kind: tool.WorkspaceModified})
	engine.observeChangeEvidence(tool.WorkspaceChange{Path: "new.go", Kind: tool.WorkspaceCreated})

	blind := map[string]bool{}
	for _, risk := range engine.EvidenceSnapshot().Risks {
		if risk.Kind == agentcontext.RiskBlindChange {
			blind[risk.Path] = true
		}
	}
	if len(blind) != 1 || !blind["b.go"] {
		t.Fatalf("blind changes = %v, want only the file nobody read", blind)
	}
}

func TestDiagnosticsCloseAndOpenTheEvidenceGap(t *testing.T) {
	engine := evidenceEngine(t)
	engine.observeChangeEvidence(tool.WorkspaceChange{Path: "a.go", Kind: tool.WorkspaceModified})
	engine.observeDiagnosticsEvidence([]verify.DiagnosticReceipt{{
		Path: "a.go", Status: "failed",
		Diagnostics: []verify.Diagnostic{{Path: "a.go", Message: "broken"}},
	}})
	if !hasRisk(engine, agentcontext.RiskOpenDiagnostics) {
		t.Fatal("a failing check left no risk")
	}
	// An unavailable runner checked nothing, so it must not read as clean.
	engine.observeDiagnosticsEvidence([]verify.DiagnosticReceipt{{Path: "a.go", Status: "unavailable"}})
	if !hasRisk(engine, agentcontext.RiskOpenDiagnostics) {
		t.Fatal("an unavailable runner cleared the risk")
	}
	engine.observeDiagnosticsEvidence([]verify.DiagnosticReceipt{{Path: "a.go", Status: "passed"}})
	if hasRisk(engine, agentcontext.RiskOpenDiagnostics) {
		t.Fatal("a clean check left the risk standing")
	}
}

func TestVerifiedPathsClearTheRiskWorkspaceRelative(t *testing.T) {
	engine := evidenceEngine(t)
	absolute := filepath.Join(engine.options.Workspace, "a.go")
	engine.observeChangeEvidence(tool.WorkspaceChange{Path: absolute, Kind: tool.WorkspaceModified})
	if !hasRisk(engine, agentcontext.RiskUnverifiedChange) {
		t.Fatal("a fresh change is not unverified")
	}
	// The gate reports the paths the way the guard spelled them, absolute; the
	// evidence set keys on workspace-relative paths, so the two must be lined up.
	engine.observeVerifiedEvidence([]string{absolute})
	if hasRisk(engine, agentcontext.RiskUnverifiedChange) {
		t.Fatal("verification did not clear the risk")
	}
}

func TestRepeatedCallAndConsumedHandleAreObservedFromCalls(t *testing.T) {
	engine := evidenceEngine(t)
	engine.noteToolCall(provider.ToolCall{Name: "search_text", Arguments: `{"query":"a"}`})
	engine.noteToolCall(provider.ToolCall{Name: "search_text", Arguments: `{"query":"a"}`})
	reminders := engine.EvidenceSnapshot().Reminders
	if len(reminders) != 1 || reminders[0].Kind != agentcontext.ReminderRepeatedCall {
		t.Fatalf("reminders = %+v", reminders)
	}

	engine.observeEvidence(
		provider.ToolCall{Name: "search_project"},
		tool.Result{Outcome: &tool.Outcome{
			Facts: &tool.OutcomeFacts{ResultHandle: "h1"},
		}},
	)
	engine.turn = 2
	engine.context.Evidence().BeginTurn(2)
	if !hasReminder(engine, agentcontext.ReminderUnconsumedResult) {
		t.Fatal("an unread handle from the previous turn is not reported")
	}
	engine.noteToolCall(provider.ToolCall{Name: "result_get", Arguments: `{"handle":"h1"}`})
	if hasReminder(engine, agentcontext.ReminderUnconsumedResult) {
		t.Fatal("reading the handle did not clear the reminder")
	}
}

func TestRereadingAnUnchangedFileReminds(t *testing.T) {
	engine := evidenceEngine(t)
	path := filepath.Join(engine.options.Workspace, "a.go")
	result := tool.Result{Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{
		WorkspaceRead: &tool.WorkspaceReadFact{Path: path, Digest: "sha-1"},
	}}}
	call := provider.ToolCall{Name: "file_read"}
	engine.observeEvidence(call, result)
	if hasReminder(engine, agentcontext.ReminderRepeatedRead) {
		t.Fatal("a first read reminded")
	}
	engine.observeEvidence(call, result)
	if !hasReminder(engine, agentcontext.ReminderRepeatedRead) {
		t.Fatal("re-reading unchanged content did not remind")
	}
}

func TestForkInheritsTheEvidenceWithoutSharingIt(t *testing.T) {
	parent := evidenceEngine(t)
	parent.observeChangeEvidence(tool.WorkspaceChange{Path: "a.go", Kind: tool.WorkspaceModified})
	child, err := parent.Fork()
	if err != nil {
		t.Fatal(err)
	}
	parent.observeVerifiedEvidence([]string{"a.go"})

	if !hasRisk(child, agentcontext.RiskUnverifiedChange) {
		t.Fatal("the fork lost the inherited risk or shares the parent's verification")
	}
}

// Compaction is exactly when the model loses the history that said an edit
// happened, so the summary has to say which edits are still unproved.
func TestCompactionSummaryCarriesUnverifiedChanges(t *testing.T) {
	engine := evidenceEngine(t)
	engine.observeChangeEvidence(tool.WorkspaceChange{Path: "a.go", Kind: tool.WorkspaceModified})
	engine.observeChangeEvidence(tool.WorkspaceChange{Path: "b.go", Kind: tool.WorkspaceModified})
	engine.observeVerifiedEvidence([]string{"b.go"})

	rendered, _, sections := engine.buildCompactSummary(nil).Render(0)
	if !strings.Contains(rendered, "a.go (turn 1) — nothing verified it") {
		t.Fatalf("summary = %q, want the unproved change to survive compaction", rendered)
	}
	if !strings.Contains(rendered, "b.go (turn 1) — verified") {
		t.Fatalf("summary = %q, want the verified change reported as verified", rendered)
	}
	if !slices.Contains(sections, agentcontext.SectionChanges) {
		t.Fatalf("sections = %v", sections)
	}
}

func TestTailRenderCountsRisksAndReminders(t *testing.T) {
	engine := evidenceEngine(t)
	metrics := telemetry.NewMetrics()
	engine.options.Metrics = metrics
	engine.options.RepoContext = &stubRepoContext{}
	engine.observeChangeEvidence(tool.WorkspaceChange{Path: "a.go", Kind: tool.WorkspaceModified})
	engine.noteToolCall(provider.ToolCall{Name: "search_text", Arguments: `{"query":"a"}`})
	engine.noteToolCall(provider.ToolCall{Name: "search_text", Arguments: `{"query":"a"}`})

	engine.turnContextMessages(t.Context())
	snapshot := metrics.Snapshot()
	// Two risks: the change is both unverified and unread.
	if snapshot.EvidenceRisks != 2 || snapshot.PolicyReminders != 1 {
		t.Fatalf("metrics = %+v", snapshot)
	}
}

func hasRisk(engine *Engine, kind string) bool {
	for _, risk := range engine.EvidenceSnapshot().Risks {
		if risk.Kind == kind {
			return true
		}
	}
	return false
}

func hasReminder(engine *Engine, kind string) bool {
	for _, reminder := range engine.EvidenceSnapshot().Reminders {
		if reminder.Kind == kind {
			return true
		}
	}
	return false
}
