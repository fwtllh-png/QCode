package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/observability/diagnostics"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// ToolRef is the authority-frozen identity of one executable catalog entry.
// Authority is Registry-private and intentionally omitted from serialized
// receipts.
type ToolRef struct {
	Name       string `json:"name"`
	Source     string `json:"source"`
	CatalogID  string `json:"catalog_id"`
	Generation uint64 `json:"generation"`
	Revision   uint64 `json:"revision"`
	Authority  uint64 `json:"-"`
}

func (r ToolRef) Validate() error {
	if strings.TrimSpace(r.Name) == "" ||
		strings.TrimSpace(r.Source) == "" ||
		strings.TrimSpace(r.CatalogID) == "" {
		return errors.New("tool reference identity is incomplete")
	}
	if r.Generation == 0 || r.Revision == 0 || r.Authority == 0 {
		return errors.New("tool reference authority is incomplete")
	}
	return nil
}

func (r ToolRef) Binding() CatalogBinding {
	return CatalogBinding{
		CatalogID: r.CatalogID, Generation: r.Generation,
		Revision: r.Revision, Authority: r.Authority,
	}
}

type InvocationSource string

type resultTokenBudgetKey struct{}

func WithResultTokenBudget(ctx context.Context, tokens uint64) context.Context {
	return context.WithValue(ctx, resultTokenBudgetKey{}, tokens)
}

func ResultTokenBudget(ctx context.Context) uint64 {
	tokens, _ := ctx.Value(resultTokenBudgetKey{}).(uint64)
	return tokens
}

type resultBatchBudgetKey struct{}

// WithResultBatchBudget records the aggregate admission pool shared by the
// results of one tool batch. Producers keep reading ResultTokenBudget for the
// per-result ceiling; only batch admission consumes this total.
func WithResultBatchBudget(ctx context.Context, tokens uint64) context.Context {
	return context.WithValue(ctx, resultBatchBudgetKey{}, tokens)
}

func ResultBatchBudget(ctx context.Context) uint64 {
	tokens, _ := ctx.Value(resultBatchBudgetKey{}).(uint64)
	return tokens
}

const (
	InvocationSourceUnknown  InvocationSource = "unknown"
	InvocationSourceModel    InvocationSource = "model"
	InvocationSourceHost     InvocationSource = "host"
	InvocationSourceReplay   InvocationSource = "replay"
	InvocationSourceInternal InvocationSource = "internal"
)

type ExecutionDisposition string

const (
	DispositionAbortImmediately ExecutionDisposition = "abort_immediately"
	DispositionWaitForTeardown  ExecutionDisposition = "wait_for_teardown"
	DispositionDetached         ExecutionDisposition = "detached"
)

func (d ExecutionDisposition) Valid() bool {
	switch d {
	case DispositionAbortImmediately, DispositionWaitForTeardown, DispositionDetached:
		return true
	default:
		return false
	}
}

// DispositionProvider lets typed and lifecycle-aware executors declare how
// Runtime cancellation must treat their work.
type DispositionProvider interface {
	ExecutionDisposition() ExecutionDisposition
}

func DispositionFor(executor Executor) ExecutionDisposition {
	if provider, ok := executor.(DispositionProvider); ok {
		if disposition := provider.ExecutionDisposition(); disposition.Valid() {
			return disposition
		}
	}
	return DispositionWaitForTeardown
}

// PreparedInvocation is immutable after Guard preparation. Replacement
// arguments always produce a new value and repeat policy authorization.
type PreparedInvocation struct {
	Identity    InvocationIdentity   `json:"identity"`
	CallID      string               `json:"call_id"`
	Tool        string               `json:"tool"`
	Ref         ToolRef              `json:"tool_ref"`
	Arguments   json.RawMessage      `json:"arguments"`
	Resources   []Resource           `json:"resources"`
	Descriptor  Descriptor           `json:"descriptor"`
	Binding     TrustedBinding       `json:"trusted_binding"`
	Source      InvocationSource     `json:"source"`
	Disposition ExecutionDisposition `json:"disposition"`
}

type OutcomeStatus string

const (
	OutcomeSucceeded OutcomeStatus = "succeeded"
	OutcomeFailed    OutcomeStatus = "failed"
	OutcomeRejected  OutcomeStatus = "rejected"
	OutcomeCanceled  OutcomeStatus = "canceled"
)

