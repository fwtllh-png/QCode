package agentcontext

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestPlanStepsAcceptBothShapes(t *testing.T) {
	var plan Plan
	raw := `{"steps":["bare step",{"title":"typed step","status":"in_progress"},{"title":"odd","status":"WAT"}]}`
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		t.Fatal(err)
	}
	want := []PlanStep{
		{Title: "bare step", Status: StepPending},
		{Title: "typed step", Status: StepInProgress},
		{Title: "odd", Status: StepPending},
	}
	for index, step := range want {
		if !reflect.DeepEqual(plan.Steps[index], step) {
			t.Fatalf("step %d = %+v, want %+v", index, plan.Steps[index], step)
		}
	}
}

func TestOutstandingStepsCountsFinishedWork(t *testing.T) {
	plan := Plan{Steps: []PlanStep{
		{Title: "a", Status: StepDone},
		{Title: "b", Status: StepInProgress},
	}}
	open, done := plan.OutstandingSteps()
	if done != 1 || len(open) != 1 || open[0].Title != "b" {
		t.Fatalf("open = %+v done = %d", open, done)
	}
}

func TestPlanTruthIdentitySurvivesRenameAndReorder(t *testing.T) {
	plan := Plan{Steps: []PlanStep{{ID: "a", Title: "same", Status: StepPending}, {ID: "b", Title: "same", Status: StepPending}}}
	before := PlanTruthEntities(plan, 1)
	if before[0].ID == before[1].ID {
		t.Fatal("same titles merged truth identity")
	}
	plan.Steps[1].Title = "renamed"
	plan.Steps[0], plan.Steps[1] = plan.Steps[1], plan.Steps[0]
	after := PlanTruthEntities(plan, 2)
	if before[0].ID != after[1].ID || before[1].ID != after[0].ID {
		t.Fatal("plan truth identity changed")
	}
}
