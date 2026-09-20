// Package turnkernel defines the deterministic Turn state machine.
//
// The package deliberately contains no provider, tool, persistence, clock, or
// host dependencies. It can therefore run in Coordinator and replay modes before it
// becomes authoritative for the Engine business loop.
package turnkernel

import (
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerassembly "github.com/fwtllh-png/QCode/internal/adapter/provider/assembly"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type Phase string

const (
	PhaseCreated          Phase = "created"
	PhasePreparing        Phase = "preparing"
	PhaseSampling         Phase = "sampling"
	PhaseExecutingTools   Phase = "executing_tools"
	PhaseAwaitingApproval Phase = "awaiting_approval"
	PhaseAwaitingInput    Phase = "awaiting_input"
	PhaseVerifying        Phase = "verifying"
	PhaseCommitting       Phase = "committing"
	PhaseCompleted        Phase = "completed"
	PhaseFailed           Phase = "failed"
	PhaseCanceled         Phase = "canceled"
)

func (p Phase) Terminal() bool {
	return p == PhaseCompleted || p == PhaseFailed || p == PhaseCanceled
}

type VerificationStatus string

const (
	VerificationNotEvaluated VerificationStatus = "not_evaluated"
	VerificationPassed       VerificationStatus = "passed"
	VerificationFailed       VerificationStatus = "failed"
	VerificationUnavailable  VerificationStatus = "unavailable"
)

type JournalStatus string

const (
	JournalNone       JournalStatus = "none"
	JournalOpen       JournalStatus = "open"
	JournalSuspended  JournalStatus = "suspended"
	JournalCommitted  JournalStatus = "committed"
	JournalRolledBack JournalStatus = "rolled_back"
)

type SampleStatus string

const (
	SampleRequested SampleStatus = "requested"
	SampleRunning   SampleStatus = "running"
	SampleCompleted SampleStatus = "completed"
	SampleFailed    SampleStatus = "failed"
)

type EffectLifecycleStatus string

const (
	EffectRequested EffectLifecycleStatus = "requested"
	EffectRunning   EffectLifecycleStatus = "running"
	EffectSucceeded EffectLifecycleStatus = "succeeded"
	EffectFailed    EffectLifecycleStatus = "failed"
)

type TerminalKind string

const (
	TerminalCompleted TerminalKind = "completed"
	TerminalFailed    TerminalKind = "failed"
	TerminalCanceled  TerminalKind = "canceled"
)

type RepairKind string

const (
	RepairCompletion   RepairKind = "completion"
	RepairWorkspace    RepairKind = "workspace"
	RepairDeclaration  RepairKind = "declaration"
	RepairVerification RepairKind = "verification"
)

type StepAction string

const (
	StepActionNone              StepAction = ""
	StepActionRepairToolFailure StepAction = "repair_tool_failure"
	StepActionRepairCompletion  StepAction = "repair_completion"
	StepActionRepairWorkspace   StepAction = "repair_workspace"
	StepActionRepairDeclaration StepAction = "repair_declaration"
	StepActionVerify            StepAction = "verify"
	StepActionFinalize          StepAction = "finalize"
	StepActionBlock             StepAction = "block"
	StepActionComplete          StepAction = "complete"
)

type ConvergenceCause string

const (
	ConvergenceOutputLimit  ConvergenceCause = "output_limit"
	ConvergenceNoProgress   ConvergenceCause = "no_progress"
	ConvergenceRepairBudget ConvergenceCause = "repair_budget"
	ConvergenceStepLimit    ConvergenceCause = "step_limit"
	ConvergenceIncomplete   ConvergenceCause = "declared_incomplete"
)

type VerificationAction string

const (
	VerificationActionPassed   VerificationAction = "passed"
	VerificationActionRepair   VerificationAction = "repair"
	VerificationActionReported VerificationAction = "reported"
	VerificationActionBlocked  VerificationAction = "blocked"
	VerificationActionFailed   VerificationAction = "failed"
	VerificationActionReverted VerificationAction = "reverted"
)

type RepairBudget struct {
	ProgressKey string `json:"progress_key"`
	Consecutive uint32 `json:"consecutive"`
	Steps       uint32 `json:"steps"`
}

type ProgressStage string

const (
	ProgressStageNone       ProgressStage = ""
	ProgressStageConverge   ProgressStage = "converge"
	ProgressStageFinishOnly ProgressStage = "finish_only"
	ProgressStageExhausted  ProgressStage = "exhausted"
)

type ProgressStallKind string

const (
	ProgressStallNone          ProgressStallKind = ""
	ProgressStallIdenticalCall ProgressStallKind = "identical_call"
	ProgressStallCycle         ProgressStallKind = "cycle"
)

type ProgressState struct {
	Signature            string            `json:"signature,omitempty"`
	SampleIdentity       string            `json:"sample_identity,omitempty"`
	PendingIdentity      string            `json:"pending_identity,omitempty"`
	ObservationKey       string            `json:"observation_key,omitempty"`
	SeenObservations     *ObservationSet   `json:"seen_observations,omitempty"`
	PendingResultDigests []string          `json:"pending_result_digests,omitempty"`
	ObservedSamples      uint32            `json:"observed_samples"`
	NoProgressSamples    uint32            `json:"no_progress_samples"`
	Stage                ProgressStage     `json:"stage,omitempty"`
	StallKind            ProgressStallKind `json:"stall_kind,omitempty"`
}

type ConvergenceState struct {
	Cause                 ConvergenceCause `json:"cause"`
	Used                  uint32           `json:"used"`
	Limit                 uint32           `json:"limit"`
	RepairKind            RepairKind       `json:"repair_kind,omitempty"`
	FinalizationAttempted bool             `json:"finalization_attempted,omitempty"`
	Summary               string           `json:"summary,omitempty"`
	PendingActions        []string         `json:"pending_actions,omitempty"`
}

type ConvergencePolicy struct {
	ProgressConverge   uint32 `json:"progress_converge"`
	ProgressFinishOnly uint32 `json:"progress_finish_only"`
	ProgressLimit      uint32 `json:"progress_limit"`
	ResearchConverge   uint32 `json:"research_converge"`
	ResearchFinishOnly uint32 `json:"research_finish_only"`
	ResearchLimit      uint32 `json:"research_limit"`
}

type Policy struct {
	CompletionRequired         bool   `json:"completion_required"`
	StructuredTerminalRequired bool   `json:"structured_terminal_required"`
	VerificationRequired       bool   `json:"verification_required"`
	VerificationMustPass       bool   `json:"verification_must_pass"`
	VerificationMode           string `json:"verification_mode,omitempty"`
	VerificationOnFailure      string `json:"verification_on_failure,omitempty"`
	CompletionRepairLimit      uint32 `json:"completion_repair_limit"`
	WorkspaceRepairLimit       uint32 `json:"workspace_repair_limit"`
	DeclarationRepairLimit     uint32 `json:"declaration_repair_limit"`
	VerificationRepairLimit    uint32 `json:"verification_repair_limit"`
	// ExecutionStepLimit is an explicit no-progress lease. Structured progress
	// renews it; zero leaves execution to context and token/cost budgets.
	ExecutionStepLimit uint32            `json:"execution_step_limit,omitempty"`
	JournalRequired    bool              `json:"journal_required"`
	Convergence        ConvergencePolicy `json:"convergence"`
	// ImplementNoProgressSamples is the public finish-only lease for
	// consecutive Samples that repeat the same work-state observation
	// (workspace content, Work Item signature, and tool-result digest)
	// with the same tool-call identity. Zero inherits the MaxSteps-derived
	// 2/3 finish-only lease. A new observation key renews both clocks.
	// Returning to a seen key with a different identity consumes the
	// MaxSteps-derived lease instead.
	ImplementNoProgressSamples uint32 `json:"implement_no_progress_samples,omitempty"`
}

func ConvergencePolicyForStepLimit(limit uint32) ConvergencePolicy {
	if limit == 0 {
		return ConvergencePolicy{}
	}
	converge := max(uint32(1), limit/3)
	finishOnly := max(converge, uint32(uint64(limit)*2/3))
	return ConvergencePolicy{
		ProgressConverge: converge, ProgressFinishOnly: finishOnly,
		ProgressLimit: limit, ResearchConverge: converge,
		ResearchFinishOnly: finishOnly, ResearchLimit: limit,
	}
}

func DefaultPolicy() Policy {
	return Policy{
		CompletionRequired:      true,
		VerificationRequired:    true,
		VerificationMustPass:    true,
		VerificationMode:        "hard",
		VerificationOnFailure:   "fail",
		CompletionRepairLimit:   2,
		WorkspaceRepairLimit:    1,
		DeclarationRepairLimit:  1,
		VerificationRepairLimit: 1,
	}
}

type ToolCallState struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Arguments         string `json:"arguments,omitempty"`
	CatalogID         string `json:"catalog_id,omitempty"`
	CatalogGeneration uint64 `json:"catalog_generation,omitempty"`
	CatalogRevision   uint64 `json:"catalog_revision,omitempty"`
	CatalogAuthority  uint64 `json:"catalog_authority,omitempty"`
}

