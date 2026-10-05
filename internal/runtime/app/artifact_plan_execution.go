package app

import (
	"context"
	"errors"
	"slices"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	securitysandbox "github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func (r *ArtifactService) PrepareStartPayload(
	ctx context.Context,
	workspace string,
	payload *protocol.StartTurnPayload,
) error {
	if payload == nil {
		return nil
	}
	if payload.Recovery != nil && payload.Recovery.PlanID != "" {
		if err := r.validatePlanRecovery(ctx, payload); err != nil {
			return err
		}
	}
	if payload.PlanExecution == nil {
		return nil
	}
	sessionID, err := r.runtime.sessionLifecycle.SessionForThread(ctx, payload.ThreadID)
	if err != nil {
		return err
	}
	request := payload.PlanExecution
	var prepared PlanExecutionPreparation
	if request.SessionID == sessionID {
		prepared, err = r.PreparePlanExecution(
			ctx, sessionID, request.PlanID, request.Transition,
		)
	} else {
		prepared, err = r.PreparePlanExecutionTo(
			ctx, request.SessionID, sessionID,
			request.PlanID, request.Transition,
		)
	}
	if err != nil {
		return err
	}
	if err := securitysandbox.VerifyPlanBaseline(workspace, []byte(prepared.Artifact.Body)); err != nil {
		var drift *securitysandbox.PlanDriftError
		if errors.As(err, &drift) {
			return protocol.NewProblem(protocol.CodeConflict, drift.Error(), true, err)
		}
		return err
	}
	payload.Prompt = prepared.Prompt
	payload.Intent = protocol.TurnIntentWorkspaceChange
	return nil
}

func (r *ArtifactService) validatePlanRecovery(
	ctx context.Context,
	payload *protocol.StartTurnPayload,
) error {
	sessionID, err := r.runtime.sessionLifecycle.SessionForThread(ctx, payload.ThreadID)
	if err != nil {
		return err
	}
	profile, err := r.runtime.RestoreSessionProfile(ctx, sessionID, payload.ThreadID)
	if err != nil {
		return err
	}
	if profile.Profile.Revision != payload.Recovery.ProfileRevision {
		return revisionProblem(
			"Turn recovery Plan binding uses a stale Session Profile",
			payload.Recovery.PlanID,
			payload.Recovery.ProfileRevision,
			profile.Profile.Revision,
		)
	}
	events, err := r.replayArtifactTurn(ctx, payload.Recovery.SourceTurnID)
	if err != nil {
		return err
	}
	inlinePlanSubmitted := false
	inlinePlanReceipted := false
	for _, event := range events {
		if event.ThreadID != payload.ThreadID ||
			event.TurnID != payload.Recovery.SourceTurnID {
			continue
		}
		started, ok := event.Data.(*protocol.TurnStartedData)
		if ok &&
			started.PlanID == payload.Recovery.PlanID &&
			started.PlanTransition == payload.Recovery.PlanTransition &&
			started.ProfileRevision == payload.Recovery.ProfileRevision {
			return nil
		}
		if submitted, ok := event.Data.(*protocol.PlanDeltaData); ok &&
			submitted.Purpose.Normalize() == protocol.PlanPurposeExecution &&
			payload.Recovery.PlanTransition == protocol.PlanTransitionAutopilot &&
			submitted.ArtifactID == payload.Recovery.PlanID &&
			submitted.ProfileRevision == payload.Recovery.ProfileRevision {
			inlinePlanSubmitted = true
		}
		if receipt, ok := event.Data.(*protocol.ExecutionReceiptData); ok &&
			receipt.Plan != "" {
			inlinePlanReceipted = true
		}
	}
	if inlinePlanSubmitted && inlinePlanReceipted {
		return nil
	}
	return runtimeProblem(
		protocol.CodeConflict,
		"Turn recovery Plan binding does not match the source Turn",
		nil,
	)
}

