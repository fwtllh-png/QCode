package protocol

import (
	"slices"
	"testing"
)

func TestSessionProfilePatchRevisionAndPromptCacheReset(t *testing.T) {
	current := testSessionProfile()
	planning := "required"
	posture := "never"
	updated, err := ApplySessionProfilePatch(current, SessionProfilePatch{
		PlanningPolicy:  &planning,
		ApprovalPosture: &posture,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Profile.Revision != 2 ||
		updated.Profile.PromptCacheRevision != 2 ||
		!updated.PromptCacheReset ||
		updated.ResetReason != "planning_policy" ||
		updated.Profile.PlanningPolicy != planning ||
		updated.Profile.ApprovalPosture != posture {
		t.Fatalf("updated profile = %+v", updated)
	}
}

func TestSessionProfilePatchNoopDoesNotAdvanceRevision(t *testing.T) {
	current := testSessionProfile()
	model := current.Model
	updated, err := ApplySessionProfilePatch(
		current,
		SessionProfilePatch{Model: &model},
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Profile.Revision != current.Revision ||
		updated.PromptCacheReset {
		t.Fatalf("no-op update = %+v", updated)
	}
}

func TestSessionProfileValidationRejectsUnsafeOrUnsupportedValues(t *testing.T) {
	current := testSessionProfile()
	for _, mode := range []string{"plan", "operate", ""} {
		invalid := current
		invalid.Mode = mode
		if err := invalid.Validate(); err == nil {
			t.Fatalf("unsupported mode %q was accepted", mode)
		}
	}
	invalid := "turbo"
	if _, err := ApplySessionProfilePatch(
		current,
		SessionProfilePatch{ReasoningEffort: &invalid},
	); err == nil {
		t.Fatal("invalid reasoning effort was accepted")
	}
	duplicate := []string{"file_read", "file_read"}
	if _, err := ApplySessionProfilePatch(
		current,
		SessionProfilePatch{EnabledToolIDs: &duplicate},
	); err == nil {
		t.Fatal("duplicate tool ids were accepted")
	}
	sandbox := "sandbox"
	if _, err := ApplySessionProfilePatch(
		current,
		SessionProfilePatch{ExecutionTarget: &sandbox},
	); err == nil {
		t.Fatal("unimplemented sandbox execution target was accepted")
	}
}

func TestSessionProfileAllowsUncappedExecutionSteps(t *testing.T) {
	profile := testSessionProfile()
	profile.MaxSteps = 0
	if err := profile.Validate(); err != nil {
		t.Fatalf("uncapped profile: %v", err)
	}
}

func TestSessionProfileEnabledToolIDsAreSetOrderInsensitive(t *testing.T) {
	// An unsorted initial profile followed by a patch carrying the same tool
	// set in a different order must be a no-op: tool-set order is not part of
	// the projected prompt prefix, so it must never bump Revision or
	// PromptCacheRevision.
	current := testSessionProfile()
	current.EnabledToolIDs = []string{"zeta", "alpha"}
	patch := SessionProfilePatch{EnabledToolIDs: &[]string{"alpha", "zeta"}}
	updated, err := ApplySessionProfilePatch(current, patch)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Profile.Revision != current.Revision ||
		updated.Profile.PromptCacheRevision != current.PromptCacheRevision ||
		updated.PromptCacheReset {
		t.Fatalf("set-reordered tool ids changed the profile: %+v", updated)
	}
}

func TestSessionProfileProviderModelChangeResetsPromptCache(t *testing.T) {
	current := testSessionProfile()
	current.EnabledToolIDs = []string{"file_read", "file_write"}
	provider := "other-provider"
	model := "other-model"
	updated, err := ApplySessionProfilePatch(current, SessionProfilePatch{
		Provider: &provider, Model: &model,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Profile.Revision != current.Revision+1 ||
		updated.Profile.PromptCacheRevision != current.PromptCacheRevision+1 ||
		!updated.PromptCacheReset ||
		updated.ResetReason != "provider,model" {
		t.Fatalf("provider/model switch did not reset prompt cache: %+v", updated)
	}
}

func TestSessionProfileUnrelatedMetadataDoesNotResetPromptCache(t *testing.T) {
	current := testSessionProfile()
	steps := 64
	posture := "never"
	target := "local"
	updated, err := ApplySessionProfilePatch(current, SessionProfilePatch{
		MaxSteps: &steps, ApprovalPosture: &posture, ExecutionTarget: &target,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Profile.Revision != current.Revision+1 ||
		updated.Profile.PromptCacheRevision != current.PromptCacheRevision ||
		updated.PromptCacheReset {
		t.Fatalf("unrelated metadata reset the prompt cache: %+v", updated)
	}
}

func TestAgentPresetProfileNormalizesEnabledToolIDs(t *testing.T) {
	preset := AgentPresetProfile{
		Mode: "act", Provider: "fixture", Model: "fixture-model",
		ApprovalPosture: "suggest", ExecutionTarget: "local", MaxSteps: 8,
		EnabledToolIDs: []string{"zeta", "alpha"},
	}
	derived := preset.sessionProfile(1, 1)
	if got := derived.EnabledToolIDs; !slices.Equal(got, []string{"alpha", "zeta"}) {
		t.Fatalf("sessionProfile tool ids not sorted: %v", got)
	}
	fromProfile := NewAgentPresetProfile(derived)
	if got := fromProfile.EnabledToolIDs; !slices.Equal(got, []string{"alpha", "zeta"}) {
		t.Fatalf("NewAgentPresetProfile tool ids not sorted: %v", got)
	}
}

func TestAgentPresetPatchIsSetOrderInsensitive(t *testing.T) {
	current := testSessionProfile()
	current.EnabledToolIDs = []string{"alpha", "zeta"}
	preset := AgentPresetProfile{
		Mode: "act", Provider: "fixture", Model: "fixture-model",
		ApprovalPosture: "suggest", ExecutionTarget: "local", MaxSteps: 32,
		EnabledToolIDs: []string{"zeta", "alpha"},
	}
	patch := preset.Patch(current)
	if patch.EnabledToolIDs != nil {
		t.Fatalf("same tool set in different order produced a tool patch: %+v", patch.EnabledToolIDs)
	}
}

func testSessionProfile() SessionProfile {
	return SessionProfile{
		Version:             SessionProfileVersion,
		Revision:            1,
		Mode:                "act",
		PlanningPolicy:      "adaptive",
		Provider:            "fixture",
		Model:               "fixture-model",
		ReasoningEffort:     "low",
		ApprovalPosture:     "suggest",
		ExecutionTarget:     "local",
		MaxSteps:            32,
		PromptCacheRevision: 1,
	}
}
