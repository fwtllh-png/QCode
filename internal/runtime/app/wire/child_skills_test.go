package wire

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/skill"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	webtool "github.com/fwtllh-png/QCode/internal/adapter/tool/web"
	"github.com/fwtllh-png/QCode/internal/buildinfo"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

func TestChildSkillsUseOwnCatalogAndRediscoverPrivateHome(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		name := "writing-agent"
		if interactive {
			name = "isolated-chat"
		}
		t.Run(name, func(t *testing.T) {
			workspace, parentHome := t.TempDir(), t.TempDir()
			writeChildSkill(t, parentHome, "parent-only", "Parent private instructions.")
			writeChildSkill(t, workspace, "local-guide", "Parent workspace instructions.")
			paths := childSkillPaths(t, workspace)
			writeChildSkill(t, paths.UserHome, "user-guide", "Host user instructions.")
			builder := childSkillBuilder(t, paths, workspace, parentHome)
			root, siblingRoot := t.TempDir(), t.TempDir()
			writeChildSkill(t, root, "local-guide", "Child workspace instructions.")
			spec := app.ChildSpec{
				Workspace: root, HostWorkspace: workspace, HostSeeded: interactive,
			}
			child, err := builder.BuildChild(spec)
			if err != nil {
				t.Fatal(err)
			}
			options := child.Underlying().OptionsSeed()
			listed := listChildSkills(t, options.Tools)
			for _, name := range []string{"local-guide", "user-guide", "system-debugging"} {
				if listed[name].Handle == "" {
					t.Fatalf("child skills missing %s: %+v", name, listed)
				}
			}
			if _, exists := listed["parent-only"]; exists {
				t.Fatal("child inherited parent private HOME")
			}
			snapshot, err := agentengine.SnapshotTurnSpec(
				options, agentengine.TurnIdentity{TurnID: "child-skill-turn"},
				agentengine.TurnRequest{Prompt: "Use local-guide"},
			)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, selected := range snapshot.Skills {
				if selected.Name != "local-guide" {
					continue
				}
				found = true
				if selected.Handle != listed["local-guide"].Handle {
					t.Fatal("child selector and tools use different catalogs")
				}
				read, err := readChildSkill(t, options.Tools, selected.Handle)
				if err != nil || !strings.Contains(read.Content, "Child workspace instructions.") {
					t.Fatalf("selected skill read = %+v, %v", read, err)
				}
			}
			if !found {
				t.Fatal("child turn snapshot did not select its local-guide")
			}
			parentSkills := listChildSkills(t, builder.seed.Tools)
			for _, name := range []string{"parent-only", "local-guide"} {
				if _, err := readChildSkill(t, options.Tools, parentSkills[name].Handle); err == nil {
					t.Fatalf("child accepted parent-only handle for %s", name)
				}
			}

			toolset := builder.childTools.built[root]
			executionPolicy, ok := sandbox.BackendPolicy(toolset.backend)
			if !ok || executionPolicy.PrivateTemp == "" {
				t.Fatal("child execution HOME is missing")
			}
			writeChildSkill(t, executionPolicy.PrivateTemp, "installed", "Installed in child HOME.")
			freshSelection, _, err := options.TurnSnapshots.SkillSelection("Use installed")
			if err != nil || len(freshSelection) == 0 || freshSelection[0].Name != "installed" {
				t.Fatalf("next turn did not refresh installed skills: %+v, %v", freshSelection, err)
			}
			if fresh := listChildSkills(t, options.Tools)["installed"]; fresh.Handle != freshSelection[0].Handle {
				t.Fatal("current tool catalog did not refresh with turn selection")
			}
			builder.childTools.release(root)
			child, err = builder.BuildChild(spec)
			if err != nil {
				t.Fatal(err)
			}
			options = child.Underlying().OptionsSeed()
			listed = listChildSkills(t, options.Tools)
			installed, exists := listed["installed"]
			if !exists {
				t.Fatal("rebuild did not discover installed skill in child HOME")
			}
			selected, _, err := options.TurnSnapshots.SkillSelection("Use installed")
			if err != nil || len(selected) == 0 || selected[0].Handle != installed.Handle {
				t.Fatalf("rebuilt selection = %+v, %v", selected, err)
			}
			if read, err := readChildSkill(t, options.Tools, installed.Handle); err != nil ||
				!strings.Contains(read.Content, "Installed in child HOME.") {
				t.Fatalf("installed read = %+v, %v", read, err)
			}
			sibling, err := builder.BuildChild(app.ChildSpec{
				Workspace: siblingRoot, HostWorkspace: workspace, HostSeeded: interactive,
			})
			if err != nil {
				t.Fatal(err)
			}
			siblingTools := sibling.Underlying().OptionsSeed().Tools
			if _, exists := listChildSkills(t, siblingTools)["installed"]; exists {
				t.Fatal("sibling discovered another child's HOME")
			}
			if _, err := readChildSkill(t, siblingTools, installed.Handle); err == nil {
				t.Fatal("sibling accepted another child's skill handle")
			}
			if _, exists := listChildSkills(t, builder.seed.Tools)["installed"]; exists {
				t.Fatal("parent catalog changed after child install")
			}

			state, err := skill.NewStateStore(paths.SkillsStatePath)
			if err != nil {
				t.Fatal(err)
			}
			if err := state.SetEnabled("installed", false); err != nil {
				t.Fatal(err)
			}
			if _, exists := listChildSkills(t, options.Tools)["installed"]; exists {
				t.Fatal("disabled skill is still listed")
			}
			if _, err := readChildSkill(t, options.Tools, installed.Handle); err == nil {
				t.Fatal("disabled skill is still readable")
			}
			selected, _, err = options.TurnSnapshots.SkillSelection("Use installed")
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range selected {
				if item.Name == "installed" {
					t.Fatal("disabled skill is still selected")
				}
			}
		})
	}
}

