package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// lockRanks is the documented Engine/Scope lock hierarchy (see the Engine
// struct comment). A nested acquisition is legal only when the outer lock
// ranks strictly above the inner one; the leaf locks rank equally and must
// never nest with anything, including each other.
var lockRanks = map[string]int{
	"engine.mu":           0,
	"scope.mu":            1,
	"engine.scopeMu":      2,
	"engine.planMu":       3,
	"engine.checkpointMu": 4,
	"engine.prefixMu":     4,
}

var engineLockNames = map[string]bool{
	"mu": true, "scopeMu": true, "planMu": true,
	"checkpointMu": true, "prefixMu": true,
}

// TestLockContractNestingFollowsDocumentedOrder scans every non-test source
// file of this package and fails when one function acquires a lock while
// already holding another lock of equal or higher rank, or re-enters a lock
// it already holds. An acquisition whose Unlock is deferred stays held until
// the end of its function. Cross-function nesting through helpers is not
// tracked here; the documented evidence chains in the Engine comment name
// the sites that establish the order, and the race and stress lanes cover
// them.
func TestLockContractNestingFollowsDocumentedOrder(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fileset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fileset, path, nil, 0)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			events := lockEvents(function)
			held := make(map[string]bool)
			for _, event := range events {
				if event.unlock {
					delete(held, event.lock)
					continue
				}
				for outer := range held {
					if lockRanks[outer] >= lockRanks[event.lock] {
						t.Errorf(
							"%s: %s acquires %s while holding %s (ranks %d >= %d)",
							fileset.Position(event.pos), function.Name.Name,
							event.lock, outer,
							lockRanks[outer], lockRanks[event.lock],
						)
					}
				}
				if held[event.lock] {
					t.Errorf(
						"%s: %s re-enters %s",
						fileset.Position(event.pos), function.Name.Name, event.lock,
					)
				}
				held[event.lock] = true
			}
		}
	}
}

type lockEvent struct {
	pos     token.Pos
	lock    string
	unlock  bool
	isDefer bool
}

// lockEvents returns the lock and unlock calls of one function in source
// order. Deferred unlocks never release within the scan, so their lock stays
// held until the end of the function; deferred lock calls are ignored.
func lockEvents(function *ast.FuncDecl) []lockEvent {
	var events []lockEvent
	ast.Inspect(function.Body, func(node ast.Node) bool {
		var call *ast.CallExpr
		isDefer := false
		switch statement := node.(type) {
		case *ast.DeferStmt:
			call, isDefer = statement.Call, true
		case *ast.ExprStmt:
			if candidate, ok := statement.X.(*ast.CallExpr); ok {
				call = candidate
			}
		}
		if call == nil {
			return true
		}
		lock, kind, ok := lockCallName(call)
		if !ok {
			return true
		}
		events = append(events, lockEvent{
			pos: call.Lparen, lock: lock,
			unlock: kind == "Unlock", isDefer: isDefer,
		})
		return true
	})
	filtered := events[:0]
	for _, event := range events {
		if event.isDefer {
			continue
		}
		filtered = append(filtered, event)
	}
	return filtered
}

// lockCallName classifies receiver.<lock>.Lock()/Unlock() calls on Engine
// and Scope receivers.
func lockCallName(call *ast.CallExpr) (string, string, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (selector.Sel.Name != "Lock" && selector.Sel.Name != "Unlock") {
		return "", "", false
	}
	base, ok := selector.X.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	receiver, ok := base.X.(*ast.Ident)
	if !ok {
		return "", "", false
	}
	switch {
	case receiver.Name == "e" || receiver.Name == "engine":
		if engineLockNames[base.Sel.Name] {
			return "engine." + base.Sel.Name, selector.Sel.Name, true
		}
	case receiver.Name == "s" || receiver.Name == "scope":
		if base.Sel.Name == "mu" {
			return "scope.mu", selector.Sel.Name, true
		}
	}
	return "", "", false
}
