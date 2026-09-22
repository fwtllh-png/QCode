package skill

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	skillruntime "github.com/fwtllh-png/QCode/internal/adapter/skill"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

func TestSkillsReadExecutesThroughTestRegistry(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(workspace, ".agents", "skills", "review")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(`---
name: review
description: Review changes
---
Follow the review checklist.
`), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := skillruntime.Discover(skillruntime.DiscoveryOptions{
		Workspace: workspace, UserHome: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterDiscovery(registry, catalog); err != nil {
		t.Fatal(err)
	}
	handle, err := catalog.HandleForName(t.Context(), "review")
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(map[string]string{"handle": handle})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(context.Background(), registry, tool.Call{
		Name: "skills_read", Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(result.Content, "\n\nFollow the review checklist.") ||
		result.Metadata["name"] != "review" {
		t.Fatalf("result = %+v", result)
	}
}

func TestSkillsReadReturnsLockedDependencyPlan(t *testing.T) {
	workspace := t.TempDir()
	configured := filepath.Join(workspace, "configured")
	writeGoverned := func(name, version, body, dependencies string) {
		t.Helper()
		directory := filepath.Join(configured, name)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(`---
name: `+name+`
description: `+name+`
---
`+body+`
`), 0o600); err != nil {
			t.Fatal(err)
		}
		manifest := `schema_version = 1
name = "` + name + `"
version = "` + version + `"
qcode = ">=1.0.0 <2.0.0"
` + dependencies
		if err := os.WriteFile(
			filepath.Join(directory, "skill.toml"), []byte(manifest), 0o600,
		); err != nil {
			t.Fatal(err)
		}
	}
	writeGoverned("base", "2.1.0", "Base instructions.", "")
	writeGoverned(
		"review", "1.0.0", "Review instructions.",
		"\n[dependencies]\nbase = \"^2.0.0\"\n",
	)
	lock, err := skillruntime.NewLockStore(filepath.Join(t.TempDir(), "skills.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := skillruntime.Discover(skillruntime.DiscoveryOptions{
		Workspace: workspace, ConfiguredDir: configured, UserHome: t.TempDir(),
		RuntimeVersion: "1.1.0", Lock: lock,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.WriteLock(t.Context()); err != nil {
		t.Fatal(err)
	}
	handle, err := catalog.HandleForName(t.Context(), "review")
	if err != nil {
		t.Fatal(err)
	}
	result, err := (&readTool{catalog: catalog}).run(
		t.Context(),
		readInput{Handle: handle},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "Skill dependency: base@2.1.0") ||
		!strings.Contains(result.Content, "Skill root: review@1.0.0") {
		t.Fatalf("content = %q", result.Content)
	}
	resolved, ok := result.Metadata["resolved_skills"].([]skillruntime.ResolvedSkill)
	if !ok || len(resolved) != 2 || !resolved[0].Locked || resolved[1].Name != "review" {
		t.Fatalf("resolved_skills = %#v", result.Metadata["resolved_skills"])
	}
}
