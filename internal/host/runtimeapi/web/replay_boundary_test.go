package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestWebHostDoesNotScanEventHistory keeps event-history interpretation in
// the Runtime. The host streams events to clients (EventsLimited) and asks the
// Runtime evidence queries; paging ReplayEvents here re-implements Runtime
// projections in the transport.
func TestWebHostDoesNotScanEventHistory(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "ReplayEvents" {
				t.Errorf("%s pages Runtime event history; add a Runtime query instead",
					files.Position(call.Pos()))
			}
			return true
		})
	}
}