func (r *ArtifactService) PreparePlanExecution(
	ctx context.Context,
	sessionID, planID string,
	transition protocol.PlanTransition,
) (PlanExecutionPreparation, error) {
	current, err := r.runtime.SessionService.sessionState(ctx, sessionID)
	if err != nil {
		return PlanExecutionPreparation{}, err
	}
	artifact, err := r.runtime.sessionArtifacts.GetPlan(ctx, planID)
	if err != nil {
		return PlanExecutionPreparation{}, err
	}
	if artifact.Purpose.Normalize() != protocol.PlanPurposeExecution {
		return PlanExecutionPreparation{}, runtimeProblem(protocol.CodeConflict,
			"deliverable Plan must be submitted as an execution Plan before implementation", nil)
	}
	if err := validateStructuredPlan(artifact.Body, true); err != nil {
		return PlanExecutionPreparation{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"Plan Artifact is not a structured Plan Document",
			err,
		)
	}
	if artifact.SessionID != sessionID || artifact.ThreadID != current.ThreadID {
		return PlanExecutionPreparation{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"Plan Artifact does not belong to the active Session Thread",
			nil,
		)
	}
	if err := r.ensurePlanExecutionReady(ctx, current, artifact); err != nil {
		return PlanExecutionPreparation{}, err
	}
	profile, err := r.runtime.SessionService.sessionProfile(ctx, sessionID)
	if err != nil {
		return PlanExecutionPreparation{}, err
	}
	if !planProfileCompatible(artifact, profile.Profile) {
		return PlanExecutionPreparation{}, retryableProblem(
			protocol.CodeConflict,
			"Plan Artifact Profile Revision is stale",
		)
	}
	switch transition {
	case protocol.PlanTransitionImplement:
		if !artifact.CanImplement {
			return PlanExecutionPreparation{}, runtimeProblem(
				protocol.CodeConflict,
				"Plan Artifact cannot start implementation",
				nil,
			)
		}
	case protocol.PlanTransitionAutopilot:
		if !artifact.CanAutopilot {
			return PlanExecutionPreparation{}, runtimeProblem(
				protocol.CodeConflict,
				"Plan Artifact cannot start Autopilot",
				nil,
			)
		}
	default:
		return PlanExecutionPreparation{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"Plan transition is invalid",
			nil,
		)
	}
	prompt := "Implement the approved structured Plan below. " +
		"Do not repeat completed external side effects; inspect current " +
		"workspace state before each consequential action.\n\n" +
		artifact.Body
	return PlanExecutionPreparation{
		Artifact: artifact,
		Prompt:   prompt,
	}, nil
}

func (r *ArtifactService) ensurePlanExecutionReady(
	ctx context.Context,
	current protocol.SessionSummary,
	artifact protocol.SessionPlanArtifact,
) error {
	if err := r.requireRetainedSource(ctx, artifact.ThreadID, artifact.TurnID); err != nil {
		return err
	}
	return r.runtime.EnsurePlanExecutionReady(ctx, current.SessionID, artifact.ThreadID, artifact.TurnID)
}

