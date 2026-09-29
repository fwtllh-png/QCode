package interact_test

import (
	"encoding/json"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
)

func TestDeliverablePlanPreservesExecutionPlan(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	var applied []interact.Plan
	if err := interact.Register(registry, interact.Options{
		Workspace: t.TempDir(),
		OnPlan: func(plan interact.Plan) error {
			applied = append(applied, plan)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	execution := map[string]any{
		"steps": []any{map[string]any{"title": "Current task"}},
	}
	execute(t, registry, "update_plan", execution)
	result := execute(t, registry, "submit_plan", map[string]any{
		"purpose": "deliverable",
		"steps": []any{map[string]any{
			"title": "Future implementation", "affected_files": []any{"future.go"},
		}},
	})
	if result.IsError || result.Metadata["submitted_plan"] != nil ||
		result.Metadata["plan_delta"] != false || result.Metadata["plan_artifact"] != true {
		t.Fatalf("deliverable execution metadata = %+v", result)
	}
	plan, err := interact.ParseSubmittedPlan([]byte(result.Content))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Purpose != protocol.PlanPurposeDeliverable ||
		plan.Steps[0].Status != interact.StepPending ||
		len(plan.FileBaseline) != 1 || !plan.FileBaseline[0].Missing {
		t.Fatalf("deliverable = %+v", plan)
	}
	if len(applied) != 1 || applied[0].Steps[0].Title != "Current task" {
		t.Fatalf("deliverable replaced execution plan: %+v", applied)
	}
	unchanged := execute(t, registry, "update_plan", execution)
	if !unchanged.IsError || unchanged.Metadata["reason"] != "plan_progress_unchanged" {
		t.Fatalf("deliverable changed execution progress signature: %+v", unchanged)
	}
}

func TestPlanPurposeDefaultsAndValidation(t *testing.T) {
	for _, value := range []string{"", "execution", "deliverable", "unknown"} {
		t.Run(value, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"purpose": value, "steps": []any{map[string]any{"title": "Plan step"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := interact.ParseSubmittedPlan(body)
			if value == "unknown" {
				if err == nil {
					t.Fatal("unknown purpose accepted")
				}
				return
			}
			if err != nil || plan.Purpose != protocol.PlanPurpose(value).Normalize() {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
		})
	}
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      "update_plan",
		Arguments: json.RawMessage(`{"purpose":"deliverable","steps":[{"title":"Future"}]}`),
	})
	if err == nil && !result.IsError {
		t.Fatal("update_plan accepted deliverable purpose")
	}
}
