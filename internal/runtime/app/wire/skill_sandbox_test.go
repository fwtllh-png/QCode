package wire

import (
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestSkillToolsAndControlUseExecutionSandboxHome(t *testing.T) {
	workspace, privateHome := t.TempDir(), t.TempDir()
	writeControlSkill(t, privateHome, "installed")
	backend, err := sandbox.BindPolicy(indexTestBackend{}, sandbox.Options{
		WorkspaceRoot: workspace, PrivateTemp: privateHome, SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := ResolveSkillPaths(SkillOptions{
		DataDir: t.TempDir(), UserHome: t.TempDir(),
	}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	state := &buildState{session: &Session{}}
	state.session.sandbox = backend
	state.config.execution = config.Execution{Workspace: workspace, Tools: true}
	state.config.skillPaths = paths
	state.platform.backend = backend
	state.tools.registry = tool.NewRegistry(nil, nil)
	if err := (capabilityToolsModule{}).Build(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	loaded, err := state.tools.skillCatalog.Load(t.Context(), "installed")
	if err != nil {
		t.Fatal(err)
	}
	canonicalHome, err := filepath.EvalSymlinks(privateHome)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Path != filepath.Join(canonicalHome, ".agents", "skills", "installed", "SKILL.md") {
		t.Fatalf("installed path = %q", loaded.Path)
	}
	control, err := state.session.OpenSkillControl(paths, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	snapshot, err := control.Service.Snapshot(t.Context(), protocol.ExtensionControlAll)
	if err != nil {
		t.Fatal(err)
	}
	installed := extensionByName(snapshot.Extensions, "installed")
	if installed == nil || !installed.Enabled || installed.Source != "workspace" {
		t.Fatalf("installed control projection = %+v", installed)
	}
}
