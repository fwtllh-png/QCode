package wire

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestWireOnlyConstructsChildRunner keeps the stateful child lifecycle —
// event observation, residency and settlement — in orchestration/childrun.
// Wire constructs the Runner and binds it; it does not drive children.
func TestWireOnlyConstructsChildRunner(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := map[string]bool{
		"ObserveEvents": true, "Settle": true, "AwaitApproval": true, "ResumeApproval": true,
		"ActivateResident": true, "DeactivateResident": true, "TouchResident": true,
	}
	fileSet := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok && lifecycle[selector.Sel.Name] {
				t.Errorf("%s: wire drives child lifecycle via %s; keep it in orchestration/childrun",
					fileSet.Position(call.Pos()), selector.Sel.Name)
			}
			return true
		})
	}
}