type ToolResultState struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	IsError bool   `json:"is_error"`
}

type ObservedChange struct {
	Path          string `json:"path"`
	Kind          string `json:"kind"`
	ContentDigest string `json:"content_digest,omitempty"`
}

type ApprovalState struct {
	RequestID string `json:"request_id"`
	CallID    string `json:"call_id"`
}

type InputState struct {
	RequestID string `json:"request_id"`
}

type CompletionDecision struct {
	Accepted          bool     `json:"accepted"`
	Summary           string   `json:"summary,omitempty"`
	Reason            string   `json:"reason,omitempty"`
	RequiredAction    string   `json:"required_action,omitempty"`
	OutputMode        string   `json:"output_mode,omitempty"`
	PendingActions    []string `json:"pending_actions,omitempty"`
	Mutation          uint64   `json:"mutation_revision"`
	ChangedPaths      []string `json:"changed_paths,omitempty"`
	VerificationCalls []string `json:"verification_call_ids,omitempty"`
	CompletionCall    string   `json:"completion_call_id,omitempty"`
}

type VerificationState struct {
	Status         VerificationStatus `json:"status"`
	Action         VerificationAction `json:"action,omitempty"`
	Mutation       uint64             `json:"mutation_revision"`
	EvidenceCalls  []string           `json:"evidence_call_ids,omitempty"`
	FailureMessage string             `json:"failure_message,omitempty"`
}

