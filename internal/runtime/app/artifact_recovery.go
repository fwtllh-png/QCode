package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type recoveryToolStart struct {
	Tool            string
	ArgumentsDigest string
	Path            string
}

func (r *ArtifactService) PrepareTurnRecovery(
	ctx context.Context,
	request protocol.TurnRecoveryRequest,
) (TurnRecoveryPreparation, error) {
	if err := request.Validate(); err != nil {
		return TurnRecoveryPreparation{},
			runtimeProblem(protocol.CodeInvalidArgument, err.Error(), err)
	}
	current, err := r.runtime.SessionService.sessionState(ctx, request.SessionID)
	if err != nil {
		return TurnRecoveryPreparation{}, err
	}
	if err := ensureSessionQuiescent(current, string(request.Action)); err != nil {
		return TurnRecoveryPreparation{}, err
	}
	if current.LatestTurnWithdrawn && current.LatestTurnID == request.SourceTurnID {
		return TurnRecoveryPreparation{}, runtimeProblem(protocol.CodeConflict, "Turn was withdrawn and cannot be recovered", nil)
	}
	if current.LatestTurnID != "" &&
		current.LatestTurnID != request.SourceTurnID {
		return TurnRecoveryPreparation{}, resourceProblem(
			protocol.CodeConflict,
			"recovery source is not the latest Turn in the Session",
			false,
			protocol.ProblemReasonStaleRecoverySource,
			string(request.SourceTurnID),
		)
	}
	var recoveredProfile *protocol.SessionProfile
	if r.runtime.SessionProfilesAvailable() {
		snapshot, err := r.runtime.SessionService.restoreSessionProfile(
			ctx,
			current,
			current.ThreadID,
		)
		if err != nil {
			return TurnRecoveryPreparation{}, fmt.Errorf(
				"restore current session profile for Turn recovery: %w",
				err,
			)
		}
		recoveredProfile = &snapshot.Profile
	}
	events, err := r.replayArtifactTurn(ctx, request.SourceTurnID)
	if err != nil {
		return TurnRecoveryPreparation{}, err
	}
	var started *protocol.TurnStartedData
	var submittedPlan *protocol.PlanDeltaData
	toolStarts := make(map[string]recoveryToolStart)
	var closedTools []RecoveryToolEvidence
	var sourceReceipt *protocol.ExecutionReceiptData
	terminal := false
	terminalState := ""
	journalAdmissionFailure := false
	var startedOperationID protocol.OperationID
	var sourceThreadID protocol.ThreadID
	var partialOutput strings.Builder
	for _, event := range events {
		if event.TurnID != request.SourceTurnID {
			continue
		}
		if sourceThreadID == "" {
			sourceThreadID = event.ThreadID
			sourceSessionID, ownerErr := r.runtime.sessionLifecycle.SessionForThread(ctx, sourceThreadID)
			if ownerErr != nil || sourceSessionID != request.SessionID {
				return TurnRecoveryPreparation{}, resourceProblem(
					protocol.CodeConflict,
					"source Turn belongs to another Session",
					false,
					protocol.ProblemReasonSessionBusy,
					string(request.SourceTurnID),
				)
			}
		} else if event.ThreadID != sourceThreadID {
			return TurnRecoveryPreparation{}, resourceProblem(
				protocol.CodeConflict,
				"source Turn has inconsistent Thread identity",
				false,
				protocol.ProblemReasonSessionBusy,
				string(request.SourceTurnID),
			)
		}
		switch data := event.Data.(type) {
		case *protocol.TurnStartedData:
			copy := *data
			started = &copy
			startedOperationID = event.OperationID
		case *protocol.OutputDeltaData:
			appendBoundedRecoveryOutput(&partialOutput, data.Text)
		case *protocol.ToolStartData:
			toolStarts[data.CallID] = recoveryToolStart{
				Tool:            data.Tool,
				ArgumentsDigest: RecoveryDigestJSON(data.Arguments),
				Path:            recoveryToolPath(data.Tool, data.Arguments),
			}
		case *protocol.ToolResultData:
			start, ok := toolStarts[data.CallID]
			if !ok || start.Tool != data.Tool {
				continue
			}
			closedTools = append(closedTools, RecoveryToolEvidence{
				Tool: data.Tool, CallID: data.CallID,
				ArgumentsDigest: start.ArgumentsDigest,
				OutputDigest:    RecoveryDigest([]byte(data.Output)),
				IsError:         data.IsError,
				Path:            recoveryToolResultPath(start.Path, data.Changes),
				Changes:         append([]protocol.FileChange(nil), data.Changes...),
				Reason:          recoveryOutcomeReason(data.Tool, data.IsError, data.Output),
			})
			delete(toolStarts, data.CallID)
		case *protocol.PlanDeltaData:
			if data.ArtifactID != "" && data.Purpose.Normalize() == protocol.PlanPurposeExecution {
				copy := *data
				submittedPlan = &copy
			}
		case *protocol.ExecutionReceiptData:
			copy := *data
			sourceReceipt = &copy
		case *protocol.TurnCompletedData:
			terminal = true
			terminalState = "completed"
		case *protocol.TurnFailedData:
			terminal = true
			terminalState = fmt.Sprintf("failed (%s): %s", data.Code, data.Message)
			journalAdmissionFailure = journalAdmissionFailure ||
				isRetainedDraftMessage(data.Message)
		case *protocol.TurnCanceledData:
			terminal = true
			terminalState = "canceled: " + protocol.NormalizeCancelReason(data.Reason)
		case *protocol.OperationRejectedData:
			if isRetainedDraftMessage(data.Message) ||
				(event.OperationID == startedOperationID &&
					protocol.FaultAllowsTurnRecovery(data.Fault)) {
				terminal = true
				terminalState = fmt.Sprintf(
					"interrupted before terminal commit (%s): %s",
					data.Code,
					data.Message,
				)
				journalAdmissionFailure = journalAdmissionFailure ||
					isRetainedDraftMessage(data.Message)
			}
		}
	}
	if !terminal || (started == nil && !journalAdmissionFailure) {
		return TurnRecoveryPreparation{}, resourceProblem(
			protocol.CodeConflict,
			"source Turn is unavailable or not terminal",
			false,
			protocol.ProblemReasonSessionBusy,
			string(request.SourceTurnID),
		)
	}
	if started == nil {
		started = synthesizeJournalAdmissionStart(request)
	}
	rawSourcePrompt := started.Prompt
	if rawSourcePrompt == "" {
		rawSourcePrompt = started.DisplayPrompt
	}
	if strings.TrimSpace(rawSourcePrompt) == "" {
		return TurnRecoveryPreparation{}, runtimeProblem(protocol.CodeConflict, "source Turn has no durable model-visible request", nil)
	}
	sourcePrompt := rawSourcePrompt
	sourceDisplayPrompt := strings.TrimSpace(started.DisplayPrompt)
	if sourceDisplayPrompt == "" {
		sourceDisplayPrompt = rawSourcePrompt
	}
	intent := protocol.NormalizeTurnIntent(started.Intent)
	if !intent.Valid() {
		return TurnRecoveryPreparation{}, runtimeProblem(protocol.CodeConflict, "source Turn has no valid durable intent", nil)
	}
	planID := started.PlanID
	planTransition := started.PlanTransition
	planProfileRevision := started.ProfileRevision
	if planID == "" && submittedPlan != nil && sourceReceipt != nil &&
		strings.TrimSpace(sourceReceipt.Plan) != "" &&
		recoveredProfile != nil {
		planID = submittedPlan.ArtifactID
		planTransition = protocol.PlanTransitionAutopilot
		planProfileRevision = submittedPlan.ProfileRevision
	}
	planStale := planID != "" && recoveredProfile != nil &&
		planProfileRevision != recoveredProfile.Revision
	if planStale {
		planID, planTransition, planProfileRevision = "", "", 0
	}
	prompt := sourcePrompt
	displayPrompt := sourceDisplayPrompt
	if request.Action == protocol.TurnRecoveryContinue {
		sourcePrompt = r.recoveryEffectiveRequest(
			ctx,
			sourceThreadID,
			rawSourcePrompt,
			started.DisplayPrompt,
		)
		sourceDisplayPrompt = sourcePrompt
		currentGoal := strings.TrimSpace(request.Prompt)
		if currentGoal == "" {
			displayPrompt = "Continue: " + sourceDisplayPrompt
			currentGoal = sourceDisplayPrompt
		} else {
			displayPrompt = currentGoal
		}
		prompt = currentGoal
		if capsule := RenderRecoveryEvidence(
			request.SourceTurnID,
			intent,
			terminalState,
			closedTools,
			sourceReceipt,
			partialOutput.String(),
		); capsule != "" {
			prompt += "\n\n<recovery_evidence>\n" + capsule +
				"\n</recovery_evidence>"
		}
		prompt += fmt.Sprintf(
			"\n<source_request turn=%q/>",
			request.SourceTurnID,
		)
	}
	if planStale {
		prompt += "\n\nThe prior structured Plan was invalidated by a " +
			"Session Profile change. Submit a fresh structured Plan before " +
			"performing consequential actions."
	}
	return TurnRecoveryPreparation{
		Prompt:         prompt,
		DisplayPrompt:  displayPrompt,
		Intent:         intent,
		IdempotencyKey: request.IdempotencyKey,
		Recovery: protocol.TurnRecoveryContext{
			Action: request.Action, SourceTurnID: request.SourceTurnID,
			PlanID: planID, PlanTransition: planTransition,
			ProfileRevision: planProfileRevision,
		},
	}, nil
}

