package app

import (
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func runtimeProblem(code protocol.ErrorCode, message string, cause error) *protocol.Problem {
	return protocol.NewProblem(code, message, false, cause)
}

func retryableProblem(code protocol.ErrorCode, message string) *protocol.Problem {
	return protocol.NewProblem(code, message, true, nil)
}

func turnNotActiveProblem() *protocol.Problem {
	return runtimeProblem(protocol.CodeInvalidArgument, "turn is not active", nil)
}

func sessionBusyProblem(message string, summary protocol.SessionSummary) *protocol.Problem {
	return protocol.NewProblemWithDetails(protocol.CodeConflict, message, true,
		protocol.ProblemDetails{Reason: protocol.ProblemReasonSessionBusy,
			ResourceID: summary.SessionID, SessionStatus: string(summary.Status)}, nil)
}

func resourceProblem(
	code protocol.ErrorCode,
	message string,
	retryable bool,
	reason string,
	resourceID string,
) *protocol.Problem {
	return protocol.NewProblemWithDetails(
		code,
		message,
		retryable,
		protocol.ProblemDetails{Reason: reason, ResourceID: resourceID},
		nil,
	)
}

func revisionProblem(
	message string,
	resourceID string,
	expected uint64,
	actual uint64,
) *protocol.Problem {
	return protocol.NewProblemWithDetails(
		protocol.CodeConflict,
		message,
		true,
		protocol.ProblemDetails{
			Reason:           protocol.ProblemReasonStaleProfileRevision,
			ResourceID:       resourceID,
			ExpectedRevision: expected,
			ActualRevision:   actual,
		},
		nil,
	)
}
