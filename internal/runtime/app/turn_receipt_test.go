package app

import (
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	skillruntime "github.com/fwtllh-png/QCode/internal/adapter/skill"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/trace"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// The receipt now carries real line statistics, so diff_line_stats must be gone
// from the not-collected list, and a rollback that could not restore a path has
// to surface as an unresolved issue rather than a count inside an error string.
func TestReceiptReportsLineStatsAndRollbackConflicts(t *testing.T) {
	recorder := newTurnReceiptRecorder("fix add")
	receipt := recorder.build(turnReceiptObservations{
		changes: []agentengine.TurnDiffEntry{
			{Path: "calc.py", Tool: "file_apply", Kind: "modified", Added: 3, Removed: 1},
			{Path: "gone.py", Tool: "file_apply", Kind: "deleted", Removed: 4},
		},
		conflicts: []string{"workspace rollback could not restore calc.py: content changed"},
	})

	if len(receipt.Changes) != 2 {
		t.Fatalf("changes = %+v", receipt.Changes)
	}
	if receipt.Changes[0].Added != 3 || receipt.Changes[0].Removed != 1 {
		t.Fatalf("line stats = %+v", receipt.Changes[0])
	}
	if receipt.Changes[1].Kind != "deleted" || receipt.Changes[1].Removed != 4 {
		t.Fatalf("deletion = %+v", receipt.Changes[1])
	}
	found := false
	for _, issue := range receipt.UnresolvedIssues {
		if strings.Contains(issue, "could not restore calc.py") {
			found = true
		}
	}
	if !found {
		t.Fatalf("unresolved issues = %v, want the rollback conflict", receipt.UnresolvedIssues)
	}
	for _, section := range receipt.NotCollected {
		if section == "diff_line_stats" {
			t.Fatal("receipt still claims diff line stats are not collected")
		}
	}
}

func TestReceiptReportsProviderRetrySummary(t *testing.T) {
	recorder := newTurnReceiptRecorder("retry provider")
	recorder.observe(agentengine.Event{ProviderRetry: &agentengine.ProviderRetry{
		Attempt: 1, Code: protocol.CodeUnavailable, Category: "connection_reset",
	}})
	recorder.observe(agentengine.Event{ProviderRetry: &agentengine.ProviderRetry{
		Attempt: 2, Code: protocol.CodeUnavailable, Category: "unexpected_eof",
	}})

	receipt := recorder.build(turnReceiptObservations{})
	if receipt.ProviderRetry == nil ||
		receipt.ProviderRetry.Count != 2 ||
		receipt.ProviderRetry.LastCode != protocol.CodeUnavailable ||
		receipt.ProviderRetry.LastCategory != "unexpected_eof" {
		t.Fatalf("provider retry = %#v", receipt.ProviderRetry)
	}
}

func TestReceiptRetainsModelMetadataProvenance(t *testing.T) {
	recorder := newTurnReceiptRecorder("inspect model")
	metadata := &protocol.ModelMetadataProvenance{
		CanonicalID:  "operator_config",
		WireID:       "operator_config",
		Limits:       "operator_config",
		Capabilities: "operator_config",
	}
	recorder.observe(agentengine.Event{
		Provider:      "openai-compatible",
		Model:         "custom-model",
		Purpose:       "act",
		ModelMetadata: metadata,
	})
	receipt := recorder.build(turnReceiptObservations{})
	if len(receipt.Routes) != 1 ||
		receipt.Routes[0].ModelMetadata == nil ||
		receipt.Routes[0].ModelMetadata.Limits != "operator_config" {
		t.Fatalf("receipt routes = %+v", receipt.Routes)
	}
}

func TestReceiptProjectsSkillSelectionDiagnostics(t *testing.T) {
	selection := receiptSkillSelection(agentengine.SkillSelectionMetrics{
		Method: "weighted_lexical_v1", CatalogSize: 1024,
		CandidateSize: 20, VisibleSize: 20, ExplicitMatches: 1,
		QueryTerms: 64, QueryTruncated: true, CandidateSetTruncated: true,
		OriginalTokens: 4096, ProjectedTokens: 512, TokenSavings: 0.875,
		Recall: 1, Precision: 0.05, CacheHit: true,
	})
	receipt := newTurnReceiptRecorder("load target").build(turnReceiptObservations{
		skillSelection: selection,
	})
	if receipt.SkillSelection == nil ||
		receipt.SkillSelection.CatalogSize != 1024 ||
		!receipt.SkillSelection.QueryTruncated ||
		!receipt.SkillSelection.CandidateSetTruncated ||
		receipt.SkillSelection.ExplicitMatches != 1 {
		t.Fatalf("skill selection = %+v", receipt.SkillSelection)
	}
}

func TestReceiptSeparatesProviderAttemptsSamplesAndCompletionRepairs(t *testing.T) {
	recorder := newTurnReceiptRecorder("repair model output")
	for _, event := range []agentengine.ModelExecution{
		{Kind: "model_sample", SampleID: "sample-1", Reason: promptcontext.SampleNormal},
		{Kind: "provider_attempt", SampleID: "sample-1", Attempt: 1, Status: protocol.ProviderAttemptStarted},
		{Kind: "provider_attempt", SampleID: "sample-1", Attempt: 1, Status: protocol.ProviderAttemptFailed},
		{Kind: "provider_attempt", SampleID: "sample-1", Attempt: 1, Status: protocol.ProviderAttemptRetryWait},
		{Kind: "provider_attempt", SampleID: "sample-1", Attempt: 2, Status: protocol.ProviderAttemptStarted},
		{Kind: "provider_attempt", SampleID: "sample-1", Attempt: 2, Status: protocol.ProviderAttemptCompleted},
		{Kind: "model_sample", SampleID: "sample-2", Reason: promptcontext.SampleCompletionRepair},
		{Kind: "model_sample", SampleID: "sample-3", Reason: promptcontext.SampleVerificationRepair},
		{Kind: "provider_attempt", SampleID: "sample-2", Attempt: 1, Status: protocol.ProviderAttemptStarted},
	} {
		value := event
		recorder.observe(agentengine.Event{ModelExecution: &value})
	}
	receipt := recorder.build(turnReceiptObservations{})
	if receipt.ModelExecution.ProviderAttempts != 3 ||
		receipt.ModelExecution.ModelSamples != 3 ||
		receipt.ModelExecution.CompletionRepairs != 1 ||
		receipt.ModelExecution.SampleReasons[promptcontext.SampleVerificationRepair] != 1 {
		t.Fatalf("model execution = %+v", receipt.ModelExecution)
	}
}

func TestReceiptClassifiesToolExecutions(t *testing.T) {
	recorder := newTurnReceiptRecorder("run and verify")
	for _, event := range []struct {
		name         string
		failed       bool
		verification bool
	}{
		{name: "exec_command"},
		{name: "exec_command", verification: true},
		{name: "turn_complete"},
		{name: "search_files", failed: true},
	} {
		call := provider.ToolCall{Name: event.name}
		result := tool.Result{IsError: event.failed}
		if event.verification {
			tool.EnsureOutcomeFacts(&result).Verification = &verify.Evidence{Status: verify.StatusPassed}
		}
		recorder.observe(agentengine.Event{
			State: agentengine.RunningTools, ToolCall: &call, Result: &result,
		})
	}
	receipt := recorder.build(turnReceiptObservations{})
	if receipt.ToolExecution["business"] != 2 ||
		receipt.ToolExecution["control"] != 1 ||
		receipt.ToolExecution["verification"] != 1 ||
		receipt.ToolExecution["failed"] != 1 {
		t.Fatalf("tool execution = %+v", receipt.ToolExecution)
	}
}

func TestReceiptReportsReadPathsAndContextSections(t *testing.T) {
	recorder := newTurnReceiptRecorder("fix add")
	recorder.editorContext = []protocol.EditorContextReceipt{{
		Kind:   protocol.EditorContextSymbol,
		Source: protocol.EditorContextSourceSelectionCommand,
		Path:   "calc.py", Digest: strings.Repeat("a", 64),
		Range: &protocol.EditorRange{
			Start: protocol.EditorPosition{}, End: protocol.EditorPosition{Character: 12},
		},
		Symbol:        &protocol.EditorSymbol{Name: "calculate", Kind: "function"},
		OriginalBytes: 12, RetainedBytes: 12,
	}}
	recorder.observe(agentengine.Event{State: agentengine.Preparing, Turn: 3})
	receipt := recorder.build(turnReceiptObservations{
		readPaths: []string{"calc.py"},
		context: []promptcontext.Receipt{
			{Kind: promptcontext.PartitionBase, OriginalBytes: 10, RetainedBytes: 10, Digest: "sha256:a"},
			{
				Kind: promptcontext.PartitionRepoMap, OriginalBytes: 400, RetainedBytes: 64,
				Truncated: true, TruncationReason: "byte_budget", Digest: "sha256:b",
			},
		},
		selections: []promptcontext.Selection{{
			Path: "calc_test.py", Kind: "test", Reasons: []string{"search"},
			Evidence: []promptcontext.SelectionEvidence{{
				Kind: "test", Tool: "search_related_tests", Turn: 3,
			}},
			Score: 5, FirstTurn: 3, LastTurn: 3,
			Included: false, Truncated: true, TruncationReason: "byte_budget",
		}},
	})

	if recorder.turn != 3 {
		t.Fatalf("turn = %d, want the turn the events carried", recorder.turn)
	}
	if len(receipt.ReadPaths) != 1 || receipt.ReadPaths[0] != "calc.py" {
		t.Fatalf("read paths = %v", receipt.ReadPaths)
	}
	// read_paths is collected now, so claiming otherwise would be a lie.
	for _, section := range receipt.NotCollected {
		if section == "read_paths" {
			t.Fatal("receipt still claims read paths are not collected")
		}
	}
	if len(receipt.ContextSections) != 2 {
		t.Fatalf("context sections = %+v", receipt.ContextSections)
	}
	if len(receipt.ContextSelections) != 1 ||
		receipt.ContextSelections[0].Kind != "test" ||
		receipt.ContextSelections[0].Reasons[0] != "search" ||
		receipt.ContextSelections[0].Evidence[0].Tool != "search_related_tests" ||
		!receipt.ContextSelections[0].Truncated {
		t.Fatalf("context selections = %+v", receipt.ContextSelections)
	}
	if len(receipt.EditorContext) != 1 ||
		receipt.EditorContext[0].Kind != protocol.EditorContextSymbol ||
		receipt.EditorContext[0].Path != "calc.py" {
		t.Fatalf("editor context = %+v", receipt.EditorContext)
	}
	truncated := receipt.ContextSections[1]
	if truncated.Kind != promptcontext.PartitionRepoMap || !truncated.Truncated ||
		truncated.TruncationReason != "byte_budget" || truncated.RetainedBytes != 64 ||
		truncated.OriginalBytes != 400 || truncated.Digest != "sha256:b" {
		t.Fatalf("truncated section = %+v", truncated)
	}
}

// TestReceiptPrefersTerminalUsage pins that the turn-cumulative usage on the
// terminal event replaces the streaming sum. Adding them together inflated
// every receipt by the last call's tokens.
func TestReceiptUsesFrozenKernelUsage(t *testing.T) {
	recorder := newTurnReceiptRecorder("fix add")
	recorder.observe(agentengine.Event{
		State: agentengine.Streaming, Sample: 1,
		Usage: &provider.Usage{InputTokens: 999, OutputTokens: 999},
	})
	recorder.observe(agentengine.Event{
		State:   agentengine.Completed,
		Usage:   &provider.Usage{InputTokens: 888, OutputTokens: 888},
		CostUSD: 99, CostKnown: true,
	})
	receipt := recorder.build(turnReceiptObservations{
		measurement: receiptMeasurement(t, turnkernel.UsageState{
			InputTokens: 48, OutputTokens: 6, CachedTokens: 16,
			CostMicrounits: 500, CostKnown: true, Frozen: true,
		}, &trace.Latency{}),
	})
	if receipt.InputTokens != 48 || receipt.OutputTokens != 6 || receipt.CachedTokens != 16 {
		t.Fatalf("receipt usage = %+v", receipt)
	}
	if receipt.CostMicrounits != 500 || !receipt.CostKnown {
		t.Fatalf("receipt cost = %d known=%v", receipt.CostMicrounits, receipt.CostKnown)
	}
	if !receipt.MeasurementRecorded ||
		receipt.MeasurementDigest == "" ||
		receipt.UsageDigest == "" {
		t.Fatalf("measurement binding = %+v", receipt)
	}
}

// TestReceiptCostKnownComesFromPricingNotAmount pins the distinction that makes
// the flag worth having. A model with published pricing that happens to cost
// nothing reports a known cost of zero; a model with no pricing metadata reports
// unknown. Deriving the flag from the amount collapsed the two, so every free
// call looked unpriced.
func TestReceiptCostKnownComesFromPricingNotAmount(t *testing.T) {
	free := newTurnReceiptRecorder("ask something cheap")
	free.observe(agentengine.Event{
		State: agentengine.Completed,
		Usage: &provider.Usage{InputTokens: 12, OutputTokens: 3},
		// A priced model whose rates are zero: cost is known to be nothing.
		CostUSD: 0, CostKnown: true,
	})
	receipt := free.build(turnReceiptObservations{
		measurement: receiptMeasurement(t, turnkernel.UsageState{
			InputTokens: 12, OutputTokens: 3,
			CostKnown: true, Frozen: true,
		}, &trace.Latency{}),
	})
	if receipt.CostMicrounits != 0 || !receipt.CostKnown {
		t.Fatalf("free call = %d known=%v, want a known zero", receipt.CostMicrounits, receipt.CostKnown)
	}

	unpriced := newTurnReceiptRecorder("ask something unpriced")
	unpriced.observe(agentengine.Event{
		State: agentengine.Completed,
		Usage: &provider.Usage{InputTokens: 12, OutputTokens: 3},
	})
	receipt = unpriced.build(turnReceiptObservations{
		measurement: receiptMeasurement(t, turnkernel.UsageState{
			InputTokens: 12, OutputTokens: 3, Frozen: true,
		}, &trace.Latency{}),
	})
	if receipt.CostKnown {
		t.Fatal("unpriced call reported a known cost")
	}
}

// TestReceiptFallbackKeepsLastReportPerCall covers the streaming path with a
// provider that reports input and output separately: the two events are
// cumulative snapshots of one call, so the receipt keeps the later one instead of
// adding them and counting the input twice.
func TestReceiptDoesNotReaggregateStreamingUsage(t *testing.T) {
	recorder := newTurnReceiptRecorder("fix add")
	recorder.observe(agentengine.Event{
		State: agentengine.Streaming, Sample: 1, CostKnown: true,
		Usage: &provider.Usage{InputTokens: 100},
	})
	recorder.observe(agentengine.Event{
		State: agentengine.Streaming, Sample: 1, CostKnown: true,
		Usage: &provider.Usage{InputTokens: 100, OutputTokens: 50},
	})
	recorder.observe(agentengine.Event{
		State: agentengine.Streaming, Sample: 2, CostKnown: true,
		Usage: &provider.Usage{InputTokens: 30, OutputTokens: 8},
	})
	receipt := recorder.build(turnReceiptObservations{
		measurement: receiptMeasurement(t, turnkernel.UsageState{
			InputTokens: 130, OutputTokens: 58,
			CostKnown: true, Frozen: true,
		}, &trace.Latency{}),
	})
	if receipt.InputTokens != 130 || receipt.OutputTokens != 58 {
		t.Fatalf("receipt usage = %d in / %d out, want 130 / 58", receipt.InputTokens, receipt.OutputTokens)
	}
}

// TestReceiptFallsBackToStreamingUsage covers failed turns, whose terminal
// event carries no cumulative usage.
func TestFailedReceiptUsesFrozenKernelUsage(t *testing.T) {
	recorder := newTurnReceiptRecorder("fix add")
	recorder.observe(agentengine.Event{
		State: agentengine.Streaming,
		Usage: &provider.Usage{InputTokens: 30, OutputTokens: 4},
	})
	recorder.observe(agentengine.Event{State: agentengine.Failed, Error: "tool file_edit failed"})
	receipt := recorder.build(turnReceiptObservations{
		measurement: receiptMeasurement(t, turnkernel.UsageState{
			InputTokens: 30, OutputTokens: 4, Frozen: true,
		}, &trace.Latency{}),
	})
	if receipt.InputTokens != 30 || receipt.OutputTokens != 4 {
		t.Fatalf("receipt usage = %+v", receipt)
	}
	if receipt.CostKnown {
		t.Fatal("cost reported as known without pricing")
	}
	if len(receipt.UnresolvedIssues) != 1 {
		t.Fatalf("unresolved issues = %v", receipt.UnresolvedIssues)
	}
}

func TestReceiptSeparatesTerminalSecondaryIssues(t *testing.T) {
	recorder := newTurnReceiptRecorder("fix add")
	recorder.observe(agentengine.Event{
		State: agentengine.Failed, Error: "verification conflict",
		SecondaryIssues: []agentengine.TerminalIssue{{
			Phase: "terminal_context", Code: protocol.CodeResourceExhausted,
			Message: "history compaction failed",
		}},
	})
	receipt := recorder.build(turnReceiptObservations{})
	if len(receipt.UnresolvedIssues) != 1 ||
		receipt.UnresolvedIssues[0] != "verification conflict" {
		t.Fatalf("primary issues = %v", receipt.UnresolvedIssues)
	}
	if len(receipt.SecondaryIssues) != 1 ||
		receipt.SecondaryIssues[0].Phase != "terminal_context" ||
		receipt.SecondaryIssues[0].Code != protocol.CodeResourceExhausted {
		t.Fatalf("secondary issues = %+v", receipt.SecondaryIssues)
	}
}

// TestReceiptCarriesTheMeasuredLatencyPartition pins the two rules a reader of
// the partition depends on: a phase that did not happen reports zero, and the
// flat total agrees with the partition instead of measuring its own thing.
func TestReceiptCarriesTheMeasuredLatencyPartition(t *testing.T) {
	recorder := newTurnReceiptRecorder("fix add")
	firstToken := 250 * time.Millisecond
	receipt := recorder.build(turnReceiptObservations{
		measurement: receiptMeasurement(t, turnkernel.UsageState{
			Frozen: true,
		}, &trace.Latency{
			Total: 6 * time.Second, FirstToken: &firstToken,
			Provider: 1200 * time.Millisecond, Tool: 3 * time.Second,
		}),
	})

	if receipt.Latency == nil {
		t.Fatal("a measured turn reported no latency partition")
	}
	if receipt.Latency.FirstTokenMS == nil || *receipt.Latency.FirstTokenMS != 250 {
		t.Fatalf("first token = %v, want 250ms", receipt.Latency.FirstTokenMS)
	}
	if receipt.Latency.ProviderMS != 1200 || receipt.Latency.ToolMS != 3000 {
		t.Fatalf("latency = %+v", receipt.Latency)
	}
	// Measured and zero: no approval was needed and no gate ran.
	if receipt.Latency.ApprovalWaitMS != 0 || receipt.Latency.VerifyMS != 0 {
		t.Fatalf("latency = %+v, want zero for the phases that did not happen", receipt.Latency)
	}
	if receipt.LatencyMS != 6000 {
		t.Fatalf("flat latency = %dms, want the partition's total", receipt.LatencyMS)
	}
}

// A turn measured by nothing must not claim zeros. Without the partition the flat
// number falls back to the adapter's own wall clock, which is what an in-memory
// runtime and the no-op engine still have.
func TestReceiptWithoutMeasurementHasNoLatencyPartition(t *testing.T) {
	recorder := newTurnReceiptRecorder("fix add")
	receipt := recorder.build(turnReceiptObservations{})
	if receipt.Latency != nil {
		t.Fatalf("latency = %+v, want no partition", receipt.Latency)
	}
	if receipt.Budget != nil {
		t.Fatalf("budget = %+v, want no partition", receipt.Budget)
	}
}

// TestReceiptBudgetIncludesThisTurn covers the off-by-one a reader would
// otherwise hit: the engine folds a turn into its pool only once the turn
// completes, and the receipt is written before that.
func TestReceiptBudgetIncludesThisTurn(t *testing.T) {
	recorder := newTurnReceiptRecorder("fix add")
	recorder.observe(agentengine.Event{
		State: agentengine.Completed,
		Usage: &provider.Usage{InputTokens: 300, OutputTokens: 100},
		// testRoute-style pricing: 2 USD of spend on this turn.
		CostUSD: 2, CostKnown: true,
	})
	receipt := recorder.build(turnReceiptObservations{
		measurement: receiptMeasurement(t, turnkernel.UsageState{
			InputTokens: 300, OutputTokens: 100,
			CostMicrounits: 2_000_000, CostKnown: true, Frozen: true,
		}, &trace.Latency{}),
		spend: agentengine.BudgetSnapshot{
			TokensUsed: 1000, MaxTokens: 10_000, CostUSD: 3, MaxCostUSD: 25,
		},
	})

	if receipt.Budget == nil {
		t.Fatal("no budget partition")
	}
	if receipt.Budget.TokensUsed != 1400 || receipt.Budget.MaxTokens != 10_000 {
		t.Fatalf("budget tokens = %+v, want this turn's 400 on top of the pool's 1000", receipt.Budget)
	}
	if receipt.Budget.CostMicrounits != 5_000_000 ||
		receipt.Budget.MaxCostMicrounits != 25_000_000 {
		t.Fatalf("budget cost = %+v, want 5 USD of 25", receipt.Budget)
	}
}

// The receipt reports the gate's real verdict. Tests only claim a result when
// the gate ran the repository's own commands, since the diagnostics scope runs
// no tests at all.
func TestReceiptReportsVerificationGateVerdict(t *testing.T) {
	tests := map[string]struct {
		receipt   *agentengine.VerificationReceipt
		wantVerif string
		wantTests string
	}{
		"no gate": {
			wantVerif: protocol.ReceiptNotEvaluated, wantTests: protocol.ReceiptNotEvaluated,
		},
		"diagnostics passed": {
			receipt: &agentengine.VerificationReceipt{
				Receipt: verify.Receipt{Scope: verify.ScopeDiagnostics, Status: verify.StatusPassed},
				Action:  "passed",
			},
			wantVerif: protocol.ReceiptPassed, wantTests: protocol.ReceiptNotEvaluated,
		},
		"repository failed": {
			receipt: &agentengine.VerificationReceipt{
				Receipt: verify.Receipt{Scope: verify.ScopeRepository, Status: verify.StatusFailed},
				Action:  "failed",
			},
			wantVerif: protocol.ReceiptFailed, wantTests: protocol.ReceiptFailed,
		},
		"repository unavailable": {
			receipt: &agentengine.VerificationReceipt{
				Receipt: verify.Receipt{
					Scope: verify.ScopeRepository, Status: verify.StatusUnavailable,
				},
				Action: "passed",
			},
			wantVerif: protocol.ReceiptUnavailable, wantTests: protocol.ReceiptUnavailable,
		},
		"affected passed": {
			receipt: &agentengine.VerificationReceipt{
				Receipt: verify.Receipt{Scope: verify.ScopeAffected, Status: verify.StatusPassed},
				Action:  "passed",
			},
			wantVerif: protocol.ReceiptPassed, wantTests: protocol.ReceiptPassed,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			recorder := newTurnReceiptRecorder("fix add")
			recorder.observe(agentengine.Event{
				State: agentengine.Verifying, Verification: test.receipt,
			})
			receipt := recorder.build(turnReceiptObservations{})
			if receipt.Verification.Verify != test.wantVerif ||
				receipt.Verification.Tests != test.wantTests {
				t.Fatalf("verification = %+v", receipt.Verification)
			}
		})
	}
}