func (r *ArtifactService) recoveryEffectiveRequest(
	ctx context.Context,
	threadID protocol.ThreadID,
	modelPrompt string,
	displayPrompt string,
) string {
	prompt := strings.TrimSpace(modelPrompt)
	display := strings.TrimSpace(displayPrompt)
	visited := make(map[protocol.TurnID]struct{})
	for strings.HasPrefix(prompt, TurnRecoveryPromptPrefix) {
		if current, ok := recoveryCurrentRequest(prompt); ok {
			return current
		}
		sourceTurnID, ok := recoverySourceTurnID(prompt)
		if !ok {
			break
		}
		if _, duplicate := visited[sourceTurnID]; duplicate {
			break
		}
		visited[sourceTurnID] = struct{}{}
		ancestors, err := r.replayArtifactTurn(ctx, sourceTurnID)
		if err != nil {
			break
		}
		var source *protocol.TurnStartedData
		for _, event := range ancestors {
			if event.ThreadID != threadID || event.TurnID != sourceTurnID {
				continue
			}
			if data, ok := event.Data.(*protocol.TurnStartedData); ok {
				copy := *data
				source = &copy
			}
		}
		if source == nil {
			break
		}
		prompt = strings.TrimSpace(source.Prompt)
		display = strings.TrimSpace(source.DisplayPrompt)
	}
	if display != "" {
		for strings.HasPrefix(display, "Continue: ") {
			display = strings.TrimSpace(strings.TrimPrefix(display, "Continue: "))
		}
		return display
	}
	return RecoverySourcePrompt(prompt)
}

func synthesizeJournalAdmissionStart(
	request protocol.TurnRecoveryRequest,
) *protocol.TurnStartedData {
	prompt := strings.TrimSpace(request.Prompt)
	if prompt == "" {
		prompt = "Settle the retained workspace journal draft, then continue the user's request."
	}
	return &protocol.TurnStartedData{
		Prompt: prompt, DisplayPrompt: prompt,
		Intent: protocol.TurnIntentAnswer,
	}
}
