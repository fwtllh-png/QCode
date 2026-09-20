package skill

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

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
