package session_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/persist/session"
	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestProfilePersistsWithRevisionCASAndPreservesMetadata(t *testing.T) {
	store, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repository := session.NewSQLiteRepository(store)
	if err := repository.EnsureSeed(t.Context(), "session", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(
		t.Context(),
		`UPDATE sessions SET metadata_json = ? WHERE id = ?`,
		[]byte(`{"transport":"web","isolation":"worktree"}`),
		"session",
	); err != nil {
		t.Fatal(err)
	}
	defaults := persistedProfile()
	current, err := repository.EnsureProfile(t.Context(), "session", defaults)
	if err != nil {
		t.Fatal(err)
	}
	model := "other-model"
	updated, err := repository.UpdateProfile(
		t.Context(),
		"session",
		current.Revision,
		defaults,
		protocol.SessionProfilePatch{Model: &model},
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Profile.Revision != 2 ||
		updated.Profile.PromptCacheRevision != 2 {
		t.Fatalf("updated profile = %+v", updated)
	}
	if _, err := repository.UpdateProfile(
		t.Context(),
		"session",
		current.Revision,
		defaults,
		protocol.SessionProfilePatch{Model: &model},
	); !errors.Is(err, session.ErrProfileRevisionConflict) {
		t.Fatalf("stale update error = %v", err)
	}
	record, err := repository.Get(t.Context(), "session")
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(record.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if string(metadata["transport"]) != `"web"` ||
		string(metadata["isolation"]) != `"worktree"` ||
		len(metadata["profile"]) == 0 {
		t.Fatalf("metadata = %s", record.Metadata)
	}
	recovered, err := repository.Profile(t.Context(), "session", defaults)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Revision != updated.Profile.Revision ||
		recovered.Model != model {
		t.Fatalf("recovered profile = %+v", recovered)
	}
}

func TestRebindWorkspaceProfilesUpdatesOnlySelectedWorkspaces(t *testing.T) {
	store, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repository := session.NewSQLiteRepository(store)
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	if err := repository.EnsureSeed(t.Context(), "first", firstRoot); err != nil {
		t.Fatal(err)
	}
	if err := repository.EnsureSeed(t.Context(), "second", secondRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(
		t.Context(),
		`UPDATE sessions SET metadata_json = ? WHERE id IN (?, ?)`,
		[]byte(`{}`),
		"first",
		"second",
	); err != nil {
		t.Fatal(err)
	}
	defaults := persistedProfile()
	if _, err := repository.EnsureProfile(t.Context(), "first", defaults); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.EnsureProfile(t.Context(), "second", defaults); err != nil {
		t.Fatal(err)
	}
	next := defaults
	next.Provider = "openai"
	next.Model = "gpt-5"
	next.ReasoningEffort = "high"
	if err := repository.RebindWorkspaceProfiles(
		t.Context(),
		[]string{firstRoot},
		next,
	); err != nil {
		t.Fatal(err)
	}
	first, err := repository.Profile(t.Context(), "first", next)
	if err != nil {
		t.Fatal(err)
	}
	if first.Provider != "openai" || first.Model != "gpt-5" ||
		first.ReasoningEffort != "high" || first.Revision != defaults.Revision+1 {
		t.Fatalf("rebound profile = %+v", first)
	}
	second, err := repository.Profile(t.Context(), "second", defaults)
	if err != nil {
		t.Fatal(err)
	}
	if second.Provider != defaults.Provider ||
		second.Model != defaults.Model ||
		second.Revision != defaults.Revision {
		t.Fatalf("unselected Workspace profile = %+v", second)
	}
}

func TestEnsureProfileMigratesOnlyUntouchedLegacyStepDefaults(t *testing.T) {
	store, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repository := session.NewSQLiteRepository(store)
	workspaceRoot := t.TempDir()
	legacy := persistedProfile()
	legacy.MaxSteps = 64
	encoded, err := json.Marshal(map[string]any{"profile": legacy})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.EnsureSeed(t.Context(), "legacy", workspaceRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(
		t.Context(),
		`UPDATE sessions SET metadata_json = ? WHERE id = ?`,
		[]byte(encoded),
		"legacy",
	); err != nil {
		t.Fatal(err)
	}
	defaults := persistedProfile()
	defaults.MaxSteps = 0
	migrated, err := repository.EnsureProfile(t.Context(), "legacy", defaults)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.MaxSteps != 0 || migrated.Revision != 2 {
		t.Fatalf("migrated profile = %+v", migrated)
	}

	explicit := legacy
	explicit.MaxSteps = 64
	explicit.Revision = 2
	encoded, err = json.Marshal(map[string]any{"profile": explicit})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.EnsureSeed(t.Context(), "explicit", workspaceRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(
		t.Context(),
		`UPDATE sessions SET metadata_json = ? WHERE id = ?`,
		[]byte(encoded),
		"explicit",
	); err != nil {
		t.Fatal(err)
	}
	preserved, err := repository.EnsureProfile(t.Context(), "explicit", defaults)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.MaxSteps != 64 || preserved.Revision != 2 {
		t.Fatalf("explicit profile = %+v", preserved)
	}
}

func TestEnsureProfileMigratesLegacyPlanningPolicy(t *testing.T) {
	store, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repository := session.NewSQLiteRepository(store)
	workspaceRoot := t.TempDir()
	legacy := persistedProfile()
	legacy.Revision = 3
	legacy.PromptCacheRevision = 2
	legacy.PlanningPolicy = "required"
	encoded, err := json.Marshal(map[string]any{"profile": legacy})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.EnsureSeed(t.Context(), "legacy-planning", workspaceRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(
		t.Context(),
		`UPDATE sessions SET metadata_json = ? WHERE id = ?`,
		[]byte(encoded),
		"legacy-planning",
	); err != nil {
		t.Fatal(err)
	}
	defaults := persistedProfile()
	defaults.PlanningPolicy = "adaptive"
	migrated, err := repository.EnsureProfile(
		t.Context(),
		"legacy-planning",
		defaults,
	)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.PlanningPolicy != "adaptive" ||
		migrated.Revision != legacy.Revision+1 ||
		migrated.PromptCacheRevision != legacy.PromptCacheRevision+1 {
		t.Fatalf("migrated profile = %+v", migrated)
	}
	recovered, err := repository.Profile(t.Context(), "legacy-planning", defaults)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.PlanningPolicy != migrated.PlanningPolicy ||
		recovered.Revision != migrated.Revision ||
		recovered.PromptCacheRevision != migrated.PromptCacheRevision {
		t.Fatalf("persisted profile = %+v, want %+v", recovered, migrated)
	}
}

func persistedProfile() protocol.SessionProfile {
	return protocol.SessionProfile{
		Version:             protocol.SessionProfileVersion,
		Revision:            1,
		Mode:                "act",
		Provider:            "fixture",
		Model:               "fixture-model",
		ApprovalPosture:     "suggest",
		ExecutionTarget:     "local",
		MaxSteps:            32,
		PromptCacheRevision: 1,
	}
}
