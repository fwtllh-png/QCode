package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func TestWorktreeCleanupDoesNotTouchSibling(t *testing.T) {
	gate := &fakeGate{}
	manager, err := Open(Options{
		Root: t.TempDir(), Gate: gate, Budget: Budget{MaxDepth: 3, MaxParallel: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := manager.Spawn("", RoleGeneral, "one")
	if err != nil {
		t.Fatal(err)
	}
	b, err := manager.Spawn("", RoleReview, "two")
	if err != nil {
		t.Fatal(err)
	}
	siblingMarker := filepath.Join(b.Worktree, "keep.txt")
	if err := os.WriteFile(siblingMarker, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(siblingMarker); err != nil {
		t.Fatalf("sibling polluted: %v", err)
	}
	if _, err := manager.ExecuteTool(context.Background(), b.ID, "c1", "read", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if gate.calls != 1 {
		t.Fatalf("gate calls = %d", gate.calls)
	}
	out, err := manager.Takeover(context.Background(), b.ID, "finish")
	if err != nil || out == "" {
		t.Fatalf("takeover=%q err=%v", out, err)
	}
}

func TestWorktreeAllocationSurvivesSpawnCommitFailure(t *testing.T) {
	root := t.TempDir()
	provider := &allocationFaultWorktrees{
		root: root, discardErr: errors.New("injected cleanup failure"),
	}
	manager, err := Open(Options{
		Root: root, Workspace: "/workspace", SessionID: "process-old",
		Gate: allocationPassGate{}, Worktrees: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AttachGraph(allocationGraph(
		"process-old",
		func(GraphEdge) error { return errors.New("injected spawn commit failure") },
		nil,
	)); err != nil {
		t.Fatal(err)
	}
	spec, err := manager.roles.Resolve(RoleGeneral)
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.spawn(DelegationIntent{
		SessionID: "session-owner", TaskName: "inspect",
		Role: RoleGeneral, Objective: "inspect",
		ExpectedOutput: "evidence", Trigger: TriggerUser,
	}, spec)
	if err == nil ||
		!strings.Contains(err.Error(), "spawn commit failure") ||
		!strings.Contains(err.Error(), "cleanup failure") {
		t.Fatalf("spawn error = %v", err)
	}
	if provider.observed.SessionID != "session-owner" {
		t.Fatalf("provision allocation = %+v", provider.observed)
	}

	var spawned GraphEdge
	var terminal GraphTransition
	restarted, err := Open(Options{
		Root: root, Workspace: "/workspace", SessionID: "process-new",
		Gate: allocationPassGate{}, Worktrees: NewScratchWorktrees(root),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.AttachGraph(allocationGraph(
		"process-new",
		func(edge GraphEdge) error {
			spawned = edge
			return nil
		},
		func(transition GraphTransition) error {
			terminal = transition
			return nil
		},
	)); err != nil {
		t.Fatal(err)
	}
	recovered, ok := restarted.Agent(provider.observed.ChildID)
	if !ok || recovered.Status != StatusFailed ||
		recovered.SessionID != "session-owner" {
		t.Fatalf("recovered Agent = %+v, ok=%v", recovered, ok)
	}
	if spawned.SessionID != "session-owner" ||
		terminal.SessionID != "session-owner" ||
		terminal.CompletionMessage == nil ||
		terminal.CompletionMessage.SessionID != "session-owner" {
		t.Fatalf("spawn=%+v terminal=%+v", spawned, terminal)
	}
	if _, err := os.Stat(filepath.Join(
		root, worktreeAllocations, recovered.ID+".json",
	)); !os.IsNotExist(err) {
		t.Fatalf("settled allocation still exists: %v", err)
	}
}

func TestWorktreeAllocationClearsCleanProvisionFailure(t *testing.T) {
	root := t.TempDir()
	provider := &allocationFaultWorktrees{
		root:       root,
		failBefore: errors.New("injected clean provision failure"),
	}
	manager, err := Open(Options{
		Root: root, Workspace: "/workspace", SessionID: "process",
		Gate: allocationPassGate{}, Worktrees: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := manager.roles.Resolve(RoleGeneral)
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.spawn(DelegationIntent{
		SessionID: "session-owner", TaskName: "inspect",
		Role: RoleGeneral, Objective: "inspect",
		ExpectedOutput: "evidence", Trigger: TriggerUser,
	}, spec)
	if !errors.Is(err, provider.failBefore) {
		t.Fatalf("spawn error = %v", err)
	}
	if provider.observed.SessionID != "session-owner" {
		t.Fatalf("provision allocation = %+v", provider.observed)
	}
	if _, err := os.Stat(filepath.Join(
		root, worktreeAllocations, provider.observed.ChildID+".json",
	)); !os.IsNotExist(err) {
		t.Fatalf("clean provision failure retained allocation: %v", err)
	}
}

type allocationFaultWorktrees struct {
	root       string
	failBefore error
	discardErr error
	observed   GraphEdge
}

func (p *allocationFaultWorktrees) Provision(
	agentID string,
	_ Stance,
) (Worktree, error) {
	raw, err := os.ReadFile(filepath.Join(
		p.root, worktreeAllocations, agentID+".json",
	))
	if err != nil {
		return Worktree{}, err
	}
	var allocation worktreeAllocation
	if err := json.Unmarshal(raw, &allocation); err != nil {
		return Worktree{}, err
	}
	p.observed = allocation.Edge
	if p.failBefore != nil {
		return Worktree{}, p.failBefore
	}
	path := filepath.Join(p.root, "worktrees", agentID)
	if err := os.MkdirAll(path, 0o700); err != nil {
		return Worktree{}, err
	}
	if err := os.WriteFile(
		filepath.Join(path, worktreeMarker),
		[]byte(agentID+"\n"),
		0o600,
	); err != nil {
		return Worktree{}, err
	}
	return Worktree{ID: agentID, Path: path}, nil
}

func (p *allocationFaultWorktrees) Discard(Worktree) error {
	return p.discardErr
}

type allocationPassGate struct{}

func (allocationPassGate) Execute(
	context.Context,
	string,
	string,
	json.RawMessage,
) (tool.Result, error) {
	return tool.Result{Content: "ok"}, nil
}

func allocationGraph(
	sessionID string,
	spawn func(GraphEdge) error,
	status func(GraphTransition) error,
) DurableGraph {
	return DurableGraph{
		Workspace: "/workspace", SessionID: sessionID,
		AppendSpawn: spawn,
		AppendStatus: func(transition GraphTransition) error {
			if status == nil {
				return nil
			}
			return status(transition)
		},
		AppendMessage:  func(Message) error { return nil },
		DeliverMessage: func(Message) error { return nil },
		Sessions:       func() ([]string, error) { return nil, nil },
		Children: func(string, string) ([]GraphEdge, error) {
			return nil, nil
		},
		Messages: func(string, string) ([]Message, error) {
			return nil, nil
		},
		Result: func(string, string) (Result, bool, error) {
			return Result{}, false, nil
		},
		IntegrationResult: func(string, string) (Result, bool, error) {
			return Result{}, false, nil
		},
		AppendIntegration: func(IntegrationCandidate) error { return nil },
		Integration: func(
			string,
			string,
			string,
		) (IntegrationCandidate, bool, error) {
			return IntegrationCandidate{}, false, nil
		},
	}
}
