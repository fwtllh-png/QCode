package common_test

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

const (
	modulePath = "github.com/fwtllh-png/QCode"
	commonPath = modulePath + "/internal/common"
)

func TestCommonImportDirection(t *testing.T) {
	violations, err := commonImportViolations(os.DirFS("."))
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("internal/common must not import application layers:\n%s", strings.Join(violations, "\n"))
	}
}

// Parse source directly so dependencies in files for other platforms and build
// tags are checked too. Tests and fixtures may exercise application consumers.
func commonImportViolations(root fs.FS) ([]string, error) {
	var violations []string
	files := token.NewFileSet()
	scanned := 0
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
		source, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		parsed, err := parser.ParseFile(files, name, source, parser.ImportsOnly)
		if err != nil {
			return err
		}
		scanned++
		for _, spec := range parsed.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if imported != modulePath && !strings.HasPrefix(imported, modulePath+"/") {
				continue
			}
			if imported == commonPath || strings.HasPrefix(imported, commonPath+"/") {
				continue
			}
			violations = append(violations, name+" -> "+imported)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if scanned == 0 {
		return nil, fmt.Errorf("no internal/common production files were scanned")
	}
	slices.Sort(violations)
	return violations, nil
}

func TestCommonImportCheckIncludesTaggedFilesAndSkipsFixtures(t *testing.T) {
	source := func(imported string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte("package example\nimport " + strconv.Quote(imported) + "\n")}
	}
	fixture := fstest.MapFS{
		"standard.go":          source("context"),
		"thirdparty.go":        source("golang.org/x/sync/errgroup"),
		"similar_module.go":    source(modulePath + "-extra/client"),
		"nested/common.go":     source(commonPath + "/fault"),
		"nested/bad.go":        source(modulePath + "/internal/config"),
		"similar_directory.go": source(commonPath + "extra"),
		"module.go":            source(modulePath),
		"consumer_test.go":     source(modulePath + "/internal/runtime/protocol"),
		"testdata/example.go":  source(modulePath + "/internal/host"),
		".hidden/example.go":   source(modulePath + "/internal/host"),
		"tagged_windows.go":    {Data: []byte("//go:build windows && capability\n\npackage example\nimport \"" + modulePath + "/internal/adapter/tool\"\n")},
	}
	got, err := commonImportViolations(fixture)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"module.go -> " + modulePath,
		"nested/bad.go -> " + modulePath + "/internal/config",
		"similar_directory.go -> " + commonPath + "extra",
		"tagged_windows.go -> " + modulePath + "/internal/adapter/tool",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("violations = %v, want %v", got, want)
	}
}