func TestChildSkillsKeepOwnerLockAndRejectDrift(t *testing.T) {
	workspace, root := t.TempDir(), t.TempDir()
	paths := childSkillPaths(t, workspace)
	for _, directory := range []string{workspace, root} {
		writeChildSkill(t, directory, "governed", "Locked instructions.")
		manifest := "schema_version = 1\nname = \"governed\"\nversion = \"1.0.0\"\nqcode = \">=0.0.0-0\"\n"
		if err := os.WriteFile(filepath.Join(directory, ".agents", "skills", "governed", "skill.toml"),
			[]byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := skill.NewLockStore(paths.SkillsLockPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := skill.Discover(skill.DiscoveryOptions{
		Workspace: workspace, UserHome: paths.UserHome, IncludeBuiltins: true,
		Lock: lock, RuntimeVersion: buildinfo.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.WriteLock(t.Context()); err != nil {
		t.Fatal(err)
	}
	lockedBytes, err := os.ReadFile(paths.SkillsLockPath)
	if err != nil {
		t.Fatal(err)
	}
	builder := childSkillBuilder(t, paths, workspace, t.TempDir())
	spec := app.ChildSpec{Workspace: root, HostWorkspace: workspace}
	child, err := builder.BuildChild(spec)
	if err != nil {
		t.Fatalf("child rejected identical owner-locked skill: %v", err)
	}
	registry := child.Underlying().OptionsSeed().Tools
	handle := listChildSkills(t, registry)["governed"].Handle
	if _, err := readChildSkill(t, registry, handle); err != nil {
		t.Fatal(err)
	}
	writeChildSkill(t, root, "governed", "Unapproved changed instructions.")
	if _, err := readChildSkill(t, registry, handle); !errors.Is(err, skill.ErrLockDrift) {
		t.Fatalf("changed governed skill read = %v", err)
	}
	builder.childTools.release(root)
	child, err = builder.BuildChild(spec)
	if err != nil {
		t.Fatalf("lock drift blocked child runtime: %v", err)
	}
	registry = child.Underlying().OptionsSeed().Tools
	handle = listChildSkills(t, registry)["governed"].Handle
	if _, err := readChildSkill(t, registry, handle); !errors.Is(err, skill.ErrLockDrift) {
		t.Fatalf("rebuilt child accepted unlocked content: %v", err)
	}
	builtin := listChildSkills(t, registry)["system-debugging"]
	if _, err := readChildSkill(t, registry, builtin.Handle); err != nil {
		t.Fatalf("lock drift blocked builtin skill: %v", err)
	}
	after, err := os.ReadFile(paths.SkillsLockPath)
	if err != nil || string(after) != string(lockedBytes) {
		t.Fatalf("child changed owner's lock: %v", err)
	}
	writeChildSkill(t, root, "governed", "Locked instructions.")
	builder.childTools.release(root)
	child, err = builder.BuildChild(spec)
	if err != nil {
		t.Fatalf("rebuild after restoring locked content: %v", err)
	}
	registry = child.Underlying().OptionsSeed().Tools
	handle = listChildSkills(t, registry)["governed"].Handle
	if _, err := readChildSkill(t, registry, handle); err != nil {
		t.Fatalf("restored locked skill read: %v", err)
	}
}

func TestSharedChildrenKeepParentSkillCatalog(t *testing.T) {
	workspace, parentHome := t.TempDir(), t.TempDir()
	writeChildSkill(t, parentHome, "parent-only", "Parent instructions.")
	builder := childSkillBuilder(t, childSkillPaths(t, workspace), workspace, parentHome)
	for _, spec := range []app.ChildSpec{
		{Workspace: workspace, HostWorkspace: workspace, ReadOnly: true},
		{Workspace: workspace, HostWorkspace: workspace, Serialized: true},
	} {
		child, err := builder.BuildChild(spec)
		if err != nil {
			t.Fatal(err)
		}
		options := child.Underlying().OptionsSeed()
		if options.Tools != builder.seed.Tools {
			t.Fatal("shared child replaced parent tools")
		}
		selected, _, err := options.TurnSnapshots.SkillSelection("Use parent-only")
		if err != nil || len(selected) == 0 || selected[0].Name != "parent-only" {
			t.Fatalf("shared child selection = %+v, %v", selected, err)
		}
	}
	if len(builder.childTools.built) != 0 {
		t.Fatal("shared child allocated an isolated toolset")
	}
}

func childSkillPaths(t *testing.T, workspace string) SkillPaths {
	t.Helper()
	paths, err := ResolveSkillPaths(SkillOptions{
		DataDir: t.TempDir(), UserHome: t.TempDir(),
	}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func childSkillBuilder(t *testing.T, paths SkillPaths, workspace, parentHome string) runtimeCoreBuilder {
	t.Helper()
	registry := tool.NewRegistry(nil, nil)
	t.Cleanup(func() { _ = registry.Close() })
	var capabilities capabilityBuildState
	if err := (skillContributor{
		paths: paths, workspace: workspace, sandboxHome: parentHome, output: &capabilities,
	}).Contribute(t.Context(), registry); err != nil {
		t.Fatal(err)
	}
	route, err := resolveExecRoute(execRouteOptions{
		ProviderID: "fixture", ModelID: "fixture-model", BaseURL: "http://127.0.0.1:1",
		Fixture: true, Model: fixtureModel("fixture-model"), Protocol: model.ProtocolOpenAIChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	toolsets := newChildToolsets("", contentstore.NewMemory(contentstore.Options{}),
		webtool.Options{}, config.Verify{}, config.Journal{}, nil, nil, nil,
		"", 0, t.TempDir(), paths)
	toolsets.bindInteractions(nil, nil)
	t.Cleanup(toolsets.closeAll)
	return runtimeCoreBuilder{
		childTools: toolsets,
		seed: agentengine.Options{
			ProviderConfig: agentengine.ProviderConfig{Provider: unusedSkillProvider{}, Route: route},
			ToolConfig:     agentengine.ToolConfig{Tools: registry},
			SecurityConfig: agentengine.SecurityConfig{
				Workspace: workspace, Security: policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
			},
			LifecycleConfig: agentengine.LifecycleConfig{
				TurnCoordinatorRuntime: turnkernel.NewEphemeralCoordinatorRuntime(),
			},
			ContextConfig: agentengine.ContextConfig{TurnSnapshots: agentengine.TurnSnapshotSources{
				SkillSelection: func(query string) ([]agentengine.SkillSummary, agentengine.SkillSelectionMetrics, error) {
					return selectTurnSkills(capabilities.skillCatalog, query)
				},
			}},
		},
	}
}

type unusedSkillProvider struct{}

func (unusedSkillProvider) Stream(context.Context, provider.ModelRequest) (provider.Stream, error) {
	return nil, errors.New("skill wiring test must not call a provider")
}

func writeChildSkill(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, ".agents", "skills", name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: Use " + name + ".\n---\n" + body + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func listChildSkills(t *testing.T, registry *tool.Registry) map[string]skill.Summary {
	t.Helper()
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "skills_list", Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Skills     []skill.Summary `json:"skills"`
		NextCursor string          `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(result.Content), &page); err != nil {
		t.Fatal(err)
	}
	if page.NextCursor != "" {
		t.Fatal("test fixture unexpectedly needs pagination")
	}
	resultByName := make(map[string]skill.Summary, len(page.Skills))
	for _, item := range page.Skills {
		resultByName[item.Name] = item
	}
	return resultByName
}

func readChildSkill(t *testing.T, registry *tool.Registry, handle string) (tool.Result, error) {
	t.Helper()
	arguments, err := json.Marshal(map[string]string{"handle": handle})
	if err != nil {
		t.Fatal(err)
	}
	return tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "skills_read", Arguments: arguments,
	})
}
