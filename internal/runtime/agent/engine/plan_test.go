package engine

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestApplyPlanPreservesAndOwnsToolPayload(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	input := interact.Plan{
		Title: "Parser fix", Notes: "keep behavior", Objective: "handle overflow",
		ContextSummary: "parser review", SourcesUsed: []string{"review"},
		CriticalFiles: []string{"parser.go"}, Constraints: []string{"preserve API"},
		RecommendedApproach: "validate input", VerificationPlan: "go test",
		RisksAndUnknowns: "integer width", HandoffPacket: "check boundary cases",
		Steps: []interact.PlanStep{
			{ID: "inspect-id", Title: "inspect", Status: interact.StepDone},
			{ID: "implement-id", Title: "implement", Status: interact.StepInProgress},
			{ID: "verify-id", Title: "verify", Status: interact.StepPending},
		},
	}
	want, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplyPlan(input); err != nil {
		t.Fatal(err)
	}
	input.Steps[0].Title = "changed by caller"
	input.SourcesUsed[0] = "changed by caller"
	input.CriticalFiles[0] = "changed by caller"
	input.Constraints[0] = "changed by caller"
	plan := engine.currentPlan()
	got, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("runtime plan = %s, want %s", got, want)
	}
	if engine.planText != promptcontext.FormatPlan(plan) {
		t.Fatalf("plan projection is inconsistent: %q", engine.planText)
	}
	if expected := promptcontext.PlanReceipt(plan); engine.planReceipt == nil ||
		!reflect.DeepEqual(*engine.planReceipt, expected) {
		t.Fatalf("plan receipt = %+v, want %+v", engine.planReceipt, expected)
	}
}

func TestContinuationPlanningRestoresOnlyMatchingSubmission(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		policy    string
		submitted bool
		want      bool
	}{
		{"submitted", "adaptive", true, true},
		{"plan_text_only", "adaptive", false, false},
		{"changed_policy", "off", true, false},
		{"missing_policy", "", true, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
			runtime := engine.guard.Policy()
			runtime.ConfigurePlanning(policy.PlanningAdaptive)
			runtime.SetPermission(policy.PermissionNever)
			engine.restoreContinuationPlanning(agentcontext.TurnContinuation{
				PlanningPolicy: scenario.policy, PlanSubmitted: scenario.submitted,
				Plan: &agentcontext.Plan{Steps: []agentcontext.PlanStep{{Title: "a plan is not submission evidence"}}},
			})
			if runtime.PlanningSnapshot().PlanSubmitted != scenario.want || runtime.PermissionValue() != policy.PermissionNever {
				t.Fatal("restoration changed the current permission or inferred submission from plan text")
			}
			revision := runtime.Revision
			engine.restoreContinuationPlanning(agentcontext.TurnContinuation{PlanningPolicy: scenario.policy, PlanSubmitted: scenario.submitted})
			if runtime.Revision != revision {
				t.Fatal("repeated restoration changed policy revision")
			}
		})
	}
}