type TerminalDecision struct {
	Kind        TerminalKind            `json:"kind"`
	Code        string                  `json:"code,omitempty"`
	Message     string                  `json:"message,omitempty"`
	Fault       *protocol.FaultMetadata `json:"fault,omitempty"`
	Convergence *ConvergenceState       `json:"convergence,omitempty"`
}

type UsageState struct {
	Calls           uint32 `json:"calls,omitempty"`
	InputTokens     uint64 `json:"input_tokens"`
	OutputTokens    uint64 `json:"output_tokens"`
	ReasoningTokens uint64 `json:"reasoning_tokens,omitempty"`
	CachedTokens    uint64 `json:"cached_tokens,omitempty"`
	CostMicrounits  uint64 `json:"cost_microunits,omitempty"`
	CostKnown       bool   `json:"cost_known"`
	Frozen          bool   `json:"frozen"`
}

type ContextState struct {
	Digest        string `json:"digest,omitempty"`
	HistoryBytes  int    `json:"history_bytes,omitempty"`
	MaxBytes      int    `json:"max_bytes,omitempty"`
	Compactions   uint32 `json:"compactions,omitempty"`
	Frozen        bool   `json:"frozen"`
	Truncated     bool   `json:"truncated,omitempty"`
	TruncateCause string `json:"truncate_cause,omitempty"`
}

type ModelSampleState struct {
	ID              string                             `json:"id"`
	Attempt         uint32                             `json:"attempt"`
	Status          SampleStatus                       `json:"status"`
	ProviderRetries uint32                             `json:"provider_retries,omitempty"`
	LastFailure     *provider.Failure                  `json:"last_failure,omitempty"`
	Retry           *ProviderRetryState                `json:"retry,omitempty"`
	Assembly        *providerassembly.ResponseAssembly `json:"assembly,omitempty"`
	Error           string                             `json:"error,omitempty"`
}

type Commentary struct {
	SampleID string   `json:"sample_id"`
	Text     string   `json:"text"`
	CallIDs  []string `json:"call_ids"`
}

type ProviderRetryState struct {
	EffectID         string    `json:"effect_id"`
	Attempt          uint32    `json:"attempt"`
	Retry            uint32    `json:"retry"`
	EffectiveDelayMS uint64    `json:"effective_delay_ms"`
	RetryAt          time.Time `json:"retry_at"`
	PolicyRevision   string    `json:"policy_revision"`
}

type CancellationState struct {
	Requested bool   `json:"requested"`
	Accepted  bool   `json:"accepted"`
	Reason    string `json:"reason,omitempty"`
}

type RecoveryRelation struct {
	SourceTurnID   string `json:"source_turn_id"`
	RecoveryTurnID string `json:"recovery_turn_id"`
	Action         string `json:"action"`
	DraftResumed   bool   `json:"draft_resumed,omitempty"`
}

