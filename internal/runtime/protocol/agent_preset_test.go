package protocol

import (
	"testing"
	"time"
)

func TestAgentPresetProfileBuildsOnlyChangedPatchFields(t *testing.T) {
	current := testSessionProfile()
	preset := NewAgentPresetProfile(current)
	preset.PlanningPolicy = "required"
	preset.EnabledToolIDs = []string{"builtin:read", "builtin:search"}

	patch := preset.Patch(current)
	if patch.EnabledToolIDs == nil || len(*patch.EnabledToolIDs) != 2 {
		t.Fatalf("patch = %+v", patch)
	}
	if patch.PlanningPolicy != nil || patch.Model != nil || patch.ApprovalPosture != nil ||
		patch.MaxSteps != nil {
		t.Fatalf("unchanged fields entered patch = %+v", patch)
	}
}

func TestAgentPresetValidationRejectsInvalidIdentityAndScope(t *testing.T) {
	now := time.Now().UTC()
	valid := AgentPreset{
		Version: AgentPresetVersion, ID: "preset-review", Revision: 1,
		Name: "Review", Scope: AgentPresetScopeWorkspace,
		Profile:   NewAgentPresetProfile(testSessionProfile()),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.Name = "bad\nname"
	if err := invalid.Validate(); err == nil {
		t.Fatal("preset with invalid name was accepted")
	}
	invalid = valid
	invalid.Scope = "browser"
	if err := invalid.Validate(); err == nil {
		t.Fatal("browser-owned preset scope was accepted")
	}
	for _, mode := range []string{"plan", "operate"} {
		invalid = valid
		invalid.Profile.Mode = mode
		if err := invalid.Validate(); err == nil {
			t.Fatalf("preset mode %q was accepted", mode)
		}
	}
}