// A repair round is followed by another evaluation, and the receipt must report
// the verdict the turn ended on.
func TestReceiptReportsFinalVerificationAfterRepair(t *testing.T) {
	recorder := newTurnReceiptRecorder("fix add")
	recorder.observe(agentengine.Event{
		State: agentengine.Verifying,
		Verification: &agentengine.VerificationReceipt{
			Receipt: verify.Receipt{Scope: verify.ScopeDiagnostics, Status: verify.StatusFailed},
			Action:  "repair",
		},
	})
	recorder.observe(agentengine.Event{
		State: agentengine.Verifying,
		Verification: &agentengine.VerificationReceipt{
			Receipt:     verify.Receipt{Scope: verify.ScopeDiagnostics, Status: verify.StatusPassed},
			Action:      "passed",
			RepairSteps: 1,
			Attempts: []verify.Receipt{
				{
					Scope: verify.ScopeDiagnostics, Status: verify.StatusFailed,
					Checks: []verify.Check{{
						Name: "gopls", Command: "diagnostics calc.go",
						Reason: "post-edit diagnostics", Category: "diagnostic_failure",
						Status: verify.StatusFailed,
					}},
				},
				{Scope: verify.ScopeDiagnostics, Status: verify.StatusPassed},
			},
			Workspace: &agentengine.VerificationWorkspace{Status: "changed"},
		},
	})

	receipt := recorder.build(turnReceiptObservations{
		changes: []agentengine.TurnDiffEntry{{Path: "calc.go"}},
	})
	if receipt.Verification.Verify != protocol.ReceiptPassed {
		t.Fatalf("verify = %q, want the post-repair verdict", receipt.Verification.Verify)
	}
	if receipt.VerificationDetail == nil ||
		len(receipt.VerificationDetail.Attempts) != 2 ||
		receipt.VerificationDetail.Attempts[0].Checks[0].Category != "diagnostic_failure" ||
		receipt.WorkspaceOutcome == nil ||
		receipt.WorkspaceOutcome.Status != "changed" {
		t.Fatalf("detailed receipt = %+v workspace = %+v",
			receipt.VerificationDetail, receipt.WorkspaceOutcome)
	}
}

