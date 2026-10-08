package protocol

import (
	"errors"
	"slices"
)

// Verification outcomes carried by ExecutionReceiptData. NotEvaluated means the
// runtime never ran that check, which is deliberately distinct from Passed so a
// receipt can never imply verification that did not happen.
const (
	ReceiptNotRequired  = "not_required"
	ReceiptNotEvaluated = "not_evaluated"
	ReceiptPassed       = "passed"
	ReceiptFailed       = "failed"
	// ReceiptUnavailable means the check was attempted but could not run, which
	// is also not evidence of correctness.
	ReceiptUnavailable = "unavailable"
)

// UncollectedReceiptSections names receipt sections this build does not
// populate. Emitting it keeps an empty section from being read as "nothing
// happened"; entries are removed as the sections are implemented.
var UncollectedReceiptSections = []string{
	"unreverted_side_effects",
}

// ReceiptVerification records which verification layers ran for a turn.
type ReceiptVerification struct {
	Diagnostics string `json:"diagnostics"`
	Tests       string `json:"tests"`
	Verify      string `json:"verify"`
}

type ReceiptVerificationAttempt struct {
	Step    int                 `json:"step"`
	Scope   string              `json:"scope"`
	Status  string              `json:"status"`
	Checks  []VerificationCheck `json:"checks,omitempty"`
	Message string              `json:"message,omitempty"`
}

// ReceiptVerificationDetail preserves the complete gate history rather than
// reducing repair to the final pass/fail bit.
type ReceiptVerificationDetail struct {
	Mode           string                       `json:"mode"`
	FinalStatus    string                       `json:"final_status"`
	Action         string                       `json:"action"`
	RepairSteps    int                          `json:"repair_steps"`
	UncoveredPaths []string                     `json:"uncovered_paths,omitempty"`
	Attempts       []ReceiptVerificationAttempt `json:"attempts"`
}

// ReceiptWorkspaceOutcome is the final workspace state after verification and
// rollback policy have settled.
type ReceiptWorkspaceOutcome struct {
	Status                     string   `json:"status"`
	Changed                    []string `json:"changed,omitempty"`
	Restored                   []string `json:"restored,omitempty"`
	Conflicts                  []string `json:"conflicts,omitempty"`
	NonFileSideEffectsReverted bool     `json:"non_file_side_effects_reverted"`
	Note                       string   `json:"note,omitempty"`
}

func (v *ReceiptVerification) normalize() {
	for _, field := range []*string{&v.Diagnostics, &v.Tests, &v.Verify} {
		switch *field {
		case ReceiptNotRequired, ReceiptPassed, ReceiptFailed, ReceiptNotEvaluated, ReceiptUnavailable:
		default:
			*field = ReceiptNotEvaluated
		}
	}
}

// ReceiptChange is one path a tool observably changed during the turn.
type ReceiptChange struct {
	Path string `json:"path"`
	Tool string `json:"tool"`
	// Kind is created | modified | deleted.
	Kind string `json:"kind,omitempty"`
	// Added and Removed are the turn's cumulative line delta for the path,
	// measured against the content the turn started from. Both stay zero for
	// binary content, where lines mean nothing.
	Added   int    `json:"added,omitempty"`
	Removed int    `json:"removed,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// ReceiptContextSection is one partition of the prompt context as it was sent.
type ReceiptContextSection struct {
	// Kind is the partition name, such as base_system or repo_map.
	Kind string `json:"kind"`
	// Digest identifies the section before truncation, so two turns carrying the
	// same section can be recognized as carrying the same bytes.
	Digest         string `json:"digest,omitempty"`
	OriginalBytes  int    `json:"original_bytes"`
	RetainedBytes  int    `json:"retained_bytes"`
	OriginalTokens uint64 `json:"original_tokens,omitempty"`
	RetainedTokens uint64 `json:"retained_tokens,omitempty"`
	Truncated      bool   `json:"truncated,omitempty"`
	// TruncationReason is byte_budget or token_budget when Truncated is set.
	TruncationReason string   `json:"truncation_reason,omitempty"`
	Generation       uint64   `json:"generation,omitempty"`
	CandidateCount   int      `json:"candidate_count,omitempty"`
	SelectedIDs      []string `json:"selected_ids,omitempty"`
}

type ReceiptContextSelectionEvidence struct {
	Kind   string `json:"kind"`
	Line   int    `json:"line,omitempty"`
	Symbol string `json:"symbol,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Turn   uint64 `json:"turn"`
}