type TerminalOwner string

const (
	TerminalOwnerGuard    TerminalOwner = "guard"
	TerminalOwnerExecutor TerminalOwner = "executor"
)

type NetworkTarget struct {
	Host     string `json:"host"`
	Protocol string `json:"protocol"`
}

// SecuritySignal carries policy-relevant output outside arbitrary Metadata.
type SecuritySignal struct {
	SandboxDenied *sandbox.Denial `json:"sandbox_denied,omitempty"`
	EgressDenied  *NetworkTarget  `json:"egress_denied,omitempty"`
}

// Outcome is the typed, non-model execution projection. Result remains the
// model projection while hooks and telemetry can consume these explicit facts.
type Outcome struct {
	Status    OutcomeStatus   `json:"status"`
	Security  *SecuritySignal `json:"security,omitempty"`
	Facts     *OutcomeFacts   `json:"facts,omitempty"`
	Hook      any             `json:"hook,omitempty"`
	Telemetry map[string]any  `json:"telemetry,omitempty"`
}

type OutcomeFacts struct {
	WorkspaceRead    *WorkspaceReadFact     `json:"workspace_read,omitempty"`
	WorkspaceChanges []WorkspaceChange      `json:"workspace_changes,omitempty"`
	Diagnostics      []diagnostics.Receipt  `json:"diagnostics,omitempty"`
	Evidence         []EvidenceHit          `json:"evidence,omitempty"`
	Verification     *verify.Evidence       `json:"verification,omitempty"`
	Completion       *CompletionDeclaration `json:"completion,omitempty"`
	Failure          *FailureFact           `json:"failure,omitempty"`
	ProcessSession   *ProcessSessionFact    `json:"process_session,omitempty"`
	ResultHandle     string                 `json:"result_handle,omitempty"`
}

type WorkspaceReadFact struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

const (
	WorkspaceCreated  = "created"
	WorkspaceModified = "modified"
	WorkspaceDeleted  = "deleted"
)

type WorkspaceChange struct {
	Path        string `json:"path"`
	Kind        string `json:"kind"`
	Added       int    `json:"added,omitempty"`
	Removed     int    `json:"removed,omitempty"`
	Summary     string `json:"summary,omitempty"`
	AfterDigest string `json:"after_digest,omitempty"`
}

type FailureFact struct {
	Category string `json:"category,omitempty"`
}

type ProcessSessionFact struct {
	SessionID       string `json:"session_id,omitempty"`
	SourceSessionID string `json:"source_session_id,omitempty"`
	Terminated      bool   `json:"terminated,omitempty"`
	Cursor          uint64 `json:"cursor"`
	Running         bool   `json:"running"`
	ExitCode        int    `json:"exit_code"`
	TimedOut        bool   `json:"timed_out"`
	TTY             bool   `json:"tty"`
	Archived        bool   `json:"archived,omitempty"`
	PendingBytes    int    `json:"pending_bytes,omitempty"`
	OmittedBytes    int    `json:"omitted_bytes,omitempty"`
}

func OutcomeFromResult(result Result) Outcome {
	status := OutcomeSucceeded
	if result.IsError {
		status = OutcomeFailed
	}
	outcome := Outcome{Status: status}
	facts := factsFromResult(result)
	if facts != nil {
		outcome.Facts = facts
	}
	return outcome
}

func EnsureOutcomeFacts(result *Result) *OutcomeFacts {
	if result.Outcome == nil {
		outcome := OutcomeFromResult(*result)
		result.Outcome = &outcome
	}
	if result.Outcome.Facts == nil {
		result.Outcome.Facts = &OutcomeFacts{}
	}
	return result.Outcome.Facts
}

