package turnkernel

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestNonInteractiveReadOnlyResearchCompletesWithoutDeclaration(t *testing.T) {
	for _, intent := range []protocol.TurnIntent{
		protocol.TurnIntentAnswer,
		protocol.TurnIntentPlan,
	} {
		t.Run(string(intent), func(t *testing.T) {
			state := startSampling(t, intent)
			state = apply(t, state, ModelTextReceived{
				Text: "analysis complete",
			}).State
			released := apply(t, state, ReleaseProvisionalOutput{})
			if len(released.Effects) != 0 ||
				!released.State.OutputEligibility ||
				len(released.State.ProvisionalOutput) != 1 {
				t.Fatalf("release effects = %+v", released.Effects)
			}
			state = released.State

			prepared := apply(t, state, TerminalRequested{})
			if prepared.State.Phase != PhaseCommitting ||
				len(prepared.Effects) != 0 {
				t.Fatalf("prepared = %+v", prepared)
			}
			finished := apply(t, prepared.State, FinishTerminal{})
			if finished.State.Phase != PhaseCompleted ||
				finished.State.Terminal == nil ||
				len(finished.Effects) != 0 {
				t.Fatalf("finished = %+v", finished)
			}
			if len(finished.State.ProvisionalOutput) != 0 ||
				!finished.State.OutputEligibility ||
				len(finished.State.FinalOutput) != 1 {
				t.Fatalf("output state = %+v", finished.State)
			}
		})
	}
}

func TestStructuredInteractiveTurnRequiresDeclarationAndUsesItsSummary(
	t *testing.T,
) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.StructuredTerminalRequired = true
	state = apply(t, state, ModelTextReceived{
		Text: "I will continue investigating.",
	}).State
	repair := apply(t, state, EvaluateTurnStep{
		ProgressKey: "sample=1",
	})
	if repair.State.NextAction != StepActionRepairDeclaration {
		t.Fatalf("ordinary text action = %q", repair.State.NextAction)
	}
	state = apply(t, repair.State, CompletionEvaluated{
		Candidate: CompletionCandidate{
			DeclarationValid: true,
			Status:           "complete",
			Summary:          "The investigation is complete.",
			CompletionCall:   "complete-1",
			BatchSize:        1,
		},
	}).State
	if state.Completion == nil || !state.Completion.Accepted ||
		len(state.ProvisionalOutput) != 1 ||
		state.ProvisionalOutput[0] != "The investigation is complete." {
		t.Fatalf("completion output = %+v", state)
	}
	completed := apply(t, state, EvaluateTurnStep{
		ProgressKey: "sample=2",
	})
	if completed.State.NextAction != StepActionComplete {
		t.Fatalf("declared action = %q", completed.State.NextAction)
	}
}

func TestZeroDeclarationRepairBudgetConvergesWithoutAnotherRepair(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.StructuredTerminalRequired = true
	state.Policy.DeclarationRepairLimit = 0
	state = apply(t, state, ModelTextReceived{Text: "provisional"}).State
	evaluated := apply(t, state, EvaluateTurnStep{ProgressKey: "sample=1"})
	if evaluated.State.NextAction != StepActionFinalize ||
		evaluated.State.Convergence == nil ||
		evaluated.State.Convergence.RepairKind != RepairDeclaration ||
		evaluated.State.Convergence.Limit != 0 {
		t.Fatalf("zero declaration budget = %+v", evaluated.State)
	}
}

func TestRequiresCompletionFollowsStructuredTerminalPolicy(t *testing.T) {
	answer := startSampling(t, protocol.TurnIntentAnswer)
	answer.ClosedCalls["read"] = ToolResultState{ID: "read", Name: "file_read"}
	answer.MutationRevision = 1
	if RequiresCompletion(answer) {
		t.Fatal("tool-backed mutation required structured completion")
	}
	answer.Policy.StructuredTerminalRequired = true
	if !RequiresCompletion(answer) {
		t.Fatal("structured terminal did not require completion")
	}
	answer.Policy.CompletionRequired = false
	if RequiresCompletion(answer) {
		t.Fatal("disabled completion policy still requires completion")
	}
	workspaceChange := startSampling(t, protocol.TurnIntentWorkspaceChange)
	if RequiresCompletion(workspaceChange) {
		t.Fatal("workspace change required structured completion by default")
	}
}

func TestCanceledApprovalAcceptsLateToolResult(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentWorkspaceChange)
	state = apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "call-edit", Name: "file_edit"}},
	}).State
	state = apply(t, state, ApprovalRequired{
		RequestID: "approval-edit", CallID: "call-edit",
	}).State
	state = apply(t, state, CancelRequested{
		Reason: protocol.CancelReasonHostInterrupted,
	}).State
	closed := apply(t, state, ToolResultReceived{
		CallID: "call-edit", IsError: true,
	})
	if closed.State.Phase != PhaseSampling ||
		len(closed.State.OpenCalls) != 0 ||
		len(closed.State.PendingApprovals) != 0 ||
		len(closed.State.PendingEffects) != 0 ||
		len(closed.State.ClosedCalls) != 1 {
		t.Fatalf("late canceled result = %+v", closed.State)
	}
	terminal := apply(t, closed.State, TerminalRequested{
		CancelReason: protocol.CancelReasonHostInterrupted,
	})
	terminal = apply(t, terminal.State, FinishTerminal{})
	if terminal.State.Phase != PhaseCanceled ||
		terminal.State.Terminal == nil ||
		terminal.State.Terminal.Kind != TerminalCanceled {
		t.Fatalf("terminal = %+v", terminal.State)
	}
}

func TestRepairBudgetResetsOnlyOnStructuredProgress(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	for range 2 {
		state = apply(t, state, RepairRequested{
			Kind: RepairCompletion, ProgressKey: "mutation=0;tools=0", Limit: 2,
		}).State
	}
	before := state
	_, err := (Reducer{}).Apply(state, RepairRequested{
		Kind: RepairCompletion, ProgressKey: "mutation=0;tools=0", Limit: 2,
	})
	if !errors.Is(err, ErrRepairBudgetExhausted) {
		t.Fatalf("third repair error = %v", err)
	}
	if got, want := before.RepairBudgets[RepairCompletion], state.RepairBudgets[RepairCompletion]; got != want {
		t.Fatalf("rejected repair changed budget: before=%+v after=%+v", got, want)
	}
	state = apply(t, state, RepairRequested{
		Kind: RepairCompletion, ProgressKey: "mutation=0;tools=1", Limit: 2,
	}).State
	budget := state.RepairBudgets[RepairCompletion]
	if budget.Consecutive != 1 || budget.Steps != 3 {
		t.Fatalf("progressed repair budget = %+v", budget)
	}
}

