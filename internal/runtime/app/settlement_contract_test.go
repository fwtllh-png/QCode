package app

import (
	"go/ast"
	"testing"
)

// TestAcceptedOperationsSettleThroughOneOwner keeps "every accepted operation
// ends committed or rejected" structural: only the settlement path writes a
// commit receipt, and only one function builds the rejection event, so no
// failure branch can log and return with an operation still accepted.
func TestAcceptedOperationsSettleThroughOneOwner(t *testing.T) {
	commits, rejections := 0, 0
	for _, parsed := range parseRuntimePackage(t) {
		for _, declaration := range parsed.file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.CallExpr:
					if !isLifecycleCommit(value.Fun) {
						return true
					}
					commits++
					if function.Name.Name != "advance" {
						t.Errorf(
							"%s: %s writes a commit receipt outside OperationService.advance",
							parsed.fileset.Position(value.Pos()), function.Name.Name,
						)
					}
				case *ast.CompositeLit:
					if !isProtocolType(value.Type, "OperationRejectedData") {
						return true
					}
					rejections++
					if function.Name.Name != "operationRejection" {
						t.Errorf(
							"%s: %s builds OperationRejected outside operationRejection",
							parsed.fileset.Position(value.Pos()), function.Name.Name,
						)
					}
				}
				return true
			})
		}
	}
	if commits == 0 || rejections == 0 {
		t.Fatalf("settlement scan found commits=%d rejections=%d", commits, rejections)
	}
}

func isLifecycleCommit(expr ast.Expr) bool {
	call, ok := expr.(*ast.SelectorExpr)
	if !ok || call.Sel.Name != "Commit" {
		return false
	}
	owner, ok := call.X.(*ast.SelectorExpr)
	return ok && owner.Sel.Name == "lifecycle"
}

func isProtocolType(expr ast.Expr, name string) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "protocol"
}
