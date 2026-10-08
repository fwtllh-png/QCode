package turnkernel

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func applyEvaluateTurnStep(
	transition *Transition,
	current State,
	command EvaluateTurnStep,
) error {
	if err := requirePhase(current, command, PhaseSampling); err != nil {
		return err
	}
	if strings.TrimSpace(command.ProgressKey) == "" {
		return illegal(current, command, "turn step progress key is empty")
	}
	transition.State.NextAction = StepActionNone
	if current.UnresolvedToolFailure && current.RecoveryToolSucceeded {
		transition.State.UnresolvedToolFailure = false
		transition.State.RecoveryToolSucceeded = false
		current.UnresolvedToolFailure = false
		current.RecoveryToolSucceeded = false
	}
	spend := func(kind RepairKind, limit uint32, action StepAction) error {
		if limit == 0 {
			beginConvergence(transition, ConvergenceRequested{
				Cause: ConvergenceRepairBudget, RepairKind: kind,
			})
			return nil
		}
		if err := spendRepairBudget(
			transition,
			kind,
			command.ProgressKey,
			limit,
		); err != nil {
			if errors.Is(err, ErrRepairBudgetExhausted) {
				beginConvergence(transition, ConvergenceRequested{
					Cause:      ConvergenceRepairBudget,
					Used:       limit,
					Limit:      limit,
					RepairKind: kind,
				})
				return nil
			}
			return err
		}
		transition.State.NextAction = action
		return nil
	}
	switch {
	case current.Completion != nil && current.Completion.Accepted:
		if verificationPending(current) {
			transition.State.NextAction = StepActionVerify
		} else {
			transition.State.NextAction = StepActionComplete
		}
	case current.Convergence != nil:
		if current.Convergence.FinalizationAttempted {
			transition.State.NextAction = StepActionBlock
			if current.Completion != nil &&
				(current.Completion.Reason == "invalid_declaration" ||
					current.Completion.Reason == "incomplete_declaration") &&
				current.RepairBudgets[RepairDeclaration].Steps < current.Policy.DeclarationRepairLimit {
				if err := spendRepairBudget(transition, RepairDeclaration,
					"convergence_declaration", current.Policy.DeclarationRepairLimit); err != nil {
					return err
				}
				transition.State.Convergence.FinalizationAttempted = false
				transition.State.NextAction = StepActionFinalize
			}
		} else {
			transition.State.NextAction = StepActionFinalize
		}
	case current.UnresolvedToolFailure:
		if err := spend(
			RepairCompletion,
			current.Policy.CompletionRepairLimit,
			StepActionRepairToolFailure,
		); err != nil {
			return err
		}
		if current.RecoveryToolSucceeded ||
			current.Intent == protocol.TurnIntentAnswer ||
			current.Intent == protocol.TurnIntentPlan {
			transition.State.UnresolvedToolFailure = false
			transition.State.RecoveryToolSucceeded = false
		}
	case current.LastModelContinued && len(current.ClosedCalls) != 0 &&
		(current.Completion == nil || !current.Completion.Accepted):
		if err := spend(
			RepairCompletion,
			current.Policy.CompletionRepairLimit,
			StepActionRepairCompletion,
		); err != nil {
			return err
		}
	case len(current.ProvisionalOutput) == 0:
		if err := spend(
			RepairCompletion,
			current.Policy.CompletionRepairLimit,
			StepActionRepairCompletion,
		); err != nil {
			return err
		}
	case workspaceCompletionMissing(current):
		if err := spend(
			RepairWorkspace,
			current.Policy.WorkspaceRepairLimit,
			StepActionRepairWorkspace,
		); err != nil {
			return err
		}
	case RequiresCompletion(current) &&
		(current.Completion == nil || !current.Completion.Accepted):
		if err := spend(
			RepairDeclaration,
			current.Policy.DeclarationRepairLimit,
			StepActionRepairDeclaration,
		); err != nil {
			return err
		}
	case verificationPending(current):
		transition.State.NextAction = StepActionVerify
	default:
		transition.State.NextAction = StepActionComplete
	}
	return nil
}

func completionRejectionAction(reason string) string {
	switch reason {
	case "no_observed_changes":
		return "perform_workspace_mutation_or_explain_no_change"
	case "verification_evidence_required":
		return "exec_command"
	case "pending_actions":
		return "continue_work"
	case "convergence_blocked":
		return "finalize_blocked"
	default:
		return "correct_and_retry_turn_complete"
	}
}

func requirePhase(state State, command Command, allowed ...Phase) error {
	if slices.Contains(allowed, state.Phase) {
		return nil
	}
	return illegal(
		state,
		command,
		fmt.Sprintf("expected phase %v", allowed),
	)
}

func move(transition *Transition, phase Phase) {
	from := transition.State.Phase
	transition.State.Phase = phase
	transition.Events = append(transition.Events, Event{
		Kind: EventTransition, From: from, To: phase,
	})
}

func illegal(state State, command Command, reason string) error {
	name := "<nil>"
	if command != nil {
		name = command.commandName()
	}
	return &TransitionError{Phase: state.Phase, Command: name, Reason: reason}
}
