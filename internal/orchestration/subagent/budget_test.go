package subagent

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestRecoveredOrphanWorktreeReturnsItsConcurrencySlot(t *testing.T) {
	root := t.TempDir()
	provider := &allocationFaultWorktrees{
		root: root, discardErr: errors.New("injected cleanup failure"),
	}
	budget := Budget{MaxParallel: 1, MaxResident: 1, MaxTotal: 4}
	manager, err := Open(Options{
		Root: root, Workspace: "/workspace", SessionID: "session-owner",
		Gate: allocationPassGate{}, Worktrees: provider, Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AttachGraph(allocationGraph(
		"session-owner",
		func(GraphEdge) error { return errors.New("injected spawn commit failure") },
		nil,
	)); err != nil {
		t.Fatal(err)
	}
	spec, err := manager.roles.Resolve(RoleGeneral)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.spawn(DelegationIntent{
		SessionID: "session-owner", TaskName: "inspect",
		Role: RoleGeneral, Objective: "inspect",
		ExpectedOutput: "evidence", Trigger: TriggerUser,
	}, spec); err == nil {
		t.Fatal("spawn commit failure was not reported")
	}

	restarted, err := Open(Options{
		Root: root, Workspace: "/workspace", SessionID: "session-owner",
		Gate: allocationPassGate{}, Worktrees: NewScratchWorktrees(root),
		Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.AttachGraph(allocationGraph(
		"session-owner", func(GraphEdge) error { return nil }, nil,
	)); err != nil {
		t.Fatal(err)
	}
	recovered, ok := restarted.Agent(provider.observed.ChildID)
	if !ok || recovered.Status != StatusFailed {
		t.Fatalf("recovered Agent = %+v, ok=%v", recovered, ok)
	}
	if ledger := restarted.SessionBudget("session-owner"); ledger.ReservedSlots != 0 {
		t.Fatalf("recovered orphan still holds a slot: %+v", ledger)
	}
	next, err := restarted.Spawn("", RoleExplore, "inspect")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Takeover(t.Context(), next.ID, "inspect"); err != nil {
		t.Fatalf("the only slot leaked to a recovered orphan: %v", err)
	}
}

func TestDelegationClosedBeforeItsFirstTurnDoesNotConsumeSpawnBudget(t *testing.T) {
	manager, err := Open(Options{
		Root: t.TempDir(), Gate: allocationPassGate{},
		Budget: Budget{MaxParallel: 1, MaxResident: 1, MaxTotal: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := range 3 {
		rejected, err := manager.Spawn("", RoleExplore, "inspect")
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if err := manager.Close(rejected.ID); err != nil {
			t.Fatal(err)
		}
	}
	ran, err := manager.Spawn("", RoleExplore, "inspect")
	if err != nil {
		t.Fatalf("rejected delegations consumed the spawn budget: %v", err)
	}
	if _, err := manager.Takeover(t.Context(), ran.ID, "inspect"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Complete(ran.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(ran.ID); err != nil {
		t.Fatal(err)
	}
	_, err = manager.Spawn("", RoleExplore, "inspect")
	var problem *protocol.Problem
	if !errors.As(err, &problem) || problem.Code != protocol.CodeResourceExhausted {
		t.Fatalf("an Agent that ran must still count toward max_total: %v", err)
	}
}

func TestDepthAndConcurrencyBudgets(t *testing.T) {
	manager, err := Open(Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Budget: Budget{MaxDepth: 1, MaxParallel: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := manager.Spawn("", RolePlan, "root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Takeover(t.Context(), parent.ID, "run"); err != nil {
		t.Fatal(err)
	}
	blocked, err := manager.Spawn("", RoleGeneral, "blocked")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Takeover(t.Context(), blocked.ID, "run"); err == nil {
		t.Fatal("expected running concurrency rejection")
	}
	if err := manager.Close(parent.ID); err != nil {
		t.Fatal(err)
	}
	child, err := manager.Spawn("", RoleGeneral, "child")
	if err != nil {
		t.Fatal(err)
	}
	deep, err := Open(Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Budget: Budget{MaxDepth: 1, MaxParallel: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	root, err := deep.Spawn("", RolePlan, "root")
	if err != nil {
		t.Fatal(err)
	}
	mid, err := deep.Spawn(root.ID, RoleGeneral, "mid")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deep.Spawn(mid.ID, RoleGeneral, "too-deep"); err == nil {
		t.Fatal("expected depth rejection")
	}
	_ = child
}

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
