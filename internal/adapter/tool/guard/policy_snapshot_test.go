package guard

import (
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestSharedPolicyKeepsWorkspaceRulesAndLiveUpdatesIsolated(t *testing.T) {
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
	rule := policy.Rule{Tool: "*", Resource: "app/**", Action: policy.ActionDeny}
	runtime.Grants = []policy.Rule{rule}
	runtime.Repository = []policy.Rule{rule}
	runtime.Constitution = []policy.Rule{rule}
	runtime.User = []policy.Rule{rule}
	var guards []*Guard
	for range 2 {
		g, err := New(Options{
			Registry: tool.NewRegistry(nil, nil), Policy: runtime, Workspace: t.TempDir(),
		})
		if err != nil {
			t.Fatal(err)
		}
		guards = append(guards, g)
	}
	for _, rules := range [][]policy.Rule{runtime.Grants, runtime.Repository, runtime.Constitution, runtime.User} {
		if rules[0] != rule {
			t.Fatalf("constructing a Guard mutated shared rules: %+v", rules[0])
		}
	}
	snapshots := make([]*policy.Runtime, len(guards))
	for index, g := range guards {
		snapshot, err := g.samplePolicy()
		if err != nil {
			t.Fatal(err)
		}
		snapshots[index] = snapshot
		want := policy.EscapePathPattern(filepath.ToSlash(filepath.Join(g.workspace, "app"))) + "/**"
		for _, rules := range [][]policy.Rule{snapshot.Grants, snapshot.Repository, snapshot.Constitution, snapshot.User} {
			if rules[0].ResourcePath != want {
				t.Fatalf("workspace %d rule = %q, want %q", index, rules[0].ResourcePath, want)
			}
		}
	}
	updated := rule
	updated.Resource = "updated/**"
	if _, err := runtime.ReloadSources([]policy.Rule{updated}, []policy.Rule{updated}); err != nil {
		t.Fatal(err)
	}
	runtime.SetPermission(policy.PermissionNever)
	for index, g := range guards {
		snapshot, err := g.samplePolicy()
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Permission != policy.PermissionNever || snapshot.Repository[0].Resource != updated.Resource {
			t.Fatalf("Guard missed a live policy update: %+v", snapshot)
		}
		if snapshots[index].Permission != policy.PermissionBypass || snapshots[index].Repository[0].Resource != rule.Resource {
			t.Fatal("live policy update changed a frozen snapshot")
		}
	}
}
