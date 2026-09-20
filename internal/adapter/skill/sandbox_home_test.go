package skill

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoveryIncludesOnlyBoundSandboxHome(t *testing.T) {
	workspace, home, privateHome, otherHome := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	writeSkill(t, filepath.Join(home, ".agents", "skills"), "shared", "host", "host")
	writeSkill(t, filepath.Join(privateHome, ".agents", "skills"), "shared", "sandbox", "sandbox")
	writeSkill(t, filepath.Join(otherHome, ".agents", "skills"), "other-workspace", "other", "other")
	for _, dir := range userSkillDirectories {
		name := filepath.Base(filepath.Dir(dir))
		name = name[1:]
		writeSkill(t, filepath.Join(privateHome, dir), name, name, name)
	}
	options := DiscoveryOptions{Workspace: workspace, UserHome: home, SandboxHome: privateHome}
	catalog, err := Discover(options)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := catalog.Load(t.Context(), "shared")
	if err != nil || loaded.Content != "sandbox" || loaded.Source != SourceWorkspace {
		t.Fatalf("sandbox skill = %+v, error = %v", loaded, err)
	}
	if _, err := catalog.Load(t.Context(), "other-workspace"); err == nil {
		t.Fatal("another Workspace's private skill was discovered")
	}
	for _, name := range []string{"agents", "claude", "qcode"} {
		if _, err := catalog.Load(t.Context(), name); err != nil {
			t.Fatalf("private skill %s: %v", name, err)
		}
	}
	// Catalog reconstruction must retain installed files and the same handles.
	reopened, err := Discover(options)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := reopened.Load(t.Context(), "shared")
	if err != nil || reloaded.Handle != loaded.Handle {
		t.Fatalf("reopened skill = %+v, error = %v", reloaded, err)
	}
	writeSkill(t, filepath.Join(workspace, ".agents", "skills"), "shared", "project", "project")
	project, err := Discover(options)
	if err != nil {
		t.Fatal(err)
	}
	overridden, err := project.Load(t.Context(), "shared")
	if err != nil || overridden.Content != "project" {
		t.Fatalf("workspace precedence = %+v, error = %v", overridden, err)
	}
}

func TestSandboxSkillDirectoryCannotEscapeThroughParentSymlink(t *testing.T) {
	privateHome, outside := t.TempDir(), t.TempDir()
	writeSkill(t, filepath.Join(outside, "skills"), "outside", "outside", "outside")
	if err := os.Symlink(outside, filepath.Join(privateHome, ".agents")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	catalog, err := Discover(DiscoveryOptions{
		Workspace: t.TempDir(), UserHome: t.TempDir(), SandboxHome: privateHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Names()) != 0 || len(catalog.Issues()) == 0 {
		t.Fatalf("escaped sandbox skill: names=%v issues=%v", catalog.Names(), catalog.Issues())
	}
}
