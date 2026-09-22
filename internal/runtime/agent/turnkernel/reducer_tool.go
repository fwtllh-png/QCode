package turnkernel

import (
	"slices"
	"strings"
)

func applyToolCalls(
	transition *Transition,
	current State,
	command ToolCallsProposed,
) error {
	if err := requirePhase(
		current,
		command,
		PhaseSampling,
		PhaseExecutingTools,
	); err != nil {
		return err
	}
	if len(command.Calls) == 0 {
		return illegal(current, command, "tool batch is empty")
	}
	if current.Cancellation.Accepted {
		return illegal(current, command, "cancellation was accepted")
	}
	transition.State.Completion = nil
	transition.State.OutputEligibility = false
	// Keep captured narration across later tool batches. A text-only sample
	// is the stop signal; throwing the draft away forced the model to
	// rewrite the answer inside turn_complete.
	seen := make(map[string]struct{}, len(command.Calls))
	for _, call := range command.Calls {
		if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
			return illegal(current, command, "tool call identity is incomplete")
		}
		if _, ok := seen[call.ID]; ok {
			return illegal(current, command, "tool call id is duplicated")
		}
		if _, ok := current.OpenCalls[call.ID]; ok {
			return illegal(current, command, "tool call id is already open")
		}
		if _, ok := current.ClosedCalls[call.ID]; ok {
			return illegal(current, command, "tool call id was already closed")
		}
		seen[call.ID] = struct{}{}
		transition.State.OpenCalls[call.ID] = call
		transition.Events = append(transition.Events, Event{
			Kind: EventToolStarted, CallID: call.ID,
		})
		requestEffect(
			transition,
			EffectExecuteTool,
			call,
			"tool:"+call.ID,
			call.ID,
		)
	}
	if current.Phase == PhaseSampling {
		move(transition, PhaseExecutingTools)
	}
	transition.State.Progress.PendingIdentity = FormatToolCallsIdentity(
		command.Calls,
	)
	transition.State.Progress.PendingResultDigests = nil
	return nil
}

func applyToolResult(
	transition *Transition,
	current State,
	command ToolResultReceived,
) error {
	callAwaitingApproval := false
	for _, approval := range current.PendingApprovals {
		if approval.CallID == command.CallID {
			callAwaitingApproval = true
			break
		}
	}
	cancelingApproval := callAwaitingApproval && current.Cancellation.Accepted
	// A request_user_input call whose wait never got a reply (TTL expiry,
	// turn cancellation, or execution error) finishes through the normal
	// tool-result path. Retire the bound input wait instead of rejecting
	// the transition: failing here would abort the whole turn.
	inputBound := current.PendingInput != nil &&
		current.PendingInput.CallID == command.CallID
	phases := []Phase{PhaseExecutingTools, PhaseAwaitingApproval}
	if inputBound {
		phases = append(phases, PhaseAwaitingInput)
	}
	if err := requirePhase(current, command, phases...); err != nil {
		return err
	}
	if callAwaitingApproval && !cancelingApproval {
		return illegal(
			current,
			command,
			"tool result cannot bypass a pending approval",
		)
	}
	if strings.TrimSpace(command.EffectID) == "" {
		return illegal(current, command, "tool effect id is empty")
	}
	call, ok := current.OpenCalls[command.CallID]
	if !ok {
		return illegal(current, command, "tool result does not match an open call")
	}
	effect, ok := current.PendingEffects[command.EffectID]
	if !ok ||
		effect.Kind != EffectExecuteTool ||
		effect.CallID != command.CallID ||
		effect.Status != EffectRunning {
		return illegal(current, command, "tool result effect does not match call")
	}
	if err := finishEffect(
		transition,
		command.EffectID,
		true,
		"",
	); err != nil {
		return illegal(current, command, err.Error())
	}
	if inputBound {
		requestID := current.PendingInput.RequestID
		transition.State.PendingInput = nil
		if transition.State.Convergence != nil {
			transition.State.Convergence.FinalizationAttempted = false
			transition.State.Convergence.Summary = ""
			transition.State.Convergence.PendingActions = nil
		}
		closeEffectByIdentity(
			transition,
			EffectAwaitInput,
			requestID,
			true,
			"",
		)
	}
	delete(transition.State.OpenCalls, command.CallID)
	transition.State.ClosedCalls[command.CallID] = ToolResultState{
		ID: command.CallID, Name: call.Name, IsError: command.IsError,
	}
	if cancelingApproval {
		for requestID, approval := range current.PendingApprovals {
			if approval.CallID != command.CallID {
				continue
			}
			delete(transition.State.PendingApprovals, requestID)
			closeEffectByIdentity(
				transition,
				EffectAwaitApproval,
				requestID,
				false,
				current.Cancellation.Reason,
			)
		}
	}
	if command.IsError {
		transition.State.UnresolvedToolFailure = true
		transition.State.RecoveryToolSucceeded = false
	} else if current.UnresolvedToolFailure {
		transition.State.RecoveryToolSucceeded = true
	}
	transition.Events = append(transition.Events, Event{
		Kind: EventToolClosed, CallID: command.CallID,
	})
	if len(command.Changes) != 0 {
		transition.State.MutationRevision++
		transition.State.Changes = append(
			transition.State.Changes,
			command.Changes...,
		)
		transition.State.Completion = nil
		transition.State.Verification = VerificationState{
			Status: VerificationNotEvaluated,
		}
		transition.State.Journal = JournalOpen
		transition.Events = append(transition.Events, Event{
			Kind:     EventMutationObserved,
			Mutation: transition.State.MutationRevision,
		})
	}
	applyWorkItemObservation(
		&transition.State,
		call,
		command.Changes,
		command.Observation,
		command.IsError,
	)
	if digest := strings.TrimSpace(command.Observation.ResultDigest); digest != "" {
		transition.State.Progress.PendingResultDigests = append(
			transition.State.Progress.PendingResultDigests,
			digest,
		)
	}
	switch {
	case len(transition.State.PendingApprovals) != 0:
		move(transition, PhaseAwaitingApproval)
	case len(transition.State.OpenCalls) != 0:
		move(transition, PhaseExecutingTools)
	default:
		move(transition, PhaseSampling)
	}
	return nil
}

func applyAbortOpenCalls(
	transition *Transition,
	current State,
	command AbortOpenCalls,
) error {
	if err := requirePhase(
		current,
		command,
		PhaseExecutingTools,
		PhaseAwaitingApproval,
	); err != nil {
		return err
	}
	if strings.TrimSpace(command.Reason) == "" {
		return illegal(current, command, "abort reason is empty")
	}
	if len(current.OpenCalls) == 0 {
		return illegal(current, command, "no tool calls are open")
	}
	callIDs := make([]string, 0, len(current.OpenCalls))
	for callID := range current.OpenCalls {
		callIDs = append(callIDs, callID)
	}
	slices.Sort(callIDs)
	for _, callID := range callIDs {
		call := current.OpenCalls[callID]
		delete(transition.State.OpenCalls, callID)
		transition.State.ClosedCalls[callID] = ToolResultState{
			ID: callID, Name: call.Name, IsError: true,
		}
		closeEffectByCall(
			transition,
			EffectExecuteTool,
			callID,
			false,
			command.Reason,
		)
		transition.Events = append(transition.Events, Event{
			Kind: EventToolClosed, CallID: callID,
		})
	}
	clear(transition.State.PendingApprovals)
	closeEffectsByKind(
		transition,
		EffectAwaitApproval,
		false,
		command.Reason,
	)
	move(transition, PhaseSampling)
	return nil
}
