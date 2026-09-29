package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// runtimeLockRanks is the documented Runtime lock hierarchy (see
// docs/zh-CN/architecture.md). A nested acquisition is legal only when the
// outer lock ranks strictly above the inner one, so equal ranks never nest.
// The eventhub locks sit between publish and state: EventService.publishMu >
// Hub.publishMu > EventService.mu > Hub.mu; they live in another package and
// are covered by its own tests.
var runtimeLockRanks = map[string]int{
	"session.mutationMu": 0,
	"operation.mu":       1,
	"event.publishMu":    2,
	"event.mu":           3,
	"queue.mu":           3,
	"event.observerMu":   4,
	"session.titleMu":    4,
}

var runtimeServiceLocks = map[string]map[string]string{
	"SessionService": {
		"mutationMu": "session.mutationMu", "titleMu": "session.titleMu",
	},
	"OperationService": {"mu": "operation.mu"},
	"EventService": {
		"publishMu": "event.publishMu", "mu": "event.mu",
		"observerMu": "event.observerMu",
	},
	"TurnQueueService": {"mu": "queue.mu"},
}

// runtimeLockMethods are owner methods that acquire a lock for their caller
// and return its release func.
var runtimeLockMethods = map[string]string{
	"lockMutations": "session.mutationMu",
}

// stateLockForbidden names Runtime collaborators that perform I/O or run
// foreign callbacks, Runtime entry points that publish or submit, and other
// lock owners. None may be reached while EventService.mu is held.
var stateLockForbidden = map[string]bool{
	"lifecycle": true, "hub": true, "events": true, "content": true,
	"terminalStore": true, "sessionLifecycle": true, "profiles": true,
	"engine": true, "ArtifactService": true, "contextRebaseStore": true,
	"terminal": true, "publish": true, "publishStable": true,
	"publishWithIdentity": true, "publishProjected": true,
	"PublishExternal": true, "PublishTerminalProjection": true,
	"Submit": true, "SubmitWithKey": true, "reject": true,
	"rejectAndCommit": true, "commit": true, "Drain": true,
	"dispatchObservers": true, "TurnQueueService": true,
	"OperationService": true, "SessionService": true,
}

// TestRuntimeLockContractNestingFollowsDocumentedOrder fails when one function
// body acquires a Runtime lock while holding another of equal or higher rank,
// or reaches I/O, a callback, or a publish path while holding EventService.mu.
// A deferred Unlock keeps its lock held to the end of the body. Function
// literals are scanned as their own bodies because they run later.
// Cross-function reachability is not tracked; the publish lock tests cover
// the dynamic side.
func TestRuntimeLockContractNestingFollowsDocumentedOrder(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fileset := token.NewFileSet()
		file, err := parser.ParseFile(fileset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			scope := lockScope{
				receiver: receiverName(function), service: receiverService(function),
				aliases: map[string]string{},
			}
			bodies := []*ast.BlockStmt{function.Body}
			for len(bodies) > 0 {
				body := bodies[0]
				bodies = bodies[1:]
				events, nested := scope.events(body)
				bodies = append(bodies, nested...)
				checked += len(events)
				assertLockOrder(t, fileset, function.Name.Name, events)
			}
		}
	}
	if checked == 0 {
		t.Fatal("lock contract scan found no Runtime lock calls")
	}
}

func assertLockOrder(
	t *testing.T,
	fileset *token.FileSet,
	function string,
	events []lockEvent,
) {
	t.Helper()
	held := map[string]bool{}
	for _, event := range events {
		if event.call != "" {
			if held["event.mu"] {
				t.Errorf(
					"%s: %s reaches %s while holding event.mu",
					fileset.Position(event.pos), function, event.call,
				)
			}
			continue
		}
		if event.unlock {
			delete(held, event.lock)
			continue
		}
		for outer := range held {
			if runtimeLockRanks[outer] >= runtimeLockRanks[event.lock] {
				t.Errorf(
					"%s: %s acquires %s while holding %s (ranks %d >= %d)",
					fileset.Position(event.pos), function, event.lock, outer,
					runtimeLockRanks[outer], runtimeLockRanks[event.lock],
				)
			}
		}
		held[event.lock] = true
	}
}

