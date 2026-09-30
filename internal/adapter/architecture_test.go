package adapter_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAdapterImportBoundaries(t *testing.T) {
	const internal = "github.com/fwtllh-png/QCode/internal/"
	for _, rule := range []struct{ owner, forbidden string }{
		{"provider", "adapter/tool"},
		{"tool", "runtime/agent"},
		{"mcp", "runtime/app/wire"},
		{"provider/modelcatalog", "adapter/provider/router"},
	} {
		t.Run(rule.owner, func(t *testing.T) {
			files := token.NewFileSet()
			err := filepath.WalkDir(rule.owner, func(name string, entry fs.DirEntry, walkErr error) error {
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
				// Parse all build tags and platforms; integration tests may import consumers.
				parsed, err := parser.ParseFile(files, name, nil, parser.ImportsOnly)
				if err != nil {
					return err
				}
				for _, spec := range parsed.Imports {
					imported, err := strconv.Unquote(spec.Path.Value)
					if err != nil {
						return err
					}
					prefix := internal + rule.forbidden
					if imported == prefix || strings.HasPrefix(imported, prefix+"/") {
						t.Errorf("%s must not import %s", name, imported)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
