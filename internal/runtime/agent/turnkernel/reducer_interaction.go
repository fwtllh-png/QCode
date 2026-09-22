package turnkernel

import (
	"strings"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func applyApprovalResult(
	transition *Transition,
	current State,
	command ApprovalResultReceived,
) error {
	if strings.TrimSpace(command.EffectID) == "" {
		return illegal(current, command, "approval effect id is empty")
	}
	if effect, ok := current.PendingEffects[command.EffectID]; !ok ||
		effect.Kind != EffectAwaitApproval ||
		effect.Status != EffectRunning {
		return illegal(current, command, "approval effect is not running")
	}
	if err := finishEffect(
		transition,
		command.EffectID,
		command.Accepted,
		command.Error,
	); err != nil {
		return illegal(current, command, err.Error())
	}
	if err := applyApprovalResolved(
		transition,
		transition.State,
		ApprovalResolved{RequestID: command.RequestID},
	); err != nil {
		return err
	}
	if command.Canceled {
		return applyCancelRequested(
			transition,
			transition.State,
			CancelRequested{Reason: protocol.CancelReasonApprovalCanceled},
		)
	}
	return nil
}

func applyInputResult(
	transition *Transition,
	current State,
	command InputResultReceived,
) error {
	if strings.TrimSpace(command.EffectID) == "" {
		return illegal(current, command, "input effect id is empty")
	}
	if effect, ok := current.PendingEffects[command.EffectID]; !ok ||
		effect.Kind != EffectAwaitInput ||
		effect.Status != EffectRunning {
		return illegal(current, command, "input effect is not running")
	}
	if err := finishEffect(
		transition,
		command.EffectID,
		command.Accepted,
		command.Error,
	); err != nil {
		return illegal(current, command, err.Error())
	}
	return applyInputResolved(
		transition,
		transition.State,
		InputResolved{RequestID: command.RequestID},
	)
}

func applyCancelRequested(
	transition *Transition,
	current State,
	command CancelRequested,
) error {
	if strings.TrimSpace(command.Reason) == "" {
		return illegal(current, command, "cancel reason is empty")
	}
	if current.Cancellation.Accepted {
		return illegal(current, command, "cancellation was already accepted")
	}
	transition.State.Cancellation = CancellationState{
		Requested: true,
		Accepted:  true,
		Reason:    command.Reason,
	}
	transition.Events = append(
		transition.Events,
		Event{Kind: EventCancelAccepted},
	)
	return nil
}

func applyRecoveryRequested(
	transition *Transition,
	current State,
	command RecoveryRequested,
) error {
	if err := requirePhase(current, command, PhaseCreated); err != nil {
		return err
	}
	if strings.TrimSpace(command.SourceTurnID) == "" ||
		strings.TrimSpace(command.RecoveryTurnID) == "" ||
		command.CurrentProfileRevision == 0 ||
		(command.Action != string(protocol.TurnRecoveryRetry) &&
			command.Action != string(protocol.TurnRecoveryContinue)) ||
		(command.DraftResumed && len(command.Changes) == 0) {
		return illegal(current, command, "recovery relation is incomplete")
	}
	transition.State.ProfileRevision = command.CurrentProfileRevision
	transition.State.RecoveryRelation = &RecoveryRelation{
		SourceTurnID:   command.SourceTurnID,
		RecoveryTurnID: command.RecoveryTurnID,
		Action:         command.Action,
		DraftResumed:   command.DraftResumed,
	}
	if len(command.Changes) != 0 {
		transition.State.MutationRevision = 1
		transition.State.Changes = append(
			[]ObservedChange(nil),
			command.Changes...,
		)
		transition.State.Journal = JournalOpen
		transition.Events = append(transition.Events, Event{
			Kind: EventMutationObserved, Mutation: 1,
		})
	}
	transition.Events = append(
		transition.Events,
		Event{Kind: EventRecoveryBound},
	)
	return nil
}

func applyApprovalRequired(
	transition *Transition,
	current State,
	command ApprovalRequired,
) error {
	if err := requirePhase(
		current,
		command,
		PhaseExecutingTools,
		PhaseAwaitingApproval,
	); err != nil {
		return err
	}
	if strings.TrimSpace(command.RequestID) == "" {
		return illegal(current, command, "approval request id is empty")
	}
	if _, ok := current.OpenCalls[command.CallID]; !ok {
		return illegal(current, command, "approval does not reference an open call")
	}
	if _, exists := current.PendingApprovals[command.RequestID]; exists {
		return illegal(current, command, "approval request id is duplicated")
	}
	transition.State.PendingApprovals[command.RequestID] = ApprovalState{
		RequestID: command.RequestID,
		CallID:    command.CallID,
	}
	requestEffect(
		transition,
		EffectAwaitApproval,
		command,
		"approval:"+command.RequestID,
		command.CallID,
	)
	if current.Phase == PhaseExecutingTools {
		move(transition, PhaseAwaitingApproval)
	}
	return nil
}

func applyApprovalResolved(
	transition *Transition,
	current State,
	command ApprovalResolved,
) error {
	if err := requirePhase(current, command, PhaseAwaitingApproval); err != nil {
		return err
	}
	if _, ok := current.PendingApprovals[command.RequestID]; !ok {
		return illegal(current, command, "approval request does not match")
	}
	delete(transition.State.PendingApprovals, command.RequestID)
	closeEffectByIdentity(
		transition,
		EffectAwaitApproval,
		command.RequestID,
		true,
		"",
	)
	if len(transition.State.PendingApprovals) == 0 {
		move(transition, PhaseExecutingTools)
	}
	return nil
}

func applyInputRequired(
	transition *Transition,
	current State,
	command InputRequired,
) error {
	if err := requirePhase(
		current,
		command,
		PhaseSampling,
		PhaseExecutingTools,
	); err != nil {
		return err
	}
	if strings.TrimSpace(command.RequestID) == "" {
		return illegal(current, command, "input request id is empty")
	}
	transition.State.PendingInput = &InputState{
		RequestID: command.RequestID,
		CallID:    command.CallID,
	}
	requestEffect(
		transition,
		EffectAwaitInput,
		command,
		"input:"+command.RequestID,
		command.CallID,
	)
	move(transition, PhaseAwaitingInput)
	return nil
}

func applyInputResolved(
	transition *Transition,
	current State,
	command InputResolved,
) error {
	if err := requirePhase(current, command, PhaseAwaitingInput); err != nil {
		return err
	}
	if current.PendingInput == nil ||
		current.PendingInput.RequestID != command.RequestID {
		return illegal(current, command, "input request does not match")
	}
	transition.State.PendingInput = nil
	if transition.State.Convergence != nil {
		transition.State.Convergence.FinalizationAttempted = false
		transition.State.Convergence.Summary = ""
		transition.State.Convergence.PendingActions = nil
	}
	closeEffectByIdentity(
		transition,
		EffectAwaitInput,
		command.RequestID,
		true,
		"",
	)
	if len(current.OpenCalls) != 0 {
		move(transition, PhaseExecutingTools)
	} else {
		move(transition, PhaseSampling)
	}
	return nil
}