type lockEvent struct {
	pos    token.Pos
	lock   string
	unlock bool
	// call names a forbidden collaborator reached by a non-lock call.
	call string
}

type lockScope struct {
	receiver string
	service  string
	aliases  map[string]string
}

// events returns one body's lock calls in source order, skipping deferred
// ones, plus the function literal bodies found inside it.
func (s lockScope) events(body *ast.BlockStmt) ([]lockEvent, []*ast.BlockStmt) {
	var events []lockEvent
	var nested []*ast.BlockStmt
	ast.Inspect(body, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.FuncLit:
			nested = append(nested, value.Body)
			return false
		case *ast.DeferStmt:
			if literal, ok := value.Call.Fun.(*ast.FuncLit); ok {
				nested = append(nested, literal.Body)
			}
			return false
		case *ast.AssignStmt:
			for index, left := range value.Lhs {
				ident, ok := left.(*ast.Ident)
				if !ok || index >= len(value.Rhs) {
					continue
				}
				if service := selectorService(value.Rhs[index]); service != "" {
					s.aliases[ident.Name] = service
				}
			}
		case *ast.CallExpr:
			if lock, unlock, ok := s.classify(value); ok {
				events = append(events, lockEvent{
					pos: value.Lparen, lock: lock, unlock: unlock,
				})
			} else if name := forbiddenCall(value); name != "" {
				events = append(events, lockEvent{pos: value.Lparen, call: name})
			}
		}
		return true
	})
	return events, nested
}

// forbiddenCall reports the first stateLockForbidden name in a call's
// selector chain, such as r.lifecycle.Project or r.hub.Publish.
func forbiddenCall(call *ast.CallExpr) string {
	for expr := call.Fun; ; {
		selector, ok := expr.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		if stateLockForbidden[selector.Sel.Name] {
			return selector.Sel.Name
		}
		expr = selector.X
	}
}

// classify reports the Runtime lock a call acquires or releases. A lock
// acquired through a runtimeLockMethods helper stays held to the end of the
// body because its release func is not tracked.
func (s lockScope) classify(call *ast.CallExpr) (string, bool, bool) {
	method, ok := call.Fun.(*ast.SelectorExpr)
	if ok {
		if lock, helper := runtimeLockMethods[method.Sel.Name]; helper {
			return lock, false, true
		}
	}
	if !ok || (method.Sel.Name != "Lock" && method.Sel.Name != "Unlock") {
		return "", false, false
	}
	field, ok := method.X.(*ast.SelectorExpr)
	if !ok {
		return "", false, false
	}
	service := selectorService(field.X)
	if ident, ok := field.X.(*ast.Ident); ok {
		switch {
		case s.aliases[ident.Name] != "":
			service = s.aliases[ident.Name]
		case ident.Name == s.receiver:
			service = s.service
			if service == "Runtime" && field.Sel.Name == "mutationMu" {
				service = "SessionService"
			}
		}
	}
	lock, ok := runtimeServiceLocks[service][field.Sel.Name]
	return lock, method.Sel.Name == "Unlock", ok
}

// selectorService names the Runtime service an expression such as
// r.EventService or s.runtime.OperationService refers to.
func selectorService(expr ast.Expr) string {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if _, known := runtimeServiceLocks[selector.Sel.Name]; known {
		return selector.Sel.Name
	}
	return ""
}

func receiverName(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) == 0 ||
		len(function.Recv.List[0].Names) == 0 {
		return ""
	}
	return function.Recv.List[0].Names[0].Name
}

func receiverService(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) == 0 {
		return ""
	}
	expr := function.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}
