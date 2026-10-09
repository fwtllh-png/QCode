package session_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
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

func TestRebindUnavailableWorkspaceProfilesRepairsOnlyStaleRoutes(t *testing.T) {
	store, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repository := session.NewSQLiteRepository(store)
	workspaceRoot := t.TempDir()
	if err := repository.EnsureSeed(t.Context(), "stale", workspaceRoot); err != nil {
		t.Fatal(err)
	}
	if err := repository.EnsureSeed(t.Context(), "current", workspaceRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(
		t.Context(),
		`UPDATE sessions SET metadata_json = ? WHERE id IN (?, ?)`,
		[]byte(`{}`),
		"stale",
		"current",
	); err != nil {
		t.Fatal(err)
	}
	defaults := persistedProfile()
	if _, err := repository.EnsureProfile(t.Context(), "current", defaults); err != nil {
		t.Fatal(err)
	}
	stale := defaults
	stale.Provider = "openai-compatible:old"
	stale.Model = "old-model"
	encoded, err := json.Marshal(map[string]any{"profile": stale})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(
		t.Context(),
		`UPDATE sessions SET metadata_json = ? WHERE id = ?`,
		[]byte(encoded),
		"stale",
	); err != nil {
		t.Fatal(err)
	}
	next := defaults
	next.Provider = "openai-compatible:new"
	next.Model = "new-model"
	available := func(provider, model string) bool {
		return provider == "fixture" && model == "fixture-model"
	}
	if err := repository.RebindUnavailableWorkspaceProfiles(
		t.Context(),
		[]string{workspaceRoot},
		next,
		available,
	); err != nil {
		t.Fatal(err)
	}
	repaired, err := repository.Profile(t.Context(), "stale", next)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Provider != next.Provider || repaired.Model != next.Model ||
		repaired.ReasoningEffort != next.ReasoningEffort ||
		repaired.Revision != stale.Revision+1 {
		t.Fatalf("repaired profile = %+v", repaired)
	}
	kept, err := repository.Profile(t.Context(), "current", defaults)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Provider != defaults.Provider || kept.Model != defaults.Model ||
		kept.Revision != defaults.Revision {
		t.Fatalf("still-valid profile = %+v", kept)
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
		ApprovalPosture:     "auto",
		ExecutionTarget:     "local",
		MaxSteps:            32,
		PromptCacheRevision: 1,
	}
}

func TestRetiredSuggestProfileBecomesAutoExactlyOnce(t *testing.T) {
	store, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repository := session.NewSQLiteRepository(store)
	defaults := persistedProfile()
	for _, posture := range []string{"suggest", "never", "auto", "bypass"} {
		t.Run(posture, func(t *testing.T) {
			if err := repository.EnsureSeed(t.Context(), posture, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			legacy := defaults
			legacy.ApprovalPosture = posture
			legacy.Revision = 5
			metadata, err := json.Marshal(map[string]any{"profile": legacy, "transport": "web"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().ExecContext(t.Context(), `UPDATE sessions SET metadata_json = ? WHERE id = ?`, metadata, posture); err != nil {
				t.Fatal(err)
			}
			updated, err := repository.EnsureProfile(t.Context(), posture, defaults)
			if err != nil {
				t.Fatal(err)
			}
			want := legacy
			if posture == "suggest" {
				want.ApprovalPosture = "auto"
				want.Revision++
			}
			if updated.ApprovalPosture != want.ApprovalPosture || updated.Revision != want.Revision || updated.PromptCacheRevision != want.PromptCacheRevision {
				t.Fatalf("updated profile = %+v; want %+v", updated, want)
			}
			again, err := repository.EnsureProfile(t.Context(), posture, defaults)
			if err != nil || again.Revision != updated.Revision {
				t.Fatalf("repeated migration = %+v, %v", again, err)
			}
			record, err := repository.Get(t.Context(), posture)
			if err != nil {
				t.Fatal(err)
			}
			var saved map[string]json.RawMessage
			if err := json.Unmarshal(record.Metadata, &saved); err != nil {
				t.Fatal(err)
			}
			if string(saved["transport"]) != `"web"` {
				t.Fatal("unrelated metadata was changed")
			}
		})
	}
}

func TestRetiredHostSettingIsRemovedWithoutChangingPermissions(t *testing.T) {
	for _, retired := range []string{"true", "false"} {
		for _, posture := range []string{"auto", "bypass", "never", "suggest"} {
			t.Run(retired+"/"+posture, func(t *testing.T) {
				store, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				repository := session.NewSQLiteRepository(store)
				if err := repository.EnsureSeed(t.Context(), "legacy", t.TempDir()); err != nil {
					t.Fatal(err)
				}
				defaults := persistedProfile()
				legacy := defaults
				legacy.ApprovalPosture = posture
				legacy.Revision = 5
				legacy.PromptCacheRevision = 3
				encoded, err := json.Marshal(legacy)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(encoded, &fields); err != nil {
					t.Fatal(err)
				}
				fields["allow_host_execution"] = json.RawMessage(retired)
				metadata, err := json.Marshal(map[string]any{"profile": fields, "transport": "web"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.DB().ExecContext(t.Context(), `UPDATE sessions SET metadata_json = ? WHERE id = ?`, metadata, "legacy"); err != nil {
					t.Fatal(err)
				}
				loaded, err := repository.Profile(t.Context(), "legacy", defaults)
				if err != nil || !reflect.DeepEqual(loaded, legacy) {
					t.Fatalf("read legacy profile = %+v, %v; want %+v", loaded, err, legacy)
				}
				want := legacy
				want.Revision++
				want.ApprovalPosture = protocol.NormalizeSessionApprovalPosture(posture)
				for range 2 {
					updated, err := repository.EnsureProfile(t.Context(), "legacy", defaults)
					if err != nil || !reflect.DeepEqual(updated, want) {
						t.Fatalf("ensure profile = %+v, %v; want %+v", updated, err, want)
					}
				}
				record, err := repository.Get(t.Context(), "legacy")
				if err != nil {
					t.Fatal(err)
				}
				var saved map[string]json.RawMessage
				if err := json.Unmarshal(record.Metadata, &saved); err != nil {
					t.Fatal(err)
				}
				if string(saved["transport"]) != `"web"` || strings.Contains(string(saved["profile"]), "allow_host_execution") {
					t.Fatalf("retired field remains or unrelated metadata changed: %s", record.Metadata)
				}
				recovered, err := repository.Profile(t.Context(), "legacy", defaults)
				if err != nil || !reflect.DeepEqual(recovered, want) {
					t.Fatalf("persisted profile = %+v, %v; want %+v", recovered, err, want)
				}
			})
		}
	}
}

func TestStoredProfileStillRejectsUnknownFields(t *testing.T) {
	store, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repository := session.NewSQLiteRepository(store)
	if err := repository.EnsureSeed(t.Context(), "unknown", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defaults := persistedProfile()
	metadata, err := json.Marshal(map[string]any{"profile": struct {
		protocol.SessionProfile
		Unknown bool `json:"unknown_permission"`
	}{SessionProfile: defaults, Unknown: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE sessions SET metadata_json = ? WHERE id = ?`, metadata, "unknown"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Profile(t.Context(), "unknown", defaults); err == nil || !strings.Contains(err.Error(), `unknown field "unknown_permission"`) {
		t.Fatalf("read unknown field error = %v", err)
	}
	if _, err := repository.EnsureProfile(t.Context(), "unknown", defaults); err == nil || !strings.Contains(err.Error(), `unknown field "unknown_permission"`) {
		t.Fatalf("ensure unknown field error = %v", err)
	}
	var saved []byte
	if err := store.DB().QueryRowContext(t.Context(), `SELECT metadata_json FROM sessions WHERE id = ?`, "unknown").Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if string(saved) != string(metadata) {
		t.Fatal("rejected metadata was modified")
	}
}
