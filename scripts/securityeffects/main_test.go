package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositorySideEffectInventory(t *testing.T) {
	var output bytes.Buffer
	if err := run(
		filepath.Clean("../.."),
		"testdata/contracts/security-side-effect-entrypoints.json",
		&output,
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "security side-effect inventory valid") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestUnownedProcessEntryPointFails(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "internal/unsafe/run.go", `package unsafe

import "os/exec"

func run() {
	_ = exec.Command("untrusted")
}
`)
	writeFixture(t, root, "policy.json", `{
  "version": 1,
  "id": "SEC-EXEC-BOUNDARY-001",
  "rules": []
}`)
	var output bytes.Buffer
	err := run(root, "policy.json", &output)
	if err == nil || !strings.Contains(err.Error(), "unowned process side effect") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestUnownedPlatformProcessEntryPointFails(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "internal/unsafe/run.go", `package unsafe

import "github.com/fwtllh-png/QCode/internal/platform/process"

func run() {
	_, _ = process.Run(nil, process.Options{})
}
`)
	writeFixture(t, root, "policy.json", `{
  "version": 1,
  "id": "SEC-EXEC-BOUNDARY-001",
  "rules": []
}`)
	var output bytes.Buffer
	err := run(root, "policy.json", &output)
	if err == nil ||
		!strings.Contains(
			err.Error(),
			"internal/platform/process.Run",
		) {
		t.Fatalf("run() error = %v", err)
	}
}

func TestPlatformProcessFunctionReferenceIsInventoried(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "internal/unsafe/run.go", `package unsafe

import "github.com/fwtllh-png/QCode/internal/platform/process"

var run = process.Run
`)
	writeFixture(t, root, "policy.json", `{
  "version": 1,
  "id": "SEC-EXEC-BOUNDARY-001",
  "rules": []
}`)
	var output bytes.Buffer
	err := run(root, "policy.json", &output)
	if err == nil ||
		!strings.Contains(
			err.Error(),
			"internal/platform/process.Run",
		) {
		t.Fatalf("run() error = %v", err)
	}
}

func TestWorkspaceAndVCSWritersStayBrokered(t *testing.T) {
	root := filepath.Clean("../..")
	for path, forbidden := range map[string][]string{
		"internal/adapter/tool/file": {
			".AtomicWrite(", ".AtomicCreate(", "process.NewCommand(",
		},
		"internal/adapter/tool/content/content.go":        {".AtomicWrite("},
		"internal/orchestration/subagent/git_worktree.go": {"process.Run("},
		"internal/orchestration/workspacemerge/service.go": {
			"copyRegularFile(", `c.git(ctx, worktree, "apply"`,
			`c.git(ctx, worktree, "add"`, `c.git(ctx, worktree, "commit"`,
		},
	} {
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		var files []string
		if info.IsDir() {
			entries, err := os.ReadDir(filepath.Join(root, path))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".go") &&
					!strings.HasSuffix(entry.Name(), "_test.go") {
					files = append(files, filepath.Join(root, path, entry.Name()))
				}
			}
		} else {
			files = []string{filepath.Join(root, path)}
		}
		for _, file := range files {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, token := range forbidden {
				if strings.Contains(string(data), token) {
					t.Errorf("%s contains broker bypass %q", file, token)
				}
			}
		}
	}
}

func writeFixture(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
