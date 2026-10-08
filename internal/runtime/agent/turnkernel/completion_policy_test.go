package turnkernel

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestNoChangeCompletionRequiresObservedWorkspaceAndReadEvidence(t *testing.T) {
	state := startSampling(t, protocol.TurnIntentWorkspaceChange)
	state = apply(t, state, ToolCallsProposed{Calls: []ToolCallState{{ID: "read", Name: "file_read"}}}).State
	state = apply(t, state, ToolResultReceived{CallID: "read", Observation: WorkItemObservation{
		ReadPath: "a.go", CallID: "read", ContentDigest: "baseline",
	}}).State
	base := CompletionCandidate{DeclarationValid: true, Status: "complete", Summary: "Already implemented", BatchSize: 1, CompletionCall: "complete",
		NoChangeReason: "The requested behavior is already present in a.go", NoChangeEvidence: []string{"read"}}
	if got := apply(t, state, CompletionEvaluated{Candidate: base}).State.Completion; got.Accepted {
		t.Fatal("accepted no-change without workspace reconciliation")
	}
	state = apply(t, state, WorkspaceReconciled{}).State
	for _, calls := range [][]string{nil, {"unknown"}, {"complete"}} {
		candidate := base
		candidate.NoChangeEvidence = calls
		if got := apply(t, state, CompletionEvaluated{Candidate: candidate}).State.Completion; got.Accepted {
			t.Fatalf("accepted invalid evidence: %v", calls)
		}
	}
	state = apply(t, state, CompletionEvaluated{Candidate: base}).State
	if !state.Completion.Accepted {
		t.Fatalf("decision: %+v", state.Completion)
	}
	state = apply(t, state, EvaluateTurnStep{ProgressKey: "no-change"}).State
	if state.NextAction != StepActionVerify {
		t.Fatalf("action: %s", state.NextAction)
	}
	state = apply(t, state, VerificationStarted{}).State
	state = apply(t, state, VerificationFinished{Status: VerificationNotRequired}).State
	if err := validateCompletionPolicy(state); err != nil {
		t.Fatal(err)
	}
	state = apply(t, state, ToolCallsProposed{Calls: []ToolCallState{{ID: "write", Name: "file_write"}}}).State
	state = apply(t, state, ToolResultReceived{CallID: "write", Changes: []ObservedChange{{Path: "a.go", Kind: "modified"}}}).State
	if state.Workspace != nil || state.Completion != nil || !verificationPending(state) {
		t.Fatal("mutation retained stale readiness")
	}
}

func TestNotRequiredCannotApproveRemainingChanges(t *testing.T) {
	state := verifiedMutation(t)
	state = apply(t, state, WorkspaceReconciled{Mutation: state.MutationRevision, Changes: []ObservedChange{{Path: "a.go", Kind: "modified"}}}).State
	state = apply(t, state, VerificationStarted{}).State
	effectID := pendingEffectID(state, EffectRunVerification, "")
	state = startPendingEffect(t, state, effectID)
	if _, err := (Reducer{}).Apply(state, VerificationFinished{EffectID: effectID, Status: VerificationNotRequired}); err == nil {
		t.Fatal("remaining changes were exempted from verification")
	}
}

func TestWorkspaceReconciliationFencesRevisionAndOpenTools(t *testing.T) {
	state := verifiedMutation(t)
	if _, err := (Reducer{}).Apply(state, WorkspaceReconciled{}); err == nil {
		t.Fatal("stale workspace observation accepted")
	}
	state = apply(t, state, ToolCallsProposed{Calls: []ToolCallState{{ID: "open", Name: "file_write"}}}).State
	if _, err := (Reducer{}).Apply(state, WorkspaceReconciled{Mutation: state.MutationRevision}); err == nil {
		t.Fatal("workspace observation accepted while tools remain open")
	}
}
