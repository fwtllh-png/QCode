package turnkernel

import (
	"errors"
	"slices"
	"strings"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

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
		Summary:          candidate.Summary,
		OutputMode:       candidate.OutputMode,
		PendingActions:   append([]string(nil), candidate.PendingActions...),
		Mutation:         current.MutationRevision,
		ChangedPaths:     changedPaths(effectiveChanges(current)),
		NoChangeReason:   candidate.NoChangeReason,
		NoChangeEvidence: append([]string(nil), candidate.NoChangeEvidence...),
		CompletionCall:   candidate.CompletionCall,
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
		decision.RequiredAction = "final_answer"
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

// Legacy fields remain in State to preserve historical fact digests. They are
// cleared only by this recorded transition, never while decoding old facts.
func legacyVerificationActive(state State) bool {
	return state.Policy.VerificationRequired || state.Policy.VerificationMode != "" ||
		state.Verification.Status != VerificationNotEvaluated || state.Phase == PhaseVerifying ||
		len(state.WorkItem.Open.UnverifiedPaths) != 0 || len(state.WorkItem.Open.CoveredPaths) != 0
}

func retireVerification(t *Transition) error {
	for _, id := range sortedEffectIDs(t.State.PendingEffects) {
		if t.State.PendingEffects[id].Kind == EffectRunVerification {
			if err := finishEffect(t, id, false, "verification gate was removed; no check was executed"); err != nil {
				return err
			}
		}
	}
	t.State.Policy.VerificationRequired = false
	t.State.Policy.VerificationMustPass = false
	t.State.Policy.VerificationMode = ""
	t.State.Policy.VerificationOnFailure = ""
	t.State.Policy.VerificationRepairLimit = 0
	t.State.Verification = VerificationState{Status: VerificationNotEvaluated}
	delete(t.State.RepairBudgets, RepairVerification)
	if t.State.Convergence != nil && t.State.Convergence.RepairKind == RepairVerification {
		t.State.Convergence = nil
	}
	t.State.WorkItem.Open.UnverifiedPaths = nil
	t.State.WorkItem.Open.CoveredPaths = nil
	t.State.WorkItem.RequiredAction = DeriveRequiredAction(t.State)
	if t.State.NextAction == StepActionVerify {
		t.State.NextAction = StepActionNone
	}
	if t.State.Phase == PhaseVerifying {
		move(t, PhaseSampling)
	}
	return nil
}
