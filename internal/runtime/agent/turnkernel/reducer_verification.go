package turnkernel

import (
	"errors"
	"slices"
	"strings"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func applyVerificationFinished(
	transition *Transition,
	current State,
	command VerificationFinished,
) error {
	if err := requirePhase(current, command, PhaseVerifying); err != nil {
		return err
	}
	switch command.Status {
	case VerificationNotRequired:
		if current.Workspace == nil || hasEffectiveChanges(current) || len(command.EvidenceCalls) != 0 {
			return illegal(current, command, "not_required requires an observed unchanged workspace without checks")
		}
	case VerificationPassed, VerificationFailed, VerificationUnavailable:
	default:
		return illegal(current, command, "verification status is not terminal")
	}
	effectID := command.EffectID
	if effectID == "" {
		for _, candidate := range sortedEffectIDs(current.PendingEffects) {
			if current.PendingEffects[candidate].Kind == EffectRunVerification {
				effectID = candidate
				break
			}
		}
	}
	if effectID == "" {
		return illegal(current, command, "verification effect is missing")
	}
	effect := current.PendingEffects[effectID]
	if effect.Kind != EffectRunVerification ||
		effect.Status != EffectRunning {
		return illegal(current, command, "verification effect is not running")
	}
	if err := finishEffect(transition, effectID, true, ""); err != nil {
		return illegal(current, command, err.Error())
	}
	action := VerificationActionPassed
	needsRepair := command.Status != VerificationPassed && command.Status != VerificationNotRequired &&
		current.Policy.VerificationMode != "soft"
	if needsRepair && current.Policy.VerificationRepairLimit != 0 {
		err := spendRepairBudget(
			transition,
			RepairVerification,
			command.RepairKey,
			current.Policy.VerificationRepairLimit,
		)
		switch {
		case err == nil:
			action = VerificationActionRepair
		case errors.Is(err, ErrRepairBudgetExhausted):
		default:
			return err
		}
	}
	if action != VerificationActionRepair {
		switch {
		case command.Status == VerificationNotRequired:
			action = VerificationActionNotRequired
		case command.Status == VerificationPassed:
			action = VerificationActionPassed
		case current.Policy.VerificationMustPass:
			action = VerificationActionBlocked
		case current.Policy.VerificationMode == "soft":
			action = VerificationActionReported
		case current.Policy.VerificationOnFailure == "revert":
			action = VerificationActionReverted
		default:
			action = VerificationActionFailed
		}
	}
	transition.State.Verification = VerificationState{
		Status:         command.Status,
		Action:         action,
		Mutation:       current.MutationRevision,
		EvidenceCalls:  append([]string(nil), command.EvidenceCalls...),
		FailureMessage: command.Message,
	}
	if command.Status != VerificationPassed && command.Status != VerificationNotRequired && action != VerificationActionReported {
		transition.State.Completion = nil
	} else if command.Status == VerificationPassed {
		transition.State.WorkItem.Open.UnverifiedPaths = nil
	}
	transition.State.WorkItem.RequiredAction = DeriveRequiredAction(
		transition.State,
	)
	transition.State.NextAction = StepActionNone
	transition.Events = append(transition.Events, Event{
		Kind: EventVerification, Mutation: current.MutationRevision,
	})
	move(transition, PhaseSampling)
	return nil
}

func applyCompletion(
	transition *Transition,
	current State,
	candidate CompletionCandidate,
) error {
	command := CompletionEvaluated{Candidate: candidate}
	if err := requirePhase(current, command, PhaseSampling); err != nil {
		return err
	}
	if strings.TrimSpace(candidate.CompletionCall) == "" {
		return illegal(current, command, "completion call id is empty")
	}
	decision := CompletionDecision{
		Summary:           candidate.Summary,
		OutputMode:        candidate.OutputMode,
		PendingActions:    append([]string(nil), candidate.PendingActions...),
		Mutation:          current.MutationRevision,
		ChangedPaths:      changedPaths(effectiveChanges(current)),
		NoChangeReason:    candidate.NoChangeReason,
		NoChangeEvidence:  append([]string(nil), candidate.NoChangeEvidence...),
		VerificationCalls: append([]string(nil), candidate.VerificationCalls...),
		CompletionCall:    candidate.CompletionCall,
	}
	switch {
	case candidate.BatchMutated:
		decision.Reason = "same_batch_mutation"
	case candidate.BatchSize != 1:
		decision.Reason = "declaration_must_be_only_call"
	case !candidate.DeclarationValid:
		decision.Reason = "invalid_declaration"
	case candidate.OutputMode != "" &&
		candidate.OutputMode != "exact" &&
		candidate.OutputMode != "preserve_provisional":
		decision.Reason = "invalid_output_mode"
	case candidate.Status == "incomplete" &&
		strings.TrimSpace(candidate.Summary) != "" &&
		len(candidate.PendingActions) != 0 &&
		current.Convergence != nil &&
		current.Convergence.FinalizationAttempted:
		decision.Reason = "convergence_blocked"
		transition.State.Convergence.Summary =
			strings.TrimSpace(candidate.Summary)
		transition.State.Convergence.PendingActions = append(
			[]string(nil),
			candidate.PendingActions...,
		)
	case candidate.Status == "incomplete" &&
		strings.TrimSpace(candidate.Summary) != "" &&
		len(candidate.PendingActions) != 0:
		decision.Reason = "convergence_blocked"
		transition.State.Convergence = &ConvergenceState{
			Cause:                 ConvergenceIncomplete,
			FinalizationAttempted: true,
			Summary: strings.TrimSpace(
				candidate.Summary,
			),
			PendingActions: append(
				[]string(nil),
				candidate.PendingActions...,
			),
		}
	case candidate.ToolError ||
		candidate.Status != "complete" ||
		strings.TrimSpace(candidate.Summary) == "" ||
		len(candidate.PendingActions) != 0:
		decision.Reason = "incomplete_declaration"
	case (candidate.NoChangeReason != "" || len(candidate.NoChangeEvidence) != 0) &&
		!validNoChange(current, candidate.NoChangeReason, candidate.NoChangeEvidence):
		decision.Reason = "invalid_no_change_evidence"
	case current.Intent == protocol.TurnIntentWorkspaceChange &&
		current.MutationRevision == 0 && !validNoChange(current, candidate.NoChangeReason, candidate.NoChangeEvidence):
		decision.Reason = "no_observed_changes"
	case candidate.OutputMode == "preserve_provisional" &&
		len(current.ProvisionalOutput) == 0:
		decision.Reason = "provisional_output_unavailable"
	default:
		decision.Accepted = true
		if verificationPending(current) {
			decision.RequiredAction = "await_runtime_verification"
		} else {
			decision.RequiredAction = "final_answer"
		}
	}
	if !decision.Accepted {
		decision.RequiredAction = completionRejectionAction(decision.Reason)
	} else {
		summary := strings.TrimSpace(candidate.Summary)
		if candidate.OutputMode == "preserve_provisional" {
			transition.State.ProvisionalOutput = append(
				transition.State.ProvisionalOutput,
				"\n\n"+summary,
			)
		} else {
			// Outside convergence finalization, the accepted declaration owns
			// the exact user-facing terminal output.
			transition.State.ProvisionalOutput = []string{summary}
		}
		transition.State.OutputEligibility = false
	}
	copy := decision
	copy.PendingActions = append(
		[]string(nil),
		decision.PendingActions...,
	)
	copy.ChangedPaths = append([]string(nil), decision.ChangedPaths...)
	copy.VerificationCalls = append([]string(nil), decision.VerificationCalls...)
	transition.State.Completion = &copy
	transition.Events = append(transition.Events, Event{
		Kind:     EventCompletionDecided,
		Mutation: current.MutationRevision,
	})
	return nil
}

func applyCompletionInvalidated(
	transition *Transition,
	current State,
	command CompletionInvalidated,
) error {
	if err := requirePhase(current, command, PhaseSampling); err != nil {
		return err
	}
	if strings.TrimSpace(command.Reason) == "" {
		return illegal(current, command, "completion invalidation reason is empty")
	}
	if current.Completion == nil || !current.Completion.Accepted {
		return illegal(current, command, "accepted completion is unavailable")
	}
	transition.State.Completion = nil
	transition.Events = append(transition.Events, Event{
		Kind:     EventCompletionDecided,
		Mutation: current.MutationRevision,
	})
	return nil
}

func validateCompletionReadiness(state State) error {
	if !state.OutputEligibility {
		return errors.New("final output is not eligible")
	}
	return validateCompletionContract(state)
}

func validateCompletionContract(state State) error {
	if err := validateCompletionPolicy(state); err != nil {
		return err
	}
	if state.MutationRevision == 0 {
		if state.Journal != JournalNone {
			return errors.New("unchanged turn has an open journal")
		}
		return nil
	}
	if state.Journal != JournalOpen {
		return errors.New("mutation journal is not open")
	}
	return nil
}

func changedPaths(changes []ObservedChange) []string {
	unique := make(map[string]struct{}, len(changes))
	for _, change := range changes {
		unique[change.Path] = struct{}{}
	}
	paths := make([]string, 0, len(unique))
	for path := range unique {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths
}

func samePaths(left, right []string) bool {
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}
