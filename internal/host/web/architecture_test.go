package web

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestOnlyHostsImportHostPackages keeps dependencies pointing inward: hosts
// compose runtime, persistence, and adapters, never the reverse. Tests are
// included because a lower layer's test importing a host hides a missing seam.
func TestOnlyHostsImportHostPackages(t *testing.T) {
	const hostPrefix = "github.com/fwtllh-png/QCode/internal/host"
	internalRoot := filepath.Clean("../..")
	hostRoot := filepath.Join(internalRoot, "host")
	files := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(internalRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path == hostRoot {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(files, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		for _, importSpec := range file.Imports {
			imported, err := strconv.Unquote(importSpec.Path.Value)
			if err != nil {
				return err
			}
			if imported == hostPrefix || strings.HasPrefix(imported, hostPrefix+"/") {
				t.Errorf("%s imports host package %s", filepath.ToSlash(path), imported)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("layering scan found no Go files outside internal/host")
	}
}

func TestWebHostDoesNotDependOnExecutionImplementations(t *testing.T) {
	forbidden := []string{
		"github.com/fwtllh-png/QCode/internal/runtime/agent",
		"github.com/fwtllh-png/QCode/internal/adapter/provider",
		"github.com/fwtllh-png/QCode/internal/security/sandbox",
		"github.com/fwtllh-png/QCode/internal/adapter/tool",
		"github.com/fwtllh-png/QCode/internal/adapter/skill",
		"github.com/fwtllh-png/QCode/internal/runtime/app/extension",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(files, entry.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, importSpec := range file.Imports {
			path, err := strconv.Unquote(importSpec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			for _, prefix := range forbidden {
				if path == prefix || strings.HasPrefix(path, prefix+"/") {
					t.Errorf("%s imports forbidden execution package %s", entry.Name(), path)
				}
			}
		}
	}
}
