package turnkernel

import (
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"testing"
)

func TestRestoreRetiresLegacyGateWithoutRewritingHistory(t *testing.T) {
	legacy := verifiedMutation(t)
	legacy.Policy.VerificationRequired = true
	legacy.Policy.VerificationMustPass = true
	legacy.Policy.VerificationMode = "hard"
	legacy.Policy.VerificationOnFailure = "fail"
	legacy.Policy.VerificationRepairLimit = 1
	legacy.WorkItem.Open.UnverifiedPaths = []string{"a.go"}
	legacy.Phase = PhaseVerifying
	transition := Transition{State: legacy}
	requestEffect(&transition, EffectRunVerification, map[string]uint64{"mutation_revision": legacy.MutationRevision}, "legacy-verification", "")
	legacy = transition.State
	digest, err := Digest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryTerminalEnvelopeStore(nil, nil)
	const id = "legacy-turn"
	original := DomainFact{TurnID: id, Sequence: 1, Command: "verification_started", State: legacy, StateDigest: digest}
	if err := store.AppendDomainFacts(t.Context(), id, 1, []DomainFact{original}); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := NewRuntimeKernel(KernelIdentity{TurnID: id, ProfileRevision: 1}, protocol.TurnIntentWorkspaceChange, "act", nil, false, nil, nil, nil, nil, nil, DefaultPolicy(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	state := kernel.Snapshot()
	if state.Phase != PhaseSampling || legacyVerificationActive(state) || len(state.PendingEffects) != 0 {
		t.Fatalf("restored legacy gate remains active: %+v", state)
	}
	for _, effect := range state.CompletedEffects {
		if effect.Kind == EffectRunVerification && (effect.Status != EffectFailed || effect.Error == "") {
			t.Fatalf("retired check invented success: %+v", effect)
		}
	}
	facts, err := store.LoadDomainFacts(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if facts[0].StateDigest != digest || facts[0].State.Phase != PhaseVerifying {
		t.Fatal("historical state was rewritten")
	}
	if facts[len(facts)-1].Command != "verification_retired" {
		t.Fatalf("missing durable retirement: %+v", facts)
	}
	if err := ValidateDomainFacts(id, facts); err != nil {
		t.Fatal(err)
	}
	action, err := kernel.EvaluateTurnStep("after-retirement")
	if err != nil || action != StepActionComplete {
		t.Fatalf("old policy still blocks completion: %s %v", action, err)
	}
}

func TestModelCoverageDoesNotCreateWorkObligationsOrProgress(t *testing.T) {
	call := ToolCallState{ID: "check", Name: "exec_command", Arguments: `{"command":"true","covered_paths":["a.go"]}`}
	observation := ObserveWorkItemResult(call, tool.Result{})
	if len(observation.CoveredPaths) != 0 {
		t.Fatal("trusted model supplied coverage")
	}
}