func TestRepairExhaustionRequestsKernelConvergence(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentWorkspaceChange)
	state.Policy.WorkspaceRepairLimit = 1
	state.ProvisionalOutput = []string{"no changes yet"}
	first := apply(t, state, EvaluateTurnStep{ProgressKey: "mutation=0"})
	if first.State.NextAction != StepActionRepairWorkspace {
		t.Fatalf("first action = %q", first.State.NextAction)
	}
	second, err := (Reducer{}).Apply(
		first.State,
		EvaluateTurnStep{ProgressKey: "mutation=0"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.State.NextAction != StepActionFinalize ||
		second.State.Convergence == nil ||
		second.State.Convergence.Cause != ConvergenceRepairBudget ||
		second.State.Convergence.RepairKind != RepairWorkspace {
		t.Fatalf("repair convergence = %+v", second.State)
	}
}

func TestConvergenceFinalizationPreservesCapturedOutput(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.ProvisionalOutput = []string{"captured answer"}
	state = apply(t, state, ConvergenceRequested{
		Cause: ConvergenceOutputLimit,
		Used:  3,
		Limit: 3,
	}).State
	state = apply(t, state, ConvergenceFinalizationStarted{}).State
	state = apply(t, state, CompletionEvaluated{
		Candidate: CompletionCandidate{
			DeclarationValid: true,
			Status:           "complete",
			Summary:          "Done.",
			OutputMode:       "preserve_provisional",
			CompletionCall:   "complete-1",
			BatchSize:        1,
		},
	}).State
	if state.Completion == nil || !state.Completion.Accepted ||
		len(state.ProvisionalOutput) != 2 ||
		state.ProvisionalOutput[0] != "captured answer" ||
		state.ProvisionalOutput[1] != "\n\nDone." {
		t.Fatalf("preserved completion = %+v", state)
	}
	evaluated := apply(t, state, EvaluateTurnStep{ProgressKey: "finalized"})
	if evaluated.State.NextAction != StepActionComplete {
		t.Fatalf("finalized action = %q", evaluated.State.NextAction)
	}
}

func TestIncompleteConvergenceDeclarationBecomesBlockedOutcome(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, ConvergenceRequested{
		Cause: ConvergenceStepLimit,
		Used:  64,
		Limit: 64,
	}).State
	state = apply(t, state, ConvergenceFinalizationStarted{}).State
	state = apply(t, state, CompletionEvaluated{
		Candidate: CompletionCandidate{
			DeclarationValid: true,
			Status:           "incomplete",
			Summary:          "Implementation is partially complete.",
			PendingActions:   []string{"Run the remaining verification."},
			CompletionCall:   "incomplete-1",
			BatchSize:        1,
		},
	}).State
	if state.Completion == nil ||
		state.Completion.Reason != "convergence_blocked" ||
		state.Convergence.Summary == "" ||
		len(state.Convergence.PendingActions) != 1 {
		t.Fatalf("blocked declaration = %+v", state)
	}
	evaluated := apply(t, state, EvaluateTurnStep{ProgressKey: "blocked"})
	if evaluated.State.NextAction != StepActionBlock {
		t.Fatalf("blocked action = %q", evaluated.State.NextAction)
	}
}

func TestIncompleteDeclarationWithOpenPlanBecomesBlockedOutcome(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, CompletionEvaluated{
		Candidate: CompletionCandidate{
			DeclarationValid: true,
			Status:           "incomplete",
			Summary:          "Node is unavailable in this environment.",
			PendingActions:   []string{"Resume the web build with Node available."},
			CompletionCall:   "incomplete-open-plan",
			BatchSize:        1,
			PlanOpenSteps:    2,
		},
	}).State
	if state.Completion == nil ||
		state.Completion.Reason != "convergence_blocked" ||
		state.Convergence == nil ||
		state.Convergence.Cause != ConvergenceIncomplete ||
		state.Convergence.Summary == "" ||
		len(state.Convergence.PendingActions) != 1 {
		t.Fatalf("blocked declaration = %+v", state)
	}
	evaluated := apply(t, state, EvaluateTurnStep{ProgressKey: "blocked"})
	if evaluated.State.NextAction != StepActionBlock {
		t.Fatalf("blocked action = %q", evaluated.State.NextAction)
	}
}

func TestMutationOutputCannotReleaseBeforeCompletionAndVerification(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "write-1", Name: "file_write"}},
	}).State
	state = apply(t, state, ToolResultReceived{
		CallID:  "write-1",
		Changes: []ObservedChange{{Path: "a.go", Kind: "modified"}},
	}).State
	state = apply(t, state, ModelTextReceived{Text: "premature"}).State
	_, err := (Reducer{}).Apply(state, ReleaseProvisionalOutput{})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("release error = %v", err)
	}
}

func TestWorkspaceChangeWithoutMutationFailsClosed(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentWorkspaceChange)
	_, err := (Reducer{}).Apply(state, TerminalRequested{})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("error = %v, want illegal transition", err)
	}
}

func TestMutationCompletesThroughDeclarationVerificationAndJournal(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "write-1", Name: "file_write"}},
	}).State
	state = apply(t, state, ToolResultReceived{
		CallID:  "write-1",
		Changes: []ObservedChange{{Path: "a.go", Kind: "modified"}},
	}).State
	if state.MutationRevision != 1 || state.Journal != JournalOpen {
		t.Fatalf("mutation state = %+v", state)
	}
	state = apply(t, state, CompletionEvaluated{
		Candidate: CompletionCandidate{
			DeclarationValid: true,
			Status:           "complete",
			Summary:          "implemented",
			CompletionCall:   "complete-1",
			BatchSize:        1,
		},
	}).State
	state = apply(t, state, VerificationStarted{}).State
	state = apply(t, state, VerificationFinished{
		Status: VerificationPassed, EvidenceCalls: []string{"verify-1"},
	}).State
	state = apply(t, state, ModelTextReceived{Text: "done"}).State
	state = apply(t, state, ReleaseProvisionalOutput{}).State

	prepared := apply(t, state, TerminalRequested{})
	if len(prepared.Effects) != 1 ||
		prepared.Effects[0].Kind != EffectCommitJournal {
		t.Fatalf("terminal effects = %+v", prepared.Effects)
	}
	effect := prepared.Effects[0]
	state = apply(t, prepared.State, EffectStarted{
		EffectID: effect.ID,
		Attempt:  1,
	}).State
	state = apply(t, state, JournalResultReceived{
		EffectID: effect.ID,
		Status:   JournalCommitted,
		Error:    "injected durable journal failure",
	}).State
	retry := state.PendingEffects[effect.ID]
	if retry.Status != EffectRequested ||
		retry.Attempt != 1 ||
		retry.Error == "" ||
		state.Phase != PhaseCommitting {
		t.Fatalf("retryable journal state = %+v", state)
	}
	state = apply(t, state, EffectStarted{
		EffectID: effect.ID,
		Attempt:  2,
	}).State
	state = apply(t, state, JournalResultReceived{
		EffectID: effect.ID,
		Status:   JournalCommitted,
	}).State
	state = apply(t, state, FinishTerminal{}).State
	if state.Phase != PhaseCompleted ||
		state.Journal != JournalCommitted ||
		state.Terminal == nil {
		t.Fatalf("terminal state = %+v", state)
	}
}

