package app

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestPlanExecutionProfileDigestIgnoresPlanControlChanges(t *testing.T) {
	profile := protocol.SessionProfile{
		Version: 1, Revision: 1, Mode: "act",
		PlanningPolicy: "adaptive",
		Provider:       "fixture", Model: "fixture-model",
		EnabledToolIDs:  []string{"write", "read"},
		ApprovalPosture: "suggest", ExecutionTarget: "local",
		MaxSteps: 32, PromptCacheRevision: 1,
	}
	original, err := PlanExecutionProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	profile.Revision++
	profile.PromptCacheRevision++
	profile.PlanningPolicy = "required"
	updated, err := PlanExecutionProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	if updated != original {
		t.Fatalf("Plan control changes altered execution digest: %s != %s", updated, original)
	}
	profile.Model = "other-model"
	changed, err := PlanExecutionProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	if changed == original {
		t.Fatal("model change did not alter execution digest")
	}
}