func (r *ArtifactService) PreparePlanExecutionTo(
	ctx context.Context,
	sourceSessionID, targetSessionID, planID string,
	transition protocol.PlanTransition,
) (PlanExecutionPreparation, error) {
	artifact, err := r.runtime.sessionArtifacts.GetPlan(ctx, planID)
	if err != nil {
		return PlanExecutionPreparation{}, err
	}
	if artifact.Purpose.Normalize() != protocol.PlanPurposeExecution {
		return PlanExecutionPreparation{}, runtimeProblem(protocol.CodeConflict,
			"deliverable Plan must be submitted as an execution Plan before implementation", nil)
	}
	if err := validateStructuredPlan(artifact.Body, true); err != nil {
		return PlanExecutionPreparation{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"Plan Artifact is not a structured Plan Document",
			err,
		)
	}
	if artifact.SessionID != sourceSessionID {
		return PlanExecutionPreparation{}, resourceProblem(
			protocol.CodeInvalidArgument,
			"Plan Artifact does not belong to the source Session",
			false,
			protocol.ProblemReasonWrongSession,
			planID,
		)
	}
	if err := r.requireRetainedSource(ctx, artifact.ThreadID, artifact.TurnID); err != nil {
		return PlanExecutionPreparation{}, err
	}
	sourceProfile, err := r.runtime.SessionProfile(ctx, sourceSessionID)
	if err != nil {
		return PlanExecutionPreparation{}, err
	}
	if !planProfileCompatible(artifact, sourceProfile.Profile) {
		return PlanExecutionPreparation{}, revisionProblem(
			"Plan Artifact Profile Revision is stale",
			planID,
			artifact.ProfileRevision,
			sourceProfile.Profile.Revision,
		)
	}
	target, err := r.runtime.SessionService.sessionState(ctx, targetSessionID)
	if err != nil {
		return PlanExecutionPreparation{}, err
	}
	if err := ensureSessionQuiescent(target, "implement Plan"); err != nil {
		return PlanExecutionPreparation{}, err
	}
	if sourceSessionID == targetSessionID &&
		target.ParentThreadID != artifact.ThreadID {
		return PlanExecutionPreparation{}, resourceProblem(
			protocol.CodeInvalidArgument,
			"Plan Artifact does not belong to the parent Fork Thread",
			false,
			protocol.ProblemReasonWrongSession,
			planID,
		)
	}
	targetProfile := sourceProfile
	if targetSessionID != sourceSessionID {
		targetProfile, err = r.runtime.SessionService.sessionProfile(ctx, targetSessionID)
		if err != nil {
			return PlanExecutionPreparation{}, err
		}
	}
	if !samePlanTargetProfile(sourceProfile.Profile, targetProfile.Profile) {
		return PlanExecutionPreparation{}, resourceProblem(
			protocol.CodeConflict,
			"target Session Profile does not match the Plan source Profile",
			false,
			protocol.ProblemReasonWrongSession,
			targetSessionID,
		)
	}
	switch transition {
	case protocol.PlanTransitionImplement:
		if !artifact.CanImplement {
			return PlanExecutionPreparation{}, runtimeProblem(
				protocol.CodeConflict,
				"Plan Artifact cannot start implementation",
				nil,
			)
		}
	case protocol.PlanTransitionAutopilot:
		if !artifact.CanAutopilot {
			return PlanExecutionPreparation{}, runtimeProblem(
				protocol.CodeConflict,
				"Plan Artifact cannot start Autopilot",
				nil,
			)
		}
	default:
		return PlanExecutionPreparation{}, runtimeProblem(
			protocol.CodeInvalidArgument,
			"Plan transition is invalid",
			nil,
		)
	}
	return PlanExecutionPreparation{
		Artifact: artifact,
		Prompt: "Implement the approved structured Plan below. " +
			"Do not repeat completed external side effects; inspect current " +
			"workspace state before each consequential action.\n\n" +
			artifact.Body,
	}, nil
}

func samePlanTargetProfile(
	source, target protocol.SessionProfile,
) bool {
	return source.Mode == target.Mode &&
		source.PlanningPolicy == target.PlanningPolicy &&
		source.Provider == target.Provider &&
		source.Model == target.Model &&
		source.ReasoningEffort == target.ReasoningEffort &&
		slices.Equal(source.EnabledToolIDs, target.EnabledToolIDs) &&
		source.ApprovalPosture == target.ApprovalPosture &&
		source.ExecutionTarget == target.ExecutionTarget &&
		source.MaxSteps == target.MaxSteps
}

func planProfileCompatible(
	artifact protocol.SessionPlanArtifact,
	profile protocol.SessionProfile,
) bool {
	if artifact.ExecutionProfileDigest == "" {
		return profile.Revision == artifact.ProfileRevision
	}
	digest, err := PlanExecutionProfileDigest(profile)
	return err == nil && digest == artifact.ExecutionProfileDigest
}