func TestReducerOwnsCompletionAcceptanceAndRuntimeBindings(t *testing.T) {
	mutated := startSampling(t, protocol.TurnIntentAnswer)
	mutated = apply(t, mutated, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "write-1", Name: "file_write"}},
	}).State
	mutated = apply(t, mutated, ToolResultReceived{
		CallID:  "write-1",
		Changes: []ObservedChange{{Path: "a.go", Kind: "modified"}},
	}).State
	base := CompletionCandidate{
		DeclarationValid: true,
		Status:           "complete",
		Summary:          "implemented",
		CompletionCall:   "complete-1",
		BatchSize:        1,
	}
	tests := []struct {
		name      string
		state     State
		candidate CompletionCandidate
		accepted  bool
		reason    string
		action    string
	}{
		{
			name:      "accepted",
			state:     mutated,
			candidate: base,
			accepted:  true,
			action:    "await_runtime_verification",
		},
		{
			name:  "same batch mutation",
			state: mutated,
			candidate: func() CompletionCandidate {
				value := base
				value.BatchMutated = true
				return value
			}(),
			reason: "same_batch_mutation",
		},
		{
			name:      "read only answer",
			state:     startSampling(t, protocol.TurnIntentAnswer),
			candidate: base,
			accepted:  true,
			action:    "final_answer",
		},
		{
			name:  "open plan steps do not block a complete declaration",
			state: mutated,
			candidate: func() CompletionCandidate {
				value := base
				value.PlanOpenSteps = 1
				return value
			}(),
			accepted: true,
			action:   "await_runtime_verification",
		},
		{
			name:  "answer planning may complete with open plan steps",
			state: startSampling(t, protocol.TurnIntentAnswer),
			candidate: func() CompletionCandidate {
				value := base
				value.PlanOpenSteps = 3
				value.Summary = "plan delivered for the user to implement"
				return value
			}(),
			accepted: true,
			action:   "final_answer",
		},
		{
			name:      "workspace change without mutation",
			state:     startSampling(t, protocol.TurnIntentWorkspaceChange),
			candidate: base,
			reason:    "no_observed_changes",
			action:    "perform_workspace_mutation",
		},
		{
			name:  "incomplete work becomes resumable blocked outcome",
			state: startSampling(t, protocol.TurnIntentAnswer),
			candidate: CompletionCandidate{
				DeclarationValid: true,
				Status:           "incomplete",
				Summary:          "workspace edits remain",
				PendingActions:   []string{"apply the workspace edits"},
				CompletionCall:   "complete-incomplete",
				BatchSize:        1,
				PlanOpenSteps:    1,
			},
			reason: "convergence_blocked",
			action: "finalize_blocked",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			state := apply(t, testCase.state, CompletionEvaluated{
				Candidate: testCase.candidate,
			}).State
			if state.Completion == nil ||
				state.Completion.Accepted != testCase.accepted ||
				state.Completion.Reason != testCase.reason {
				t.Fatalf("completion decision = %+v", state.Completion)
			}
			if testCase.action != "" &&
				state.Completion.RequiredAction != testCase.action {
				t.Fatalf("required action = %q, want %q",
					state.Completion.RequiredAction, testCase.action)
			}
			if testCase.accepted &&
				(state.Completion.Mutation != testCase.state.MutationRevision ||
					!samePaths(
						state.Completion.ChangedPaths,
						changedPaths(testCase.state.Changes),
					) ||
					state.Completion.CompletionCall != "complete-1") {
				t.Fatalf("runtime bindings = %+v", state.Completion)
			}
		})
	}
}

func TestRecoverableFaultsSuspendJournalInsteadOfRollingBack(t *testing.T) {
	state := NewState(protocol.TurnIntentWorkspaceChange, "act", 1)
	state.MutationRevision = 1
	state.Journal = JournalOpen
	for _, disposition := range []protocol.FaultDisposition{
		protocol.FaultRetryStep,
		protocol.FaultRetryTurn,
		protocol.FaultResumeTurn,
	} {
		kind, status := terminalJournalOutcome(state, TerminalDecision{
			Kind:  TerminalFailed,
			Fault: &protocol.FaultMetadata{Disposition: disposition},
		})
		if kind != EffectSuspendJournal || status != JournalSuspended {
			t.Fatalf(
				"disposition %s outcome = %s/%s",
				disposition,
				kind,
				status,
			)
		}
	}
}

func TestUserInterruptionSuspendsJournalInsteadOfRollingBack(t *testing.T) {
	state := NewState(protocol.TurnIntentWorkspaceChange, "act", 1)
	state.MutationRevision = 1
	state.Journal = JournalOpen
	kind, status := terminalJournalOutcome(state, TerminalDecision{
		Kind: TerminalCanceled, Message: protocol.CancelReasonUserInterrupted,
	})
	if kind != EffectSuspendJournal || status != JournalSuspended {
		t.Fatalf("user interruption outcome = %s/%s", kind, status)
	}
}

func TestMutationInvalidatesCompletionAndVerification(t *testing.T) {
	state := verifiedMutation(t)
	state = apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "write-2", Name: "file_write"}},
	}).State
	state = apply(t, state, ToolResultReceived{
		CallID:  "write-2",
		Changes: []ObservedChange{{Path: "b.go", Kind: "created"}},
	}).State
	if state.MutationRevision != 2 ||
		state.Completion != nil ||
		state.Verification.Status != VerificationNotEvaluated {
		t.Fatalf("state after later mutation = %+v", state)
	}
	_, err := (Reducer{}).Apply(state, TerminalRequested{})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("error = %v, want illegal transition", err)
	}
}

func TestRecoveredToolFailureDoesNotBlockCapturedText(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "fail-1", Name: "exec_command"}},
	}).State
	state = apply(t, state, ToolResultReceived{
		CallID: "fail-1", IsError: true,
	}).State
	state = apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "ok-1", Name: "exec_command"}},
	}).State
	state = apply(t, state, ToolResultReceived{CallID: "ok-1"}).State
	state.ProvisionalOutput = []string{"matrix templates and how to think about them"}
	if !state.UnresolvedToolFailure || !state.RecoveryToolSucceeded {
		t.Fatalf("expected recovered failure: %+v", state)
	}

	transition := apply(t, state, EvaluateTurnStep{ProgressKey: "recovered"})
	if transition.State.NextAction != StepActionComplete ||
		transition.State.UnresolvedToolFailure ||
		transition.State.RecoveryToolSucceeded {
		t.Fatalf("recovered failure still repaired: %+v", transition.State)
	}
}