// ReceiptContextSelection explains one path offered by the working-set
// selector. Included distinguishes a retained line from one cut by budget.
type ReceiptContextSelection struct {
	Path             string                            `json:"path"`
	Kind             string                            `json:"kind"`
	Reasons          []string                          `json:"reasons"`
	Evidence         []ReceiptContextSelectionEvidence `json:"evidence,omitempty"`
	Score            int                               `json:"score"`
	Critical         bool                              `json:"critical,omitempty"`
	FirstTurn        uint64                            `json:"first_turn"`
	LastTurn         uint64                            `json:"last_turn"`
	Included         bool                              `json:"included"`
	Truncated        bool                              `json:"truncated,omitempty"`
	TruncationReason string                            `json:"truncation_reason,omitempty"`
}

// ReceiptEvidenceFact is one thing a lookup established during the session.
type ReceiptEvidenceFact struct {
	// Kind is definition | reference | test | config | text_match. The
	// classification is lexical, so it is a hint about the path rather than a
	// compiler's verdict.
	Kind string `json:"kind"`
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
	// Symbol names the declaration a symbol lookup matched.
	Symbol string `json:"symbol,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Turn   uint64 `json:"turn"`
}

// ReceiptEvidenceRisk is one gap between what the session changed and what it
// proved.
type ReceiptEvidenceRisk struct {
	// Kind is changed_without_verification | changed_without_read |
	// unresolved_diagnostics.
	Kind string `json:"kind"`
	Path string `json:"path"`
	Turn uint64 `json:"turn"`
}

// ReceiptEvidence is what the session found out and what it still owes.
//
// It spans the session rather than the turn: a file changed three turns ago and
// still unverified is a risk this turn's receipt has to report, because that is
// when a reviewer is looking.
type ReceiptEvidence struct {
	Facts []ReceiptEvidenceFact `json:"facts,omitempty"`
	Risks []ReceiptEvidenceRisk `json:"risks,omitempty"`
	// Reminders are the wasteful call patterns the turn was told about.
	Reminders []string `json:"reminders,omitempty"`
	// OmittedFacts is how many facts the entry limit left out.
	OmittedFacts int `json:"omitted_facts,omitempty"`
}

// ReceiptContextBudget records the token-native active window at termination.
//
// It answers a question the section list cannot: whether a turn that lost detail
// lost it to a budget that is about to bite again. A thread on its fourth
// compaction is one whose early history now exists only as summary.
type ReceiptContextBudget struct {
	WindowID              string `json:"window_id,omitempty"`
	WindowNumber          uint64 `json:"window_number,omitempty"`
	Observed              bool   `json:"observed,omitempty"`
	ActiveTokens          uint64 `json:"active_tokens"`
	FullActiveTokens      uint64 `json:"full_active_tokens,omitempty"`
	PrefillTokens         uint64 `json:"prefill_tokens,omitempty"`
	BodyTokens            uint64 `json:"body_tokens,omitempty"`
	ToolDefinitionTokens  uint64 `json:"tool_definition_tokens,omitempty"`
	PendingTokens         uint64 `json:"pending_tokens,omitempty"`
	OutputReserve         uint64 `json:"output_reserve,omitempty"`
	AutoCompactTokens     uint64 `json:"auto_compact_tokens"`
	PrepareTokens         uint64 `json:"prepare_tokens,omitempty"`
	EmergencyTokens       uint64 `json:"emergency_tokens,omitempty"`
	RecentTailTurns       int    `json:"recent_tail_turns,omitempty"`
	KeepRecentToolResults int    `json:"keep_recent_tool_results,omitempty"`
	HistoryTokenCeiling   uint64 `json:"history_token_ceiling,omitempty"`
	Digest                string `json:"digest,omitempty"`
	NarrativeMode         string `json:"narrative_mode,omitempty"`
	EstimatedTokens       uint64 `json:"estimated_tokens,omitempty"`
	MaxContextTokens      uint64 `json:"max_context_tokens,omitempty"`
	Compactions           int    `json:"compactions"`
}

// ReceiptLatency records measured phase duration. Phases overlap:
//
//	ApprovalWaitMS ⊆ ToolMS     a tool parks for approval inside its own call
//	ProviderMS     ⊆ TotalMS    model calls are sequential within a turn
//	ToolMS         ⋛ TotalMS    tools run in parallel, so their sum can exceed
//	                            the wall clock the turn actually took
//
// ToolMS sums calls for cost comparison. Zero means measured with no work;
// FirstTokenMS is optional because a model may produce no output.
type ReceiptLatency struct {
	TotalMS        int64  `json:"total_ms"`
	FirstTokenMS   *int64 `json:"first_token_ms,omitempty"`
	ProviderMS     int64  `json:"provider_ms"`
	ToolMS         int64  `json:"tool_ms"`
	ApprovalWaitMS int64  `json:"approval_wait_ms"`
	VerifyMS       int64  `json:"verify_ms"`
}

// ReceiptBudget is how much of the session's spending limits the thread has used,
// including the turn this receipt describes.
//
// It is on the receipt because the engine is the only place the pool is known,
// and a host that recomputed it from usage rows would get a different answer:
// the pool spans the thread, not the turn.
//
// A zero maximum is no limit rather than a limit of zero. Cost is only meaningful
// when CostKnown on the receipt is true, for the same reason the totals beside it
// are.
type ReceiptBudget struct {
	TokensUsed        uint64 `json:"tokens_used"`
	MaxTokens         uint64 `json:"max_tokens,omitempty"`
	CostMicrounits    uint64 `json:"cost_microunits"`
	MaxCostMicrounits uint64 `json:"max_cost_microunits,omitempty"`
}

// ReceiptRoute is one purpose's resolved route: which model answered, and what
// it was answering for.
//
// Purpose is the part worth carrying. Provider and model are already on the
// events a turn emits, but not why they were chosen, and "why did this turn use
// the expensive model" is the question a per-purpose routing table creates.
type ReceiptRoute struct {
	Purpose       string                   `json:"purpose"`
	Provider      string                   `json:"provider"`
	Model         string                   `json:"model"`
	ModelMetadata *ModelMetadataProvenance `json:"model_metadata_provenance,omitempty"`
}

type ReceiptSkill struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Source  string `json:"source"`
	Digest  string `json:"digest"`
	Locked  bool   `json:"locked"`
}

type ReceiptSkillSelection struct {
	Method                string  `json:"method"`
	CatalogSize           int     `json:"catalog_size"`
	CandidateSize         int     `json:"candidate_size"`
	VisibleSize           int     `json:"visible_size"`
	ExplicitMatches       int     `json:"explicit_matches"`
	QueryTerms            int     `json:"query_terms"`
	QueryTruncated        bool    `json:"query_truncated"`
	CandidateSetTruncated bool    `json:"candidate_set_truncated"`
	OriginalTokens        uint64  `json:"original_tokens"`
	ProjectedTokens       uint64  `json:"projected_tokens"`
	TokenSavings          float64 `json:"token_savings"`
	Recall                float64 `json:"recall"`
	Precision             float64 `json:"precision"`
	CacheHit              bool    `json:"cache_hit"`
}

type ReceiptProviderRetry struct {
	Count        int       `json:"count"`
	LastCode     ErrorCode `json:"last_code"`
	LastCategory string    `json:"last_category"`
}

type ReceiptModelExecution struct {
	ProviderAttempts  int            `json:"provider_attempts"`
	ModelSamples      int            `json:"model_samples"`
	CompletionRepairs int            `json:"completion_repairs"`
	SampleReasons     map[string]int `json:"sample_reasons,omitempty"`
}

type ReceiptDelegation struct {
	Mode     string `json:"mode"`
	Outcome  string `json:"outcome"`
	Attempts int    `json:"attempts"`
	Spawned  int    `json:"spawned"`
}

// ExecutionReceiptData is the per-turn audit record: what the turn was asked to
// do, what it touched, what verified it, and what it cost.
// It is emitted for completed and failed turns alike, immediately before the
// terminal event, so a host can render or persist one authoritative summary.
//
// Every field reflects observed execution. Sections the runtime cannot yet
// determine are listed in NotCollected rather than left silently empty.
type ExecutionReceiptData struct {
	Goal               string                 `json:"goal"`
	Intent             TurnIntent             `json:"intent,omitempty"`
	Outcome            TurnOutcome            `json:"outcome,omitempty"`
	Plan               string                 `json:"plan,omitempty"`
	Mode               string                 `json:"mode,omitempty"`
	Posture            string                 `json:"posture,omitempty"`
	Sandbox            string                 `json:"sandbox,omitempty"`
	Workspace          string                 `json:"workspace,omitempty"`
	WorkspaceIsolation string                 `json:"workspace_isolation,omitempty"`
	Completion         *CompletionDeclaration `json:"completion,omitempty"`
	Convergence        *TurnConvergence       `json:"convergence,omitempty"`
	ProviderRetry      *ReceiptProviderRetry  `json:"provider_retry,omitempty"`
	ModelExecution     ReceiptModelExecution  `json:"model_execution"`
	ToolExecution      map[string]int         `json:"tool_execution"`
	Delegation         *ReceiptDelegation     `json:"delegation,omitempty"`

	// Routes are the routes the turn actually sampled on, one entry per purpose.
	// It is what the turn did, not the table it could have used: a slot the turn
	// never reached would read as a model that charged for something.
	Routes []ReceiptRoute `json:"routes,omitempty"`

	// Changes is observed: every path whose content the turn actually altered,
	// derived from before/after fingerprints rather than tool arguments.
	Changes []ReceiptChange `json:"changes,omitempty"`
	// ReadPaths is every path the turn read, so a reviewer can tell an edit made
	// after reading a file from one made blind.
	ReadPaths      []string               `json:"read_paths,omitempty"`
	ToolsSucceeded []string               `json:"tools_succeeded,omitempty"`
	ToolsFailed    []string               `json:"tools_failed,omitempty"`
	Skills         []ReceiptSkill         `json:"skills,omitempty"`
	SkillSelection *ReceiptSkillSelection `json:"skill_selection,omitempty"`
	// ApprovalsRequested counts approval prompts raised during the turn.
	ApprovalsRequested int `json:"approvals_requested"`

	Verification       ReceiptVerification        `json:"verification"`
	VerificationDetail *ReceiptVerificationDetail `json:"verification_detail,omitempty"`
	WorkspaceOutcome   *ReceiptWorkspaceOutcome   `json:"workspace_outcome,omitempty"`
	DiagnosticCount    int                        `json:"diagnostic_count"`

	// ContextSections reports what the assembled prompt context cost and whether
	// a budget cut any of it. A truncated section is the usual explanation for a
	// model that ignored something it was told, so it belongs in the audit trail.
	ContextSections   []ReceiptContextSection   `json:"context_sections,omitempty"`
	ContextSelections []ReceiptContextSelection `json:"context_selections,omitempty"`
	// EditorContext reports each explicit editor reference after Runtime
	// identity/range validation and content truncation.
	EditorContext []EditorContextReceipt `json:"editor_context,omitempty"`
	Catalog       *ReceiptCatalog        `json:"catalog,omitempty"`

	// ContextBudget reports the public view contract and replacement ceiling.
	ContextBudget *ReceiptContextBudget `json:"context_budget,omitempty"`
	// The last sampled selection and calibration are retained even if the
	// provider returned no usage. Earlier selections live on provider attempts.
	ContextProjection *ReceiptContextProjection `json:"context_projection,omitempty"`
	ContextSample     *SampleContextData        `json:"context_sample,omitempty"`
	ContextRecovery   *ReceiptContextRecovery   `json:"context_recovery,omitempty"`

	// Evidence reports what lookups established and which changes are still
	// unproved. It is observed like Changes: nothing here comes from the model
	// declaring what it believes.
	Evidence *ReceiptEvidence `json:"evidence,omitempty"`

	InputTokens     uint64 `json:"input_tokens"`
	OutputTokens    uint64 `json:"output_tokens"`
	ReasoningTokens uint64 `json:"reasoning_tokens,omitempty"`
	CachedTokens    uint64 `json:"cached_tokens,omitempty"`
	CostMicrounits  uint64 `json:"cost_microunits"`
	// CostKnown is false when the model has no pricing metadata; CostMicrounits
	// is then meaningless and must be shown as unknown rather than zero.
	CostKnown bool `json:"cost_known"`
	// MeasurementRecorded is true only when both frozen Usage and Turn wall
	// time were measured. Digests bind this Receipt to the Terminal Envelope's
	// immutable measurement snapshot.
	MeasurementRecorded bool   `json:"measurement_recorded"`
	MeasurementDigest   string `json:"measurement_digest,omitempty"`
	UsageDigest         string `json:"usage_digest,omitempty"`
	// PermissionDigests are the distinct SG7 EffectivePermissionProfile
	// digests actually used by guarded tool attempts during this Turn.
	PermissionDigests []string `json:"permission_digests,omitempty"`
	// LatencyMS is the turn's wall clock, kept flat beside the partition for the
	// hosts that only want one number. It equals Latency.TotalMS whenever a
	// partition is present.
	LatencyMS int64 `json:"latency_ms"`
	// Latency splits that wall clock into phases. It is absent when the engine
	// that ran the turn does not measure them, which is what separates "this turn
	// had no approvals" from "nobody timed the approvals".
	Latency *ReceiptLatency `json:"latency,omitempty"`
	// Budget is what the thread has spent against its limits. Absent means the
	// turn ran without an engine that tracks a pool.
	Budget *ReceiptBudget `json:"budget,omitempty"`

	// UnresolvedIssues records why a turn did not end clean, such as the failure
	// message or a tool error the model never recovered from.
	UnresolvedIssues []string `json:"unresolved_issues,omitempty"`
	// SecondaryIssues preserves cleanup/finalization failures separately from
	// the primary terminal error.
	SecondaryIssues []TerminalIssue `json:"secondary_issues,omitempty"`
	NotCollected    []string        `json:"not_collected,omitempty"`
}

type ReceiptCatalog struct {
	CatalogID     string   `json:"catalog_id"`
	Generation    uint64   `json:"generation"`
	Digest        string   `json:"digest"`
	Advertised    []string `json:"advertised,omitempty"`
	Materialized  []string `json:"materialized,omitempty"`
	DeferredCount int      `json:"deferred_count"`
	OmittedCount  int      `json:"omitted_count"`
}

func (*ExecutionReceiptData) eventKind() EventKind { return EventExecutionReceipt }

func (d *ExecutionReceiptData) validate() error {
	d.Verification.normalize()
	if !NormalizeTurnIntent(d.Intent).Valid() {
		return errors.New("receipt turn intent is invalid")
	}
	switch d.Outcome {
	case "", TurnOutcomeAnswered, TurnOutcomePlanned, TurnOutcomeChanged, TurnOutcomeUnchanged, TurnOutcomeOperated:
	default:
		return errors.New("receipt turn outcome is invalid")
	}
	if err := validateEditorContextReceipts(d.EditorContext); err != nil {
		return err
	}
	if d.Catalog != nil &&
		(d.Catalog.CatalogID == "" || d.Catalog.Generation == 0 || d.Catalog.Digest == "") {
		return errors.New("receipt catalog requires catalog_id, generation, and digest")
	}
	if d.WorkspaceIsolation != "" &&
		d.WorkspaceIsolation != "shared" &&
		d.WorkspaceIsolation != "worktree" {
		return errors.New("receipt workspace isolation is invalid")
	}
	if err := validateReceiptDelegation(d.Delegation); err != nil {
		return err
	}
	for _, route := range d.Routes {
		if route.Purpose == "" || route.Provider == "" || route.Model == "" {
			return errors.New("receipt route identity is incomplete")
		}
		// ModelMetadata is optional only for persisted pre-contract receipts.
		if route.ModelMetadata != nil {
			if err := route.ModelMetadata.Validate(); err != nil {
				return err
			}
		}
	}
	if d.Completion != nil {
		if err := d.Completion.validate(); err != nil {
			return err
		}
		if !d.Completion.Accepted &&
			(d.Convergence == nil ||
				d.Completion.Status != "incomplete" ||
				d.Completion.Summary != d.Convergence.Summary ||
				!slices.Equal(
					d.Completion.PendingActions,
					d.Convergence.PendingActions,
				)) {
			return errors.New("receipt completion declaration must be accepted")
		}
	}
	if d.Convergence != nil {
		if err := d.Convergence.validate(); err != nil {
			return err
		}
	}
	for _, change := range d.Changes {
		if change.Path == "" {
			return errors.New("receipt change path is required")
		}
	}
	if d.VerificationDetail != nil {
		if d.VerificationDetail.Mode == "" ||
			d.VerificationDetail.FinalStatus == "" ||
			d.VerificationDetail.Action == "" {
			return errors.New("receipt verification detail requires mode, final_status, and action")
		}
		for _, attempt := range d.VerificationDetail.Attempts {
			if attempt.Step < 0 || attempt.Scope == "" || attempt.Status == "" {
				return errors.New("receipt verification attempt is invalid")
			}
		}
	}
	if d.WorkspaceOutcome != nil && d.WorkspaceOutcome.Status == "" {
		return errors.New("receipt workspace outcome requires status")
	}
	for _, digest := range d.PermissionDigests {
		if !validSHA256(digest) {
			return errors.New("receipt permission digest is invalid")
		}
	}
	for _, selection := range d.ContextSelections {
		if selection.Path == "" || selection.Kind == "" || len(selection.Reasons) == 0 {
			return errors.New("receipt context selection requires path, kind, and reasons")
		}
		if selection.Truncated == selection.Included ||
			(selection.Truncated && selection.TruncationReason == "") ||
			(!selection.Truncated && selection.TruncationReason != "") {
			return errors.New("receipt context selection truncation is inconsistent")
		}
	}
	for _, skill := range d.Skills {
		if skill.Name == "" || skill.Version == "" || skill.Source == "" ||
			!validSHA256(skill.Digest) {
			return errors.New("receipt skill requires name, version, source, and digest")
		}
	}
	if selection := d.SkillSelection; selection != nil {
		if selection.Method == "" ||
			selection.CatalogSize < 0 ||
			selection.CandidateSize < 0 ||
			selection.VisibleSize < 0 ||
			selection.ExplicitMatches < 0 ||
			selection.QueryTerms < 0 ||
			selection.CandidateSize > selection.CatalogSize ||
			selection.VisibleSize > selection.CatalogSize ||
			selection.ExplicitMatches > selection.CandidateSize ||
			selection.ProjectedTokens > selection.OriginalTokens ||
			selection.TokenSavings < 0 || selection.TokenSavings > 1 ||
			selection.Recall < 0 || selection.Recall > 1 ||
			selection.Precision < 0 || selection.Precision > 1 {
			return errors.New("receipt skill selection is invalid")
		}
	}
	if d.Evidence != nil {
		for _, fact := range d.Evidence.Facts {
			if fact.Kind == "" || fact.Path == "" {
				return errors.New("receipt evidence fact needs a kind and a path")
			}
		}
		for _, risk := range d.Evidence.Risks {
			if risk.Kind == "" || risk.Path == "" {
				return errors.New("receipt evidence risk needs a kind and a path")
			}
		}
	}
	return nil
}

func validateReceiptDelegation(delegation *ReceiptDelegation) error {
	if delegation == nil {
		return nil
	}
	switch delegation.Mode {
	case "disabled", "explicit", "adaptive":
	default:
		return errors.New("receipt delegation mode is invalid")
	}
	if delegation.Attempts < 0 || delegation.Spawned < 0 ||
		delegation.Spawned > delegation.Attempts {
		return errors.New("receipt delegation counts are invalid")
	}
	switch delegation.Outcome {
	case "delegated":
		if delegation.Spawned == 0 || delegation.Mode == "disabled" {
			return errors.New("receipt delegated outcome is inconsistent")
		}
	case "blocked":
		if delegation.Attempts == 0 || delegation.Spawned != 0 ||
			delegation.Mode == "disabled" {
			return errors.New("receipt blocked delegation outcome is inconsistent")
		}
	case "retained_parent":
		if delegation.Mode != "adaptive" ||
			delegation.Attempts != 0 || delegation.Spawned != 0 {
			return errors.New("receipt retained-parent delegation outcome is inconsistent")
		}
	case "not_evaluated":
		if delegation.Attempts != 0 || delegation.Spawned != 0 {
			return errors.New("receipt non-evaluated delegation outcome is inconsistent")
		}
	default:
		return errors.New("receipt delegation outcome is invalid")
	}
	return nil
}
