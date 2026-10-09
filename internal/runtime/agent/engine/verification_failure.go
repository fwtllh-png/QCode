package engine

import (
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func verificationFailure(receipt *verify.GateReceipt, resumable bool) error {
	metadata := protocol.FaultMetadata{
		Origin: protocol.FaultOriginVerification, Reason: protocol.ProblemReasonVerificationFailed,
		Disposition: protocol.FaultReject, SideEffects: protocol.SideEffectUnknown,
		RecoveryAction: "inspect the failed checks and resolve their cause before retrying",
	}
	if resumable {
		metadata.Disposition, metadata.SideEffects = protocol.FaultResumeTurn, protocol.SideEffectDraft
		metadata.RecoveryAction = "inspect the failed checks, resolve their cause, then continue the retained draft"
	}
	if receipt != nil && receipt.Status == verify.StatusUnavailable {
		metadata.Reason = protocol.ProblemReasonVerificationUnavailable
		metadata.RecoveryAction = "restore the verification environment or supply the required execution evidence before continuing"
	}
	return protocol.NewFault(protocol.CodeConflict, receipt.ProblemMessage(), resumable, metadata, nil)
}