func TestToolAssistedReadOnlyTurnCompletesFromCapturedText(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "read-1", Name: "file_read"}},
	}).State
	state = apply(t, state, ToolResultReceived{CallID: "read-1"}).State
	state.ProvisionalOutput = []string{"The review is complete."}

	transition := apply(t, state, EvaluateTurnStep{ProgressKey: "read-only"})
	if transition.State.NextAction != StepActionComplete {
		t.Fatalf("next action = %q, want %q",
			transition.State.NextAction, StepActionComplete)
	}
}

func TestAcceptedCompletionOutranksProviderContinuation(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "write-1", Name: "file_write"}},
	}).State
	state = apply(t, state, ToolResultReceived{
		CallID:  "write-1",
		Changes: []ObservedChange{{Path: "a.go", Kind: "modified"}},
	}).State
	state = apply(t, state, CompletionEvaluated{
		Candidate: CompletionCandidate{
			DeclarationValid: true,
			Status:           "complete",
			Summary:          "implemented",
			CompletionCall:   "complete-1",
			BatchSize:        1,
		},
	}).State
	state.LastModelContinued = true

	transition := apply(t, state, EvaluateTurnStep{ProgressKey: "accepted"})
	if transition.State.NextAction != StepActionVerify {
		t.Fatalf("next action = %q, want %q",
			transition.State.NextAction, StepActionVerify)
	}
	if transition.State.RepairBudgets[RepairCompletion].Steps != 0 {
		t.Fatalf("accepted completion spent repair budget: %+v",
			transition.State.RepairBudgets[RepairCompletion])
	}
}

func TestInputInjectedInvalidatesAcceptedCompletion(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.CompletionRequired = true
	state.Policy.StructuredTerminalRequired = true
	state.ProvisionalOutput = []string{"stale answer"}
	state = apply(t, state, CompletionEvaluated{
		Candidate: CompletionCandidate{
			DeclarationValid: true,
			Status:           "complete",
			Summary:          "stale",
			CompletionCall:   "complete-1",
			BatchSize:        1,
		},
	}).State
	if state.Completion == nil || !state.Completion.Accepted {
		t.Fatalf("setup completion = %+v", state.Completion)
	}
	before := apply(t, state, EvaluateTurnStep{ProgressKey: "accepted"})
	if before.State.NextAction != StepActionComplete {
		t.Fatalf("accepted next action = %q, want %q",
			before.State.NextAction, StepActionComplete)
	}
	state = apply(t, state, CompletionInvalidated{
		Reason: "input_injected",
	}).State
	if state.Completion != nil {
		t.Fatalf("injected input left completion %+v", state.Completion)
	}
	transition := apply(t, state, EvaluateTurnStep{ProgressKey: "injected"})
	if transition.State.NextAction != StepActionRepairDeclaration {
		t.Fatalf("injected next action = %q, want %q",
			transition.State.NextAction, StepActionRepairDeclaration)
	}
}

func TestObserveProgressUsesConservativeDurableThresholds(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentWorkspaceChange)
	state.Policy.ExecutionStepLimit = 64
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	state = apply(t, state, ObserveProgress{
		Signature: "mutation=0;plan_done=0",
	}).State

	for _, test := range []struct {
		samples uint32
		want    ProgressStage
	}{
		{samples: 20, want: ProgressStageNone},
		{samples: 21, want: ProgressStageConverge},
		{samples: 41, want: ProgressStageConverge},
		{samples: 42, want: ProgressStageFinishOnly},
	} {
		state = apply(t, state, ObserveProgress{
			Signature:        "mutation=0;plan_done=0",
			CompletedSamples: test.samples,
		}).State
		if state.Progress.Stage != test.want ||
			state.Progress.NoProgressSamples != test.samples {
			t.Fatalf(
				"samples=%d progress=%+v, want stage=%s",
				test.samples,
				state.Progress,
				test.want,
			)
		}
	}

	state = apply(t, state, ObserveProgress{
		Signature:        "mutation=1;plan_done=0",
		CompletedSamples: 56,
	}).State
	if state.Progress.Stage != ProgressStageNone ||
		state.Progress.NoProgressSamples != 0 {
		t.Fatalf("progress did not reset: %+v", state.Progress)
	}
	exhausted := startSampling(t, protocol.TurnIntentWorkspaceChange)
	exhausted.Policy.ExecutionStepLimit = 64
	exhausted.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	exhausted = apply(t, exhausted, ObserveProgress{
		Signature: "mutation=0;plan_done=0",
	}).State
	exhausted = apply(t, exhausted, ObserveProgress{
		Signature: "mutation=0;plan_done=0", CompletedSamples: 64,
	}).State
	if exhausted.Progress.Stage != ProgressStageExhausted {
		t.Fatalf("exhausted progress = %+v", exhausted.Progress)
	}
}

func TestConvergencePolicyDerivesFromExplicitStepLimit(t *testing.T) {
	if policy := ConvergencePolicyForStepLimit(0); policy != (ConvergencePolicy{}) {
		t.Fatalf("uncapped convergence policy = %+v", policy)
	}
	if policy := ConvergencePolicyForStepLimit(2); policy.ProgressConverge != 1 ||
		policy.ProgressFinishOnly != 1 || policy.ProgressLimit != 2 {
		t.Fatalf("small-budget convergence policy = %+v", policy)
	}
	policy := ConvergencePolicyForStepLimit(64)
	if policy.ProgressConverge != 21 ||
		policy.ProgressFinishOnly != 42 ||
		policy.ProgressLimit != 64 ||
		policy.ResearchConverge != policy.ProgressConverge ||
		policy.ResearchFinishOnly != policy.ProgressFinishOnly ||
		policy.ResearchLimit != policy.ProgressLimit {
		t.Fatalf("derived convergence policy = %+v", policy)
	}
}

func TestReadOnlyProgressUsesConsecutiveNoProgressStages(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	state = apply(t, state, ObserveProgress{
		Signature: "evidence-0",
	}).State
	for _, test := range []struct {
		samples uint32
		want    ProgressStage
	}{
		{samples: 20, want: ProgressStageNone},
		{samples: 21, want: ProgressStageConverge},
		{samples: 42, want: ProgressStageFinishOnly},
		{samples: 64, want: ProgressStageExhausted},
	} {
		state = apply(t, state, ObserveProgress{
			Signature:        "evidence-0",
			CompletedSamples: test.samples,
		}).State
		if state.Progress.Stage != test.want {
			t.Fatalf("samples=%d stage=%q, want %q",
				test.samples, state.Progress.Stage, test.want)
		}
	}
}

func TestReadOnlyProgressDoesNotConvergeWhileEvidenceAdvances(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	for samples := uint32(0); samples <=
		state.Policy.Convergence.ResearchLimit*2; samples++ {
		state = apply(t, state, ObserveProgress{
			Signature:        fmt.Sprintf("evidence-%d", samples),
			CompletedSamples: samples,
		}).State
		if state.Progress.Stage != ProgressStageNone ||
			state.Convergence != nil {
			t.Fatalf("samples=%d progress=%+v convergence=%+v",
				samples, state.Progress, state.Convergence)
		}
	}
}

