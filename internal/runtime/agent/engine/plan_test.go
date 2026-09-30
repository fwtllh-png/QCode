package engine

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
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
			{Title: "inspect", Status: interact.StepDone},
			{Title: "implement", Status: interact.StepInProgress},
			{Title: "verify", Status: interact.StepPending},
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
