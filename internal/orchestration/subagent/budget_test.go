package subagent

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

func TestWorktreeProvisioningDoesNotHoldTheManagerLock(t *testing.T) {
	root := t.TempDir()
	provider := newBlockingWorktrees(root)
	t.Cleanup(provider.release)
	manager, err := Open(Options{
		Root: root, Gate: allocationPassGate{}, Worktrees: provider,
		Budget: Budget{MaxParallel: 1, MaxResident: 1, MaxTotal: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := manager.Spawn("", RoleExplore, "inspect")
	if err != nil {
		t.Fatal(err)
	}
	type spawned struct {
		agent *Agent
		err   error
	}
	writer := make(chan spawned, 1)
	go func() {
		agent, err := manager.Spawn("", RoleImplementer, "edit")
		writer <- spawned{agent, err}
	}()
	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("writer never started provisioning")
	}

	unblocked := make(chan error, 1)
	go func() {
		if _, ok := manager.Agent(reader.ID); !ok {
			unblocked <- errors.New("reader is unavailable")
			return
		}
		_ = manager.List(ListFilter{})
		_, err := manager.Takeover(t.Context(), reader.ID, "inspect")
		unblocked <- err
	}()
	select {
	case err := <-unblocked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worktree provisioning blocked other Agents on the Manager lock")
	}

	_, err = manager.Spawn("", RoleExplore, "inspect")
	var problem *protocol.Problem
	if !errors.As(err, &problem) || problem.Code != protocol.CodeResourceExhausted {
		t.Fatalf("an in-flight provisioning must hold its max_total share: %v", err)
	}

	provider.release()
	result := <-writer
	if result.err != nil {
		t.Fatal(result.err)
	}
	if got := manager.SessionBudget(result.agent.SessionID).TotalSpawned; got != 2 {
		t.Fatalf("total spawned after provisioning = %d, want 2", got)
	}
}

func TestParentClosedDuringProvisioningDiscardsTheChildWorktree(t *testing.T) {
	root := t.TempDir()
	provider := newBlockingWorktrees(root)
	t.Cleanup(provider.release)
	manager, err := Open(Options{
		Root: root, Gate: allocationPassGate{}, Worktrees: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := manager.Spawn("", RoleExplore, "inspect")
	if err != nil {
		t.Fatal(err)
	}
	failed := make(chan error, 1)
	go func() {
		_, err := manager.Spawn(parent.ID, RoleImplementer, "edit")
		failed <- err
	}()
	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("child never started provisioning")
	}
	closed := make(chan error, 1)
	go func() { closed <- manager.Close(parent.ID) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("closing the parent waited for the child worktree")
	}
	provider.release()
	if err := <-failed; err == nil {
		t.Fatal("a child was committed under a closed parent")
	}
	if len(manager.List(ListFilter{ParentID: parent.ID})) != 0 {
		t.Fatal("orphaned child is visible")
	}
	if provider.discarded() != 1 {
		t.Fatalf("child worktree discards = %d, want 1", provider.discarded())
	}
	if _, err := os.Stat(filepath.Join(
		root, worktreeAllocations, provider.lastID()+".json",
	)); !os.IsNotExist(err) {
		t.Fatalf("discarded child retained its allocation: %v", err)
	}
}

type blockingWorktrees struct {
	root    string
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once

	mu       sync.Mutex
	discards int
	last     string
}

func newBlockingWorktrees(root string) *blockingWorktrees {
	return &blockingWorktrees{
		root:    root,
		entered: make(chan struct{}, 1),
		gate:    make(chan struct{}),
	}
}

func (p *blockingWorktrees) release() { p.once.Do(func() { close(p.gate) }) }

func (p *blockingWorktrees) Provision(agentID string, stance Stance) (Worktree, error) {
	p.mu.Lock()
	p.last = agentID
	p.mu.Unlock()
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-p.gate
	return scratchWorktrees{root: p.root}.Provision(agentID, stance)
}

func (p *blockingWorktrees) Discard(worktree Worktree) error {
	p.mu.Lock()
	p.discards++
	p.mu.Unlock()
	return scratchWorktrees{root: p.root}.Discard(worktree)
}

func (p *blockingWorktrees) discarded() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.discards
}

func (p *blockingWorktrees) lastID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}