func TestResearchProgressLeavesTotalSampleCapAfterMutation(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.ExecutionStepLimit = 64
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	state = apply(t, state, ObserveProgress{
		Signature: "mutation=0",
	}).State
	state = apply(t, state, ObserveProgress{
		Signature:        "mutation=0",
		CompletedSamples: state.Policy.Convergence.ResearchFinishOnly,
	}).State
	if state.Progress.Stage != ProgressStageFinishOnly {
		t.Fatalf("pre-mutation progress = %+v", state.Progress)
	}

	proposed := apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "write-1", Name: "file_write"}},
	})
	effect := proposed.Effects[0]
	state = apply(t, proposed.State, EffectStarted{
		EffectID: effect.ID, Attempt: 1,
	}).State
	state = apply(t, state, ToolResultReceived{
		EffectID: effect.ID,
		CallID:   "write-1",
		Changes:  []ObservedChange{{Path: "mcp.json", Kind: "created"}},
	}).State
	state = apply(t, state, ObserveProgress{
		Signature:        "mutation=1",
		CompletedSamples: state.Policy.Convergence.ResearchLimit,
	}).State
	if state.Progress.Stage != ProgressStageNone ||
		state.Progress.NoProgressSamples != 0 {
		t.Fatalf("post-mutation progress = %+v", state.Progress)
	}
}

func TestExecutionStepBudgetRenewsOnStructuredProgress(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.ExecutionStepLimit = 3
	state.Policy.Convergence = ConvergencePolicyForStepLimit(3)
	state.SampleLedger["sample-1"] = ModelSampleState{
		ID: "sample-1", Status: SampleCompleted, Attempt: 1,
	}
	first := apply(t, state, ObserveProgress{
		Signature: "evidence=one", CompletedSamples: 1,
	})
	if first.State.Convergence != nil {
		t.Fatalf("first sample converged = %+v", first.State.Convergence)
	}
	first.State.SampleLedger["sample-2"] = ModelSampleState{
		ID: "sample-2", Status: SampleCompleted, Attempt: 1,
	}
	second := apply(t, first.State, ObserveProgress{
		Signature: "evidence=two", CompletedSamples: 2,
	})
	if second.State.Convergence != nil {
		t.Fatalf("progressing Turn converged = %+v", second.State.Convergence)
	}
	stalled := apply(t, second.State, ObserveProgress{
		Signature: "evidence=two", CompletedSamples: 5,
	})
	if stalled.State.Convergence == nil ||
		stalled.State.Convergence.Cause != ConvergenceNoProgress ||
		stalled.State.Convergence.Used != 3 ||
		stalled.State.Convergence.Limit != 3 {
		t.Fatalf("stalled Turn convergence = %+v", stalled.State.Convergence)
	}
}

func TestProgressingTurnRenewsExplicitStepBudget(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.Policy.ExecutionStepLimit = 64
	state.Policy.Convergence = ConvergencePolicyForStepLimit(64)
	for sample := uint32(1); sample <= 128; sample++ {
		id := fmt.Sprintf("sample-%d", sample)
		state.SampleLedger[id] = ModelSampleState{
			ID: id, Status: SampleCompleted, Attempt: 1,
		}
		state = apply(t, state, ObserveProgress{
			Signature:        fmt.Sprintf("evidence=%d", sample),
			CompletedSamples: sample,
		}).State
		if state.Convergence != nil {
			t.Fatalf(
				"progressing sample %d converged = %+v",
				sample,
				state.Convergence,
			)
		}
	}
}

func TestToolAndApprovalLifecycleIsClosedExactlyOnce(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	started := apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "call-1", Name: "shell_read"}},
	})
	if len(started.Effects) != 1 ||
		started.Effects[0].Kind != EffectExecuteTool {
		t.Fatalf("effects = %+v", started.Effects)
	}
	state = apply(t, started.State, ApprovalRequired{
		RequestID: "approval-1", CallID: "call-1",
	}).State
	state = apply(t, state, ApprovalResolved{
		RequestID: "approval-1",
	}).State
	state = apply(t, state, ToolResultReceived{
		CallID: "call-1",
	}).State
	if len(state.OpenCalls) != 0 || len(state.ClosedCalls) != 1 {
		t.Fatalf("tool ledgers = open:%v closed:%v", state.OpenCalls, state.ClosedCalls)
	}
	_, err := (Reducer{}).Apply(state, ToolResultReceived{CallID: "call-1"})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("duplicate result error = %v", err)
	}
}

func TestParallelApprovalsRemainPendingIndependently(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{
			{ID: "call-1", Name: "first"},
			{ID: "call-2", Name: "second"},
		},
	}).State
	state = apply(t, state, ApprovalRequired{
		RequestID: "approval-1", CallID: "call-1",
	}).State
	state = apply(t, state, ApprovalRequired{
		RequestID: "approval-2", CallID: "call-2",
	}).State
	state = apply(t, state, ApprovalResolved{
		RequestID: "approval-1",
	}).State
	if state.Phase != PhaseAwaitingApproval ||
		len(state.PendingApprovals) != 1 {
		t.Fatalf("state after first approval = %+v", state)
	}
	state = apply(t, state, ApprovalResolved{
		RequestID: "approval-2",
	}).State
	if state.Phase != PhaseExecutingTools ||
		len(state.PendingApprovals) != 0 {
		t.Fatalf("state after all approvals = %+v", state)
	}
}

func TestAbortOpenCallsClosesBatchBeforeCancellation(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{
			{ID: "call-b", Name: "second"},
			{ID: "call-a", Name: "first"},
		},
	}).State
	state = apply(t, state, ApprovalRequired{
		RequestID: "approval-1", CallID: "call-a",
	}).State
	aborted := apply(t, state, AbortOpenCalls{Reason: "turn canceled"})
	if aborted.State.Phase != PhaseSampling ||
		len(aborted.State.PendingApprovals) != 0 ||
		len(aborted.State.OpenCalls) != 0 ||
		len(aborted.State.ClosedCalls) != 2 {
		t.Fatalf("aborted = %+v", aborted.State)
	}
	for _, result := range aborted.State.ClosedCalls {
		if !result.IsError {
			t.Fatalf("aborted result = %+v", result)
		}
	}
	state = apply(t, aborted.State, TerminalRequested{CancelReason: "turn canceled"}).State
	state = apply(t, state, FinishTerminal{}).State
	if state.Phase != PhaseCanceled {
		t.Fatalf("state = %+v", state)
	}
}

