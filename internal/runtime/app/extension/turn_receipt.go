package extension

import (
	"encoding/json"
	"math"
	"strings"

	"github.com/fwtllh-png/QCode/internal/observability/verify"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func costMicrounits(costUSD float64) uint64 {
	if costUSD <= 0 || math.IsNaN(costUSD) || math.IsInf(costUSD, 0) {
		return 0
	}
	return uint64(math.Round(costUSD * 1e6))
}

// turnReceiptRecorder accumulates the per-turn execution receipt from the engine
// event stream. It only records what it observes, so a receipt never claims a
// check that did not run.
type turnReceiptRecorder struct {
	goal               string
	intent             protocol.TurnIntent
	outcome            protocol.TurnOutcome
	plan               string
	mode               string
	posture            string
	sandbox            string
	workspace          string
	workspaceIsolation string
	completion         *protocol.CompletionDeclaration
	convergence        *protocol.TurnConvergence
	providerRetry      *protocol.ReceiptProviderRetry
	modelExecution     protocol.ReceiptModelExecution
	toolExecution      map[string]int
	delegationAttempts int
	delegationSpawned  int
	toolsSucceeded     []string
	toolsFailed        []string
	approvals          int
	diagnosticCount    int
	diagnosticsStatus  string
	permissionDigests  []string
	// routes is which model answered for which purpose, in the order the turn
	// used them.
	routes        []protocol.ReceiptRoute
	issues        []string
	secondary     []protocol.TerminalIssue
	skills        []protocol.ReceiptSkill
	editorContext []protocol.EditorContextReceipt
	// verification is the last gate evaluation of the turn; repair rounds
	// deliberately overwrite earlier ones so the receipt reports the verdict the
	// turn ended on.
	verification *agentengine.VerificationReceipt
	// turn is the turn the observed events belong to, which a caller needs to ask
	// the engine what that turn read. budget is frozen on the terminal event.
	turn   uint64
	budget *protocol.ReceiptContextBudget
	frozen *turnReceiptObservations
}

// turnReceiptObservations is what the engine knows at the end of a turn that the event
// stream does not carry.
type turnReceiptObservations struct {
	// changes come from the turn-diff tracker: the writes the guard observed.
	changes []agentengine.TurnDiffEntry
	// readPaths are the files the turn read.
	readPaths []string
	// context is the prompt context as it was assembled for the turn.
	context []promptcontext.Receipt
	// selections explain each working-set path from that same prompt render.
	selections []promptcontext.Selection
	// catalog is the exact snapshot used by the turn's latest model sample.
	catalog *protocol.ReceiptCatalog
	// skillSelection records how the frozen Turn Skill catalog was reduced.
	skillSelection *protocol.ReceiptSkillSelection
	// evidence is what the session has established and what it has not proved.
	evidence agentcontext.EvidenceSnapshot
	// budget is how much of the compaction threshold the history occupies.
	budget *protocol.ReceiptContextBudget
	// conflicts are paths an automatic rollback could not restore, which the turn
	// leaves behind for a human.
	conflicts   []string
	measurement *turnkernel.TerminalMeasurementSnapshot
	// spend is the thread's pool as the engine sees it, before this turn's own
	// usage is folded in.
	spend          agentengine.BudgetSnapshot
	delegationMode string
}

func newTurnReceiptRecorder(goal string) *turnReceiptRecorder {
	return &turnReceiptRecorder{goal: goal}
}

func (r *turnReceiptRecorder) configure(
	intent protocol.TurnIntent,
	editorContext []protocol.EditorContextReceipt,
) {
	if r == nil {
		return
	}
	r.intent = intent
	r.editorContext = append(
		[]protocol.EditorContextReceipt(nil),
		editorContext...,
	)
}

func (r *turnReceiptRecorder) hasBudget() bool {
	return r != nil && r.budget != nil
}

func (r *turnReceiptRecorder) setOutcome(outcome protocol.TurnOutcome) {
	if r != nil {
		r.outcome = outcome
	}
}

// observe folds one engine event into the receipt.
func (r *turnReceiptRecorder) observe(event agentengine.Event) {
	if r == nil {
		return
	}
	switch event.State {
	case agentengine.Preparing:
		r.mode, r.posture = event.Mode, event.Posture
		r.sandbox, r.workspace = event.Sandbox, event.Workspace
		r.workspaceIsolation = event.WorkspaceIsolation
	case agentengine.AwaitingApproval:
		if event.Approval != nil {
			r.approvals++
		}
	case agentengine.RunningTools:
		r.observeTool(event)
	case agentengine.Failed:
		if event.Error != "" {
			r.issues = append(r.issues, event.Error)
		}
		if event.Convergence != nil {
			value := *event.Convergence
			value.PendingActions = append(
				[]string(nil),
				event.Convergence.PendingActions...,
			)
			r.convergence = &value
		}
		for _, issue := range event.SecondaryIssues {
			r.secondary = append(r.secondary, protocol.TerminalIssue{
				Phase: issue.Phase, Code: issue.Code, Message: issue.Message,
			})
		}
	}
	if event.Verification != nil {
		r.verification = event.Verification
	}
	if event.Completion != nil {
		declaration := event.Completion
		rejection := ""
		if declaration.Status == "incomplete" {
			rejection = "convergence_blocked"
		}
		r.completion = &protocol.CompletionDeclaration{
			Status: declaration.Status, Summary: declaration.Summary,
			OutputMode:   declaration.OutputMode,
			ChangedPaths: append([]string(nil), declaration.ChangedPaths...),
			VerificationCallIDs: append(
				[]string(nil), declaration.VerificationCallIDs...,
			),
			PendingActions:   append([]string(nil), declaration.PendingActions...),
			MutationRevision: declaration.MutationRevision,
			CallID:           declaration.CallID,
			Accepted:         declaration.Status == "complete",
			Rejection:        rejection,
		}
	}
	if event.ProviderRetry != nil {
		if r.providerRetry == nil {
			r.providerRetry = &protocol.ReceiptProviderRetry{}
		}
		r.providerRetry.Count++
		r.providerRetry.LastCode = event.ProviderRetry.Code
		r.providerRetry.LastCategory = event.ProviderRetry.Category
	}
	if event.ModelExecution != nil {
		switch event.ModelExecution.Kind {
		case "provider_attempt":
			if event.ModelExecution.Status == protocol.ProviderAttemptStarted {
				r.modelExecution.ProviderAttempts++
			}
		case "model_sample":
			r.modelExecution.ModelSamples++
			reason := event.ModelExecution.Reason
			if reason == "" {
				reason = promptcontext.SampleNormal
			}
			if r.modelExecution.SampleReasons == nil {
				r.modelExecution.SampleReasons = make(map[string]int)
			}
			r.modelExecution.SampleReasons[reason]++
			if reason == promptcontext.SampleCompletionRepair {
				r.modelExecution.CompletionRepairs++
			}
		}
	}
	if event.Turn > r.turn {
		r.turn = event.Turn
	}
	if event.ContextBudget != nil {
		r.budget = &protocol.ReceiptContextBudget{
			WindowID:              event.ContextBudget.WindowID,
			WindowNumber:          event.ContextBudget.WindowNumber,
			Observed:              event.ContextBudget.Observed,
			ActiveTokens:          event.ContextBudget.ActiveTokens,
			FullActiveTokens:      event.ContextBudget.FullActiveTokens,
			PrefillTokens:         event.ContextBudget.PrefillTokens,
			BodyTokens:            event.ContextBudget.BodyTokens,
			ToolDefinitionTokens:  event.ContextBudget.ToolDefinitionTokens,
			PendingTokens:         event.ContextBudget.PendingTokens,
			OutputReserve:         event.ContextBudget.OutputReserve,
			AutoCompactTokens:     event.ContextBudget.AutoCompactTokens,
			PrepareTokens:         event.ContextBudget.PrepareTokens,
			EmergencyTokens:       event.ContextBudget.EmergencyTokens,
			RecentTailTurns:       event.ContextBudget.RecentTailTurns,
			KeepRecentToolResults: event.ContextBudget.KeepRecentToolResults,
			HistoryTokenCeiling:   event.ContextBudget.HistoryTokenCeiling,
			Digest:                event.ContextBudget.Digest,
			NarrativeMode:         event.ContextBudget.NarrativeMode,
			EstimatedTokens:       event.ContextBudget.EstimatedTokens,
			MaxContextTokens:      event.ContextBudget.MaxContextTokens,
			Compactions:           event.ContextBudget.Compactions,
		}
	}
	// Any event that names a purpose contributes to the route summary, not just
	// the turn's opening one: a tool that samples a model of its own reports it
	// with its usage, and a receipt that listed only the turn's own route would
	// omit the model that produced part of the bill.
	r.observeRoute(event)
}

func (r *turnReceiptRecorder) freeze(
	engine *agentengine.Engine,
	measurement *turnkernel.TerminalMeasurementSnapshot,
) {
	if r == nil || engine == nil || r.frozen != nil {
		return
	}
	spec := engine.CurrentTurnSpec()
	r.frozen = &turnReceiptObservations{
		changes:        engine.TurnDiff(),
		readPaths:      engine.ReadPaths(r.turn),
		context:        engine.ContextReceipts(),
		selections:     engine.ContextSelections(),
		catalog:        engine.CatalogReceipt(),
		skillSelection: receiptSkillSelection(spec.SkillSelection),
		evidence:       engine.EvidenceSnapshot(),
		budget:         r.budget,
		conflicts:      engine.RollbackConflicts(),
		measurement:    measurement,
		spend:          engine.BudgetSnapshot(),
		delegationMode: spec.DelegationMode,
	}
}

// observeRoute records which route a purpose sampled on. A purpose is recorded
// once: a turn that resamples does so on the route it started with, and repeating
// it would read as two models having answered.
func (r *turnReceiptRecorder) observeRoute(event agentengine.Event) {
	if event.Purpose == "" || event.Provider == "" {
		return
	}
	for _, existing := range r.routes {
		if existing.Purpose == event.Purpose {
			return
		}
	}
	r.routes = append(r.routes, protocol.ReceiptRoute{
		Purpose: event.Purpose, Provider: event.Provider, Model: event.Model,
		ModelMetadata: event.ModelMetadata,
	})
}

func (r *turnReceiptRecorder) observeTool(event agentengine.Event) {
	if event.ToolCall == nil || event.Result == nil {
		return
	}
	kind := "business"
	switch event.ToolCall.Name {
	case "turn_complete", "update_plan", "submit_plan", "request_user_input":
		kind = "control"
	}
	if event.Result.Outcome != nil && event.Result.Outcome.Facts != nil &&
		event.Result.Outcome.Facts.Verification != nil {
		kind = "verification"
	}
	if r.toolExecution == nil {
		r.toolExecution = make(map[string]int)
	}
	r.toolExecution[kind]++
	if event.ToolCall.Name == "spawn_agent" {
		r.delegationAttempts++
		if !event.Result.IsError {
			r.delegationSpawned++
		}
	}
	if event.Result.IsError {
		r.toolExecution["failed"]++
		r.toolsFailed = appendUniqueString(r.toolsFailed, event.ToolCall.Name)
	} else {
		r.toolsSucceeded = appendUniqueString(r.toolsSucceeded, event.ToolCall.Name)
		if event.ToolCall.Name == "submit_plan" {
			r.plan = event.Result.Content
		}
	}
	if event.Result.Execution != nil {
		for _, attempt := range event.Result.Execution.Attempts {
			if attempt.PermissionDigest != "" {
				r.permissionDigests = appendUniqueString(
					r.permissionDigests,
					attempt.PermissionDigest,
				)
			}
		}
	}
	if event.ToolCall.Name == "skills_read" ||
		event.ToolCall.Name == "skills.read" {
		var resolved []struct {
			Name, Version, Source, Digest string
			Locked                        bool
		}
		if encoded, err := json.Marshal(event.Result.Metadata["resolved_skills"]); err == nil &&
			json.Unmarshal(encoded, &resolved) == nil {
			for _, item := range resolved {
				duplicate := false
				for _, existing := range r.skills {
					if existing.Name == item.Name && existing.Digest == item.Digest {
						duplicate = true
						break
					}
				}
				if duplicate {
					continue
				}
				r.skills = append(r.skills, protocol.ReceiptSkill{
					Name: item.Name, Version: item.Version, Source: string(item.Source),
					Digest: item.Digest, Locked: item.Locked,
				})
			}
		}
	}
	for _, receipt := range event.Diagnostics {
		status := protocol.ReceiptPassed
		switch {
		case receipt.Status == "failed":
			status = protocol.ReceiptFailed
		case receipt.Status == "unavailable":
			status = protocol.ReceiptUnavailable
		case len(receipt.Diagnostics) != 0:
			status = protocol.ReceiptFailed
		}
		r.diagnosticsStatus = mergeReceiptStatus(r.diagnosticsStatus, status)
		r.diagnosticCount += len(receipt.Diagnostics)
	}
}

// build renders the receipt from the events it folded plus what the engine
// observed outside the stream.
func (r *turnReceiptRecorder) build(
	supplied ...turnReceiptObservations,
) *protocol.ExecutionReceiptData {
	if r == nil {
		return nil
	}
	var observed turnReceiptObservations
	switch {
	case len(supplied) != 0:
		observed = supplied[0]
	case r.frozen != nil:
		observed = *r.frozen
	default:
		return nil
	}
	var measurement turnkernel.TerminalMeasurementSnapshot
	if observed.measurement != nil {
		measurement = *observed.measurement
	}
	usage := measurement.Usage
	receipt := &protocol.ExecutionReceiptData{
		Goal:   r.goal,
		Intent: r.intent, Outcome: r.outcome,
		Plan: r.plan, Mode: r.mode, Posture: r.posture,
		Sandbox: r.sandbox, Workspace: r.workspace,
		WorkspaceIsolation: r.workspaceIsolation,
		Completion:         r.completion,
		Convergence:        r.convergence,
		ProviderRetry:      r.providerRetry,
		ModelExecution:     r.modelExecution,
		ToolExecution:      r.toolExecution,
		Delegation: delegationReceipt(
			observed.delegationMode,
			r.modelExecution.ModelSamples,
			r.delegationAttempts,
			r.delegationSpawned,
		),
		Routes:         append([]protocol.ReceiptRoute(nil), r.routes...),
		ToolsSucceeded: r.toolsSucceeded, ToolsFailed: r.toolsFailed,
		Skills:             append([]protocol.ReceiptSkill(nil), r.skills...),
		SkillSelection:     observed.skillSelection,
		ApprovalsRequested: r.approvals,
		Verification: protocol.ReceiptVerification{
			Diagnostics: diagnosticsOutcome(r.diagnosticsStatus),
			Tests:       r.testsOutcome(),
			Verify:      r.verifyOutcome(),
		},
		VerificationDetail: verificationDetail(r.verification),
		WorkspaceOutcome:   workspaceOutcome(r.verification, observed.changes, observed.conflicts),
		DiagnosticCount:    r.diagnosticCount,
		ContextSections:    contextSections(observed.context),
		ContextSelections:  contextSelections(observed.selections),
		EditorContext:      append([]protocol.EditorContextReceipt(nil), r.editorContext...),
		Catalog:            observed.catalog,
		ContextBudget:      observed.budget,
		Evidence:           receiptEvidence(observed.evidence),
		ReadPaths:          append([]string(nil), observed.readPaths...),
		InputTokens:        usage.InputTokens, OutputTokens: usage.OutputTokens,
		ReasoningTokens: usage.ReasoningTokens, CachedTokens: usage.CachedTokens,
		CostMicrounits: usage.CostMicrounits, CostKnown: usage.CostKnown,
		MeasurementRecorded: measurement.Recorded(),
		MeasurementDigest:   measurement.Digest,
		UsageDigest:         measurement.UsageDigest,
		PermissionDigests: append(
			[]string(nil),
			r.permissionDigests...,
		),
		Latency:          receiptLatency(observed.measurement),
		Budget:           r.receiptBudget(observed.spend, usage),
		UnresolvedIssues: append(append([]string(nil), r.issues...), observed.conflicts...),
		SecondaryIssues:  append([]protocol.TerminalIssue(nil), r.secondary...),
		NotCollected:     protocol.UncollectedReceiptSections,
	}
	// The engine's clock is the better boundary when there is one: it starts when
	// the turn starts rather than when this recorder was constructed, and it is the
	// same clock the phases were measured against, so the flat number and the
	// partition cannot disagree.
	if receipt.Latency != nil {
		receipt.LatencyMS = receipt.Latency.TotalMS
	}
	for _, change := range observed.changes {
		if change.Path == "" {
			continue
		}
		receipt.Changes = append(receipt.Changes, protocol.ReceiptChange{
			Path: change.Path, Tool: change.Tool,
			Kind: change.Kind, Added: change.Added, Removed: change.Removed,
			Summary: change.Summary,
		})
	}
	return receipt
}

func delegationReceipt(
	mode string,
	modelSamples int,
	attempts int,
	spawned int,
) *protocol.ReceiptDelegation {
	if mode == "" {
		return nil
	}
	outcome := "not_evaluated"
	switch {
	case spawned > 0:
		outcome = "delegated"
	case attempts > 0:
		outcome = "blocked"
	case mode == "adaptive" && modelSamples > 0:
		outcome = "retained_parent"
	}
	return &protocol.ReceiptDelegation{
		Mode: mode, Outcome: outcome, Attempts: attempts, Spawned: spawned,
	}
}

func receiptSkillSelection(
	metrics agentengine.SkillSelectionMetrics,
) *protocol.ReceiptSkillSelection {
	if metrics.Method == "" {
		return nil
	}
	return &protocol.ReceiptSkillSelection{
		Method:                metrics.Method,
		CatalogSize:           metrics.CatalogSize,
		CandidateSize:         metrics.CandidateSize,
		VisibleSize:           metrics.VisibleSize,
		ExplicitMatches:       metrics.ExplicitMatches,
		QueryTerms:            metrics.QueryTerms,
		QueryTruncated:        metrics.QueryTruncated,
		CandidateSetTruncated: metrics.CandidateSetTruncated,
		OriginalTokens:        metrics.OriginalTokens,
		ProjectedTokens:       metrics.ProjectedTokens,
		TokenSavings:          metrics.TokenSavings,
		Recall:                metrics.Recall,
		Precision:             metrics.Precision,
		CacheHit:              metrics.CacheHit,
	}
}

// receiptLatency renders the measured phases. Everything but the first token is
// reported even when it is zero: a zero says the phase cost nothing, which is a
// fact about the turn, while an absent partition says nobody measured.
func receiptLatency(
	measurement *turnkernel.TerminalMeasurementSnapshot,
) *protocol.ReceiptLatency {
	if measurement == nil || !measurement.Latency.Turn.Recorded {
		return nil
	}
	latency := measurement.Latency
	rendered := &protocol.ReceiptLatency{
		TotalMS:        latency.Turn.Milliseconds,
		ProviderMS:     latency.Provider.Milliseconds,
		ToolMS:         latency.Tool.Milliseconds,
		ApprovalWaitMS: latency.ApprovalWait.Milliseconds,
		VerifyMS:       latency.Verification.Milliseconds,
	}
	if latency.FirstOutput.Recorded {
		first := latency.FirstOutput.Milliseconds
		rendered.FirstTokenMS = &first
	}
	return rendered
}

// receiptBudget adds this turn's spend to the pool the engine reports, because
// the engine only folds a turn in once it completes and a receipt that excluded
// its own turn would overstate what is left.
func (r *turnReceiptRecorder) receiptBudget(
	spend agentengine.BudgetSnapshot,
	usage turnkernel.UsageState,
) *protocol.ReceiptBudget {
	if spend == (agentengine.BudgetSnapshot{}) {
		return nil
	}
	return &protocol.ReceiptBudget{
		TokensUsed:        spend.TokensUsed + usage.InputTokens + usage.OutputTokens,
		MaxTokens:         spend.MaxTokens,
		CostMicrounits:    costMicrounits(spend.CostUSD) + usage.CostMicrounits,
		MaxCostMicrounits: costMicrounits(spend.MaxCostUSD),
	}
}

// contextSections retains empty partitions so assembled-empty differs from absent.
func contextSections(receipts []promptcontext.Receipt) []protocol.ReceiptContextSection {
	if len(receipts) == 0 {
		return nil
	}
	sections := make([]protocol.ReceiptContextSection, 0, len(receipts))
	for _, receipt := range receipts {
		sections = append(sections, protocol.ReceiptContextSection{
			Kind: receipt.Kind, Digest: receipt.Digest,
			OriginalBytes: receipt.OriginalBytes, RetainedBytes: receipt.RetainedBytes,
			OriginalTokens: receipt.OriginalTokens, RetainedTokens: receipt.RetainedTokens,
			Truncated: receipt.Truncated, TruncationReason: receipt.TruncationReason,
			Generation: receipt.Generation, CandidateCount: receipt.CandidateCount,
			SelectedIDs: append([]string(nil), receipt.SelectedIDs...),
		})
	}
	return sections
}

func contextSelections(
	selections []promptcontext.Selection,
) []protocol.ReceiptContextSelection {
	rendered := make([]protocol.ReceiptContextSelection, 0, len(selections))
	for _, selection := range selections {
		item := protocol.ReceiptContextSelection{
			Path: selection.Path, Kind: selection.Kind,
			Reasons: append([]string(nil), selection.Reasons...),
			Score:   selection.Score, Critical: selection.Critical,
			FirstTurn: selection.FirstTurn, LastTurn: selection.LastTurn,
			Included: selection.Included, Truncated: selection.Truncated,
			TruncationReason: selection.TruncationReason,
		}
		for _, fact := range selection.Evidence {
			item.Evidence = append(item.Evidence, protocol.ReceiptContextSelectionEvidence{
				Kind: fact.Kind, Line: fact.Line, Symbol: fact.Symbol,
				Tool: fact.Tool, Turn: fact.Turn,
			})
		}
		rendered = append(rendered, item)
	}
	return rendered
}

// receiptEvidence renders the evidence set for the audit record. A set with
// nothing in it produces no section: an absent section says the session found
// nothing worth recording, which an empty one would not.
func receiptEvidence(snapshot agentcontext.EvidenceSnapshot) *protocol.ReceiptEvidence {
	if snapshot.Empty() {
		return nil
	}
	rendered := &protocol.ReceiptEvidence{OmittedFacts: snapshot.OmittedFacts}
	for _, fact := range snapshot.Facts {
		rendered.Facts = append(rendered.Facts, protocol.ReceiptEvidenceFact{
			Kind: string(fact.Kind), Path: fact.Path, Line: fact.Line,
			Symbol: fact.Symbol, Tool: fact.Tool, Turn: fact.Turn,
		})
	}
	for _, risk := range snapshot.Risks {
		rendered.Risks = append(rendered.Risks, protocol.ReceiptEvidenceRisk{
			Kind: risk.Kind, Path: risk.Path, Turn: risk.Turn,
		})
	}
	for _, reminder := range snapshot.Reminders {
		rendered.Reminders = append(rendered.Reminders, reminder.Detail)
	}
	return rendered
}

// turnVerificationData renders one gate evaluation as a protocol event. Check output
// is trimmed to the failing streams so the event stays readable in a log.
func turnVerificationData(receipt *agentengine.VerificationReceipt) *protocol.TurnVerificationData {
	data := &protocol.TurnVerificationData{
		Scope: string(receipt.Scope), Mode: receipt.Mode, Action: receipt.Action,
		Status: receipt.Status, RepairSteps: receipt.RepairSteps,
		Errors: receipt.Errors, Warnings: receipt.Warnings,
		Paths:          append([]string(nil), receipt.Paths...),
		UncoveredPaths: append([]string(nil), receipt.UncoveredPaths...),
		Message:        receipt.Message,
	}
	for _, check := range receipt.Checks {
		output := strings.TrimSpace(check.Stdout + "\n" + check.Stderr)
		data.Checks = append(data.Checks, protocol.VerificationCheck{
			Name: check.Name, Command: check.Command, Reason: check.Reason, Status: check.Status,
			Category: check.Category, ExitCode: check.ExitCode, Output: output,
		})
	}
	return data
}

func verificationDetail(
	receipt *agentengine.VerificationReceipt,
) *protocol.ReceiptVerificationDetail {
	if receipt == nil {
		return nil
	}
	attempts := receipt.Attempts
	if len(attempts) == 0 {
		attempts = []verify.Receipt{receipt.Receipt}
	}
	detail := &protocol.ReceiptVerificationDetail{
		Mode: receipt.Mode, FinalStatus: receipt.Status,
		Action: receipt.Action, RepairSteps: receipt.RepairSteps,
		UncoveredPaths: append([]string(nil), receipt.UncoveredPaths...),
		Attempts:       make([]protocol.ReceiptVerificationAttempt, 0, len(attempts)),
	}
	for step, attempt := range attempts {
		rendered := protocol.ReceiptVerificationAttempt{
			Step: step, Scope: string(attempt.Scope),
			Status: attempt.Status, Message: attempt.Message,
		}
		for _, check := range attempt.Checks {
			rendered.Checks = append(rendered.Checks, protocol.VerificationCheck{
				Name: check.Name, Command: check.Command, Reason: check.Reason,
				Category: check.Category, Status: check.Status,
				ExitCode: check.ExitCode,
				Output:   strings.TrimSpace(check.Stdout + "\n" + check.Stderr),
			})
		}
		detail.Attempts = append(detail.Attempts, rendered)
	}
	return detail
}

func workspaceOutcome(
	verification *agentengine.VerificationReceipt,
	changes []agentengine.TurnDiffEntry,
	rollbackConflicts []string,
) *protocol.ReceiptWorkspaceOutcome {
	outcome := &protocol.ReceiptWorkspaceOutcome{Status: "unchanged"}
	for _, change := range changes {
		if change.Path != "" {
			outcome.Changed = appendUniqueString(outcome.Changed, change.Path)
		}
	}
	if len(outcome.Changed) != 0 {
		outcome.Status = "changed"
	}
	if verification != nil && verification.Workspace != nil {
		outcome.Status = verification.Workspace.Status
		outcome.Restored = append([]string(nil), verification.Workspace.Restored...)
		outcome.Conflicts = append([]string(nil), verification.Workspace.Conflicts...)
		outcome.NonFileSideEffectsReverted =
			verification.Workspace.NonFileSideEffectsReverted
		outcome.Note = verification.Workspace.Note
	}
	for _, conflict := range rollbackConflicts {
		outcome.Conflicts = appendUniqueString(outcome.Conflicts, conflict)
	}
	if len(outcome.Conflicts) != 0 {
		outcome.Status = "conflicted"
	}
	return outcome
}

// verifyOutcome reports the verification gate's verdict for the turn.
func (r *turnReceiptRecorder) verifyOutcome() string {
	if r.verification == nil {
		return protocol.ReceiptNotEvaluated
	}
	switch r.verification.Status {
	case verify.StatusPassed:
		return protocol.ReceiptPassed
	case verify.StatusFailed:
		return protocol.ReceiptFailed
	case verify.StatusUnavailable:
		return protocol.ReceiptUnavailable
	default:
		return protocol.ReceiptNotEvaluated
	}
}

// testsOutcome only reports a verdict when the gate actually ran the
// repository's own commands; the diagnostics scope runs no tests.
func (r *turnReceiptRecorder) testsOutcome() string {
	if r.verification == nil ||
		(r.verification.Scope != verify.ScopeRepository &&
			r.verification.Scope != verify.ScopeAffected) {
		return protocol.ReceiptNotEvaluated
	}
	return r.verifyOutcome()
}

func diagnosticsOutcome(status string) string {
	if status == "" {
		return protocol.ReceiptNotEvaluated
	}
	return status
}

func mergeReceiptStatus(current, next string) string {
	priority := map[string]int{
		protocol.ReceiptNotEvaluated: 0,
		protocol.ReceiptPassed:       1,
		protocol.ReceiptUnavailable:  2,
		protocol.ReceiptFailed:       3,
	}
	if priority[next] > priority[current] {
		return next
	}
	return current
}

func appendUniqueString(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, candidate := range values {
		if candidate == value {
			return values
		}
	}
	return append(values, value)
}