func TestVerificationDataCarriesChecksAndPaths(t *testing.T) {
	data := turnVerificationData(&agentengine.VerificationReceipt{
		Receipt: verify.Receipt{
			Scope: verify.ScopeRepository, Status: verify.StatusFailed, Errors: 1,
			Checks: []verify.Check{{
				Name: "go", Command: "go test ./...", Reason: "go.mod",
				Category: "test_failure", Status: verify.StatusFailed,
				ExitCode: 1, Stdout: "--- FAIL", Stderr: "exit status 1",
			}},
		},
		Mode: "hard", Action: "failed", RepairSteps: 2, Paths: []string{"calc.py"},
	})

	if data.Scope != "repository" || data.Status != protocol.ReceiptFailed ||
		data.Action != "failed" || data.RepairSteps != 2 || data.Errors != 1 {
		t.Fatalf("verification data = %+v", data)
	}
	if len(data.Checks) != 1 || data.Checks[0].Name != "go" ||
		data.Checks[0].Reason != "go.mod" ||
		data.Checks[0].Category != "test_failure" ||
		!strings.Contains(data.Checks[0].Output, "FAIL") ||
		!strings.Contains(data.Checks[0].Output, "exit status 1") {
		t.Fatalf("checks = %+v", data.Checks)
	}
	if len(data.Paths) != 1 || data.Paths[0] != "calc.py" {
		t.Fatalf("paths = %v", data.Paths)
	}
}

