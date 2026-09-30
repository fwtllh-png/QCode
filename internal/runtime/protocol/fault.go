package protocol

import runtimefault "github.com/fwtllh-png/QCode/internal/common/fault"

type FaultOrigin = runtimefault.Origin
type FaultStage = runtimefault.Stage
type FaultRetryOwner = runtimefault.RetryOwner
type FaultResumeHint = runtimefault.ResumeHint
type DeadlineScope = runtimefault.DeadlineScope
type DeadlineMetadata = runtimefault.DeadlineMetadata
type FaultDisposition = runtimefault.Disposition
type SideEffectState = runtimefault.SideEffectState
type FaultMetadata = runtimefault.Metadata
type RecoveryAction = runtimefault.RecoveryAction
type RecoveryContext = runtimefault.RecoveryContext
type RecoveryDecision = runtimefault.RecoveryDecision

const (
	FaultOriginRuntime      = runtimefault.OriginRuntime
	FaultOriginProvider     = runtimefault.OriginProvider
	FaultOriginTool         = runtimefault.OriginTool
	FaultOriginVerification = runtimefault.OriginVerification
	FaultOriginPersistence  = runtimefault.OriginPersistence
	FaultOriginProjection   = runtimefault.OriginProjection
	FaultOriginKernel       = runtimefault.OriginKernel

	FaultStageAdmission       = runtimefault.StageAdmission
	FaultStageConnection      = runtimefault.StageConnection
	FaultStageTLSHandshake    = runtimefault.StageTLSHandshake
	FaultStageResponseHeaders = runtimefault.StageResponseHeaders
	FaultStageStreamIdle      = runtimefault.StageStreamIdle
	FaultStageModelSample     = runtimefault.StageModelSample
	FaultStageWorkflowNode    = runtimefault.StageWorkflowNode
	FaultStageWorkerAttempt   = runtimefault.StageWorkerAttempt
	FaultStageTurnLease       = runtimefault.StageTurnLease
	FaultStagePersistence     = runtimefault.StagePersistence
	FaultStageProjection      = runtimefault.StageProjection
	FaultStageTerminal        = runtimefault.StageTerminal

	FaultRetryOwnerNone     = runtimefault.RetryOwnerNone
	FaultRetryOwnerEngine   = runtimefault.RetryOwnerEngine
	FaultRetryOwnerWorkflow = runtimefault.RetryOwnerWorkflow
	FaultRetryOwnerWorker   = runtimefault.RetryOwnerWorker
	FaultRetryOwnerHost     = runtimefault.RetryOwnerHost

	FaultResumeNone       = runtimefault.ResumeNone
	FaultResumeRetryStep  = runtimefault.ResumeRetryStep
	FaultResumeRetryTurn  = runtimefault.ResumeRetryTurn
	FaultResumeResumeTurn = runtimefault.ResumeResumeTurn
	FaultResumeWait       = runtimefault.ResumeWait
	FaultResumeBlock      = runtimefault.ResumeBlock
	FaultResumeFail       = runtimefault.ResumeFail
	FaultResumeReject     = runtimefault.ResumeReject

	DeadlineProviderConnection      = runtimefault.DeadlineProviderConnection
	DeadlineProviderTLSHandshake    = runtimefault.DeadlineProviderTLSHandshake
	DeadlineProviderResponseHeaders = runtimefault.DeadlineProviderResponseHeaders
	DeadlineProviderStreamIdle      = runtimefault.DeadlineProviderStreamIdle
	DeadlineWorkflowNode            = runtimefault.DeadlineWorkflowNode
	DeadlineWorkerLease             = runtimefault.DeadlineWorkerLease
	DeadlineTurnLease               = runtimefault.DeadlineTurnLease
	DeadlineHostOperation           = runtimefault.DeadlineHostOperation

	FaultFailTurn   = runtimefault.FailTurn
	FaultRetryStep  = runtimefault.RetryStep
	FaultRetryTurn  = runtimefault.RetryTurn
	FaultResumeTurn = runtimefault.ResumeTurn
	FaultReject     = runtimefault.Reject

	SideEffectNone       = runtimefault.SideEffectNone
	SideEffectUnchanged  = runtimefault.SideEffectUnchanged
	SideEffectDraft      = runtimefault.SideEffectDraft
	SideEffectCommitted  = runtimefault.SideEffectCommitted
	SideEffectRolledBack = runtimefault.SideEffectRolledBack
	SideEffectUnknown    = runtimefault.SideEffectUnknown

	RecoveryRetry  = runtimefault.RecoveryRetry
	RecoveryResume = runtimefault.RecoveryResume
	RecoveryWait   = runtimefault.RecoveryWait
	RecoveryBlock  = runtimefault.RecoveryBlock
	RecoveryFail   = runtimefault.RecoveryFail
	RecoveryReject = runtimefault.RecoveryReject
)

func NewFault(
	code ErrorCode,
	message string,
	retryable bool,
	metadata FaultMetadata,
	cause error,
) *Problem {
	return runtimefault.NewClassified(
		code,
		message,
		retryable,
		metadata,
		cause,
	)
}

func ProblemOf(err error) *Problem { return runtimefault.Of(err) }
func CloneFaultMetadata(source *FaultMetadata) *FaultMetadata {
	return runtimefault.CloneMetadata(source)
}
func DispositionOf(err error) FaultDisposition {
	return runtimefault.DispositionOf(err)
}
func FaultAllowsTurnRecovery(fault *FaultMetadata) bool {
	if fault == nil {
		return false
	}
	switch fault.Disposition {
	case FaultRetryStep, FaultRetryTurn, FaultResumeTurn:
		return true
	default:
		return false
	}
}
func DecideRecovery(err error, context RecoveryContext) RecoveryDecision {
	return runtimefault.Decide(err, context)
}
