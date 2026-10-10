package policy

import (
	"fmt"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

// shellCommand creates a run_command invocation for policy pipeline testing.
func shellCommand(command string) invocationFixture {
	return invocation("run_command", fmt.Sprintf("adv-%d", len(command)),
		fmt.Sprintf(`{"command":%q,"cwd":"/workspace"}`, command))
}

// mediumRiskShellCommand creates a run_command invocation with a command
// argument and an explicit fixed medium-risk effect. This is the exact
// classification the old safe_command_allowed pathway targeted: a shell
// command classified as medium risk under Auto that posture converts to
// Ask. If the old prefix pathway is restored, it would auto-allow this
// invocation because the command matches a known prefix and the risk is
// medium (not high, which the old code excluded).
func mediumRiskShellCommand(command string) invocationFixture {
	call := invocation("run_command", fmt.Sprintf("med-%d", len(command)),
		fmt.Sprintf(`{"command":%q}`, command))
	call.Effect = tool.EffectContract{
		Mode:          tool.EffectFixed,
		Kind:          tool.EffectProcessMutating,
		Risk:          tool.RiskMedium,
		Reversibility: tool.Bounded,
	}
	call.Resources = nil
	return call
}

// TestAdversarialPrefixCommandsStillRequireApproval uses medium-risk shell
// commands that the old safe_command_allowed pathway would have auto-allowed.
// If the prefix pathway is restored, these tests fail because the commands
// match the removed safe prefixes AND have medium risk AND are under Auto.
func TestAdversarialPrefixCommandsStillRequireApproval(t *testing.T) {
	t.Run("baseline medium risk produces Ask", func(t *testing.T) {
		runtime := DefaultRuntime(ModeAct, PermissionAuto)
		decision := runtime.Decide(resolveFixture(mediumRiskShellCommand("some arbitrary command")))
		if decision.Action != ActionAsk {
			t.Fatalf("medium-risk shell command should Ask under Auto: action=%s code=%q layer=%s",
				decision.Action, decision.Code, decision.Layer)
		}
	})
	for _, input := range adversarialPrefixInputs {
		t.Run(input.Command, func(t *testing.T) {
			runtime := DefaultRuntime(ModeAct, PermissionAuto)
			decision := runtime.Decide(resolveFixture(mediumRiskShellCommand(input.Command)))
			if decision.Code == "safe_command_allowed" {
				t.Fatalf("prefix authorization was reintroduced for %q (prefix %q, bypass: %s): code=%q",
					input.Command, input.Prefix, input.Bypass, decision.Code)
			}
			if decision.Action == ActionAllow && decision.Layer == LayerAutoReview {
				t.Fatalf("adversarial command %q was auto-allowed: action=%s code=%q layer=%s",
					input.Command, decision.Action, decision.Code, decision.Layer)
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

// TestAutoReviewDoesNotOverrideFreshOnce verifies that the typed-grant
// auto review cannot satisfy an ApprovalOnce binding requirement.
func TestAutoReviewDoesNotOverrideFreshOnce(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionAuto)
	call := networkReadCall("example.com")
	call.Effect.Approval = "once_required"
	decision := runtime.Decide(resolveFixture(call))
	if decision.Action != ActionAsk {
		t.Fatalf("FreshOnce was overridden: action=%s code=%q layer=%s approval=%q",
			decision.Action, decision.Code, decision.Layer, decision.Approval)
	}
	if decision.Layer == LayerAutoReview {
		t.Fatalf("auto review fired despite FreshOnce: %+v", decision)
	}
}

// TestAutoReviewDoesNotOverrideUserAskPlusFreshOnce verifies the combination.
func TestAutoReviewDoesNotOverrideUserAskPlusFreshOnce(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionAuto)
	runtime.User = []Rule{{
		Tool: "fetch_page", Action: ActionAsk,
	}}
	call := networkReadCall("example.com")
	call.Effect.Approval = "once_required"
	decision := runtime.Decide(resolveFixture(call))
	if decision.Action != ActionAsk {
		t.Fatalf("User Ask + FreshOnce was overridden: action=%s code=%q layer=%s",
			decision.Action, decision.Code, decision.Layer)
	}
}

// TestAutoReviewDoesNotOverrideForceEditPlanApproval verifies that a
// journaled tool under ForceEditPlanApproval stays Ask even when the
// auto-review eligibility conditions are otherwise met. The old prefix
// pathway did not check ForceEditPlanApproval and would have auto-allowed.
func TestAutoReviewDoesNotOverrideForceEditPlanApproval(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionAuto)
	runtime.ForceEditPlanApproval = true
	call := mediumRiskShellCommand("git add .")
	call.Journaled = true
	decision := runtime.Decide(resolveFixture(call))
	if decision.Action == ActionAllow {
		t.Fatalf("ForceEditPlanApproval was overridden: action=%s code=%q layer=%s",
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
	runtime.Grants = append(runtime.Grants, Rule{
		Tool: "fetch_page", Action: ActionAsk,
	})
	decision := runtime.Decide(resolveFixture(networkReadCall("example.com")))
	if decision.Action == ActionAllow {
		t.Fatalf("Managed Ask was overridden: action=%s code=%q layer=%s",
			decision.Action, decision.Code, decision.Layer)
	}
}