func TestInputLifecycleMustResolveBeforeTerminal(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, InputRequired{RequestID: "input-1"}).State
	_, err := (Reducer{}).Apply(state, TerminalRequested{CancelReason: "turn canceled"})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("pending-input terminal error = %v", err)
	}
	state = apply(t, state, InputResolved{RequestID: "input-1"}).State
	state = apply(t, state, TerminalRequested{CancelReason: "turn canceled"}).State
	state = apply(t, state, FinishTerminal{}).State
	if state.Phase != PhaseCanceled {
		t.Fatalf("state = %+v", state)
	}
}

func TestFailureRequiresToolClosureAndRollsBackMutation(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "write-1", Name: "file_write"}},
	}).State
	_, err := (Reducer{}).Apply(state, TerminalRequested{FailureMessage: "tool failed"})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("open-call failure error = %v", err)
	}
	state = apply(t, state, ToolResultReceived{
		CallID: "write-1", IsError: true,
		Changes: []ObservedChange{{Path: "partial.txt", Kind: "created"}},
	}).State
	prepared := apply(t, state, TerminalRequested{FailureMessage: "tool failed"})
	if len(prepared.Effects) != 1 ||
		prepared.Effects[0].Kind != EffectRollbackJournal {
		t.Fatalf("effects = %+v", prepared.Effects)
	}
	state = apply(t, prepared.State, JournalFinalized{
		Status: JournalRolledBack,
	}).State
	state = apply(t, state, FinishTerminal{}).State
	if state.Phase != PhaseFailed || state.Journal != JournalRolledBack {
		t.Fatalf("state = %+v", state)
	}
}

func TestTerminalStateHasNoOutgoingTransition(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, TerminalRequested{CancelReason: "user canceled"}).State
	state = apply(t, state, FinishTerminal{}).State
	_, err := (Reducer{}).Apply(state, ModelTextReceived{Text: "late"})
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("error = %v, want illegal transition", err)
	}
}

func TestApplyDoesNotMutateInputAndDigestIsDeterministic(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	before, err := Digest(state)
	if err != nil {
		t.Fatal(err)
	}
	transition := apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{
			{ID: "b", Name: "second"},
			{ID: "a", Name: "first"},
		},
	})
	after, err := Digest(state)
	if err != nil {
		t.Fatal(err)
	}
	if before != after || len(state.OpenCalls) != 0 {
		t.Fatalf("input mutated: before=%s after=%s state=%+v", before, after, state)
	}
	reordered := cloneState(transition.State)
	reordered.OpenCalls = map[string]ToolCallState{
		"a": transition.State.OpenCalls["a"],
		"b": transition.State.OpenCalls["b"],
	}
	left, err := Digest(transition.State)
	if err != nil {
		t.Fatal(err)
	}
	right, err := Digest(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("digest depends on map insertion order: %s != %s", left, right)
	}
}

func TestPhase4R1EffectLifecycleHasStableIdentityAndClosure(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	command := ToolCallsProposed{Calls: []ToolCallState{
		{ID: "call-1", Name: "first"},
		{ID: "call-2", Name: "second"},
	}}
	left := apply(t, state, command)
	right := apply(t, state, command)
	if len(left.Effects) != 2 ||
		!reflect.DeepEqual(left.Effects[0], right.Effects[0]) ||
		!reflect.DeepEqual(left.Effects[1], right.Effects[1]) {
		t.Fatalf("effect identity is not deterministic: left=%+v right=%+v", left.Effects, right.Effects)
	}
	for index, effect := range left.Effects {
		if effect.ID == "" ||
			effect.Kind != EffectExecuteTool ||
			effect.PayloadDigest == "" ||
			effect.IdempotencyKey == "" ||
			effect.Status != EffectRequested ||
			effect.Attempt != 0 {
			t.Fatalf("effect[%d] = %+v", index, effect)
		}
	}
	state = left.State
	firstID := left.Effects[0].ID
	state = apply(t, state, EffectStarted{
		EffectID: firstID,
		Attempt:  1,
	}).State
	state = apply(t, state, ToolResultReceived{
		EffectID: firstID,
		CallID:   "call-1",
	}).State
	state = apply(t, state, AbortOpenCalls{Reason: "stop batch"}).State
	if len(state.PendingEffects) != 0 ||
		len(state.CompletedEffects) != 2 ||
		state.CompletedEffects[firstID].Attempt != 1 ||
		state.CompletedEffects[firstID].Status != EffectSucceeded {
		t.Fatalf("effect ledgers = pending:%+v completed:%+v", state.PendingEffects, state.CompletedEffects)
	}
}

func TestPhase4R1SampleUsageContextAndCancelAreStructured(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, ModelSampleRequested{SampleID: "sample-1"}).State
	effectID := pendingEffectID(state, EffectSampleProvider, "sample-1")
	state = apply(t, state, EffectStarted{
		EffectID: effectID, Attempt: 1,
	}).State
	state = apply(t, state, ProviderRetryRequested{
		EffectID: effectID, SampleID: "sample-1",
		Attempt: 1, Retry: 1,
		Failure: provider.Failure{
			Code: provider.FailureStreamClosed, Message: "unexpected eof",
		},
		EffectiveDelayMS: 125,
		RetryAt:          time.Now(),
		PolicyRevision:   "test/v1",
	}).State
	state = apply(t, state, EffectStarted{
		EffectID: effectID, Attempt: 2,
	}).State
	state = apply(t, state, ModelSampleResultReceived{
		EffectID: effectID, SampleID: "sample-1",
		Usage: UsageState{
			InputTokens: 10, OutputTokens: 3, CostKnown: true,
		},
		Context: ContextState{
			Digest: "sha256:context", HistoryBytes: 128, MaxBytes: 1024,
		},
	}).State
	sample := state.SampleLedger["sample-1"]
	if sample.Status != SampleCompleted ||
		sample.ProviderRetries != 1 ||
		state.Usage.InputTokens != 10 ||
		state.Context.Digest != "sha256:context" {
		t.Fatalf("sample state = %+v usage=%+v context=%+v", sample, state.Usage, state.Context)
	}
	state = apply(t, state, CancelRequested{Reason: "user interrupted"}).State
	if !state.Cancellation.Accepted {
		t.Fatalf("cancellation = %+v", state.Cancellation)
	}
	if _, err := (Reducer{}).Apply(state, ModelSampleStarted{
		SampleID: "sample-2",
		Attempt:  1,
	}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("post-cancel sample error = %v", err)
	}
	if _, err := (Reducer{}).Apply(state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "call-1", Name: "tool"}},
	}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("post-cancel tool error = %v", err)
	}
}

