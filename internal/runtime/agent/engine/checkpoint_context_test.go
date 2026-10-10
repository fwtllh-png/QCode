package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func TestContextSnapshotUsesRunningTurnOwners(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(workspace, "changed.go"),
		[]byte("package changed\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Workspace = workspace
	engine.options.WorkspaceIdentity = "workspace:test"
	engine.turn = 3
	scope := &Scope{engine: engine, state: newScopeState(engine)}
	scope.state.context.WorkingSet().Observe(agentcontext.SourceEdited, 3, "changed.go")
	scope.state.context.Evidence().BeginTurn(3)
	scope.state.context.Evidence().MarkChanged("changed.go", 3, false)
	engine.publishScope(scope)

	snapshot, err := engine.ExportContextSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.WorkingSet.Observations) != 1 ||
		snapshot.WorkingSet.Observations[0].Path != "changed.go" ||
		len(snapshot.Evidence.Changes) != 1 ||
		snapshot.Evidence.Changes[0].Path != "changed.go" ||
		len(snapshot.Workspace.BoundPaths) != 1 ||
		snapshot.Workspace.BoundPaths[0].Path != "changed.go" {
		t.Fatalf("snapshot omitted running Turn owners: %+v", snapshot)
	}
}

func TestContextCheckpointRestoresOwnersAndInvalidatesChangedWorkspace(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "main.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Workspace = workspace
	engine.options.WorkspaceIdentity = "workspace:test"
	engine.sessionRevision = 4
	engine.stateEpoch = 2
	engine.turn = 4
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, "implement parser", 4),
		messageWithText(provider.RoleAssistant, "working", 4),
	}
	engine.context.Evidence().NoteRead("main.go", "sha256:old")
	engine.context.Evidence().MarkChanged("main.go", 4, true)
	if err := engine.ApplyPlan(interact.Plan{
		Objective: "implement parser",
		Steps: []interact.PlanStep{{
			Title: "verify parser", Status: interact.StepPending,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := engine.ExportContextSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	engine.usage = provider.Usage{InputTokens: 100}
	engine.costUSD = 2.5
	if err := engine.ApplyPlan(interact.Plan{
		Objective: "newer unrelated work",
		Steps: []interact.PlanStep{{
			Title: "do not retain", Status: interact.StepPending,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	receipt, err := engine.RestoreContextSnapshot(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	usage, cost := engine.Usage()
	if receipt.BindingMatch != true || receipt.Invalidated != 1 ||
		receipt.Stale != 2 || usage.InputTokens != 100 || cost != 2.5 ||
		!strings.Contains(engine.planText, "implement parser") ||
		strings.Contains(engine.planText, "newer unrelated") ||
		engine.stateEpoch <= checkpoint.Epoch ||
		engine.sessionRevision <= checkpoint.Revision {
		t.Fatalf(
			"receipt=%+v usage=%+v cost=%f epoch=%d revision=%d plan=%q",
			receipt,
			usage,
			cost,
			engine.stateEpoch,
			engine.sessionRevision,
			engine.planText,
		)
	}
	changes := engine.context.Evidence().Changes()
	if len(changes) != 1 || !changes[0].Stale {
		t.Fatalf("workspace mismatch did not invalidate the change claim: %+v", changes)
	}
	capsule := engine.buildTruthCapsule(engine.buildCompactSummary(nil), nil)
	for _, entity := range capsule.Entities {
		if entity.Kind == agentcontext.EntityChange &&
			entity.Key == "main.go" {
			if entity.WorkspaceClaimStatus != agentcontext.WorkspaceClaimStale ||
				entity.Verified {
				t.Fatalf("restored change truth=%+v", entity)
			}
			return
		}
	}
	t.Fatal("restored change was not projected into truth")
}

func TestCheckpointForkStartsFromCheckpointOwners(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Workspace = t.TempDir()
	engine.options.WorkspaceIdentity = "workspace:test"
	engine.sessionRevision = 3
	engine.turn = 3
	if err := engine.ApplyPlan(interact.Plan{
		Objective: "checkpoint objective",
		Steps: []interact.PlanStep{{
			Title: "checkpoint step", Status: interact.StepPending,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := engine.ExportContextSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplyPlan(interact.Plan{
		Objective: "parent future",
		Steps: []interact.PlanStep{{
			Title: "future step", Status: interact.StepPending,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	forked, receipt, err := engine.ForkFromContextSnapshot(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.BindingMatch ||
		!strings.Contains(forked.planText, "checkpoint objective") ||
		strings.Contains(forked.planText, "parent future") ||
		forked.stateEpoch != 1 || forked.sessionRevision != 1 {
		t.Fatalf(
			"receipt=%+v epoch=%d revision=%d plan=%q",
			receipt,
			forked.stateEpoch,
			forked.sessionRevision,
			forked.planText,
		)
	}
}