func factsFromResult(result Result) *OutcomeFacts {
	facts := &OutcomeFacts{ResultHandle: result.Handle}
	if result.Metadata != nil {
		if value, ok := result.Metadata[MetadataEvidence].([]EvidenceHit); ok {
			facts.Evidence = append([]EvidenceHit(nil), value...)
		}
		if value, ok := result.Metadata["diagnostics"].([]diagnostics.Receipt); ok {
			facts.Diagnostics = append([]diagnostics.Receipt(nil), value...)
		}
		if value, ok := result.Metadata[MetadataCompletionDeclaration].(CompletionDeclaration); ok {
			copy := value
			facts.Completion = &copy
		}
		if value, ok := result.Metadata[verify.EvidenceMetadataKey].(verify.Evidence); ok {
			copy := value
			facts.Verification = &copy
		}
		if value, ok := result.Metadata["error_category"].(string); ok {
			facts.Failure = &FailureFact{Category: value}
		}
		if facts.ResultHandle == "" {
			facts.ResultHandle, _ = result.Metadata["handle"].(string)
		}
	}
	if facts.ResultHandle == "" && len(facts.Evidence) == 0 &&
		len(facts.Diagnostics) == 0 && facts.Completion == nil &&
		facts.Verification == nil && facts.Failure == nil {
		return nil
	}
	return facts
}

func CloneOutcome(source *Outcome) *Outcome {
	if source == nil {
		return nil
	}
	cloned := *source
	cloned.Telemetry = maps.Clone(source.Telemetry)
	if source.Facts != nil {
		facts := *source.Facts
		facts.WorkspaceChanges = append(
			[]WorkspaceChange(nil),
			source.Facts.WorkspaceChanges...,
		)
		facts.Diagnostics = append(
			[]diagnostics.Receipt(nil),
			source.Facts.Diagnostics...,
		)
		facts.Evidence = append([]EvidenceHit(nil), source.Facts.Evidence...)
		if source.Facts.WorkspaceRead != nil {
			read := *source.Facts.WorkspaceRead
			facts.WorkspaceRead = &read
		}
		if source.Facts.Verification != nil {
			verification := *source.Facts.Verification
			verification.CoveredPaths = append(
				[]string(nil),
				source.Facts.Verification.CoveredPaths...,
			)
			facts.Verification = &verification
		}
		if source.Facts.Completion != nil {
			completion := *source.Facts.Completion
			completion.ChangedPaths = append(
				[]string(nil),
				source.Facts.Completion.ChangedPaths...,
			)
			completion.VerificationCallIDs = append(
				[]string(nil),
				source.Facts.Completion.VerificationCallIDs...,
			)
			completion.PendingActions = append(
				[]string(nil),
				source.Facts.Completion.PendingActions...,
			)
			facts.Completion = &completion
		}
		if source.Facts.Failure != nil {
			failure := *source.Facts.Failure
			facts.Failure = &failure
		}
		if source.Facts.ProcessSession != nil {
			session := *source.Facts.ProcessSession
			facts.ProcessSession = &session
		}
		cloned.Facts = &facts
	}
	if source.Security != nil {
		security := *source.Security
		if source.Security.EgressDenied != nil {
			egress := *source.Security.EgressDenied
			security.EgressDenied = &egress
		}
		if source.Security.SandboxDenied != nil {
			denial := *source.Security.SandboxDenied
			security.SandboxDenied = &denial
		}
		cloned.Security = &security
	}
	return &cloned
}

type PermissionProvenance struct {
	Kind     string `json:"kind"`
	Value    string `json:"value"`
	Digest   string `json:"digest,omitempty"`
	Revision uint64 `json:"revision,omitempty"`
}

type PermissionAmendmentReceipt struct {
	BasePermissionDigest    string     `json:"base_permission_digest"`
	Kind                    string     `json:"kind"`
	Resource                string     `json:"resource"`
	Protocol                string     `json:"protocol,omitempty"`
	Port                    uint16     `json:"port,omitempty"`
	Capability              Capability `json:"capability,omitempty"`
	Decision                string     `json:"decision"`
	AmendedPermissionDigest string     `json:"amended_permission_digest,omitempty"`
}

