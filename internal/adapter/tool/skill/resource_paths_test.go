package skill

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	skillruntime "github.com/fwtllh-png/QCode/internal/adapter/skill"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

func TestSkillReadLocatesResourcesForRootAndDependency(t *testing.T) {
	workspace, home := t.TempDir(), t.TempDir()
	// Both packages use a directory name different from their declared name.
	roots := map[string]string{
		"review": filepath.Join(workspace, ".agents", "skills", "group", "package-one"),
		"guide":  filepath.Join(home, ".agents", "skills", "package-two"),
	}
	for name, root := range roots {
		if err := os.MkdirAll(filepath.Join(root, "references"), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: test\n---\nRead references/checklist.md."
		if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		manifest := "schema_version = 1\nname = " + strconv.Quote(name) +
			"\nversion = \"1.0.0\"\nqcode = \">=1.0.0\"\n"
		if name == "review" {
			manifest += "[dependencies]\nguide = \"^1.0.0\"\n"
		}
		if err := os.WriteFile(filepath.Join(root, "skill.toml"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "references", "checklist.md"), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := skillruntime.NewLockStore(filepath.Join(t.TempDir(), "skills.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := skillruntime.Discover(skillruntime.DiscoveryOptions{
		Workspace: workspace, UserHome: home, Lock: lock, RuntimeVersion: "1.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.WriteLock(t.Context()); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterDiscovery(registry, catalog); err != nil {
		t.Fatal(err)
	}
	listed, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "skills_list", Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var page struct{ Skills []listedSkill }
	if err := json.Unmarshal([]byte(listed.Content), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Skills) != 2 {
		t.Fatalf("listed skills = %+v", page)
	}
	for _, summary := range page.Skills {
		canonicalRoot, err := filepath.EvalSymlinks(roots[summary.Name])
		if err != nil {
			t.Fatal(err)
		}
		if summary.Path != filepath.Join(canonicalRoot, "SKILL.md") {
			t.Fatalf("listed path = %s", summary.Path)
		}
		args, _ := json.Marshal(readInput{Handle: summary.Handle})
		result, err := tooltest.Execute(t.Context(), registry, tool.Call{Name: "skills_read", Arguments: args})
		if err != nil || result.IsError {
			t.Fatalf("read=%+v err=%v", result, err)
		}
		if result.Metadata["path"] != summary.Path {
			t.Fatalf("root provenance lost: %+v", result.Metadata)
		}
		resolved := result.Metadata["resolved_skills"].([]skillruntime.ResolvedSkill)
		wantCount := 1
		if summary.Name == "review" {
			wantCount = 2
		}
		if len(resolved) != wantCount {
			t.Fatalf("resolved=%+v", resolved)
		}
		for _, item := range resolved {
			base := filepath.Dir(item.Path)
			if !strings.Contains(result.Content, "source_path="+strconv.Quote(item.Path)) ||
				!strings.Contains(result.Content, "resource_base="+strconv.Quote(base)) {
				t.Fatalf("missing loaded source: %s", result.Content)
			}
			data, err := os.ReadFile(filepath.Join(base, "references", "checklist.md"))
			if err != nil || string(data) != item.Name {
				t.Fatalf("relative resource resolved to wrong package: %q %v", data, err)
			}
		}
	}
}

func TestBuiltinSkillSourceIsNotPresentedAsFilesystemPath(t *testing.T) {
	content := renderLoadedPlan([]skillruntime.Loaded{{
		Summary: skillruntime.Summary{Name: "example", Source: skillruntime.SourceBuiltin,
			Path: "builtin://builtins/example/SKILL.md"},
		Content: "self-contained",
	}})
	if !strings.Contains(content, "not a filesystem path") ||
		strings.Contains(content, "resource_base=") {
		t.Fatalf("builtin path guidance = %s", content)
	}
}
