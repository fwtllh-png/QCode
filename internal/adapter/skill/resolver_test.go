package skill

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveLockLoadPlanAndDigestDrift(t *testing.T) {
	workspace := t.TempDir()
	configured := t.TempDir()
	writeGovernedSkill(
		t, configured, "repository-context", "2.1.3",
		"Repository context", "Load repository context.", nil,
	)
	writeGovernedSkill(
		t, configured, "review", "1.2.0", "Review", "Run the review.",
		map[string]string{"repository-context": "^2.1.0"},
	)
	lock, err := NewLockStore(filepath.Join(t.TempDir(), "skills.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Discover(DiscoveryOptions{
		Workspace: workspace, ConfiguredDir: configured, UserHome: t.TempDir(),
		RuntimeVersion: "1.4.0", Lock: lock,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.LoadPlan(t.Context(), "review"); err == nil ||
		!strings.Contains(err.Error(), "skill lock") {
		t.Fatalf("unlocked load error = %v", err)
	}
	lockfile, err := catalog.WriteLock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(lockfile.Skills) != 2 ||
		lockfile.Skills[0].Name != "repository-context" ||
		lockfile.Skills[1].Name != "review" {
		t.Fatalf("lockfile = %+v", lockfile)
	}
	plan, err := catalog.LoadPlan(t.Context(), "review")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 || plan[0].Name != "repository-context" ||
		plan[1].Name != "review" || !plan[0].Locked || !plan[1].Locked {
		t.Fatalf("plan = %+v", plan)
	}
	path := filepath.Join(configured, "repository-context", "SKILL.md")
	if err := os.WriteFile(path, []byte(`---
name: repository-context
description: Repository context
---
Changed after lock.
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Verify(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "digest drifted") {
		t.Fatalf("Verify() error = %v", err)
	}
}

func TestResolverRejectsCycleConflictAndLegacyShadow(t *testing.T) {
	tests := map[string]func(*testing.T, string, string){
		"cycle": func(t *testing.T, workspace, configured string) {
			writeGovernedSkill(t, configured, "alpha", "1.0.0", "alpha", "alpha",
				map[string]string{"beta": "^1.0.0"})
			writeGovernedSkill(t, configured, "beta", "1.0.0", "beta", "beta",
				map[string]string{"alpha": "^1.0.0"})
		},
		"conflict": func(t *testing.T, workspace, configured string) {
			writeGovernedSkill(t, configured, "alpha", "1.0.0", "alpha", "alpha",
				map[string]string{"beta": "^2.0.0"})
			writeGovernedSkill(t, configured, "beta", "1.0.0", "beta", "beta", nil)
		},
		"legacy shadow": func(t *testing.T, workspace, configured string) {
			writeSkill(
				t, filepath.Join(workspace, ".agents", "skills"),
				"beta", "legacy beta", "legacy beta",
			)
			writeGovernedSkill(t, configured, "alpha", "1.0.0", "alpha", "alpha",
				map[string]string{"beta": "^1.0.0"})
			writeGovernedSkill(t, configured, "beta", "1.0.0", "beta", "beta", nil)
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			configured := t.TempDir()
			setup(t, workspace, configured)
			lock, err := NewLockStore(filepath.Join(t.TempDir(), "skills.lock.json"))
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := Discover(DiscoveryOptions{
				Workspace: workspace, ConfiguredDir: configured, UserHome: t.TempDir(),
				RuntimeVersion: "1.0.0", Lock: lock,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = catalog.WriteLock(t.Context())
			if err == nil {
				t.Fatal("invalid dependency graph was locked")
			}
			want := ErrDependencyConflict
			if name == "cycle" {
				want = ErrDependencyCycle
			}
			if !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
		})
	}
}

func TestLegacyWorkspaceSkillRemainsLocalUnlocked(t *testing.T) {
	workspace := t.TempDir()
	writeSkill(
		t, filepath.Join(workspace, ".agents", "skills"),
		"review", "Review", "Run the review.",
	)
	catalog, err := Discover(DiscoveryOptions{
		Workspace: workspace, UserHome: t.TempDir(), RuntimeVersion: "1.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := catalog.Load(t.Context(), "review")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != legacySkillVersion || loaded.Locked {
		t.Fatalf("loaded = %+v", loaded)
	}
}

func TestSkillLockInventoryIsIndependentOfEnablement(t *testing.T) {
	configured, stateDir := t.TempDir(), t.TempDir()
	writeGovernedSkill(t, configured, "alpha", "1.0.0", "alpha", "Alpha instructions.", nil)
	writeGovernedSkill(t, configured, "beta", "1.0.0", "beta", "Beta instructions.", nil)
	writeGovernedSkill(t, configured, "dependent", "1.0.0", "dependent", "Dependent instructions.",
		map[string]string{"alpha": "^1.0.0"})
	state, err := NewStateStore(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := NewLockStore(filepath.Join(stateDir, "lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	options := DiscoveryOptions{
		Workspace: t.TempDir(), ConfiguredDir: configured, UserHome: t.TempDir(),
		RuntimeVersion: "1.0.0", State: state, Lock: lock,
	}
	catalog, err := Discover(options)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := catalog.WriteLock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	lockedBytes, err := os.ReadFile(lock.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.SetEnabled("alpha", false); err != nil {
		t.Fatal(err)
	}
	afterDisable, err := os.ReadFile(lock.Path())
	if err != nil || string(afterDisable) != string(lockedBytes) {
		t.Fatalf("disable changed lock bytes: %v", err)
	}
	for _, reopen := range []bool{false, true} {
		if reopen {
			catalog, err = Discover(options)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := catalog.Verify(t.Context()); err != nil {
			t.Fatalf("reopen=%t: disable invalidated lock: %v", reopen, err)
		}
		loaded, err := catalog.Load(t.Context(), "beta")
		if err != nil || loaded.Content != "Beta instructions." || !loaded.Locked {
			t.Fatalf("reopen=%t: independent load = %+v, %v", reopen, loaded, err)
		}
		for _, name := range []string{"alpha", "dependent"} {
			if _, err := catalog.LoadPlan(t.Context(), name); !errors.Is(err, ErrDependencyConflict) {
				t.Fatalf("reopen=%t: disabled dependency load %q = %v", reopen, name, err)
			}
		}
		inventory, err := catalog.Resolve(t.Context())
		if err != nil || len(inventory) != 3 {
			t.Fatalf("reopen=%t: inventory = %+v, %v", reopen, inventory, err)
		}
		relocked, err := catalog.WriteLock(t.Context())
		if err != nil || !reflect.DeepEqual(baseline, relocked) {
			t.Fatalf("reopen=%t: relock changed inventory: %+v, %v", reopen, relocked, err)
		}
	}
	if err := catalog.SetEnabled("alpha", true); err != nil {
		t.Fatal(err)
	}
	if plan, err := catalog.LoadPlan(t.Context(), "dependent"); err != nil || len(plan) != 2 {
		t.Fatalf("re-enabled dependency plan = %+v, %v", plan, err)
	}

	// Disabling an entry must not exempt its installed bytes from integrity checks.
	if err := catalog.SetEnabled("alpha", false); err != nil {
		t.Fatal(err)
	}
	writeGovernedSkill(t, configured, "alpha", "1.0.0", "alpha", "Changed instructions.", nil)
	if err := catalog.Verify(t.Context()); !errors.Is(err, ErrLockDrift) {
		t.Fatalf("disabled content drift verification = %v", err)
	}
	if _, err := catalog.WriteLock(t.Context()); !errors.Is(err, ErrLockDrift) {
		t.Fatalf("stale catalog relocked changed disabled content: %v", err)
	}
}
