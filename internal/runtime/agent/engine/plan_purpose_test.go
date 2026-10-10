package engine

import (
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestDeliverablePlanCompletionPreservesExecutionObligations(t *testing.T) {
	for _, openExecution := range []bool{false, true} {
		name := "completed current work"
		if openExecution {
			name = "unfinished current work"
		}
		t.Run(name, func(t *testing.T) {
			registry := declarationRegistry(t, false)
			var engine *Engine
			if err := interact.Register(registry, interact.Options{
				Workspace: t.TempDir(),
				OnPlan:    func(plan interact.Plan) error { return engine.ApplyPlan(plan) },
			}); err != nil {
				t.Fatal(err)
			}
			streams := []provider.Stream{
				toolCallStream("current-plan", "update_plan",
					`{"steps":[{"title":"Write requested document","status":"in_progress"}]}`),
				toolCallStream("write-document", "write_fixture", `{}`),
			}
			if !openExecution {
				streams = append(streams, toolCallStream("current-done", "update_plan",
					`{"steps":[{"title":"Write requested document","status":"done"}]}`))
			}
			streams = append(streams,
				toolCallStream("future-plan", "submit_plan",
					`{"purpose":"deliverable","steps":[{"title":"Implement future project","status":"pending"}]}`),
				toolCallStream("complete", "turn_complete",
					`{"status":"complete","summary":"Plan delivered.","pending_actions":[]}`),
			)
			runtime := &scriptedProvider{streams: streams}
			engine = declarationEngine(t, runtime, registry)
			// Leave capacity for the plan lifecycle rather than testing context pressure.
			engine.options.Route = mustTestRouteWithContext(t, 8192)
			engine.options.Routes, _ = model.NewRouteSet(engine.options.Route, nil, false)
			var rejection string
			result, err := engine.RunForTurnWithIntentAndAttachments(
				t.Context(), "turn-deliverable", "write the plan document",
				protocol.TurnIntentAnswer, nil, func(event Event) error {
					if event.ToolCall != nil && event.ToolCall.ID == "complete" && event.Result != nil {
						rejection, _ = event.Result.Metadata["completion_declaration_rejection"].(string)
					}
					return nil
				},
			)
			if err != nil || result.State != Completed || result.Text != "Plan delivered." {
				t.Fatalf("deliverable blocked completion: result=%+v rejection=%s err=%v", result, rejection, err)
			}
			if len(runtime.requests) != len(streams) {
				t.Fatalf("unexpected retry: requests=%d want=%d", len(runtime.requests), len(streams))
			}
			plan := engine.currentPlan()
			if len(plan.Steps) != 1 || plan.Steps[0].Title != "Write requested document" {
				t.Fatalf("deliverable overwrote current plan: %+v", plan)
			}
			if !openExecution {
				snapshot, err := engine.ExportContextSnapshot()
				if err != nil {
					t.Fatal(err)
				}
				engine.setPlan(agentcontext.Plan{})
				if _, err := engine.RestoreContextSnapshot(snapshot); err != nil {
					t.Fatal(err)
				}
				restored := engine.currentPlan()
				if len(restored.Steps) != 1 || !restored.Steps[0].Done() {
					t.Fatalf("checkpoint restored future work: %+v", restored)
				}
			}
		})
	}
}

func TestDeliverableWithoutExecutionPlanCompletesAfterMutation(t *testing.T) {
	registry := declarationRegistry(t, false)
	var engine *Engine
	if err := interact.Register(registry, interact.Options{
		Workspace: t.TempDir(),
		OnPlan:    func(plan interact.Plan) error { return engine.ApplyPlan(plan) },
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("write-document", "write_fixture", `{}`),
		toolCallStream("future-plan", "submit_plan",
			`{"purpose":"deliverable","steps":[{"title":"Future project","status":"pending"}]}`),
		toolCallStream("complete", "turn_complete",
			`{"status":"complete","summary":"Plan delivered.","pending_actions":[]}`),
	}}
	engine = declarationEngine(t, runtime, registry)
	result, err := engine.RunForTurnWithIntentAndAttachments(t.Context(),
		"turn-plan-only", "write the plan document", protocol.TurnIntentAnswer, nil, nil)
	if err != nil || result.State != Completed || len(runtime.requests) != 3 {
		t.Fatalf("result=%+v requests=%d err=%v", result, len(runtime.requests), err)
	}
	if len(engine.currentPlan().Steps) != 0 {
		t.Fatal("deliverable entered execution context")
	}
}

func TestDeliverablePlanCompletesWithoutVerificationGate(t *testing.T) {
	registry := declarationRegistry(t, true)
	var engine *Engine
	if err := interact.Register(registry, interact.Options{
		Workspace: t.TempDir(),
		OnPlan:    func(plan interact.Plan) error { return engine.ApplyPlan(plan) },
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("write-document", "write_fixture", `{}`),
		toolCallStream("future-plan", "submit_plan",
			`{"purpose":"deliverable","steps":[{"title":"Future project","status":"pending"}]}`),
		toolCallStream("complete-before-check", "turn_complete",
			`{"status":"complete","summary":"Plan delivered.","pending_actions":[]}`),
	}}
	engine = declarationEngine(t, runtime, registry)
	// Keep room for the complete tool surface; this test checks completion policy.
	engine.options.Route = mustTestRouteWithContext(t, 8192)
	engine.options.Routes, _ = model.NewRouteSet(engine.options.Route, nil, false)
	result, err := engine.RunForTurnWithIntentAndAttachments(t.Context(),
		"turn-plan-verify", "write and verify a plan", protocol.TurnIntentAnswer, nil, nil)
	if err != nil || result.State != Completed || result.Text != "Plan delivered." {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(runtime.requests) != 3 {
		t.Fatalf("unexpected implicit verification requests=%d", len(runtime.requests))
	}
}
