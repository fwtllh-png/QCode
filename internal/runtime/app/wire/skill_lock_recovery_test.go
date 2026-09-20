package wire

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/skill"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestSkillLockDriftKeepsRuntimeAndControlAvailable(t *testing.T) {
	workspace, dataDir, home := t.TempDir(), t.TempDir(), t.TempDir()
	tools := true
	skillOptions := SkillOptions{DataDir: dataDir, UserHome: home}
	paths, err := ResolveSkillPaths(skillOptions, workspace)
	if err != nil {
		t.Fatal(err)
	}
	options := withNonDurableTestJournal(t, ExecOptions{
		FixturePath: subagentFixture(t, "subagent"), Permission: "auto",
		ConfigOverrides: config.Overrides{
			Workspace: &workspace, Tools: &tools, StateDataDir: &dataDir,
		},
		Skills: skillOptions,
	})
	open := func() (*Session, *SkillControlHandle) {
		t.Helper()
		session, err := NewExec(t.Context(), options)
		if err != nil {
			t.Fatalf("open runtime: %v", err)
		}
		t.Cleanup(func() { _ = session.Close(context.Background()) })
		control, err := session.OpenSkillControl(paths, workspace)
		if err != nil {
			t.Fatalf("open control: %v", err)
		}
		t.Cleanup(func() { _ = control.Close() })
		return session, control
	}
	submit := func(control *SkillControlHandle, id string, action protocol.ExtensionControlAction, name string) {
		t.Helper()
		result, err := control.Service.Submit(t.Context(), controlOperation(id, action, name))
		if err != nil || result.Receipt == nil || result.Receipt.Status != "committed" {
			t.Fatalf("%s: result=%+v, err=%v", action, result, err)
		}
	}
	session, control := open()
	submit(control, "initial-lock", protocol.ExtensionActionLock, "")
	for _, name := range []string{"alpha", "beta"} {
		writeControlSkill(t, workspace, name)
		manifest := "schema_version = 1\nname = \"" + name +
			"\"\nversion = \"1.0.0\"\nqcode = \">=0.0.0-0\"\n"
		path := filepath.Join(workspace, ".agents", "skills", name, "skill.toml")
		if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The running control plane still owns the pre-install discovery snapshot.
	submit(control, "old-snapshot-lock", protocol.ExtensionActionLock, "")
	lock, err := skill.NewLockStore(paths.SkillsLockPath)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := lock.Read()
	if err != nil || len(locked.Skills) != 0 {
		t.Fatalf("old discovery snapshot lock = %+v, %v", locked, err)
	}
	lockedBytes, err := os.ReadFile(paths.SkillsLockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	session, control = open()
	after, err := os.ReadFile(paths.SkillsLockPath)
	if err != nil || string(after) != string(lockedBytes) {
		t.Fatalf("runtime implicitly changed lock: %v", err)
	}
	snapshot, err := control.Service.Snapshot(t.Context(), protocol.ExtensionControlAll)
	if err != nil || extensionByName(snapshot.Extensions, "beta") == nil {
		t.Fatalf("installed skill not available to control: %+v, %v", snapshot, err)
	}
	if _, err := control.Service.Submit(t.Context(),
		controlOperation("verify-drift", protocol.ExtensionActionVerify, ""),
	); !errors.Is(err, skill.ErrLockDrift) {
		t.Fatalf("control did not report lock drift: %v", err)
	}
	submit(control, "accept-install", protocol.ExtensionActionLock, "")
	submit(control, "verify-install", protocol.ExtensionActionVerify, "")
	submit(control, "disable-alpha", protocol.ExtensionActionDisable, "alpha")
	submit(control, "verify-after-disable", protocol.ExtensionActionVerify, "")
	if err := session.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, control = open()
	submit(control, "verify-after-reopen", protocol.ExtensionActionVerify, "")
	snapshot, err = control.Service.Snapshot(t.Context(), protocol.ExtensionControlAll)
	if err != nil {
		t.Fatal(err)
	}
	alpha, beta := extensionByName(snapshot.Extensions, "alpha"), extensionByName(snapshot.Extensions, "beta")
	if alpha == nil || alpha.Enabled || beta == nil || !beta.Enabled {
		t.Fatalf("enablement after reopen: alpha=%+v, beta=%+v", alpha, beta)
	}
}
