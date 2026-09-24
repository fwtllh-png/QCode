package app

import (
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/agentpreset"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestAgentPresetLifecycleValidatesPersistsAndAppliesProfile(t *testing.T) {
	defaults := runtimeTestProfile()
	profiles := &memoryProfileStore{profile: defaults}
	engine := &profileTestEngine{}
	lifecycle := &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session-preset", ThreadID: "thread-preset",
		Title: "Preset", Status: protocol.SessionStatusIdle,
		Isolation: "shared", WorkspaceRoot: "/workspace",
		WorkspaceLabel: "workspace",
		CreatedAt:      time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	runtime := NewRuntime(Options{
		WorkspaceRoot:       "/workspace",
		Engine:              engine,
		SessionProfiles:     profiles,
		AgentPresets:        agentpreset.NewMemory(),
		DefaultProfile:      defaults,
		ProfileCapabilities: runtimeTestCapabilities(defaults),
		ProfileModels: map[string]protocol.ModelCapabilities{
			defaults.Provider + "\x00" + defaults.Model: runtimeTestCapabilities(
				defaults,
			).ModelCapabilities,
		},
		SessionLifecycle: lifecycle,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })

	presetProfile := protocol.NewAgentPresetProfile(defaults)
	presetProfile.ReasoningEffort = "high"
	created, err := runtime.AgentPresetService.Save(
		t.Context(),
		protocol.AgentPresetSaveRequest{
			SessionID: "session-preset", ID: "preset-review",
			Name: "Review", Profile: presetProfile,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if created.Preset == nil || created.Preset.Revision != 1 {
		t.Fatalf("created = %+v", created)
	}

	list, err := runtime.AgentPresetService.List(
		t.Context(),
		protocol.AgentPresetListRequest{SessionID: "session-preset"},
	)
	if err != nil || len(list.Presets) != 1 {
		t.Fatalf("list = %+v, err = %v", list, err)
	}
	if _, err := runtime.AgentPresetService.Apply(
		t.Context(),
		protocol.AgentPresetApplyRequest{
			SessionID: "session-preset", ThreadID: "thread-foreign",
			PresetID: "preset-review", ExpectedProfileRevision: defaults.Revision,
		},
	); err == nil || protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("foreign thread apply error = %v", err)
	}

	applied, err := runtime.AgentPresetService.Apply(
		t.Context(),
		protocol.AgentPresetApplyRequest{
			SessionID: "session-preset", ThreadID: "thread-preset",
			PresetID: "preset-review", ExpectedProfileRevision: defaults.Revision,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if applied.ProfileUpdate.Profile.ReasoningEffort != "high" ||
		applied.ProfileUpdate.Profile.Revision != defaults.Revision+1 ||
		!applied.RestartRequired {
		t.Fatalf("applied = %+v", applied)
	}
	engine.mu.Lock()
	engineEffort := engine.applied.ReasoningEffort
	engine.mu.Unlock()
	if engineEffort != "high" {
		t.Fatalf("engine reasoning effort = %q, want high", engineEffort)
	}

	if _, err := runtime.AgentPresetService.Delete(
		t.Context(),
		protocol.AgentPresetDeleteRequest{
			SessionID: "session-preset", ID: "preset-review",
			ExpectedRevision: created.Preset.Revision,
		},
	); err != nil {
		t.Fatal(err)
	}
}

func TestAgentPresetRejectsUnavailableProfileFields(t *testing.T) {
	defaults := runtimeTestProfile()
	lifecycle := &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session-preset", ThreadID: "thread-preset",
		Title: "Preset", Status: protocol.SessionStatusIdle,
		Isolation: "shared", WorkspaceRoot: "/workspace",
		WorkspaceLabel: "workspace",
		CreatedAt:      time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	runtime := NewRuntime(Options{
		WorkspaceRoot:       "/workspace",
		Engine:              &profileTestEngine{},
		SessionProfiles:     &memoryProfileStore{profile: defaults},
		AgentPresets:        agentpreset.NewMemory(),
		DefaultProfile:      defaults,
		ProfileCapabilities: runtimeTestCapabilities(defaults),
		SessionLifecycle:    lifecycle,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	presetProfile := protocol.NewAgentPresetProfile(defaults)
	presetProfile.Provider = "unavailable"

	if _, err := runtime.AgentPresetService.Save(
		t.Context(),
		protocol.AgentPresetSaveRequest{
			SessionID: "session-preset", ID: "preset-invalid",
			Name: "Invalid", Profile: presetProfile,
		},
	); err == nil || protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("unavailable preset error = %v", err)
	}
}
