package fault

import (
	"context"
	"errors"
	"fmt"
)

type Code string

const Version = 1

const (
	InvalidArgument   Code = "invalid_argument"
	Conflict          Code = "conflict"
	ResourceExhausted Code = "resource_exhausted"
	Unavailable       Code = "unavailable"
	Canceled          Code = "canceled"
	DeadlineExceeded  Code = "deadline_exceeded"
	Internal          Code = "internal"
)

type Origin string

const (
	OriginRuntime      Origin = "runtime"
	OriginProvider     Origin = "provider"
	OriginTool         Origin = "tool"
	OriginVerification Origin = "verification"
	OriginPersistence  Origin = "persistence"
	OriginProjection   Origin = "projection"
	OriginKernel       Origin = "kernel"
)

type Stage string

const (
	StageAdmission       Stage = "admission"
	StageConnection      Stage = "connection"
	StageTLSHandshake    Stage = "tls_handshake"
	StageResponseHeaders Stage = "response_headers"
	StageStreamIdle      Stage = "stream_idle"
	StageModelSample     Stage = "model_sample"
	StageWorkflowNode    Stage = "workflow_node"
	StageWorkerAttempt   Stage = "worker_attempt"
	StageTurnLease       Stage = "turn_lease"
	StagePersistence     Stage = "persistence"
	StageProjection      Stage = "projection"
	StageTerminal        Stage = "terminal"
)

type RetryOwner string

const (
	RetryOwnerNone     RetryOwner = "none"
	RetryOwnerEngine   RetryOwner = "engine"
	RetryOwnerWorkflow RetryOwner = "workflow"
	RetryOwnerWorker   RetryOwner = "worker"
	RetryOwnerHost     RetryOwner = "host"
)

type ResumeHint string

const (
	ResumeNone       ResumeHint = "none"
	ResumeRetryStep  ResumeHint = "retry_step"
	ResumeRetryTurn  ResumeHint = "retry_turn"
	ResumeResumeTurn ResumeHint = "resume_turn"
	ResumeWait       ResumeHint = "wait"
	ResumeBlock      ResumeHint = "block"
	ResumeFail       ResumeHint = "fail"
	ResumeReject     ResumeHint = "reject"
)

type DeadlineScope string

const (
	DeadlineProviderConnection      DeadlineScope = "provider_connection"
	DeadlineProviderTLSHandshake    DeadlineScope = "provider_tls_handshake"
	DeadlineProviderResponseHeaders DeadlineScope = "provider_response_headers"
	DeadlineProviderStreamIdle      DeadlineScope = "provider_stream_idle"
	DeadlineWorkflowNode            DeadlineScope = "workflow_node"
	DeadlineWorkerLease             DeadlineScope = "worker_lease"
	DeadlineTurnLease               DeadlineScope = "turn_lease"
	DeadlineHostOperation           DeadlineScope = "host_operation"
)

type DeadlineMetadata struct {
	Scope     DeadlineScope `json:"scope"`
	TimeoutMS uint64        `json:"timeout_ms,omitempty"`
	Renewable bool          `json:"renewable,omitempty"`
}

type Disposition string

const (
	FailTurn   Disposition = "fail_turn"
	RetryStep  Disposition = "retry_step"
	RetryTurn  Disposition = "retry_turn"
	ResumeTurn Disposition = "resume_turn"
	Reject     Disposition = "reject"
)

type SideEffectState string

const (
	SideEffectNone       SideEffectState = "none"
	SideEffectUnchanged  SideEffectState = "unchanged"
	SideEffectDraft      SideEffectState = "draft"
	SideEffectCommitted  SideEffectState = "committed"
	SideEffectRolledBack SideEffectState = "rolled_back"
	SideEffectUnknown    SideEffectState = "unknown"
)

type Metadata struct {
	Origin         Origin            `json:"origin"`
	Disposition    Disposition       `json:"disposition"`
	Reason         string            `json:"reason,omitempty"`
	SideEffects    SideEffectState   `json:"side_effects,omitempty"`
	Stage          Stage             `json:"stage,omitempty"`
	OperationID    string            `json:"operation_id,omitempty"`
	RetryOwner     RetryOwner        `json:"retry_owner,omitempty"`
	ResumeHint     ResumeHint        `json:"resume_hint,omitempty"`
	Deadline       *DeadlineMetadata `json:"deadline,omitempty"`
	RecoveryAction string            `json:"recovery_action,omitempty"`
}

