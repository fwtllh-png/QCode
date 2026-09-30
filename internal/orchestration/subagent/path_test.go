package subagent

import (
	"testing"
)

func TestCanonicalAgentPathsAreReadableNestedAndUnique(t *testing.T) {
	control, err := OpenControl(Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Budget: Budget{MaxDepth: 3, MaxParallel: 4},
	}, DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := control.SpawnSystem(
		"Plan Runtime", "", RolePlan, "plan", "plan",
	)
	if err != nil {
		t.Fatal(err)
	}
	child, err := control.SpawnSystem(
		"Inspect Store", parent.ID, RoleExplore, "inspect", "report",
	)
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := control.SpawnSystem(
		"Inspect Store", parent.ID, RoleReview, "inspect", "report",
	)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Path != "/root/plan_runtime" ||
		child.Path != "/root/plan_runtime/inspect_store" ||
		sibling.Path != "/root/plan_runtime/inspect_store_3" {
		t.Fatalf(
			"paths = parent %q, child %q, sibling %q",
			parent.Path, child.Path, sibling.Path,
		)
	}
}
