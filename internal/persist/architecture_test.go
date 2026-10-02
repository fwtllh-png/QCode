package persist_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPersistenceDoesNotImportApplicationOrEngine(t *testing.T) {
	const runtime = "github.com/fwtllh-png/QCode/internal/runtime/"
	files := token.NewFileSet()
	err := filepath.WalkDir(".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(files, name, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range parsed.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			// Protocol, Context and TurnKernel contracts are valid storage inputs;
			// application services and the executing Engine are not.
			for _, owner := range []string{"app", "agent/engine"} {
				prefix := runtime + owner
				if imported == prefix || strings.HasPrefix(imported, prefix+"/") {
					t.Errorf("%s must not import %s", name, imported)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
