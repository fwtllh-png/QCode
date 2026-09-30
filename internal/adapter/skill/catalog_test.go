package skill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRefreshPreservesAuthorityAndInvalidatesChangedHandles(t *testing.T) {
	workspace, home, private, sibling := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	catalog, err := Discover(DiscoveryOptions{Workspace: workspace, UserHome: home, SandboxHome: private})
	if err != nil {
		t.Fatal(err)
	}
	request := SelectionRequest{Query: "installed"}
	if _, err := catalog.Select(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(private, ".agents", "skills"), "installed", "installed", "original")
	writeSkill(t, filepath.Join(sibling, ".agents", "skills"), "sibling", "sibling", "private")
	t.Setenv("HOME", sibling)
	if err := catalog.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	selected, err := catalog.Select(t.Context(), request)
	if err != nil || selected.Metrics.CacheHit || len(selected.Visible) != 1 {
		t.Fatalf("fresh selection=%+v error=%v", selected, err)
	}
	if _, err := catalog.HandleForName(t.Context(), "sibling"); err == nil {
		t.Fatal("refresh changed its private-home authority")
	}
	handle := selected.Visible[0].Handle
	if _, err := catalog.LoadHandle(t.Context(), handle); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(private, ".agents", "skills"), "installed", "installed", "changed")
	if err := catalog.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.LoadHandle(t.Context(), handle); !errors.Is(err, ErrSkillHandleInvalid) {
		t.Fatalf("old handle resolved changed content: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(private, ".agents", "skills", "installed")); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Refresh(t.Context()); err != nil || len(catalog.Names()) != 0 {
		t.Fatalf("removed skill retained: %v %v", catalog.Names(), err)
	}
}

func TestRefreshCancellationAndConcurrentReaders(t *testing.T) {
	workspace := t.TempDir()
	writeSkill(t, filepath.Join(workspace, ".agents", "skills"), "existing", "existing", "body")
	catalog, err := Discover(DiscoveryOptions{Workspace: workspace, UserHome: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := catalog.HandleForName(t.Context(), "existing")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := catalog.Refresh(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh=%v", err)
	}
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			for range 10 {
				if err := catalog.Refresh(t.Context()); err != nil {
					t.Error(err)
					return
				}
				if _, err := catalog.LoadHandle(t.Context(), handle); err != nil {
					t.Error(err)
				}
				if _, err := catalog.Select(t.Context(), SelectionRequest{Query: "existing"}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	group.Wait()
}

func TestRefreshKeepsEnablementAndLockEnforcement(t *testing.T) {
	configured := t.TempDir()
	writeGovernedSkill(t, configured, "governed", "1.0.0", "governed", "original", nil)
	lock, err := NewLockStore(filepath.Join(t.TempDir(), "skills.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Discover(DiscoveryOptions{
		Workspace: t.TempDir(), UserHome: t.TempDir(), ConfiguredDir: configured,
		Lock: lock, State: state, RuntimeVersion: "1.4.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.WriteLock(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SetEnabled("governed", false); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.HandleForName(t.Context(), "governed"); err == nil {
		t.Fatal("refresh enabled a disabled skill")
	}
	if err := catalog.SetEnabled("governed", true); err != nil {
		t.Fatal(err)
	}
	writeGovernedSkill(t, configured, "governed", "1.0.1", "governed", "updated", nil)
	if err := catalog.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	handle, err := catalog.HandleForName(t.Context(), "governed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.LoadHandle(t.Context(), handle); !errors.Is(err, ErrLockDrift) {
		t.Fatalf("refresh bypassed lock: %v", err)
	}
}
