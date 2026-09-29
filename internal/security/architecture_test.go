package security

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const modulePrefix = "github.com/fwtllh-png/QCode/"

// forbiddenLayers are the layers internal/security must never depend on:
// security decides and enforces, and callers above it supply the inputs.
var forbiddenLayers = []string{
	modulePrefix + "internal/adapter/",
	modulePrefix + "internal/runtime/",
	modulePrefix + "internal/persist/",
	modulePrefix + "internal/host",
}

// knownLayerViolations registers existing violations by package directory.
// Entries only ever shrink; an entry that no longer matches an import fails
// the test so it is removed in the same change that fixes it.
var knownLayerViolations = map[string][]string{
	"authority":       {modulePrefix + "internal/adapter/tool"},
	"plandrift":       {modulePrefix + "internal/runtime/protocol"},
	"policy":          {modulePrefix + "internal/adapter/tool"},
	"workspacebroker": {modulePrefix + "internal/persist/workspacejournal"},
}

// vocabularyPackages define shared security vocabulary and depend on nothing
// else in the module, so every layer can import them without a cycle.
var vocabularyPackages = []string{"effect", "netpolicy", "pathpolicy", "resource"}

func TestSecurityImportDirection(t *testing.T) {
	imports := productionImports(t)
	used := map[string]bool{}
	var violations []string
	for pkg, paths := range imports {
		for path := range paths {
			if !forbidden(path) {
				continue
			}
			key := pkg + " -> " + path
			if contains(knownLayerViolations[pkg], path) {
				used[key] = true
				continue
			}
			violations = append(violations, key)
		}
	}
	for pkg, paths := range knownLayerViolations {
		for _, path := range paths {
			if key := pkg + " -> " + path; !used[key] {
				violations = append(violations, "stale allowlist entry "+key)
			}
		}
	}
	sort.Strings(violations)
	if len(violations) != 0 {
		t.Fatalf("internal/security import direction violated:\n%s", strings.Join(violations, "\n"))
	}
}

func TestSecurityVocabularyPackagesAreLeaves(t *testing.T) {
	imports := productionImports(t)
	for _, pkg := range vocabularyPackages {
		paths, ok := imports[pkg]
		if !ok {
			t.Fatalf("vocabulary package %s has no production files", pkg)
		}
		for path := range paths {
			if strings.HasPrefix(path, modulePrefix) {
				t.Fatalf("vocabulary package %s imports %s", pkg, path)
			}
		}
	}
}

// productionImports maps each package directory below internal/security to
// the imports of its non-test files. Test fixtures may reach across layers.
func productionImports(t *testing.T) map[string]map[string]bool {
	t.Helper()
	files := token.NewFileSet()
	out := map[string]map[string]bool{}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if name := entry.Name(); path != "." && (name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(files, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(filepath.Dir(path))
		if out[pkg] == nil {
			out[pkg] = map[string]bool{}
		}
		for _, spec := range parsed.Imports {
			value, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			out[pkg][value] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("no internal/security packages were scanned")
	}
	return out
}

func forbidden(path string) bool {
	for _, prefix := range forbiddenLayers {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