func TestReceiptReportsEvidenceFactsRisksAndReminders(t *testing.T) {
	recorder := newTurnReceiptRecorder("fix add")
	receipt := recorder.build(turnReceiptObservations{evidence: agentcontext.EvidenceSnapshot{
		Turn: 2,
		Facts: []agentcontext.EvidenceFact{{
			Kind: agentcontext.KindDefinition, Path: "auth/token.go", Line: 12,
			Symbol: "Verify", Tool: "search_definition", Turn: 1,
		}},
		Risks: []agentcontext.EvidenceRisk{
			{Kind: agentcontext.RiskUnverifiedChange, Path: "auth/token.go", Turn: 2},
		},
		Reminders:    []agentcontext.EvidenceReminder{{Kind: agentcontext.ReminderRepeatedCall, Detail: "search_text ran twice"}},
		OmittedFacts: 3,
	}})

	if receipt.Evidence == nil {
		t.Fatal("the receipt carries no evidence")
	}
	if len(receipt.Evidence.Facts) != 1 || receipt.Evidence.Facts[0].Symbol != "Verify" ||
		receipt.Evidence.Facts[0].Kind != "definition" || receipt.Evidence.Facts[0].Turn != 1 {
		t.Fatalf("facts = %+v", receipt.Evidence.Facts)
	}
	if len(receipt.Evidence.Risks) != 1 ||
		receipt.Evidence.Risks[0].Kind != agentcontext.RiskUnverifiedChange {
		t.Fatalf("risks = %+v", receipt.Evidence.Risks)
	}
	if len(receipt.Evidence.Reminders) != 1 || receipt.Evidence.Reminders[0] != "search_text ran twice" {
		t.Fatalf("reminders = %+v", receipt.Evidence.Reminders)
	}
	if receipt.Evidence.OmittedFacts != 3 {
		t.Fatalf("omitted facts = %d", receipt.Evidence.OmittedFacts)
	}
}

