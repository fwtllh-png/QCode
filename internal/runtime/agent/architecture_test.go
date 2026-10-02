package agent_test

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

const agentPath = "github.com/fwtllh-png/QCode/internal/runtime/agent"

func TestAgentPackageImportDirection(t *testing.T) {
	violations, err := agentImportViolations(os.DirFS("."))
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("agent package ownership violations:\n%s", strings.Join(violations, "\n"))
	}
}

// Scan source instead of the current build so tagged and platform-specific
// implementations follow the same boundaries. Tests may import consumers.
func agentImportViolations(root fs.FS) ([]string, error) {
	dependencies := map[string][]string{
		"context":     {},
		"contextview": {"context"},
		"prompt":      {"context"},
		"turnkernel":  {},
		"engine":      {"context", "contextview", "prompt", "turnkernel"},
	}
	var violations []string
	files := token.NewFileSet()
	err := fs.WalkDir(root, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if name != "." && (entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if path.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		owner, _, _ := strings.Cut(name, "/")
		allowed, known := dependencies[owner]
		if !known {
			return fmt.Errorf("agent package %q has no declared ownership", owner)
		}
		source, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		parsed, err := parser.ParseFile(files, name, source, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range parsed.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if suffix, inside := strings.CutPrefix(imported, agentPath+"/"); inside {
				target, _, _ := strings.Cut(suffix, "/")
				if target != owner && !slices.Contains(allowed, target) {
					violations = append(violations, name+" -> "+imported)
				}
			}
			for _, prefix := range []string{
				"github.com/fwtllh-png/QCode/internal/runtime/app",
				"github.com/fwtllh-png/QCode/internal/host",
			} {
				if imported == prefix || strings.HasPrefix(imported, prefix+"/") {
					violations = append(violations, name+" -> "+imported)
				}
			}
		}
		return nil
	})
	slices.Sort(violations)
	return violations, err
}

func TestAgentImportCheckIncludesTaggedFilesAndSkipsFixtures(t *testing.T) {
	source := func(imported string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte("package example\nimport " + strconv.Quote(imported) + "\n")}
	}
	fixture := fstest.MapFS{
		"context/history.go":       source("context"),
		"contextview/view.go":      source(agentPath + "/context"),
		"prompt/turn.go":           source(agentPath + "/context"),
		"engine/turn.go":           source(agentPath + "/turnkernel"),
		"engine/view.go":           source(agentPath + "/contextview"),
		"context/bad.go":           source(agentPath + "/contextview"),
		"prompt/bad.go":            source(agentPath + "/engine"),
		"turnkernel/bad.go":        source(agentPath + "/prompt"),
		"engine/app.go":            source("github.com/fwtllh-png/QCode/internal/runtime/app/wire"),
		"engine/host.go":           source("github.com/fwtllh-png/QCode/internal/host"),
		"context/consumer_test.go": source(agentPath + "/engine"),
		"testdata/example.go":      source(agentPath + "/engine"),
		".hidden/example.go":       source(agentPath + "/engine"),
		"context/tagged_windows.go": {Data: []byte(
			"//go:build windows && capability\n\npackage agentcontext\nimport \"" + agentPath + "/prompt\"\n",
		)},
	}
	got, err := agentImportViolations(fixture)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"context/bad.go -> " + agentPath + "/contextview",
		"context/tagged_windows.go -> " + agentPath + "/prompt",
		"engine/app.go -> github.com/fwtllh-png/QCode/internal/runtime/app/wire",
		"engine/host.go -> github.com/fwtllh-png/QCode/internal/host",
		"prompt/bad.go -> " + agentPath + "/engine",
		"turnkernel/bad.go -> " + agentPath + "/prompt",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("violations = %v, want %v", got, want)
	}
}