type Details struct {
	Reason           string `json:"reason"`
	ResourceID       string `json:"resource_id,omitempty"`
	SessionStatus    string `json:"session_status,omitempty"`
	ExpectedRevision uint64 `json:"expected_revision,omitempty"`
	ActualRevision   uint64 `json:"actual_revision,omitempty"`
}

type RateLimitMetadata struct {
	Limit        string `json:"limit,omitempty"`
	Remaining    string `json:"remaining,omitempty"`
	Reset        string `json:"reset,omitempty"`
	RetryAfterMS uint64 `json:"retry_after_ms,omitempty"`
}

type Problem struct {
	Version    int                `json:"version"`
	Code       Code               `json:"code"`
	Message    string             `json:"message"`
	Retryable  bool               `json:"retryable"`
	HTTPStatus int                `json:"http_status,omitempty"`
	RateLimit  *RateLimitMetadata `json:"rate_limit,omitempty"`
	Details    *Details           `json:"details,omitempty"`
	Fault      *Metadata          `json:"fault,omitempty"`
	cause      error
}

func New(code Code, message string, retryable bool, cause error) *Problem {
	if !ValidCode(code) {
		code, retryable = Internal, false
	}
	problem := &Problem{
		Version: Version, Code: code, Message: message,
		Retryable: retryable, cause: cause,
		Fault: defaultMetadata(code),
	}
	return problem
}

func defaultMetadata(code Code) *Metadata {
	value := Metadata{
		Origin: OriginRuntime, SideEffects: SideEffectUnknown,
	}
	switch code {
	case Internal:
		value.Disposition = FailTurn
	case Unavailable, DeadlineExceeded:
		value.Disposition = ResumeTurn
	case ResourceExhausted:
		value.Disposition = RetryTurn
	default:
		value.Disposition = Reject
	}
	return &value
}

func NewWithDetails(
	code Code,
	message string,
	retryable bool,
	details Details,
	cause error,
) *Problem {
	problem := New(code, message, retryable, cause)
	if details.Reason != "" {
		problem.Details = &details
	}
	return problem
}

func NewClassified(
	code Code,
	message string,
	retryable bool,
	metadata Metadata,
	cause error,
) *Problem {
	problem := New(code, message, retryable, cause)
	normalizeMetadata(&metadata, code)
	problem.Fault = &metadata
	return problem
}

func normalizeMetadata(metadata *Metadata, code Code) {
	if metadata == nil {
		return
	}
	defaults := defaultMetadata(code)
	if metadata.Origin == "" {
		metadata.Origin = defaults.Origin
	}
	if metadata.Disposition == "" {
		metadata.Disposition = defaults.Disposition
	}
	if metadata.SideEffects == "" {
		metadata.SideEffects = defaults.SideEffects
	}
}

func (p *Problem) Error() string {
	if p == nil {
		return "<nil>"
	}
	if p.Message != "" {
		return p.Message
	}
	return string(p.Code)
}

func (p *Problem) Unwrap() error {
	if p == nil {
		return nil
	}
	return p.cause
}

func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var problem *Problem
	if errors.As(err, &problem) {
		if !ValidCode(problem.Code) {
			return Internal
		}
		return problem.Code
	}
	switch {
	case errors.Is(err, context.Canceled):
		return Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return DeadlineExceeded
	default:
		return Internal
	}
}

func ValidCode(code Code) bool {
	switch code {
	case InvalidArgument, Conflict, ResourceExhausted, Unavailable,
		Canceled, DeadlineExceeded, Internal:
		return true
	default:
		return false
	}
}