type AttemptReceipt struct {
	Sequence                uint32                      `json:"sequence"`
	Sandbox                 string                      `json:"sandbox"`
	Status                  OutcomeStatus               `json:"status"`
	TerminalOwner           TerminalOwner               `json:"terminal_owner"`
	Reason                  string                      `json:"reason,omitempty"`
	OperationSchemaVersion  int                         `json:"operation_schema_version,omitempty"`
	OperationDigest         string                      `json:"operation_digest,omitempty"`
	LeaseID                 string                      `json:"lease_id,omitempty"`
	LeaseState              string                      `json:"lease_state,omitempty"`
	LeaseAttempt            uint64                      `json:"lease_attempt,omitempty"`
	WorkspaceID             string                      `json:"workspace_id,omitempty"`
	WorkspaceGeneration     uint64                      `json:"workspace_generation,omitempty"`
	SubjectKind             string                      `json:"subject_kind,omitempty"`
	SubjectID               string                      `json:"subject_id,omitempty"`
	SubjectDigest           string                      `json:"subject_digest,omitempty"`
	SubjectGeneration       uint64                      `json:"subject_generation,omitempty"`
	PolicyRevision          uint64                      `json:"policy_revision,omitempty"`
	SandboxPolicyID         string                      `json:"sandbox_policy_id,omitempty"`
	EffectKind              string                      `json:"effect_kind,omitempty"`
	EffectRisk              string                      `json:"effect_risk,omitempty"`
	EffectReversibility     string                      `json:"effect_reversibility,omitempty"`
	WorkspaceTransaction    string                      `json:"workspace_transaction,omitempty"`
	PermissionSchemaVersion int                         `json:"permission_schema_version,omitempty"`
	PermissionRevision      uint64                      `json:"permission_revision,omitempty"`
	PermissionDigest        string                      `json:"permission_digest,omitempty"`
	PermissionCapability    Capability                  `json:"permission_capability,omitempty"`
	PermissionAccess        AccessMode                  `json:"permission_access,omitempty"`
	Enforcement             string                      `json:"enforcement,omitempty"`
	Backend                 string                      `json:"backend,omitempty"`
	EffectiveControls       controlmatrix.Matrix        `json:"effective_controls"`
	WorkspaceRoot           string                      `json:"workspace_root,omitempty"`
	ReadRoots               []string                    `json:"read_roots,omitempty"`
	WritePaths              []string                    `json:"write_paths,omitempty"`
	DeniedWriteRoots        []string                    `json:"denied_write_roots,omitempty"`
	WorkspaceBaseWrite      bool                        `json:"workspace_base_write,omitempty"`
	NetworkMode             string                      `json:"network_mode,omitempty"`
	NetworkTargets          []string                    `json:"network_targets,omitempty"`
	ManagedProxyPort        uint16                      `json:"managed_proxy_port,omitempty"`
	LoopbackAllowed         bool                        `json:"loopback_allowed,omitempty"`
	ProcessAllowed          bool                        `json:"process_allowed,omitempty"`
	Provenance              []PermissionProvenance      `json:"provenance,omitempty"`
	Denial                  *sandbox.Denial             `json:"denial,omitempty"`
	Amendment               *PermissionAmendmentReceipt `json:"amendment,omitempty"`
	StartedAt               time.Time                   `json:"started_at"`
	CompletedAt             time.Time                   `json:"completed_at"`
	DurationMS              int64                       `json:"duration_ms"`
	Teardown                time.Duration               `json:"teardown,omitempty"`
	TeardownMS              int64                       `json:"teardown_ms,omitempty"`
	TeardownTimedOut        bool                        `json:"teardown_timed_out,omitempty"`
}

type ExecutionReceipt struct {
	Tool                           ToolRef              `json:"tool"`
	Source                         InvocationSource     `json:"source"`
	Disposition                    ExecutionDisposition `json:"disposition"`
	VerificationEvidenceAuthorized bool                 `json:"verification_evidence_authorized,omitempty"`
	Attempts                       []AttemptReceipt     `json:"attempts"`
	ApprovalWait                   time.Duration        `json:"approval_wait,omitempty"`
	DispatchWait                   time.Duration        `json:"dispatch_wait,omitempty"`
	ClaimWait                      time.Duration        `json:"claim_wait,omitempty"`
	TerminalStatus                 OutcomeStatus        `json:"terminal_status"`
	TerminalOwner                  TerminalOwner        `json:"terminal_owner"`
	Teardown                       time.Duration        `json:"teardown,omitempty"`
	TeardownMS                     int64                `json:"teardown_ms,omitempty"`
	TeardownTimedOut               bool                 `json:"teardown_timed_out,omitempty"`
}

func CloneExecutionReceipt(source *ExecutionReceipt) *ExecutionReceipt {
	if source == nil {
		return nil
	}
	cloned := *source
	cloned.Attempts = make([]AttemptReceipt, len(source.Attempts))
	for index := range source.Attempts {
		cloned.Attempts[index] = cloneAttemptReceipt(source.Attempts[index])
	}
	return &cloned
}

