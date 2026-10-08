package turnkernel

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrIllegalTransition     = errors.New("illegal turn transition")
	ErrRepairBudgetExhausted = errors.New("repair budget exhausted")
)

type RepairBudgetExhaustedError struct {
	Kind        RepairKind
	ProgressKey string
	Limit       uint32
}

func (e *RepairBudgetExhaustedError) Error() string {
	return fmt.Sprintf(
		"%s: kind=%s progress_key=%q limit=%d",
		ErrRepairBudgetExhausted,
		e.Kind,
		e.ProgressKey,
		e.Limit,
	)
}

func (e *RepairBudgetExhaustedError) Unwrap() error {
	return ErrRepairBudgetExhausted
}

type TransitionError struct {
	Phase   Phase
	Command string
	Reason  string
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf(
		"%s: phase=%s command=%s: %s",
		ErrIllegalTransition,
		e.Phase,
		e.Command,
		e.Reason,
	)
}

func (e *TransitionError) Unwrap() error { return ErrIllegalTransition }

type Reducer struct{}

func (Reducer) Apply(current State, command Command) (Transition, error) {
	if command == nil {
		return Transition{}, illegal(current, nil, "command is nil")
	}
	if err := Validate(current); err != nil {
		return Transition{}, fmt.Errorf("invalid current turn state: %w", err)
	}
	if current.Phase.Terminal() {
		return Transition{}, illegal(
			current,
			command,
			"terminal states have no outgoing transitions",
		)
	}

	next := cloneState(current)
	transition := Transition{State: next}
	switch value := command.(type) {
	case StartTurn:
		if err := requirePhase(current, command, PhaseCreated); err != nil {
			return Transition{}, err
		}
		move(&transition, PhasePreparing)

	case PreparationFinished:
		if err := requirePhase(current, command, PhasePreparing); err != nil {
			return Transition{}, err
		}
		move(&transition, PhaseSampling)

	case ModelSampleRequested:
		if err := applyModelSampleRequested(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case ModelSampleStarted:
		if err := applyModelSampleStarted(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case ModelSampleFinished:
		if err := applyModelSampleFinished(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case ModelSampleProgressRecorded:
		if err := applyModelSampleProgress(
			&transition,
			current,
			value,
		); err != nil {
			return Transition{}, err
		}

	case ModelSampleResultReceived:
		if err := applyModelSampleResult(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case SupplementalUsageRecorded:
		if err := applySupplementalUsage(
			&transition,
			current,
			value,
		); err != nil {
			return Transition{}, err
		}

	case ProviderRetryRequested:
		if err := applyProviderRetry(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case ModelTextReceived:
		if err := requirePhase(current, command, PhaseSampling); err != nil {
			return Transition{}, err
		}
		if value.Text == "" {
			return Transition{}, illegal(current, command, "model text is empty")
		}
		transition.State.ProvisionalOutput = append(
			transition.State.ProvisionalOutput,
			value.Text,
		)
		transition.State.OutputEligibility = false

	case ReleaseProvisionalOutput:
		if err := applyReleaseOutput(&transition, current); err != nil {
			return Transition{}, err
		}

	case DiscardProvisionalOutput:
		if err := applyDiscardOutput(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case RepairRequested:
		if err := applyRepairRequested(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case EvaluateTurnStep:
		if err := applyEvaluateTurnStep(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case ObserveProgress:
		if err := applyObserveProgress(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case BindWorkItem:
		if err := applyBindWorkItem(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case ConvergenceRequested:
		if err := applyConvergenceRequested(
			&transition,
			current,
			value,
		); err != nil {
			return Transition{}, err
		}

	case ConvergenceFinalizationStarted:
		if err := applyConvergenceFinalizationStarted(
			&transition,
			current,
		); err != nil {
			return Transition{}, err
		}

	case ToolCallsProposed:
		if err := applyToolCalls(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case ApprovalRequired:
		if err := applyApprovalRequired(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case ApprovalResolved:
		if err := applyApprovalResolved(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case ApprovalResultReceived:
		if err := applyApprovalResult(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case InputRequired:
		if err := applyInputRequired(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case InputResolved:
		if err := applyInputResolved(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case InputResultReceived:
		if err := applyInputResult(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case ToolResultReceived:
		if err := applyToolResult(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case AbortOpenCalls:
		if err := applyAbortOpenCalls(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case WorkspaceReconciled:
		if err := applyWorkspaceReconciled(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case VerificationStarted:
		if err := requirePhase(current, command, PhaseSampling); err != nil {
			return Transition{}, err
		}
		if current.Cancellation.Accepted {
			return Transition{}, illegal(current, command, "cancellation was accepted")
		}
		if len(current.OpenCalls) != 0 {
			return Transition{}, illegal(current, command, "tool calls remain open")
		}
		if current.MutationRevision == 0 && current.Workspace == nil {
			return Transition{}, illegal(current, command, "verification requires a workspace observation")
		}
		move(&transition, PhaseVerifying)
		requestEffect(
			&transition,
			EffectRunVerification,
			struct {
				Mutation uint64 `json:"mutation_revision"`
			}{Mutation: current.MutationRevision},
			fmt.Sprintf("verification:%d", current.MutationRevision),
			"",
		)

	case VerificationFinished:
		if err := applyVerificationFinished(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case CompletionEvaluated:
		if err := applyCompletion(&transition, current, value.Candidate); err != nil {
			return Transition{}, err
		}

	case CompletionInvalidated:
		if err := applyCompletionInvalidated(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case CancelRequested:
		if err := applyCancelRequested(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case ContinuationRecorded:
		if err := applyContinuationRecorded(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case RecoveryRequested:
		if err := applyRecoveryRequested(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case EffectStarted:
		if err := applyEffectStarted(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case EffectRequeued:
		if err := applyEffectRequeued(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case EffectResultReceived:
		if err := applyEffectResult(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case PersistenceResultReceived:
		if err := applyPersistenceResult(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case TerminalRequested:
		decision := TerminalDecision{Kind: TerminalCompleted}
		switch {
		case strings.TrimSpace(value.CancelReason) != "":
			decision.Kind = TerminalCanceled
			decision.Message = value.CancelReason
		case value.Convergence != nil:
			decision.Kind = TerminalFailed
			decision.Code = value.FailureCode
			decision.Message = value.FailureMessage
			decision.Fault = cloneFault(value.Fault)
			decision.Convergence = cloneConvergence(value.Convergence)
		case strings.TrimSpace(value.FailureMessage) != "":
			decision.Kind = TerminalFailed
			decision.Code = value.FailureCode
			decision.Message = value.FailureMessage
			decision.Fault = cloneFault(value.Fault)
		}
		if err := applyTerminalRequested(
			&transition,
			current,
			value,
			decision,
		); err != nil {
			return Transition{}, err
		}

	case JournalFinalized:
		if err := applyJournalFinalized(&transition, current, value.Status); err != nil {
			return Transition{}, err
		}

	case JournalResultReceived:
		if err := applyJournalResult(&transition, current, value); err != nil {
			return Transition{}, err
		}

	case FinishTerminal:
		if err := applyFinishTerminal(&transition, current); err != nil {
			return Transition{}, err
		}

	default:
		return Transition{}, illegal(
			current,
			command,
			fmt.Sprintf("unsupported command type %T", command),
		)
	}

	if err := Validate(transition.State); err != nil {
		return Transition{}, fmt.Errorf(
			"command %s produced invalid turn state: %w",
			command.commandName(),
			err,
		)
	}
	return transition, nil
}