// An absent section says the session found nothing worth recording; an empty one
// would look like a collection failure.
func TestReceiptOmitsEvidenceWhenThereIsNone(t *testing.T) {
	receipt := newTurnReceiptRecorder("fix add").build(turnReceiptObservations{})
	if receipt.Evidence != nil {
		t.Fatalf("evidence = %+v, want it omitted", receipt.Evidence)
	}
}

func TestReceiptCarriesOnlyTerminalCompletionDeclaration(t *testing.T) {
	recorder := newTurnReceiptRecorder("change a.go")
	recorder.observe(agentengine.Event{
		State: agentengine.Preparing, Workspace: "/tmp/chat-worktree",
		WorkspaceIsolation: "worktree",
	})
	recorder.observe(agentengine.Event{
		State: agentengine.Completed,
		Completion: &tool.CompletionDeclaration{
			Status: "complete", Summary: "implemented and verified",
			ChangedPaths: []string{"a.go"}, VerificationCallIDs: []string{"verify-1"},
			MutationRevision: 1, CallID: "complete-1",
		},
	})
	receipt := recorder.build(turnReceiptObservations{})
	if receipt.WorkspaceIsolation != "worktree" ||
		receipt.Completion == nil || !receipt.Completion.Accepted ||
		receipt.Completion.CallID != "complete-1" ||
		receipt.Completion.Summary != "implemented and verified" {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestReceiptReportsObservedAdaptiveDelegation(t *testing.T) {
	retained := newTurnReceiptRecorder("small change")
	retained.observe(agentengine.Event{
		ModelExecution: &agentengine.ModelExecution{Kind: "model_sample"},
	})
	got := retained.build(turnReceiptObservations{delegationMode: "adaptive"})
	if got.Delegation == nil ||
		got.Delegation.Outcome != "retained_parent" ||
		got.Delegation.Attempts != 0 || got.Delegation.Spawned != 0 {
		t.Fatalf("retained delegation = %+v", got.Delegation)
	}

	delegated := newTurnReceiptRecorder("parallel review")
	delegated.observe(agentengine.Event{
		State: agentengine.RunningTools,
		ToolCall: &provider.ToolCall{
			Name: "spawn_agent", ID: "spawn-1",
		},
		Result: &tool.Result{Content: "started"},
	})
	got = delegated.build(turnReceiptObservations{delegationMode: "adaptive"})
	if got.Delegation == nil ||
		got.Delegation.Outcome != "delegated" ||
		got.Delegation.Attempts != 1 || got.Delegation.Spawned != 1 {
		t.Fatalf("successful delegation = %+v", got.Delegation)
	}

	blocked := newTurnReceiptRecorder("parallel review")
	blocked.observe(agentengine.Event{
		State: agentengine.RunningTools,
		ToolCall: &provider.ToolCall{
			Name: "spawn_agent", ID: "spawn-1",
		},
		Result: &tool.Result{Content: "denied", IsError: true},
	})
	got = blocked.build(turnReceiptObservations{delegationMode: "adaptive"})
	if got.Delegation == nil ||
		got.Delegation.Outcome != "blocked" ||
		got.Delegation.Attempts != 1 || got.Delegation.Spawned != 0 {
		t.Fatalf("blocked delegation = %+v", got.Delegation)
	}
}

// TestReceiptDistinguishesUnavailableDiagnostics pins that infrastructure
// failure is neither a pass nor a source-code diagnostic.
func TestReceiptDistinguishesUnavailableDiagnostics(t *testing.T) {
	for name, test := range map[string]struct {
		status string
		want   string
	}{
		"unavailable": {status: "unavailable", want: protocol.ReceiptUnavailable},
		"failed":      {status: "failed", want: protocol.ReceiptFailed},
		"ok":          {status: "ok", want: protocol.ReceiptPassed},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := newTurnReceiptRecorder("fix add")
			recorder.observe(agentengine.Event{
				State:       agentengine.RunningTools,
				ToolCall:    &provider.ToolCall{Name: "file_edit", ID: "call_edit"},
				Result:      &tool.Result{Content: "edited"},
				Diagnostics: []verify.DiagnosticReceipt{{Path: "calc.py", Status: test.status}},
			})
			receipt := recorder.build(turnReceiptObservations{})
			if receipt.Verification.Diagnostics != test.want {
				t.Fatalf(
					"diagnostics = %q want %q",
					receipt.Verification.Diagnostics, test.want,
				)
			}
		})
	}
}