func cloneAttemptReceipt(source AttemptReceipt) AttemptReceipt {
	cloned := source
	cloned.ReadRoots = append([]string(nil), source.ReadRoots...)
	cloned.WritePaths = append([]string(nil), source.WritePaths...)
	cloned.DeniedWriteRoots = append([]string(nil), source.DeniedWriteRoots...)
	cloned.NetworkTargets = append([]string(nil), source.NetworkTargets...)
	cloned.Provenance = append([]PermissionProvenance(nil), source.Provenance...)
	if source.Denial != nil {
		denial := *source.Denial
		cloned.Denial = &denial
	}
	if source.Amendment != nil {
		amendment := *source.Amendment
		cloned.Amendment = &amendment
	}
	return cloned
}

// OutcomeExecutor is the authoritative built-in execution boundary. Dynamic
// ecosystem executors may still be projected at Registry ingress.
type OutcomeExecutor interface {
	Executor
	ExecuteOutcome(context.Context, json.RawMessage) (Result, Outcome, error)
}

// ExecuteWithOutcome is the compatibility boundary for executors whose domain
// logic still implements Execute directly. New built-ins should use typed.Contract.
func ExecuteWithOutcome(
	ctx context.Context,
	executor Executor,
	raw json.RawMessage,
) (result Result, outcome Outcome, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = Result{}
			outcome = Outcome{Status: OutcomeFailed}
			err = fmt.Errorf("tool %q panicked: %v", executor.Descriptor().Name, recovered)
		}
	}()
	if err := ctx.Err(); err != nil {
		return Result{}, Outcome{Status: OutcomeCanceled}, err
	}
	result, err = executor.Execute(ctx, raw)
	if result.Outcome != nil {
		outcome = *CloneOutcome(result.Outcome)
	} else {
		outcome = OutcomeFromResult(result)
	}
	if err != nil {
		outcome.Status = OutcomeFailed
		if errors.Is(err, context.Canceled) {
			outcome.Status = OutcomeCanceled
		}
	}
	result.Outcome = CloneOutcome(&outcome)
	return result, outcome, err
}

type invocationSourceKey struct{}

func WithInvocationSource(ctx context.Context, source InvocationSource) context.Context {
	if !validInvocationSource(source) {
		source = InvocationSourceUnknown
	}
	return context.WithValue(ctx, invocationSourceKey{}, source)
}

func InvocationSourceFrom(ctx context.Context) InvocationSource {
	if ctx == nil {
		return InvocationSourceUnknown
	}
	source, _ := ctx.Value(invocationSourceKey{}).(InvocationSource)
	if !validInvocationSource(source) {
		return InvocationSourceUnknown
	}
	return source
}

func validInvocationSource(source InvocationSource) bool {
	switch source {
	case InvocationSourceUnknown, InvocationSourceModel, InvocationSourceHost,
		InvocationSourceReplay, InvocationSourceInternal:
		return true
	default:
		return false
	}
}

type ExecutionAdmission func(
	context.Context,
	ParallelPolicy,
) (release func(), err error)

type executionAdmissionKey struct{}

func WithExecutionAdmission(ctx context.Context, admission ExecutionAdmission) context.Context {
	if admission == nil {
		return ctx
	}
	return context.WithValue(ctx, executionAdmissionKey{}, admission)
}

func AdmitExecution(
	ctx context.Context,
	policy ParallelPolicy,
) (release func(), err error) {
	if ctx == nil {
		return func() {}, nil
	}
	admission, _ := ctx.Value(executionAdmissionKey{}).(ExecutionAdmission)
	if admission == nil {
		return func() {}, nil
	}
	return admission(ctx, policy)
}

type TeardownReport struct {
	Duration time.Duration
	TimedOut bool
}

type teardownObserverKey struct{}

func WithTeardownObserver(
	ctx context.Context,
	observe func(TeardownReport),
) context.Context {
	if observe == nil {
		return ctx
	}
	return context.WithValue(ctx, teardownObserverKey{}, observe)
}

func ReportTeardown(ctx context.Context, report TeardownReport) {
	if ctx == nil {
		return
	}
	observe, _ := ctx.Value(teardownObserverKey{}).(func(TeardownReport))
	if observe != nil {
		observe(report)
	}
}
