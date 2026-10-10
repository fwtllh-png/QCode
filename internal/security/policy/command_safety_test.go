package policy

import (
	"fmt"
	"testing"
)

// shellCommand creates a run_command invocation for policy pipeline testing.
func shellCommand(command string) invocationFixture {
	return invocation("run_command", fmt.Sprintf("adv-%d", len(command)),
		fmt.Sprintf(`{"command":%q,"cwd":"/workspace"}`, command))
}

// TestAdversarialPrefixCommandsStillRequireApproval verifies that the
// safe_command_allowed pathway stays removed. It uses fetch_page
// (medium-risk network read under Auto) because the old prefix pathway
// targeted exactly this classification: medium-risk Ask with a typed
// grant that auto review could satisfy. High-risk shell commands were
// already excluded by the old code and would not detect reintroduction.
//
// The test checks two things:
//  1. A known auto-review-eligible invocation does NOT produce
//     safe_command_allowed (the removed prefix pathway's code).
//  2. The adversarial command itself, when run through the full pipeline,
//     does not produce safe_command_allowed.
func TestAdversarialPrefixCommandsStillRequireApproval(t *testing.T) {
	for _, input := range adversarialPrefixInputs {
		t.Run(input.Command, func(t *testing.T) {
			runtime := DefaultRuntime(ModeAct, PermissionAuto)
			// The medium-risk baseline: this normally auto-reviews via
			// the typed-grant path. If the prefix pathway is reintroduced,
			// it might also fire, producing safe_command_allowed.
			baseline := runtime.Decide(resolveFixture(networkReadCall("example.com")))
			if baseline.Code == "safe_command_allowed" {
				t.Fatalf("prefix authorization was reintroduced (baseline): code=%q", baseline.Code)
			}
			// The adversarial shell command must not be auto-allowed
			// via the prefix pathway.
			shell := shellCommand(input.Command)
			shellDecision := runtime.Decide(resolveFixture(shell))
			if shellDecision.Code == "safe_command_allowed" {
				t.Fatalf("adversarial command %q (prefix %q, bypass: %s) was auto-allowed via prefix: code=%q",
					input.Command, input.Prefix, input.Bypass, shellDecision.Code)
			}
		})
	}
}

// TestAutoReviewDoesNotOverrideUserAsk verifies that the typed-grant auto
// review cannot override an explicit User Ask.
func TestAutoReviewDoesNotOverrideUserAsk(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionAuto)
	runtime.User = []Rule{{
		Tool: "fetch_page", Action: ActionAsk,
	}}
	decision := runtime.Decide(resolveFixture(networkReadCall("example.com")))
	if decision.Action != ActionAsk {
		t.Fatalf("User Ask was overridden: action=%s code=%q layer=%s",
			decision.Action, decision.Code, decision.Layer)
	}
	if decision.Layer == LayerAutoReview {
		t.Fatalf("auto review fired despite User Ask: %+v", decision)
	}
}

// TestAutoReviewDoesNotOverrideFreshApproval verifies that the typed-grant
// auto review cannot satisfy a Fresh (once_required) approval requirement.
func TestAutoReviewDoesNotOverrideFreshApproval(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionAuto)
	call := networkReadCall("example.com")
	call.Effect.Approval = "once_required"
	decision := runtime.Decide(resolveFixture(call))
	if decision.Action != ActionAsk {
		t.Fatalf("Fresh approval was overridden: action=%s code=%q layer=%s approval=%q",
			decision.Action, decision.Code, decision.Layer, decision.Approval)
	}
	if decision.Layer == LayerAutoReview {
		t.Fatalf("auto review fired despite Fresh approval: %+v", decision)
	}
}

// TestAutoReviewDoesNotOverrideUserAskPlusFresh verifies that the
// combination of User Ask and Fresh approval is not auto-reviewed.
func TestAutoReviewDoesNotOverrideUserAskPlusFresh(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionAuto)
	runtime.User = []Rule{{
		Tool: "fetch_page", Action: ActionAsk,
	}}
	call := networkReadCall("example.com")
	call.Effect.Approval = "once_required"
	decision := runtime.Decide(resolveFixture(call))
	if decision.Action != ActionAsk {
		t.Fatalf("User Ask + Fresh was overridden: action=%s code=%q layer=%s",
			decision.Action, decision.Code, decision.Layer)
	}
}

// TestAutoReviewDoesNotOverrideRepositoryAsk verifies that a Repository
// approval requirement prevents auto review.
func TestAutoReviewDoesNotOverrideRepositoryAsk(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionAuto)
	runtime.Repository = []Rule{{
		Tool: "fetch_page", Action: ActionAsk,
	}}
	decision := runtime.Decide(resolveFixture(networkReadCall("example.com")))
	if decision.Action == ActionAllow {
		t.Fatalf("Repository Ask was overridden: action=%s code=%q layer=%s",
			decision.Action, decision.Code, decision.Layer)
	}
}

// TestAutoReviewDoesNotOverrideManagedAsk verifies that a managed tool
// grant with ActionAsk prevents auto review.
func TestAutoReviewDoesNotOverrideManagedAsk(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionAuto)
	// Add a specific fetch_page grant with Ask; it outranks the wildcard.
	runtime.Grants = append(runtime.Grants, Rule{
		Tool: "fetch_page", Action: ActionAsk,
	})
	decision := runtime.Decide(resolveFixture(networkReadCall("example.com")))
	if decision.Action == ActionAllow {
		t.Fatalf("Managed Ask was overridden: action=%s code=%q layer=%s",
			decision.Action, decision.Code, decision.Layer)
	}
}
