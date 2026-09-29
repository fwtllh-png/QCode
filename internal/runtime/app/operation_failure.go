package app

import "github.com/fwtllh-png/QCode/internal/runtime/protocol"

func (r *Runtime) rejectResumableOperation(
	operation protocol.Operation,
	err error,
	release func(),
) bool {
	disposition := protocol.DispositionOf(err)
	if disposition != protocol.FaultRetryStep &&
		disposition != protocol.FaultResumeTurn {
		return false
	}
	release()
	r.rejectAndCommit(operation, err)
	return true
}

// operationRejection classifies a rejection cause into its durable event.
func operationRejection(err error) *protocol.OperationRejectedData {
	problem := protocol.ProblemOf(err)
	return &protocol.OperationRejectedData{
		Code: problem.Code, Message: problem.Message,
		Fault: problem.Fault,
	}
}

// restartInterruptedProblem rejects an operation that was accepted but not
// committed when the previous Runtime stopped. Only a StartTurn with durable
// domain facts can be resumed deterministically; anything else is settled so
// the client can decide whether to submit it again. Thread-wide engine
// mutations may have partially applied before the stop.
func restartInterruptedProblem(kind protocol.OperationKind) error {
	sideEffects := protocol.SideEffectNone
	switch kind {
	case protocol.OperationCompactThread,
		protocol.OperationForkThread,
		protocol.OperationRevertTurn:
		sideEffects = protocol.SideEffectUnknown
	}
	return protocol.NewFault(
		protocol.CodeUnavailable,
		"operation was interrupted by a Runtime restart before it committed; submit it again",
		true,
		protocol.FaultMetadata{
			Origin:      protocol.FaultOriginRuntime,
			Disposition: protocol.FaultReject,
			SideEffects: sideEffects,
			RetryOwner:  protocol.FaultRetryOwnerHost,
		},
		nil,
	)
}
