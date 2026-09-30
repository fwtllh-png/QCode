package symbols

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path"
	"strconv"
)

func goReferenceSites(ctx context.Context, data []byte, limit int) ([]ReferenceSite, string, bool, bool) {
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, "source.go", data, 0)
	if err != nil || ctx.Err() != nil {
		return nil, "", false, false
	}
	imports := map[string]string{}
	for _, item := range file.Imports {
		module, err := strconv.Unquote(item.Path.Value)
		if err != nil {
			continue
		}
		alias := path.Base(module)
		if item.Name != nil {
			alias = item.Name.Name
		}
		if alias != "_" && alias != "." {
			imports[alias] = module
		}
	}
	var sites []ReferenceSite
	truncated := false
	add := func(id *ast.Ident, target, module, kind string, scope int) {
		if len(sites) >= limit {
			truncated = true
			return
		}
		p := set.Position(id.Pos())
		sites = append(sites, ReferenceSite{Name: id.Name, Target: target, Module: module, Kind: kind, Scope: scope, StartByte: p.Offset, EndByte: set.Position(id.End()).Offset, Line: p.Line})
	}
	// The parser's object links identify lexical declarations and local uses.
	// Selectors require separate treatment: their member is never a free name.
	skip := map[*ast.Ident]bool{file.Name: true}
	ast.Inspect(file, func(n ast.Node) bool {
		if ctx.Err() != nil {
			return false
		}
		switch n := n.(type) {
		case *ast.SelectorExpr:
			skip[n.Sel] = true
		case *ast.FuncDecl:
			skip[n.Name] = true
		case *ast.LabeledStmt:
			skip[n.Label] = true
		case *ast.BranchStmt:
			if n.Label != nil {
				skip[n.Label] = true
			}
		case *ast.KeyValueExpr:
			// An unbound key may be a struct field. Without type information it
			// cannot safely become an inter-file reference.
			if id, ok := n.Key.(*ast.Ident); ok {
				skip[id] = true
			}
		case *ast.Field:
			for _, id := range n.Names {
				skip[id] = true
			}
		}
		return true
	})
	var visit func(ast.Node, int)
	visit = func(node ast.Node, scope int) {
		ast.Inspect(node, func(n ast.Node) bool {
			if n == nil || ctx.Err() != nil {
				return false
			}
			if n != node {
				switch n.(type) {
				case *ast.BlockStmt, *ast.FuncDecl, *ast.FuncLit:
					visit(n, set.Position(n.Pos()).Offset)
					return false
				}
			}
			switch n := n.(type) {
			case *ast.ImportSpec:
				return false
			case *ast.SelectorExpr:
				if receiver, ok := n.X.(*ast.Ident); ok && receiver.Obj == nil {
					if module, ok := imports[receiver.Name]; ok {
						add(n.Sel, n.Sel.Name, module, ReferenceImport, scope)
					}
				}
			case *ast.Ident:
				if skip[n] || n.Obj != nil || n.Name == "_" || imports[n.Name] != "" || types.Universe.Lookup(n.Name) != nil {
					return true
				}
				add(n, n.Name, "", ReferencePackage, scope)
			}
			return true
		})
	}
	visit(file, 0)
	return sites, file.Name.Name, true, truncated
}
