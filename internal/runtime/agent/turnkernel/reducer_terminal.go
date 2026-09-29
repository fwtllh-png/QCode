package turnkernel

import (
	"strings"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func applyTerminalRequested(
	transition *Transition,
	current State,
	command TerminalRequested,
	decision TerminalDecision,
) error {
	if current.Phase == PhaseCommitting {
		return illegal(current, command, "terminal transaction is already active")
	}
	if len(current.OpenCalls) != 0 {
		return illegal(current, command, "tool calls remain open")
	}
	if len(current.PendingApprovals) != 0 {
		return illegal(current, command, "approval remains pending")
	}
	if current.PendingInput != nil {
		return illegal(current, command, "user input remains pending")
	}
	if current.ActiveSampleID != "" {
		return illegal(current, command, "model sample remains active")
	}
	if len(current.PendingEffects) != 0 {
		return illegal(current, command, "effects remain pending")
	}
	switch decision.Kind {
	case TerminalCompleted:
		if current.Phase != PhaseSampling {
			return illegal(current, command, "completion must begin from sampling")
		}
		if err := validateCompletionReadiness(current); err != nil {
			return illegal(current, command, err.Error())
		}
	case TerminalFailed, TerminalCanceled:
		if strings.TrimSpace(decision.Message) == "" {
			return illegal(current, command, "non-success terminal message is empty")
		}
		if decision.Convergence != nil {
			if decision.Kind != TerminalFailed ||
				current.Convergence == nil ||
				!current.Convergence.FinalizationAttempted {
				return illegal(
					current,
					command,
					"convergence terminal is not ready",
				)
			}
			decision.Convergence = blockedConvergence(current.Convergence)
			transition.State.Convergence =
				cloneConvergence(decision.Convergence)
		}
	default:
		return illegal(current, command, "terminal kind is invalid")
	}
	copy := decision
	transition.State.PendingTerminal = &copy
	transition.State.Usage.Frozen = true
	transition.State.Context.Frozen = true
	move(transition, PhaseCommitting)
	transition.Events = append(transition.Events, Event{
		Kind: EventTerminalPrepared, Terminal: &copy,
	})
	if current.Journal == JournalOpen || current.Policy.JournalRequired {
		effect, _ := terminalJournalOutcome(current, decision)
		requestEffect(
			transition,
			effect,
			decision,
			"journal:"+string(decision.Kind),
			"",
		)
	}
	return nil
}

func applyJournalFinalized(
	transition *Transition,
	current State,
	status JournalStatus,
) error {
	command := JournalFinalized{Status: status}
	if err := requirePhase(current, command, PhaseCommitting); err != nil {
		return err
	}
	if current.PendingTerminal == nil {
		return illegal(current, command, "terminal transaction is missing")
	}
	effectKind, expected := terminalJournalOutcome(
		current,
		*current.PendingTerminal,
	)
	if current.Journal != JournalOpen || status != expected {
		return illegal(current, command, "journal result does not match terminal outcome")
	}
	transition.State.Journal = status
	closeFirstEffectByKind(transition, effectKind, true, "")
	return nil
}

func applyJournalResult(
	transition *Transition,
	current State,
	command JournalResultReceived,
) error {
	if err := requirePhase(current, command, PhaseCommitting); err != nil {
		return err
	}
	effect, ok := current.PendingEffects[command.EffectID]
	if !ok || effect.Status != EffectRunning {
		return illegal(current, command, "journal effect is not running")
	}
	if current.PendingTerminal == nil {
		return illegal(current, command, "terminal transaction is missing")
	}
	expectedKind, expectedStatus := terminalJournalOutcome(
		current,
		*current.PendingTerminal,
	)
	if effect.Kind != expectedKind || command.Status != expectedStatus {
		return illegal(current, command, "journal result does not match terminal outcome")
	}
	if current.MutationRevision == 0 {
		if !current.Policy.JournalRequired || current.Journal != JournalNone {
			return illegal(current, command, "unchanged turn has no open journal")
		}
	} else if current.Journal != JournalOpen {
		return illegal(current, command, "changed turn has no open journal")
	}
	if command.Error != "" {
		effect.Status = EffectRequested
		effect.Error = command.Error
		transition.State.PendingEffects[command.EffectID] = effect
		transition.Events = append(transition.Events, Event{
			Kind: EventEffectRequeued, EffectID: command.EffectID,
		})
		return nil
	}
	if err := finishEffect(
		transition,
		command.EffectID,
		true,
		"",
	); err != nil {
		return illegal(current, command, err.Error())
	}
	transition.State.Journal = command.Status
	return nil
}

func applyFinishTerminal(transition *Transition, current State) error {
	command := FinishTerminal{}
	if err := requirePhase(current, command, PhaseCommitting); err != nil {
		return err
	}
	if current.PendingTerminal == nil {
		return illegal(current, command, "terminal transaction is missing")
	}
	if len(current.Changes) != 0 {
		_, expected := terminalJournalOutcome(current, *current.PendingTerminal)
		if current.Journal != expected {
			return illegal(current, command, "journal is not finalized")
		}
	} else {
		expected := JournalNone
		if current.Policy.JournalRequired {
			_, expected = terminalJournalOutcome(current, *current.PendingTerminal)
		}
		if current.Journal != expected {
			return illegal(current, command, "unchanged turn journal is not finalized")
		}
	}
	decision := *current.PendingTerminal
	transition.State.PendingTerminal = nil
	transition.State.Terminal = &decision
	transition.State.FinalOutput = append(
		[]string(nil),
		current.ProvisionalOutput...,
	)
	transition.State.ProvisionalOutput = nil
	switch decision.Kind {
	case TerminalCompleted:
		move(transition, PhaseCompleted)
	case TerminalFailed:
		move(transition, PhaseFailed)
	case TerminalCanceled:
		move(transition, PhaseCanceled)
	}
	transition.Events = append(transition.Events, Event{
		Kind: EventTerminalCommitted, Terminal: &decision,
	})
	return nil
}

func terminalJournalOutcome(
	state State,
	decision TerminalDecision,
) (EffectKind, JournalStatus) {
	switch {
	case decision.Kind == TerminalCompleted &&
		state.Verification.Action != VerificationActionReverted:
		return EffectCommitJournal, JournalCommitted
	case decision.Kind == TerminalCanceled &&
		CancelSuspendsDraft(decision.Message):
		return EffectSuspendJournal, JournalSuspended
	case decision.Kind == TerminalFailed &&
		(state.Verification.Action == VerificationActionBlocked ||
			decision.Convergence != nil ||
			recoverableTerminalFault(decision.Fault) ||
			state.RecoveryRelation != nil &&
				state.RecoveryRelation.DraftResumed):
		return EffectSuspendJournal, JournalSuspended
	default:
		return EffectRollbackJournal, JournalRolledBack
	}
}

// CancelSuspendsDraft reports whether a cancel with this reason keeps the
// turn's workspace changes as a resumable draft. Every other cancel rolls the
// journal back, so the conversation retained for the turn must follow the
// same rule or history would describe edits the workspace no longer has.
func CancelSuspendsDraft(reason string) bool {
	return reason == protocol.CancelReasonUserInterrupted
}

func recoverableTerminalFault(fault *protocol.FaultMetadata) bool {
	if fault == nil {
		return false
	}
	switch fault.Disposition {
	case protocol.FaultRetryStep,
		protocol.FaultRetryTurn,
		protocol.FaultResumeTurn:
		return true
	default:
		return false
	}
}