func Of(err error) *Problem {
	if err == nil {
		return nil
	}
	var problem *Problem
	if errors.As(err, &problem) && problem != nil {
		return Clone(problem)
	}
	switch {
	case errors.Is(err, context.Canceled):
		return NewClassified(
			Canceled, err.Error(), false,
			Metadata{
				Origin: OriginRuntime, Disposition: Reject,
				SideEffects: SideEffectUnknown,
			},
			err,
		)
	case errors.Is(err, context.DeadlineExceeded):
		return NewClassified(
			DeadlineExceeded, err.Error(), true,
			Metadata{
				Origin: OriginRuntime, Disposition: ResumeTurn,
				SideEffects:    SideEffectUnknown,
				RecoveryAction: "resume after the external deadline is extended",
			},
			err,
		)
	default:
		return NewClassified(
			Unavailable, err.Error(), true,
			Metadata{
				Origin: OriginRuntime, Disposition: ResumeTurn,
				SideEffects:    SideEffectUnknown,
				RecoveryAction: "resume from the durable turn state",
			},
			err,
		)
	}
}

func Clone(source *Problem) *Problem {
	if source == nil {
		return nil
	}
	copy := *source
	if source.RateLimit != nil {
		value := *source.RateLimit
		copy.RateLimit = &value
	}
	if source.Details != nil {
		value := *source.Details
		copy.Details = &value
	}
	if source.Fault != nil {
		value := *source.Fault
		if source.Fault.Deadline != nil {
			deadline := *source.Fault.Deadline
			value.Deadline = &deadline
		}
		copy.Fault = &value
	}
	copy.cause = nil
	return &copy
}

func CloneMetadata(source *Metadata) *Metadata {
	if source == nil {
		return nil
	}
	value := *source
	if source.Deadline != nil {
		deadline := *source.Deadline
		value.Deadline = &deadline
	}
	return &value
}

type RecoveryAction string

const (
	RecoveryRetry  RecoveryAction = "retry"
	RecoveryResume RecoveryAction = "resume"
	RecoveryWait   RecoveryAction = "wait"
	RecoveryBlock  RecoveryAction = "block"
	RecoveryFail   RecoveryAction = "fail"
	RecoveryReject RecoveryAction = "reject"
)

type RecoveryContext struct {
	Owner       RetryOwner
	Idempotent  bool
	Progress    bool
	Attempt     int
	MaxAttempts int
}

type RecoveryDecision struct {
	Action     RecoveryAction
	RetryAfter uint64
}

func Decide(err error, context RecoveryContext) RecoveryDecision {
	problem := Of(err)
	if problem == nil {
		return RecoveryDecision{}
	}
	if problem.Code == Canceled {
		return RecoveryDecision{Action: RecoveryReject}
	}
	if problem.Code == ResourceExhausted {
		return RecoveryDecision{Action: RecoveryBlock}
	}
	metadata := problem.Fault
	if metadata == nil {
		return RecoveryDecision{Action: RecoveryFail}
	}
	if !problem.Retryable {
		switch metadata.Disposition {
		case Reject:
			return RecoveryDecision{Action: RecoveryReject}
		case ResumeTurn, RetryTurn:
			return RecoveryDecision{Action: RecoveryResume}
		default:
			return RecoveryDecision{Action: RecoveryFail}
		}
	}
	if metadata.RetryOwner != context.Owner ||
		!context.Idempotent ||
		context.Progress ||
		metadata.SideEffects == SideEffectUnknown ||
		(context.MaxAttempts > 0 && context.Attempt >= context.MaxAttempts) {
		return RecoveryDecision{Action: RecoveryResume}
	}
	if problem.RateLimit != nil && problem.RateLimit.RetryAfterMS > 0 {
		return RecoveryDecision{
			Action: RecoveryWait, RetryAfter: problem.RateLimit.RetryAfterMS,
		}
	}
	return RecoveryDecision{Action: RecoveryRetry}
}

func DispositionOf(err error) Disposition {
	problem := Of(err)
	if problem == nil || problem.Fault == nil {
		return ""
	}
	return problem.Fault.Disposition
}

func Wrap(
	code Code,
	message string,
	retryable bool,
	cause error,
) error {
	if cause == nil {
		return nil
	}
	return New(
		code,
		fmt.Sprintf("%s: %v", message, cause),
		retryable,
		cause,
	)
}
