package turnkernel

import (
	"encoding/json"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerassembly "github.com/fwtllh-png/QCode/internal/adapter/provider/assembly"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type Command interface {
	commandName() string
}

func CommandName(command Command) string {
	if command == nil {
		return "<nil>"
	}
	return command.commandName()
}

type TransitionRecord struct {
	Command     string `json:"command"`
	From        Phase  `json:"from"`
	To          Phase  `json:"to"`
	StateDigest string `json:"state_digest,omitempty"`
	Drift       string `json:"drift,omitempty"`
	Rejection   string `json:"rejection,omitempty"`
}

type StartTurn struct{}

func (StartTurn) commandName() string { return "start_turn" }

type PreparationFinished struct{}

func (PreparationFinished) commandName() string { return "preparation_finished" }

type ModelSampleRequested struct {
	SampleID string
}

func (ModelSampleRequested) commandName() string {
	return "model_sample_requested"
}

type ModelSampleStarted struct {
	SampleID string
	Attempt  uint32
}

func (ModelSampleStarted) commandName() string { return "model_sample_started" }

type ModelSampleFinished struct {
	SampleID string
	Usage    UsageState
	Context  ContextState
	Error    string
}

func (ModelSampleFinished) commandName() string { return "model_sample_finished" }

type ModelSampleProgressRecorded struct {
	EffectID string
	SampleID string
	Attempt  uint32
	Assembly providerassembly.ResponseAssembly
}

func (ModelSampleProgressRecorded) commandName() string {
	return "model_sample_progress_recorded"
}

type ModelSampleResultReceived struct {
	EffectID  string
	SampleID  string
	Usage     UsageState
	Context   ContextState
	Failure   *provider.Failure
	Text      string
	Calls     []ToolCallState
	Continued bool
	Error     string
}

func (ModelSampleResultReceived) commandName() string {
	return "model_sample_result_received"
}

type SupplementalUsageRecorded struct {
	Source   string
	SampleID string
	Usage    UsageState
}

func (SupplementalUsageRecorded) commandName() string {
	return "supplemental_usage_recorded"
}

type ProviderRetryRequested struct {
	EffectID         string
	SampleID         string
	Attempt          uint32
	Retry            uint32
	Failure          provider.Failure
	EffectiveDelayMS uint64
	RetryAt          time.Time
	PolicyRevision   string
}

func (ProviderRetryRequested) commandName() string { return "provider_retry_requested" }

type ModelTextReceived struct {
	Text string
}

func (ModelTextReceived) commandName() string { return "model_text_received" }

type ReleaseProvisionalOutput struct{}

func (ReleaseProvisionalOutput) commandName() string {
	return "release_provisional_output"
}

type DiscardProvisionalOutput struct {
	Reason string
}

func (DiscardProvisionalOutput) commandName() string {
	return "discard_provisional_output"
}

type RepairRequested struct {
	Kind        RepairKind
	ProgressKey string
	Limit       uint32
}

func (RepairRequested) commandName() string { return "repair_requested" }

type EvaluateTurnStep struct {
	ProgressKey string
}

func (EvaluateTurnStep) commandName() string { return "evaluate_turn_step" }

type ObserveProgress struct {
	Signature        string
	SampleIdentity   string
	ResultDigest     string
	CompletedSamples uint32
}

func (ObserveProgress) commandName() string { return "observe_progress" }

type BindWorkItem struct {
	Goal       string
	GoalDigest string
	KnownReads map[string]WorkItemRead
	KnownEdits map[string]WorkItemEdit
	Open       WorkItemOpen
}

func (BindWorkItem) commandName() string { return "bind_work_item" }

type ConvergenceRequested struct {
	Cause      ConvergenceCause
	Used       uint32
	Limit      uint32
	RepairKind RepairKind
}

func (ConvergenceRequested) commandName() string {
	return "convergence_requested"
}

type ConvergenceFinalizationStarted struct{}

func (ConvergenceFinalizationStarted) commandName() string {
	return "convergence_finalization_started"
}

type ToolCallsProposed struct {
	Calls []ToolCallState
}

func (ToolCallsProposed) commandName() string { return "tool_calls_proposed" }

type ApprovalRequired struct {
	RequestID string
	CallID    string
}

func (ApprovalRequired) commandName() string { return "approval_required" }

type ApprovalResolved struct {
	RequestID string
}

func (ApprovalResolved) commandName() string { return "approval_resolved" }

type ApprovalResultReceived struct {
	EffectID  string
	RequestID string
	Accepted  bool
	Canceled  bool
	Error     string
}

func (ApprovalResultReceived) commandName() string {
	return "approval_result_received"
}

type InputRequired struct {
	RequestID string
}

func (InputRequired) commandName() string { return "input_required" }

type InputResolved struct {
	RequestID string
}

func (InputResolved) commandName() string { return "input_resolved" }

type InputResultReceived struct {
	EffectID  string
	RequestID string
	Accepted  bool
	Error     string
}

func (InputResultReceived) commandName() string { return "input_result_received" }

type ToolResultReceived struct {
	EffectID    string
	CallID      string
	IsError     bool
	Changes     []ObservedChange
	Observation WorkItemObservation
}

func (ToolResultReceived) commandName() string { return "tool_result_received" }

type AbortOpenCalls struct {
	Reason string
}

func (AbortOpenCalls) commandName() string { return "abort_open_calls" }

type VerificationStarted struct{}

func (VerificationStarted) commandName() string { return "verification_started" }

type VerificationFinished struct {
	EffectID      string
	Status        VerificationStatus
	EvidenceCalls []string
	Message       string
	RepairKey     string
}

func (VerificationFinished) commandName() string { return "verification_finished" }

type CompletionCandidate struct {
	DeclarationValid  bool
	Status            string
	Summary           string
	OutputMode        string
	PendingActions    []string
	CompletionCall    string
	BatchMutated      bool
	BatchSize         int
	ToolError         bool
	VerificationCalls []string
	PlanOpenSteps     int
}

type CompletionEvaluated struct {
	Candidate CompletionCandidate
}

func (CompletionEvaluated) commandName() string { return "completion_evaluated" }

type CompletionInvalidated struct {
	Reason string
}

func (CompletionInvalidated) commandName() string {
	return "completion_invalidated"
}

type CancelRequested struct {
	Reason string
}

func (CancelRequested) commandName() string { return "cancel_requested" }

type ContinuationRecorded struct {
	Cursor ContinuationCursor
}

func (ContinuationRecorded) commandName() string {
	return "continuation_recorded"
}

type RecoveryRequested struct {
	SourceTurnID           string
	RecoveryTurnID         string
	CurrentProfileRevision uint64
	Action                 string
	DraftResumed           bool
	Changes                []ObservedChange
}

func (RecoveryRequested) commandName() string { return "recovery_requested" }

type EffectStarted struct {
	EffectID string
	Attempt  uint32
}

func (EffectStarted) commandName() string { return "effect_started" }

type EffectRequeued struct {
	EffectID string
}

func (EffectRequeued) commandName() string { return "effect_requeued" }

type EffectResultReceived struct {
	EffectID string
	Success  bool
	Error    string
}

func (EffectResultReceived) commandName() string {
	return "effect_result_received"
}

type PersistenceResultReceived struct {
	EffectID string
	Success  bool
	Error    string
}

func (PersistenceResultReceived) commandName() string {
	return "persistence_result_received"
}

type TerminalRequested struct {
	FailureCode    string
	FailureMessage string
	Fault          *protocol.FaultMetadata
	CancelReason   string
	Convergence    *ConvergenceState
}

func (TerminalRequested) commandName() string { return "terminal_requested" }

type JournalFinalized struct {
	Status JournalStatus
}

func (JournalFinalized) commandName() string { return "journal_finalized" }

type JournalResultReceived struct {
	EffectID string
	Status   JournalStatus
	Error    string
}

func (JournalResultReceived) commandName() string {
	return "journal_result_received"
}

type FinishTerminal struct{}

func (FinishTerminal) commandName() string { return "finish_terminal" }

type EventKind string

const (
	EventTransition        EventKind = "transition"
	EventToolStarted       EventKind = "tool_started"
	EventToolClosed        EventKind = "tool_closed"
	EventMutationObserved  EventKind = "mutation_observed"
	EventCompletionDecided EventKind = "completion_decided"
	EventVerification      EventKind = "verification_decided"
	EventOutputReleased    EventKind = "output_released"
	EventOutputDiscarded   EventKind = "output_discarded"
	EventRepairRequested   EventKind = "repair_requested"
	EventConvergence       EventKind = "convergence_requested"
	EventSampleStarted     EventKind = "sample_started"
	EventSampleFinished    EventKind = "sample_finished"
	EventProviderRetry     EventKind = "provider_retry_requested"
	EventUsageRecorded     EventKind = "usage_recorded"
	EventCancelAccepted    EventKind = "cancel_accepted"
	EventRecoveryBound     EventKind = "recovery_bound"
	EventEffectRequested   EventKind = "effect_requested"
	EventEffectStarted     EventKind = "effect_started"
	EventEffectRequeued    EventKind = "effect_requeued"
	EventEffectFinished    EventKind = "effect_finished"
	EventTerminalPrepared  EventKind = "terminal_prepared"
	EventTerminalCommitted EventKind = "terminal_committed"
)

type Event struct {
	Kind     EventKind         `json:"kind"`
	From     Phase             `json:"from,omitempty"`
	To       Phase             `json:"to,omitempty"`
	CallID   string            `json:"call_id,omitempty"`
	EffectID string            `json:"effect_id,omitempty"`
	SampleID string            `json:"sample_id,omitempty"`
	Mutation uint64            `json:"mutation_revision,omitempty"`
	Terminal *TerminalDecision `json:"terminal,omitempty"`
}

type EffectKind string

const (
	EffectSampleProvider  EffectKind = "sample_provider"
	EffectExecuteTool     EffectKind = "execute_tool"
	EffectAwaitApproval   EffectKind = "await_approval"
	EffectAwaitInput      EffectKind = "await_input"
	EffectRunVerification EffectKind = "run_verification"
	EffectCommitJournal   EffectKind = "commit_journal"
	EffectSuspendJournal  EffectKind = "suspend_journal"
	EffectRollbackJournal EffectKind = "rollback_journal"
)

type Effect struct {
	ID             string                `json:"effect_id"`
	Kind           EffectKind            `json:"kind"`
	Payload        json.RawMessage       `json:"payload"`
	PayloadDigest  string                `json:"payload_digest"`
	Attempt        uint32                `json:"attempt"`
	IdempotencyKey string                `json:"idempotency_key"`
	Status         EffectLifecycleStatus `json:"status"`
	CallID         string                `json:"call_id,omitempty"`
	Error          string                `json:"error,omitempty"`
}

type Transition struct {
	State   State
	Events  []Event
	Effects []Effect
}
