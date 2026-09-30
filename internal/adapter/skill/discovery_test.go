package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestDiscoveryFirstMatchPrecedenceAndLocale(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	configured := t.TempDir()
	locations := []struct {
		root        string
		description string
	}{
		{filepath.Join(workspace, ".agents", "skills"), "workspace agents"},
		{filepath.Join(workspace, "skills"), "workspace plain"},
		{filepath.Join(workspace, ".opencode", "skills"), "workspace opencode"},
		{filepath.Join(workspace, ".claude", "skills"), "workspace claude"},
		{filepath.Join(workspace, ".cursor", "skills"), "workspace cursor"},
		{filepath.Join(workspace, ".qcode", "skills"), "workspace qcode"},
		{configured, "configured"},
		{filepath.Join(home, ".agents", "skills"), "user agents"},
		{filepath.Join(home, ".claude", "skills"), "user claude"},
		{filepath.Join(home, ".qcode", "skills"), "user qcode"},
	}
	for _, location := range locations {
		if location.root == configured {
			writeGovernedSkill(
				t, location.root, "duplicate", "1.0.0",
				location.description, "instructions", nil,
			)
		} else {
			writeSkill(t, location.root, "duplicate", location.description, "instructions")
		}
	}
	writeRawSkill(t, filepath.Join(workspace, ".agents", "skills"), "localized", `---
name: localized
description: English description
description_zh-CN: 中文描述
---
# Localized
`)

	catalog, err := Discover(DiscoveryOptions{
		Workspace: workspace, ConfiguredDir: configured, UserHome: home, Locale: "zh_CN",
	})
	if err != nil {
		t.Fatal(err)
	}
	summaries := catalog.Summaries(context.Background())
	if len(summaries) != 2 {
		t.Fatalf("summaries = %+v", summaries)
	}
	byName := make(map[string]Summary)
	for _, summary := range summaries {
		byName[summary.Name] = summary
	}
	if got := byName["duplicate"].Description; got != "workspace agents" {
		t.Fatalf("duplicate description = %q", got)
	}
	if got := byName["localized"].Description; got != "中文描述" {
		t.Fatalf("localized description = %q", got)
	}
}

func TestDiscoveryWithoutInstalledSkillsIsEmpty(t *testing.T) {
	catalog, err := Discover(DiscoveryOptions{
		Workspace: t.TempDir(), UserHome: t.TempDir(), SandboxHome: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	summaries, issues := catalog.List(t.Context())
	if len(issues) != 0 {
		t.Fatalf("discovery issues = %+v", issues)
	}
	if len(summaries) != 0 {
		t.Fatalf("unexpected skills = %+v", summaries)
	}
}

func TestSymlinkTraversalRejectedAtDiscoveryAndLoad(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".agents", "skills")
	writeSkill(t, root, "safe", "safe", "safe body")
	outside := t.TempDir()
	writeSkill(t, outside, "escape", "escape", "outside body")
	if err := os.Symlink(filepath.Join(outside, "escape"), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	catalog, err := Discover(DiscoveryOptions{Workspace: workspace, UserHome: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if names := catalog.Names(); len(names) != 1 || names[0] != "safe" {
		t.Fatalf("discovered names = %v", names)
	}
	safeDirectory := filepath.Join(root, "safe")
	if err := os.RemoveAll(safeDirectory); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "escape"), safeDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Load(context.Background(), "safe"); err == nil ||
		!strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink swap load error = %v", err)
	}
}

func TestStrictMetadataAndDiscoveryBounds(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".agents", "skills")
	writeRawSkill(t, root, "unknown", `---
name: unknown
description: unknown
arbitrary: rejected
---
body
`)
	deep := filepath.Join(root, "one", "two", "three")
	writeSkill(t, deep, "too-deep", "deep", "body")
	catalog, err := Discover(DiscoveryOptions{
		Workspace: workspace, UserHome: t.TempDir(), Limits: Limits{MaxDepth: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if names := catalog.Names(); len(names) != 0 {
		t.Fatalf("bounded discovery names = %v", names)
	}
	if len(catalog.Issues()) == 0 {
		t.Fatal("strict metadata rejection was not reported")
	}
}

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

func TestConfiguredSkillWithoutManifestFailsDiscovery(t *testing.T) {
	configured := t.TempDir()
	writeSkill(t, configured, "review", "Review", "Run the review.")
	_, err := Discover(DiscoveryOptions{
		Workspace: t.TempDir(), ConfiguredDir: configured, UserHome: t.TempDir(),
		RuntimeVersion: "1.0.0",
	})
	if err == nil || !strings.Contains(err.Error(), "requires skill.toml") {
		t.Fatalf("Discover() error = %v", err)
	}
}

func writeSkill(t *testing.T, root, name, description, body string) {
	t.Helper()
	writeRawSkill(t, root, name, fmt.Sprintf(`---
name: %s
description: %s
---
%s
`, name, description, body))
}

func writeRawSkill(t *testing.T, root, name, content string) {
	t.Helper()
	directory := filepath.Join(root, name)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeGovernedSkill(
	t *testing.T,
	root, name, version, description, body string,
	dependencies map[string]string,
) {
	t.Helper()
	writeSkill(t, root, name, description, body)
	var dependencyLines strings.Builder
	if len(dependencies) != 0 {
		dependencyLines.WriteString("\n[dependencies]\n")
		var names []string
		for dependency := range dependencies {
			names = append(names, dependency)
		}
		sort.Strings(names)
		for _, dependency := range names {
			fmt.Fprintf(&dependencyLines, "%s = %q\n", dependency, dependencies[dependency])
		}
	}
	content := fmt.Sprintf(
		"schema_version = 1\nname = %q\nversion = %q\nqcode = \">=1.0.0 <2.0.0\"\n%s",
		name, version, dependencyLines.String(),
	)
	if err := os.WriteFile(
		filepath.Join(root, name, ManifestFileName), []byte(content), 0o600,
	); err != nil {
		t.Fatal(err)
	}
}
