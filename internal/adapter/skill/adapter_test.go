package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
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
	catalog, err := Discover(DiscoveryOptions{
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
	lock, err := NewLockStore(filepath.Join(t.TempDir(), "skills.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Discover(DiscoveryOptions{
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
	resolved, ok := result.Metadata["resolved_skills"].([]ResolvedSkill)
	if !ok || len(resolved) != 2 || !resolved[0].Locked || resolved[1].Name != "review" {
		t.Fatalf("resolved_skills = %#v", result.Metadata["resolved_skills"])
	}
}

func TestSkillDiscoveryToolsPageAndReadAuthorityBoundContent(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".agents", "skills")
	for index := range 25 {
		name := fmt.Sprintf("skill-%02d", index)
		body := "body " + name
		if index == 0 {
			body = strings.Repeat("bounded content\n", 5000)
		}
		writeToolSkill(t, root, name, "Operate "+name, body)
	}
	catalog, err := Discover(DiscoveryOptions{
		Workspace: workspace, UserHome: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterDiscovery(registry, catalog); err != nil {
		t.Fatal(err)
	}
	first, err := tooltest.Execute(context.Background(), registry, tool.Call{
		Name: "skills.list", Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Skills     []listedSkill `json:"skills"`
		NextCursor string        `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(first.Content), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Skills) != 20 || page.NextCursor == "" {
		t.Fatalf("first page = %+v", page)
	}
	secondArgs, _ := json.Marshal(listInput{Cursor: page.NextCursor})
	second, err := tooltest.Execute(context.Background(), registry, tool.Call{
		Name: "skills.list", Arguments: secondArgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	var secondPage struct {
		Skills []listedSkill `json:"skills"`
	}
	if err := json.Unmarshal([]byte(second.Content), &secondPage); err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Skills) != 5 {
		t.Fatalf("second page count = %d", len(secondPage.Skills))
	}

	target := page.Skills[0]
	for name, handle := range map[string]string{
		"skill":    target.Handle,
		"package":  target.PackageHandle,
		"resource": target.ResourceHandle,
	} {
		t.Run(name+"_handle", func(t *testing.T) {
			readArgs, _ := json.Marshal(readInput{Handle: handle})
			read, readErr := tooltest.Execute(context.Background(), registry, tool.Call{
				Name: "skills.read", Arguments: readArgs,
			})
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(read.Content) > skillReadBytes ||
				read.Metadata["next_cursor"] == "" ||
				read.Metadata["handle"] != target.Handle ||
				read.Admission == nil || read.Admission.Kind != "skill" {
				t.Fatalf("read result = %+v", read)
			}
		})
	}
	staleArgs, _ := json.Marshal(readInput{
		Handle: "skh_" + strings.Repeat("0", 40),
	})
	if _, err := tooltest.Execute(context.Background(), registry, tool.Call{
		Name: "skills.read", Arguments: staleArgs,
	}); err == nil {
		t.Fatal("mismatched authority-bound resource was accepted")
	} else if hint, ok := tool.RecoveryHintFromError(err); !ok ||
		hint.ErrorCategory != ErrorCategoryHandleInvalid ||
		hint.RequiredAction != "skills_list" || hint.RetryOriginal {
		t.Fatalf("stale skill recovery hint = %+v, found = %t", hint, ok)
	}
}

func TestSkillDiscoveryToolSchemaFitsCE7RegressionBudget(t *testing.T) {
	type schema struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Input       map[string]any `json:"input_schema"`
	}
	var total int
	for _, descriptor := range []tool.Descriptor{
		listDescriptor(), readDescriptor(),
	} {
		data, err := json.Marshal(schema{
			Name: descriptor.Name, Description: descriptor.Description,
			Input: descriptor.InputSchema,
		})
		if err != nil {
			t.Fatal(err)
		}
		total += len(data)
	}
	const maxSchemaDeltaBytes = 640
	if total > maxSchemaDeltaBytes {
		t.Fatalf(
			"skill discovery schema delta = %d bytes, maximum = %d",
			total, maxSchemaDeltaBytes,
		)
	}
	t.Logf("skill discovery schema delta = %d bytes", total)
}

func TestSkillListRefreshesFirstPageWithoutMixingPagination(t *testing.T) {
	workspace, private := t.TempDir(), t.TempDir()
	root := filepath.Join(private, ".agents", "skills")
	catalog, err := Discover(DiscoveryOptions{
		Workspace: workspace, UserHome: t.TempDir(), SandboxHome: private,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterDiscovery(registry, catalog); err != nil {
		t.Fatal(err)
	}
	for index := range 25 {
		name := fmt.Sprintf("skill-%02d", index)
		writeToolSkill(t, root, name, name, "body")
	}
	list := func(cursor string) tool.Result {
		t.Helper()
		args, _ := json.Marshal(listInput{Cursor: cursor})
		result, err := tooltest.Execute(t.Context(), registry, tool.Call{Name: "skills_list", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := list("")
	cursor := first.Metadata["next_cursor"].(string)
	if first.Metadata["count"] != 20 || cursor == "" {
		t.Fatalf("new installation was not discovered: %+v", first)
	}
	writeToolSkill(t, root, "added", "added", "new")
	second := list(cursor)
	if second.Metadata["count"] != 5 {
		t.Fatalf("continuation silently rescanned: %+v", second)
	}
	list("")
	if _, err := catalog.HandleForName(t.Context(), "added"); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(listInput{Cursor: cursor})
	if _, err := tooltest.Execute(t.Context(), registry, tool.Call{Name: "skills_list", Arguments: args}); err == nil {
		t.Fatal("stale cursor accepted after catalog changed")
	}
}

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
	lock, err := NewLockStore(filepath.Join(t.TempDir(), "skills.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Discover(DiscoveryOptions{
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
		resolved := result.Metadata["resolved_skills"].([]ResolvedSkill)
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

func writeToolSkill(
	t *testing.T,
	root, name, description, body string,
) {
	t.Helper()
	directory := filepath.Join(root, name)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description +
		"\n---\n" + body + "\n"
	if err := os.WriteFile(
		filepath.Join(directory, "SKILL.md"), []byte(content), 0o600,
	); err != nil {
		t.Fatal(err)
	}
}
