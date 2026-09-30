package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStateDisableAndMalformedStateRecovery(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	writeSkill(t, filepath.Join(workspace, ".agents", "skills"), "native", "native", "native body")
	statePath := filepath.Join(t.TempDir(), "skills.json")
	state, err := NewStateStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Discover(DiscoveryOptions{
		Workspace: workspace, UserHome: home, State: state,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.SetEnabled("native", false); err != nil {
		t.Fatal(err)
	}
	if summaries := catalog.Summaries(context.Background()); len(summaries) != 0 {
		t.Fatalf("disabled summaries = %+v", summaries)
	}
	if _, err := catalog.Load(context.Background(), "native"); err == nil {
		t.Fatal("disabled native skill loaded")
	}

	if err := os.WriteFile(statePath, []byte("{malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := Discover(DiscoveryOptions{
		Workspace: workspace, UserHome: home, State: state,
	})
	if err != nil {
		t.Fatal(err)
	}
	summaries, issues := recovered.List(context.Background())
	if len(summaries) != 1 || summaries[0].Name != "native" {
		t.Fatalf("malformed-state summaries = %+v", summaries)
	}
	if len(issues) == 0 || !strings.Contains(issues[len(issues)-1].Reason, "enable state") {
		t.Fatalf("malformed-state issues = %+v", issues)
	}
}

func TestStateConcurrentUpdatesAreAtomic(t *testing.T) {
	store, err := NewStateStore(filepath.Join(t.TempDir(), "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	const count = 64
	var wait sync.WaitGroup
	for index := range count {
		wait.Add(1)
		go func() {
			defer wait.Done()
			name := fmt.Sprintf("skill-%02d", index)
			if err := store.SetEnabled(name, index%2 == 0); err != nil {
				t.Errorf("SetEnabled(%q): %v", name, err)
			}
		}()
	}
	wait.Wait()
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != count {
		t.Fatalf("state entries = %d, want %d", len(state), count)
	}
	for index := range count {
		name := fmt.Sprintf("skill-%02d", index)
		if state[name] != (index%2 == 0) {
			t.Fatalf("state[%q] = %t", name, state[name])
		}
	}
}
