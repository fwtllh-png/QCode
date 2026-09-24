package wire

import (
	"slices"
	"testing"
)

func TestMutableSessionProfileFieldsExcludeDerivedPlanningPolicy(t *testing.T) {
	fields := mutableSessionProfileFields(
		[]string{"model", "reasoning_effort"},
		true,
		true,
	)
	if slices.Contains(fields, "planning_policy") || slices.Contains(fields, "mode") {
		t.Fatalf("fixed policy field is mutable: %v", fields)
	}
	if !slices.Contains(fields, "enabled_tool_ids") ||
		!slices.Contains(fields, "approval_posture") {
		t.Fatalf("expected mutable fields are missing: %v", fields)
	}
}