func TestSO4SupplementalUsageJoinsFrozenKernelAuthority(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, SupplementalUsageRecorded{
		Source: "vision", SampleID: "tool-sample-1",
		Usage: UsageState{
			InputTokens:    1500,
			CostMicrounits: 20,
			CostKnown:      true,
		},
	}).State
	state = apply(t, state, SupplementalUsageRecorded{
		Source: "sub_query", SampleID: "tool-sample-2",
		Usage: UsageState{
			InputTokens: 20,
			CostKnown:   false,
		},
	}).State
	if state.Usage.Calls != 2 ||
		state.Usage.InputTokens != 1520 ||
		state.Usage.CostMicrounits != 20 ||
		state.Usage.CostKnown {
		t.Fatalf("usage = %+v", state.Usage)
	}
	state = apply(t, state, ModelTextReceived{Text: "done"}).State
	state = apply(t, state, ReleaseProvisionalOutput{}).State
	state = apply(t, state, TerminalRequested{}).State
	if !state.Usage.Frozen {
		t.Fatal("terminal request did not freeze supplemental usage")
	}
	if _, err := (Reducer{}).Apply(state, SupplementalUsageRecorded{
		Source: "late", SampleID: "late",
	}); err == nil {
		t.Fatal("late usage changed frozen terminal state")
	}
}

func TestPermanentProviderFailureIsPersistedWithoutRetry(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, ModelSampleRequested{SampleID: "sample-auth"}).State
	effectID := pendingEffectID(state, EffectSampleProvider, "sample-auth")
	state = apply(t, state, EffectStarted{
		EffectID: effectID, Attempt: 1,
	}).State
	failure := &provider.Failure{
		Code: provider.FailureAuth, Message: "invalid credential",
		HTTPStatus: 401, RequestID: "request-1",
	}
	state = apply(t, state, ModelSampleResultReceived{
		EffectID: effectID, SampleID: "sample-auth",
		Error: failure.Message, Failure: failure,
	}).State
	sample := state.SampleLedger["sample-auth"]
	if sample.Status != SampleFailed ||
		sample.ProviderRetries != 0 ||
		sample.LastFailure == nil ||
		sample.LastFailure.Code != provider.FailureAuth ||
		sample.LastFailure.HTTPStatus != 401 ||
		sample.LastFailure.RequestID != "request-1" {
		t.Fatalf("sample = %+v", sample)
	}
}

func TestPhase4R1RecoveryBindsCurrentProfileRevision(t *testing.T) {
	state := NewState(protocol.TurnIntentWorkspaceChange, "act", 2)
	state = apply(t, state, RecoveryRequested{
		SourceTurnID:           "source-turn",
		RecoveryTurnID:         "recovery-turn",
		CurrentProfileRevision: 9,
		Action:                 string(protocol.TurnRecoveryContinue),
	}).State
	if state.ProfileRevision != 9 ||
		state.RecoveryRelation == nil ||
		state.RecoveryRelation.SourceTurnID != "source-turn" ||
		state.RecoveryRelation.RecoveryTurnID != "recovery-turn" {
		t.Fatalf("recovery state = %+v", state)
	}
}

func TestPhase4R1ApprovalAndInputResolveThroughEffectResults(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	started := apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "call-1", Name: "write"}},
	})
	state = apply(t, started.State, ApprovalRequired{
		RequestID: "approval-1",
		CallID:    "call-1",
	}).State
	approvalEffect := effectIDForKind(t, state, EffectAwaitApproval)
	state = apply(t, state, ApprovalResultReceived{
		EffectID:  approvalEffect,
		RequestID: "approval-1",
		Accepted:  true,
	}).State
	_, approvalPending := state.PendingEffects[approvalEffect]
	if state.Phase != PhaseExecutingTools ||
		approvalPending ||
		state.CompletedEffects[approvalEffect].Status != EffectSucceeded {
		t.Fatalf("approval result state = %+v", state)
	}
	state = apply(t, state, ToolResultReceived{CallID: "call-1"}).State
	state = apply(t, state, InputRequired{RequestID: "input-1"}).State
	inputEffect := effectIDForKind(t, state, EffectAwaitInput)
	state = apply(t, state, InputResultReceived{
		EffectID:  inputEffect,
		RequestID: "input-1",
		Accepted:  true,
	}).State
	if state.Phase != PhaseSampling ||
		state.PendingInput != nil ||
		state.CompletedEffects[inputEffect].Status != EffectSucceeded {
		t.Fatalf("input result state = %+v", state)
	}
}

func TestPhase4R1OutputRemainsProvisionalUntilTerminalCommit(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, ModelTextReceived{Text: "final answer"}).State
	state = apply(t, state, ReleaseProvisionalOutput{}).State
	if !state.OutputEligibility ||
		len(state.ProvisionalOutput) != 1 ||
		len(state.FinalOutput) != 0 {
		t.Fatalf("eligible output = %+v", state)
	}
	state = apply(t, state, TerminalRequested{}).State
	if len(state.ProvisionalOutput) != 1 ||
		!state.Usage.Frozen ||
		!state.Context.Frozen {
		t.Fatalf("committing output = %+v", state)
	}
	state = apply(t, state, FinishTerminal{}).State
	if len(state.ProvisionalOutput) != 0 ||
		len(state.FinalOutput) != 1 ||
		state.FinalOutput[0] != "final answer" {
		t.Fatalf("terminal output = %+v", state)
	}
}

func effectIDForKind(t *testing.T, state State, kind EffectKind) string {
	t.Helper()
	for effectID, effect := range state.PendingEffects {
		if effect.Kind == kind {
			return effectID
		}
	}
	t.Fatalf("pending effect %s not found: %+v", kind, state.PendingEffects)
	return ""
}

func TestValidateRejectsForgedReplayState(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*State)
	}{
		{
			name: "open call outside tool phase",
			mutate: func(state *State) {
				state.OpenCalls["call-1"] = ToolCallState{
					ID: "call-1", Name: "fixture",
				}
			},
		},
		{
			name: "completion paths disagree",
			mutate: func(state *State) {
				*state = verifiedMutation(t)
				state.Completion.ChangedPaths = []string{"forged.go"}
			},
		},
		{
			name: "completed mutation without journal",
			mutate: func(state *State) {
				*state = verifiedMutation(t)
				state.Phase = PhaseCompleted
				state.Terminal = &TerminalDecision{Kind: TerminalCompleted}
				state.Journal = JournalOpen
			},
		},
		{
			name: "terminal phase disagrees",
			mutate: func(state *State) {
				state.Phase = PhaseFailed
				state.Terminal = &TerminalDecision{Kind: TerminalCanceled, Message: "canceled"}
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			state := startSampling(t, protocol.TurnIntentAnswer)
			testCase.mutate(&state)
			if err := Validate(state); err == nil {
				t.Fatalf("forged state was accepted: %+v", state)
			}
			if _, err := Digest(state); err == nil {
				t.Fatal("forged state received a digest")
			}
		})
	}
}

