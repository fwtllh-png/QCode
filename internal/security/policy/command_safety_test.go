package policy

import (
	"fmt"
	"testing"
)

// shellCommand creates a run_command invocation with a command and cwd,
// classified as a Strong Sandbox process writing to the workspace.
func shellCommand(command string) invocationFixture {
	call := invocation("run_command", fmt.Sprintf("adv-%d", len(command)),
		fmt.Sprintf(`{"command":%q,"cwd":"/workspace"}`, command))
	return call
}

// TestAdversarialPrefixCommandsStillRequireApproval runs each adversarial
// command through the real policy pipeline under Auto posture and asserts
// the decision is Ask, not Allow. If someone reintroduces prefix-based
// authorization, these tests fail because the commands match the removed
// safe prefixes.
func TestAdversarialPrefixCommandsStillRequireApproval(t *testing.T) {
	for _, input := range adversarialPrefixInputs {
		t.Run(input.Command, func(t *testing.T) {
			runtime := DefaultRuntime(ModeAct, PermissionAuto)
			decision := runtime.Decide(resolveFixture(shellCommand(input.Command)))
			if decision.Action == ActionAllow {
				t.Fatalf("adversarial command %q (prefix %q, bypass: %s) "+
					"was auto-allowed with code=%q layer=%s; "+
					"prefix authorization may have been reintroduced",
					input.Command, input.Prefix, input.Bypass,
					decision.Code, decision.Layer)
			}
		})
	}
}

// TestAutoReviewDoesNotOverrideUserAsk verifies that the typed-grant auto
// review cannot override an explicit User Ask. The decision must remain
// Ask even when the invocation would otherwise be eligible for auto review.
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
// auto review cannot satisfy a Fresh approval requirement. The binding
// layer sets Approval=FreshOnce for tools declaring ApprovalOnce; the auto
// review must not convert this to Allow.
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
// mechanical hold prevents auto review.
func TestAutoReviewDoesNotOverrideRepositoryAsk(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionAuto)
	runtime.Repository = []Rule{{
		Tool: "run_command", Action: ActionAsk,
	}}
	call := shellCommand("echo hello")
	decision := runtime.Decide(resolveFixture(call))
	if decision.Action == ActionAllow {
		t.Fatalf("Repository Ask was overridden: action=%s code=%q layer=%s",
			decision.Action, decision.Code, decision.Layer)
	}
}

// TestAutoReviewDoesNotOverrideManagedAsk verifies that a managed tool
// grant with ActionAsk prevents auto review.
func TestAutoReviewDoesNotOverrideManagedAsk(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionAuto)
	// Find and modify the run_command grant to Ask
	for i, grant := range runtime.Grants {
		if grant.Tool == "run_command" {
			runtime.Grants[i].Action = ActionAsk
			break
		}
	}
	call := shellCommand("echo hello")
	decision := runtime.Decide(resolveFixture(call))
	if decision.Action == ActionAllow {
		t.Fatalf("Managed Ask was overridden: action=%s code=%q layer=%s",
			decision.Action, decision.Code, decision.Layer)
	}
}