type State struct {
	Phase                 Phase                       `json:"phase"`
	Intent                protocol.TurnIntent         `json:"intent"`
	Mode                  string                      `json:"mode"`
	ProfileRevision       uint64                      `json:"profile_revision"`
	Policy                Policy                      `json:"policy"`
	MutationRevision      uint64                      `json:"mutation_revision"`
	OpenCalls             map[string]ToolCallState    `json:"open_calls"`
	ClosedCalls           map[string]ToolResultState  `json:"closed_calls"`
	PendingApprovals      map[string]ApprovalState    `json:"pending_approvals"`
	PendingInput          *InputState                 `json:"pending_input,omitempty"`
	Changes               []ObservedChange            `json:"changes,omitempty"`
	Completion            *CompletionDecision         `json:"completion,omitempty"`
	Continuation          *ContinuationCursor         `json:"continuation,omitempty"`
	Verification          VerificationState           `json:"verification"`
	Journal               JournalStatus               `json:"journal"`
	Usage                 UsageState                  `json:"usage"`
	Context               ContextState                `json:"context"`
	SampleLedger          map[string]ModelSampleState `json:"sample_ledger"`
	ActiveSampleID        string                      `json:"active_sample_id,omitempty"`
	Cancellation          CancellationState           `json:"cancellation"`
	RecoveryRelation      *RecoveryRelation           `json:"recovery_relation,omitempty"`
	PendingEffects        map[string]Effect           `json:"pending_effects"`
	CompletedEffects      map[string]Effect           `json:"completed_effects"`
	NextEffectSequence    uint64                      `json:"next_effect_sequence"`
	ProvisionalOutput     []string                    `json:"provisional_output,omitempty"`
	FinalOutput           []string                    `json:"final_output,omitempty"`
	Commentary            []Commentary                `json:"commentary,omitempty"`
	OutputEligibility     bool                        `json:"output_eligibility"`
	RepairBudgets         map[RepairKind]RepairBudget `json:"repair_budgets"`
	Progress              ProgressState               `json:"progress"`
	WorkItem              WorkItem                    `json:"work_item"`
	Convergence           *ConvergenceState           `json:"convergence,omitempty"`
	NextAction            StepAction                  `json:"next_action,omitempty"`
	LastModelContinued    bool                        `json:"last_model_continued,omitempty"`
	UnresolvedToolFailure bool                        `json:"unresolved_tool_failure,omitempty"`
	RecoveryToolSucceeded bool                        `json:"recovery_tool_succeeded,omitempty"`
	PendingTerminal       *TerminalDecision           `json:"pending_terminal,omitempty"`
	Terminal              *TerminalDecision           `json:"terminal,omitempty"`
}

// ContinuationCursor references the newest durable conversation snapshot
// accepted for the running Turn. The conversation content lives in the
// context CAS; the cursor itself commits as a kernel fact, so a restored
// kernel can never claim conversation content that was never stored.
type ContinuationCursor struct {
	Sequence uint64 `json:"sequence"`
	Handle   string `json:"handle"`
	Digest   string `json:"digest"`
}

func NewState(intent protocol.TurnIntent, mode string, profileRevision uint64) State {
	return State{
		Phase:            PhaseCreated,
		Intent:           protocol.NormalizeTurnIntent(intent),
		Mode:             mode,
		ProfileRevision:  profileRevision,
		Policy:           DefaultPolicy(),
		OpenCalls:        make(map[string]ToolCallState),
		ClosedCalls:      make(map[string]ToolResultState),
		PendingApprovals: make(map[string]ApprovalState),
		SampleLedger:     make(map[string]ModelSampleState),
		PendingEffects:   make(map[string]Effect),
		CompletedEffects: make(map[string]Effect),
		RepairBudgets:    make(map[RepairKind]RepairBudget),
		Verification: VerificationState{
			Status: VerificationNotEvaluated,
		},
		Journal: JournalNone,
	}
}

func NewStateWithPolicy(
	intent protocol.TurnIntent,
	mode string,
	profileRevision uint64,
	policy Policy,
) State {
	state := NewState(intent, mode, profileRevision)
	state.Policy = policy
	return state
}

func RequiresCompletion(state State) bool {
	// Structured terminal is opt-in. Tool use and mutations stop when the
	// model writes a no-tool answer; turn_complete is optional.
	return state.Policy.CompletionRequired &&
		state.Policy.StructuredTerminalRequired
}

func IsResearchIntent(intent protocol.TurnIntent) bool {
	return intent == protocol.TurnIntentAnswer ||
		intent == protocol.TurnIntentPlan
}
