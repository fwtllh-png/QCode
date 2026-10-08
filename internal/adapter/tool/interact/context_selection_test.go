package interact_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
)

func TestPlanGeneratedIDsAndFailedCallbackOwnership(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	fail := true
	if err := interact.Register(registry, interact.Options{Workspace: t.TempDir(), OnPlan: func(interact.Plan) error {
		if fail {
			return errors.New("invalid reference")
		}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"steps": []any{map[string]any{"id": "stable", "title": "same"}}}
	raw, _ := json.Marshal(input)
	first, err := tooltest.Execute(t.Context(), registry, tool.Call{Name: "update_plan", Arguments: raw})
	if err == nil && !first.IsError {
		t.Fatal("callback failure accepted")
	}
	fail = false
	second := execute(t, registry, "update_plan", input)
	if second.IsError {
		t.Fatal("failed callback polluted local progress")
	}
	generated := execute(t, registry, "update_plan", map[string]any{"steps": []any{map[string]any{"title": "same"}, map[string]any{"title": "same"}}})
	plan, err := interact.ParseSubmittedPlan([]byte(generated.Content))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Steps[0].ID == "" || plan.Steps[0].ID == plan.Steps[1].ID {
		t.Fatal("same titles merged identity")
	}
	replay, err := interact.ParseSubmittedPlan([]byte(generated.Content))
	if err != nil || replay.Steps[0].ID != plan.Steps[0].ID {
		t.Fatal("replay changed returned ID")
	}
}