func verifiedMutation(t *testing.T) State {
	t.Helper()
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "write-1", Name: "file_write"}},
	}).State
	state = apply(t, state, ToolResultReceived{
		CallID:  "write-1",
		Changes: []ObservedChange{{Path: "a.go", Kind: "modified"}},
	}).State
	state = apply(t, state, CompletionEvaluated{
		Candidate: CompletionCandidate{
			DeclarationValid: true,
			Status:           "complete",
			Summary:          "implemented",
			CompletionCall:   "complete-1",
			BatchSize:        1,
		},
	}).State
	state = apply(t, state, VerificationStarted{}).State
	return apply(t, state, VerificationFinished{
		Status: VerificationPassed, EvidenceCalls: []string{"verify-1"},
	}).State
}

func startSampling(t *testing.T, intent protocol.TurnIntent) State {
	t.Helper()
	state := NewState(intent, "act", 1)
	state = apply(t, state, StartTurn{}).State
	return apply(t, state, PreparationFinished{}).State
}

func apply(t *testing.T, state State, command Command) Transition {
	t.Helper()
	switch value := command.(type) {
	case ToolResultReceived:
		if value.EffectID == "" {
			value.EffectID = pendingEffectID(
				state,
				EffectExecuteTool,
				value.CallID,
			)
			command = value
		}
		state = startPendingEffect(t, state, value.EffectID)
	case ApprovalResultReceived:
		state = startPendingEffect(t, state, value.EffectID)
	case InputResultReceived:
		state = startPendingEffect(t, state, value.EffectID)
	case VerificationFinished:
		if value.EffectID == "" {
			value.EffectID = pendingEffectID(
				state,
				EffectRunVerification,
				"",
			)
			command = value
		}
		state = startPendingEffect(t, state, value.EffectID)
	}
	transition, err := (Reducer{}).Apply(state, command)
	if err != nil {
		t.Fatalf("Apply(%T): %v", command, err)
	}
	return transition
}

func pendingEffectID(
	state State,
	kind EffectKind,
	callID string,
) string {
	for _, effectID := range sortedEffectIDs(state.PendingEffects) {
		effect := state.PendingEffects[effectID]
		if effect.Kind == kind &&
			(callID == "" || effect.CallID == callID) {
			return effectID
		}
	}
	return ""
}

func TestRejectedCompletionDoesNotRenewProgressOrRepairKeys(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentWorkspaceChange)
	state = apply(t, state, ToolCallsProposed{
		Calls: []ToolCallState{{ID: "write-1", Name: "file_write"}},
	}).State
	state = apply(t, state, ToolResultReceived{
		CallID:  "write-1",
		Changes: []ObservedChange{{Path: "a.go", Kind: "modified"}},
	}).State
	first := apply(t, state, CompletionEvaluated{
		Candidate: CompletionCandidate{
			DeclarationValid: true,
			Status:           "complete",
			Summary:          "done",
			PendingActions:   []string{"still open"},
			CompletionCall:   "complete-1",
			BatchSize:        1,
		},
	}).State
	second := apply(t, state, CompletionEvaluated{
		Candidate: CompletionCandidate{
			DeclarationValid: true,
			Status:           "complete",
			Summary:          "done again",
			PendingActions:   []string{"still open"},
			CompletionCall:   "complete-2",
			BatchSize:        1,
		},
	}).State
	if first.Completion == nil || first.Completion.Accepted ||
		FormatProgressSignature(state, 1, false) !=
			FormatProgressSignature(first, 1, false) ||
		FormatProgressSignature(first, 1, false) !=
			FormatProgressSignature(second, 1, false) {
		t.Fatalf(
			"rejected completion renewed progress: before=%q first=%q second=%q",
			FormatProgressSignature(state, 1, false),
			FormatProgressSignature(first, 1, false),
			FormatProgressSignature(second, 1, false),
		)
	}
	if FormatRepairProgressKey(first) != FormatRepairProgressKey(second) {
		t.Fatalf(
			"rejected completion renewed repair key: %q vs %q",
			FormatRepairProgressKey(first),
			FormatRepairProgressKey(second),
		)
	}
	state.ProvisionalOutput = []string{"working"}
	state.LastModelContinued = false
	state.UnresolvedToolFailure = false
	state.Policy.StructuredTerminalRequired = true
	state.Policy.DeclarationRepairLimit = 1
	state = apply(t, state, CompletionEvaluated{
		Candidate: CompletionCandidate{
			DeclarationValid: true,
			Status:           "complete",
			Summary:          "done",
			PendingActions:   []string{"still open"},
			CompletionCall:   "complete-3",
			BatchSize:        1,
		},
	}).State
	key := FormatRepairProgressKey(state)
	firstStep := apply(t, state, EvaluateTurnStep{ProgressKey: key})
	if firstStep.State.NextAction != StepActionRepairDeclaration {
		t.Fatalf("first repair action = %q", firstStep.State.NextAction)
	}
	secondStep, err := (Reducer{}).Apply(
		firstStep.State,
		EvaluateTurnStep{ProgressKey: key},
	)
	if err != nil {
		t.Fatal(err)
	}
	if secondStep.State.NextAction != StepActionFinalize ||
		secondStep.State.Convergence == nil ||
		secondStep.State.Convergence.Cause != ConvergenceRepairBudget {
		t.Fatalf("same-reason rejection did not converge: %+v", secondStep.State)
	}
}

func startPendingEffect(t *testing.T, state State, effectID string) State {
	t.Helper()
	effect, ok := state.PendingEffects[effectID]
	if !ok || effect.Status != EffectRequested {
		return state
	}
	transition, err := (Reducer{}).Apply(state, EffectStarted{
		EffectID: effectID,
		Attempt:  effect.Attempt + 1,
	})
	if err != nil {
		t.Fatalf("Apply(EffectStarted): %v", err)
	}
	return transition.State
}

func TestPreserveProvisionalAvailableOutsideConvergenceFinalization(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state.ProvisionalOutput = []string{"narrated answer"}
	state = apply(t, state, CompletionEvaluated{
		Candidate: CompletionCandidate{
			DeclarationValid: true,
			Status:           "complete",
			Summary:          "Closing note.",
			OutputMode:       "preserve_provisional",
			CompletionCall:   "complete-1",
			BatchSize:        1,
		},
	}).State
	if state.Completion == nil || !state.Completion.Accepted ||
		len(state.ProvisionalOutput) != 2 ||
		state.ProvisionalOutput[0] != "narrated answer" ||
		state.ProvisionalOutput[1] != "\n\nClosing note." {
		t.Fatalf("preserved completion = %+v", state)
	}
}

func TestPreserveProvisionalStillRequiresCapturedOutput(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentAnswer)
	state = apply(t, state, CompletionEvaluated{
		Candidate: CompletionCandidate{
			DeclarationValid: true,
			Status:           "complete",
			Summary:          "Closing note.",
			OutputMode:       "preserve_provisional",
			CompletionCall:   "complete-1",
			BatchSize:        1,
		},
	}).State
	if state.Completion == nil || state.Completion.Accepted ||
		state.Completion.Reason != "provisional_output_unavailable" {
		t.Fatalf("empty preserve completion = %+v", state.Completion)
	}
}