func TestReceiptCollectsActualSG7PermissionDigests(t *testing.T) {
	digest := strings.Repeat("a", 64)
	recorder := newTurnReceiptRecorder("inspect")
	recorder.observe(agentengine.Event{
		State:    agentengine.RunningTools,
		ToolCall: &provider.ToolCall{Name: "file_read", ID: "call-read"},
		Result: &tool.Result{
			Content: "ok",
			Execution: &tool.ExecutionReceipt{Attempts: []tool.AttemptReceipt{
				{PermissionDigest: digest},
				{PermissionDigest: digest},
			}},
		},
	})
	receipt := recorder.build(turnReceiptObservations{})
	if len(receipt.PermissionDigests) != 1 ||
		receipt.PermissionDigests[0] != digest {
		t.Fatalf("permission digests = %v", receipt.PermissionDigests)
	}
}

func receiptMeasurement(
	t *testing.T,
	usage turnkernel.UsageState,
	latency *trace.Latency,
) *turnkernel.TerminalMeasurementSnapshot {
	t.Helper()
	var frozen trace.FrozenMeasurement
	if latency != nil {
		frozen = trace.FrozenMeasurement{
			FrozenAt: time.Unix(10, 0),
			Latency:  *latency,
			Recorded: true,
		}
	}
	measurement, err := freezeTerminalMeasurement(frozen, usage)
	if err != nil {
		t.Fatal(err)
	}
	return &measurement
}

func TestReceiptRecordsSkillsReadInvocation(t *testing.T) {
	recorder := newTurnReceiptRecorder("review")
	recorder.observe(agentengine.Event{
		State: agentengine.RunningTools,
		ToolCall: &provider.ToolCall{
			ID: "call-1", Name: "skills_read", Arguments: `{"handle":"skh"}`,
		},
		Result: &tool.Result{
			Content: "instructions",
			Metadata: map[string]any{
				"resolved_skills": []skillruntime.ResolvedSkill{{
					Name: "review", Version: "1.0.0",
					Source: skillruntime.SourceWorkspace,
					Digest: strings.Repeat("c", 64), Locked: true,
				}},
			},
		},
	})
	receipt := recorder.build(turnReceiptObservations{})
	if len(receipt.Skills) != 1 || receipt.Skills[0].Name != "review" {
		t.Fatalf("skills = %+v", receipt.Skills)
	}
}
