package tool_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestProductionToolExecutionHasNoGuardBypass(t *testing.T) {
	root := filepath.Clean("../../..")
	allowedPrepared := filepath.FromSlash(
		"internal/adapter/tool/guard/pipeline_attempt.go",
	)
	err := filepath.WalkDir(
		filepath.Join(root, "internal"),
		func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			fileSet := token.NewFileSet()
			file, err := parser.ParseFile(fileSet, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch selector.Sel.Name {
				case "ExecutePreparedOutcome":
					if relative != allowedPrepared &&
						relative != filepath.FromSlash(
							"internal/adapter/tool/tool.go",
						) {
						t.Errorf(
							"%s calls the Guard-private execution boundary",
							relative,
						)
					}
				case "Execute":
					if registryReceiver(selector.X) &&
						relative != filepath.FromSlash(
							"internal/adapter/tool/tool.go",
						) {
						t.Errorf(
							"%s executes Registry directly instead of Guard",
							relative,
						)
					}
				}
				return true
			})
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestProductionCodeCannotImportToolTestHelpers(t *testing.T) {
	root := filepath.Clean("../../..")
	err := filepath.WalkDir(
		filepath.Join(root, "internal"),
		func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fileSet := token.NewFileSet()
			file, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imported := range file.Imports {
				name, err := strconv.Unquote(imported.Path.Value)
				if err != nil {
					return err
				}
				if name == "github.com/fwtllh-png/QCode/testutil" ||
					strings.HasPrefix(name, "github.com/fwtllh-png/QCode/testutil/") {
					relative, _ := filepath.Rel(root, path)
					t.Errorf("%s imports test-only helper %q", relative, name)
				}
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func registryReceiver(expression ast.Expr) bool {
	switch value := expression.(type) {
	case *ast.Ident:
		return strings.Contains(
			strings.ToLower(value.Name),
			"registry",
		)
	case *ast.SelectorExpr:
		return strings.Contains(
			strings.ToLower(value.Sel.Name),
			"registry",
		)
	default:
		return false
	}
}
