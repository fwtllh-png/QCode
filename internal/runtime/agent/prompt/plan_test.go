package prompt

import (
	"strings"
	"testing"

	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func TestFormatPlan(t *testing.T) {
	rendered := FormatPlan(agentcontext.Plan{
		Title: "P24", Steps: []agentcontext.PlanStep{{Title: "wire input"}}, Objective: "ship interact tools",
		VerificationPlan: "go test", CriticalFiles: []string{"interact.go"},
		HandoffPacket: "next: land relay",
	})
	if !strings.Contains(rendered, "objective: ship interact tools") ||
		!strings.Contains(rendered, "verification_plan: go test") ||
		!strings.Contains(rendered, "handoff_packet: next: land relay") {
		t.Fatalf("FormatPlan = %s", rendered)
	}
}

func TestPlanReceiptDigestTracksSameLengthChanges(t *testing.T) {
	first := PlanReceipt(agentcontext.Plan{
		Steps: []agentcontext.PlanStep{{Title: "read a.go"}},
	})
	second := PlanReceipt(agentcontext.Plan{
		Steps: []agentcontext.PlanStep{{Title: "read b.go"}},
	})
	if first.OriginalBytes != second.OriginalBytes || first.Digest == second.Digest {
		t.Fatalf("plan receipts = %+v / %+v", first, second)
	}
}

func TestFormatPlanStepStatuses(t *testing.T) {
	plan := agentcontext.Plan{Steps: []agentcontext.PlanStep{
		{Title: "bare step", Status: agentcontext.StepPending},
		{Title: "typed step", Status: agentcontext.StepInProgress},
	}}
	rendered := FormatPlan(plan)
	if !strings.Contains(rendered, "1. bare step\n") {
		t.Fatalf("pending step was decorated: %s", rendered)
	}
	if !strings.Contains(rendered, "2. typed step [in_progress]") {
		t.Fatalf("status missing: %s", rendered)
	}
}
