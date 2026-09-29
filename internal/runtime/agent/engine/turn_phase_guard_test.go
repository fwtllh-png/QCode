package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Each terminal effect of a Turn has exactly one owner step on turnRun, so the
// staged history, the kernel decision and the emitted envelope cannot be
// produced by closures or defers that disagree about the outcome.
func TestTurnTerminalEffectsHaveOneOwnerStep(t *testing.T) {
	owners := map[string]string{
		"finalizeTerminalContext": "turnRun.stageContext",
		"FinalizeTerminal":        "turnRun.finalizeKernel",
		"finishTerminal":          "turnRun.settle",
		"discardSessionDelta":     "turnRun.finalizeStaged",
	}
	for callee := range engineCalls(t) {
		want, guarded := owners[callee.name]
		if guarded && callee.owner != want {
			t.Errorf("%s calls %s; only %s may", callee.owner, callee.name, want)
		}
	}
}

// Steering redirects the next sample and must never reach the tool batch
// cancel slot, which only Cancel may fire.
func TestSteerNeverCancelsTheToolBatch(t *testing.T) {
	for _, decl := range engineFuncDecls(t) {
		name := funcOwner(decl)
		if name != "Scope.Steer" && name != "Engine.EnqueueMailbox" {
			continue
		}
		ast.Inspect(decl, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok &&
				selector.Sel.Name == "toolCancel" {
				t.Errorf("%s touches the tool batch cancel slot", name)
			}
			return true
		})
	}
}

type engineCall struct{ name, owner string }

func engineCalls(t *testing.T) map[engineCall]struct{} {
	t.Helper()
	calls := make(map[engineCall]struct{})
	for _, decl := range engineFuncDecls(t) {
		owner := funcOwner(decl)
		ast.Inspect(decl, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			case *ast.Ident:
				name = fun.Name
			}
			if name != "" {
				calls[engineCall{name: name, owner: owner}] = struct{}{}
			}
			return true
		})
	}
	return calls
}

func engineFuncDecls(t *testing.T) []*ast.FuncDecl {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var decls []*ast.FuncDecl
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				decls = append(decls, fn)
			}
		}
	}
	return decls
}

func funcOwner(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return decl.Name.Name
	}
	receiver := decl.Recv.List[0].Type
	if star, ok := receiver.(*ast.StarExpr); ok {
		receiver = star.X
	}
	if ident, ok := receiver.(*ast.Ident); ok {
		return ident.Name + "." + decl.Name.Name
	}
	return decl.Name.Name
}
