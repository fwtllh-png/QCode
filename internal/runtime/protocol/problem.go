package protocol

import (
	"errors"

	runtimefault "github.com/fwtllh-png/QCode/internal/runtime/fault"
)

type ErrorCode = runtimefault.Code
type Problem = runtimefault.Problem
type ProblemDetails = runtimefault.Details
type RateLimitMetadata = runtimefault.RateLimitMetadata

const ProblemVersion = runtimefault.Version

const (
	CodeInvalidArgument   = runtimefault.InvalidArgument
	CodeConflict          = runtimefault.Conflict
	CodeResourceExhausted = runtimefault.ResourceExhausted
	CodeUnavailable       = runtimefault.Unavailable
	CodeCanceled          = runtimefault.Canceled
	CodeDeadlineExceeded  = runtimefault.DeadlineExceeded
	CodeInternal          = runtimefault.Internal
)

const (
	ProblemReasonSessionBusy          = "session_busy"
	ProblemReasonStaleRecoverySource  = "stale_recovery_source"
	ProblemReasonStaleProfileRevision = "stale_profile_revision"
	ProblemReasonUnsupported          = "unsupported"
	ProblemReasonWrongSession         = "wrong_session"
	ProblemReasonProviderThroughput   = "provider_throughput"
	ProblemReasonProviderRateLimited  = "provider_rate_limited"
)

func NewProblem(code ErrorCode, message string, retryable bool, cause error) *Problem {
	return runtimefault.New(code, message, retryable, cause)
}

func NewProblemWithDetails(
	code ErrorCode,
	message string,
	retryable bool,
	details ProblemDetails,
	cause error,
) *Problem {
	return runtimefault.NewWithDetails(code, message, retryable, details, cause)
}

func CodeOf(err error) ErrorCode            { return runtimefault.CodeOf(err) }
func ValidErrorCode(code ErrorCode) bool    { return runtimefault.ValidCode(code) }
func IsCode(err error, code ErrorCode) bool { return CodeOf(err) == code }

func IsRetryable(err error) bool {
	var problem *Problem
	return errors.As(err, &problem) && problem.Retryable
}

func WrapProblem(code ErrorCode, message string, retryable bool, cause error) error {
	return runtimefault.Wrap(code, message, retryable, cause)
}
