package subagent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestManagerIsTheOnlyChildBudgetOwner keeps child admission in one place:
// the Session ledger is folded from Agent facts, no parallel budget package
// exists, and worktree I/O never runs from a function that holds m.mu.
func TestManagerIsTheOnlyChildBudgetOwner(t *testing.T) {
	for _, removed := range []string{"../budget", "../admission"} {
		if _, err := os.Stat(removed); !os.IsNotExist(err) {
			t.Errorf("parallel child budget package %s exists", filepath.Base(removed))
		}
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			checkBudgetOwnership(t, fset, function)
		}
	}
}

func checkBudgetOwnership(t *testing.T, fset *token.FileSet, function *ast.FuncDecl) {
	t.Helper()
	name := function.Name.Name
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.IncDecStmt:
			selector, ok := value.X.(*ast.SelectorExpr)
			if ok && name != "sessionBudgetLocked" &&
				(selector.Sel.Name == "ReservedSlots" || selector.Sel.Name == "TotalSpawned") {
				t.Errorf("%s: %s counts %s by hand instead of folding Agent state",
					fset.Position(value.Pos()), name, selector.Sel.Name)
			}
		case *ast.SelectorExpr:
			if receiver, ok := value.X.(*ast.Ident); ok && receiver.Name == "m" &&
				(value.Sel.Name == "ledgers" || value.Sel.Name == "active") {
				t.Errorf("%s: Manager keeps a running budget counter m.%s",
					fset.Position(value.Pos()), value.Sel.Name)
			}
		case *ast.CallExpr:
			selector, ok := value.Fun.(*ast.SelectorExpr)
			if !ok || !strings.HasSuffix(name, "Locked") {
				return true
			}
			if owner, ok := selector.X.(*ast.SelectorExpr); ok && owner.Sel.Name == "trees" {
				t.Errorf("%s: %s runs worktree %s under the Manager lock",
					fset.Position(value.Pos()), name, selector.Sel.Name)
			}
		}
		return true
	})
}
