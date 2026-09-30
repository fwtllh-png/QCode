package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

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

type fakeGate struct {
	calls int
}

type blockingCancelGate struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

type overlappingWorktrees struct {
	root  string
	count int
}

func (p *overlappingWorktrees) Provision(
	agentID string,
	_ Stance,
) (Worktree, error) {
	p.count++
	path := filepath.Join(p.root, "shared")
	if p.count > 1 {
		path = filepath.Join(path, agentID)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return Worktree{}, err
	}
	return Worktree{ID: agentID, Path: path}, nil
}

func (*overlappingWorktrees) Discard(Worktree) error {
	return nil
}

func (g *blockingCancelGate) Execute(
	ctx context.Context,
	_, _ string,
	_ json.RawMessage,
) (tool.Result, error) {
	close(g.started)
	<-ctx.Done()
	close(g.canceled)
	<-g.release
	return tool.Result{}, ctx.Err()
}

func (g *fakeGate) Execute(_ context.Context, _, name string, _ json.RawMessage) (tool.Result, error) {
	g.calls++
	return tool.Result{Content: "ok:" + name}, nil
}

type startingCloseRuntime struct {
	interruptRuntime
	entered chan struct{}
	release chan struct{}
}

func (r *startingCloseRuntime) StartTurn(ctx context.Context, agentID, prompt string) (string, error) {
	close(r.entered)
	<-r.release
	return r.recordingRuntime.StartTurn(ctx, agentID, prompt)
}

func TestParseRoleAliasesAndFailClosed(t *testing.T) {
	cases := []struct {
		in   string
		want Role
	}{
		{"", RoleGeneral},
		{"worker", RoleGeneral},
		{"general", RoleGeneral},
		{"explorer", RoleExplore},
		{"planner", RolePlan},
		{"reviewer", RoleReview},
		{"implement", RoleImplementer},
		{"verify", RoleVerifier},
		{"await", RoleAwaiter},
		{"custom", RoleCustom},
	}
	for _, tc := range cases {
		got, err := ParseRole(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("ParseRole(%q)=%q err=%v want %q", tc.in, got, err, tc.want)
		}
	}
	if _, err := ParseRole("nope"); err == nil {
		t.Fatal("expected unsupported role")
	}
}

func TestRouteStanceAndProfile(t *testing.T) {
	manager, err := Open(Options{
		Root: t.TempDir(), Gate: &fakeGate{},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := manager.Spawn("", RoleExplore, "map")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Role != RoleExplore || agent.Profile != "explore" || agent.Stance != StanceReadOnly {
		t.Fatalf("explore agent = %+v", agent)
	}
	impl, err := manager.Spawn("", RoleImplementer, "edit")
	if err != nil {
		t.Fatal(err)
	}
	if impl.Profile != "implement" || impl.Stance != StanceWrite {
		t.Fatalf("implementer = %+v", impl)
	}
}

func TestReadOnlySpawnSkipsWorktreeProvision(t *testing.T) {
	manager, err := Open(Options{
		Root: t.TempDir(), Gate: &fakeGate{},
		Budget:    Budget{MaxDepth: 2, MaxParallel: 2},
		SessionID: "session-readonly",
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Spawn("", RoleReview, "review only")
	if err != nil {
		t.Fatal(err)
	}
	if child.Isolated || child.Worktree != child.ExecutionRoot {
		t.Fatalf("read-only child provisioned a worktree: %+v", child)
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

func TestNestedAgentBudgetCanOnlyNarrowParentCeiling(t *testing.T) {
	control, err := OpenControl(Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Budget: Budget{
			MaxSteps: 30, MaxTokens: 1000, MaxCostUSD: 10,
			MaxDepth: 2, MaxParallel: 3, MaxResident: 3, MaxTotal: 3,
		},
	}, DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := control.SpawnIntent(DelegationIntent{
		TaskName: "parent", Role: RoleExplore, Objective: "inspect",
		ExpectedOutput: "report", Trigger: TriggerUser,
		Budget: AgentBudget{
			MaxSteps: 20, MaxTokens: 400, MaxCostUSD: 4,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = control.SpawnIntent(DelegationIntent{
		TaskName: "too_large", ParentID: parent.ID,
		Role: RoleExplore, Objective: "inspect",
		ExpectedOutput: "report", Trigger: TriggerSystem,
		Budget: AgentBudget{
			MaxSteps: 21, MaxTokens: 401, MaxCostUSD: 4.1,
		},
	})
	if err == nil {
		t.Fatal("nested agent expanded its parent budget")
	}
	child, err := control.SpawnIntent(DelegationIntent{
		TaskName: "narrow", ParentID: parent.ID,
		Role: RoleExplore, Objective: "inspect",
		ExpectedOutput: "report", Trigger: TriggerSystem,
		Budget: AgentBudget{
			MaxSteps: 10, MaxTokens: 200, MaxCostUSD: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.Budget.MaxSteps != 10 || child.Budget.MaxTokens != 200 ||
		child.Budget.MaxCostUSD != 2 ||
		child.ReservedTokens != 0 || child.ReservedMicros != 0 {
		t.Fatalf("nested budget = %+v", child)
	}
}

func TestDefaultAgentBudgetPartitionsTreeAcrossParallelSlots(t *testing.T) {
	control, err := OpenControl(Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Budget: Budget{
			MaxTokens: 1000, MaxCostUSD: 10,
			MaxParallel: 4, MaxResident: 4, MaxTotal: 4,
		},
	}, DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	child, err := control.SpawnIntent(DelegationIntent{
		TaskName: "default_share", Role: RoleExplore,
		Objective: "inspect", ExpectedOutput: "report",
		Trigger: TriggerUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.Budget.MaxTokens != 250 ||
		child.Budget.MaxCostUSD != 2.5 {
		t.Fatalf("default child budget = %+v", child.Budget)
	}
}

func TestResidentAndTotalTreeBudgets(t *testing.T) {
	manager, err := Open(Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Budget: Budget{
			MaxDepth: 2, MaxParallel: 2, MaxResident: 2, MaxTotal: 3,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Spawn("", RoleExplore, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Spawn("", RoleExplore, "second")
	if err != nil {
		t.Fatal(err)
	}
	third, err := manager.Spawn("", RoleExplore, "third")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ActivateResident(first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ActivateResident(second.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Complete(first.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Complete(second.ID, "done"); err != nil {
		t.Fatal(err)
	}
	evicted, err := manager.ActivateResident(third.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evicted) != 1 || evicted[0].ID != first.ID {
		t.Fatalf("LRU eviction = %+v, want %s", evicted, first.ID)
	}
	firstSnapshot, _ := manager.Agent(first.ID)
	thirdSnapshot, _ := manager.Agent(third.ID)
	if firstSnapshot.Resident || !thirdSnapshot.Resident {
		t.Fatalf("residency first=%+v third=%+v", firstSnapshot, thirdSnapshot)
	}
	if _, err := manager.Spawn("", RoleExplore, "total"); err == nil {
		t.Fatal("all agents must consume total spawn capacity")
	}
}

func TestCloseCancelsAndWaitsForToolExecutionLease(t *testing.T) {
	gate := &blockingCancelGate{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
		release:  make(chan struct{}),
	}
	manager, err := Open(Options{
		Root: t.TempDir(), Gate: gate,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := manager.Spawn("", RoleGeneral, "execute")
	if err != nil {
		t.Fatal(err)
	}
	executed := make(chan error, 1)
	go func() {
		_, executeErr := manager.ExecuteTool(
			t.Context(),
			agent.ID,
			"call",
			"read",
			json.RawMessage(`{}`),
		)
		executed <- executeErr
	}()
	<-gate.started
	closed := make(chan error, 1)
	go func() {
		closed <- manager.Close(agent.ID)
	}()
	<-gate.canceled
	if _, err := manager.ExecuteTool(
		t.Context(),
		agent.ID,
		"late",
		"read",
		json.RawMessage(`{}`),
	); err == nil {
		t.Fatal("tool execution acquired after close started")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned before execution exited: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(gate.release)
	if err := <-executed; !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecuteTool error = %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestCloseDrainsToolExecutionWhenWorktreeCleanupIsRefused(t *testing.T) {
	gate := &blockingCancelGate{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
		release:  make(chan struct{}),
	}
	root := t.TempDir()
	manager, err := Open(Options{
		Root: root, Gate: gate,
		Worktrees: &overlappingWorktrees{root: root},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := manager.Spawn("", RoleGeneral, "execute")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Spawn("", RoleGeneral, "overlap"); err != nil {
		t.Fatal(err)
	}
	executed := make(chan error, 1)
	go func() {
		_, executeErr := manager.ExecuteTool(
			t.Context(),
			agent.ID,
			"call",
			"read",
			json.RawMessage(`{}`),
		)
		executed <- executeErr
	}()
	<-gate.started
	closed := make(chan error, 1)
	go func() {
		closed <- manager.Close(agent.ID)
	}()
	<-gate.canceled
	select {
	case err := <-closed:
		t.Fatalf("Close returned before execution exited: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(gate.release)
	if err := <-executed; !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecuteTool error = %v", err)
	}
	if err := <-closed; err == nil ||
		!strings.Contains(err.Error(), "overlapping worktree") {
		t.Fatalf("Close error = %v", err)
	}
}

func TestCloseWaitsForRealSettlement(t *testing.T) {
	for _, terminal := range []Status{StatusInterrupted, StatusCompleted} {
		t.Run(string(terminal), func(t *testing.T) {
			requested, returnCancel := make(chan struct{}), make(chan struct{})
			runtime := &interruptRuntime{onCancel: func() error {
				close(requested)
				<-returnCancel
				return nil
			}}
			manager, err := Open(Options{
				Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
				Budget: Budget{MaxParallel: 1, MaxTokens: 1000},
			})
			if err != nil {
				t.Fatal(err)
			}
			child, err := manager.Spawn("", RoleGeneral, "work")
			if err != nil {
				t.Fatal(err)
			}
			turn, err := manager.Takeover(t.Context(), child.ID, "work")
			if err != nil {
				t.Fatal(err)
			}
			before, _ := manager.Agent(child.ID)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			closed := make(chan error, 1)
			go func() { closed <- manager.CloseContext(ctx, child.ID) }()
			<-requested
			defer close(returnCancel)
			current, ok := manager.Agent(child.ID)
			if !ok || current.Closed || current.Status != StatusRunning ||
				current.ReservedTokens != before.ReservedTokens || current.Result != nil {
				t.Fatalf("close changed unsettled agent: %+v", current)
			}
			if _, err := os.Stat(child.Worktree); err != nil {
				t.Fatalf("close removed active worktree: %v", err)
			}
			if _, err := manager.ExecuteTool(ctx, child.ID, "late", "read", json.RawMessage(`{}`)); err == nil {
				t.Fatal("closing agent accepted tool execution")
			}
			if _, err := manager.Spawn(child.ID, RoleExplore, "late"); err == nil {
				t.Fatal("closing parent accepted delegation")
			}
			result := Result{
				AgentID: child.ID, ThreadID: child.ThreadID, TurnID: turn,
				Status: terminal, Summary: "real result",
				Usage: ResultUsage{InputTokens: 17, OutputTokens: 3},
			}
			for range 2 {
				if err := manager.Settle(result); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := manager.FollowUp(ctx, child.ID, "late"); err == nil {
				t.Fatal("settled but closing agent accepted follow-up")
			}
			if _, err := manager.Takeover(ctx, child.ID, "late"); err == nil {
				t.Fatal("settled but closing agent accepted takeover")
			}
			// Finish cancellation only after checking the settlement/close window.
			returnCancel <- struct{}{}
			if err := <-closed; err != nil {
				t.Fatal(err)
			}
			if err := manager.CloseContext(ctx, child.ID); err != nil {
				t.Fatal(err)
			}
			final := manager.List(ListFilter{IncludeClosed: true})[0]
			if !final.Closed || final.Status != StatusClosed ||
				final.SpentTokens != 20 || final.ReservedTokens != 0 ||
				final.Result == nil || final.Result.Status != terminal || final.Result.TurnID != turn {
				t.Fatalf("closed result = %+v", final)
			}
			if _, err := os.Stat(child.Worktree); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("worktree not removed: %v", err)
			}
			messages := manager.Mailbox().Receive(SessionParentID)
			if len(messages) != 1 || messages[0].Kind != MessageCompletion {
				t.Fatalf("completion messages = %+v", messages)
			}
			next, err := manager.Spawn("", RoleExplore, "next")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Takeover(ctx, next.ID, "next"); err != nil {
				t.Fatalf("closed agent retained admission slot: %v", err)
			}
		})
	}
}

func TestCloseCancellationFailurePreservesAgentForRetry(t *testing.T) {
	for _, failure := range []string{"submit", "wait"} {
		t.Run(failure, func(t *testing.T) {
			runtime := &interruptRuntime{}
			manager, err := Open(Options{
				Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
			})
			if err != nil {
				t.Fatal(err)
			}
			child, err := manager.Spawn("", RoleGeneral, "work")
			if err != nil {
				t.Fatal(err)
			}
			turn, err := manager.Takeover(t.Context(), child.ID, "work")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := errors.New("cancel submission failed")
			runtime.onCancel = func() error {
				if failure == "submit" {
					return want
				}
				cancel()
				return nil
			}
			if failure == "wait" {
				want = context.Canceled
			}
			if err := manager.CloseContext(ctx, child.ID); !errors.Is(err, want) {
				t.Fatalf("close error = %v, want %v", err, want)
			}
			current, ok := manager.Agent(child.ID)
			if !ok || current.Status != StatusRunning || current.Result != nil {
				t.Fatalf("failed close changed agent: %+v", current)
			}
			if _, err := os.Stat(child.Worktree); err != nil {
				t.Fatal(err)
			}
			runtime.onCancel = func() error {
				return manager.Settle(Result{
					AgentID: child.ID, ThreadID: child.ThreadID, TurnID: turn,
					Status: StatusInterrupted,
				})
			}
			if err := manager.CloseContext(t.Context(), child.ID); err != nil {
				t.Fatalf("retry close: %v", err)
			}
		})
	}
}

func TestCloseWaitsForStartSubmission(t *testing.T) {
	runtime := &startingCloseRuntime{
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	manager, err := Open(Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Spawn("", RoleExplore, "work")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan error, 1)
	go func() {
		_, err := manager.Takeover(t.Context(), child.ID, "work")
		accepted <- err
	}()
	<-runtime.entered
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err = manager.CloseContext(ctx, child.ID)
	close(runtime.release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close did not wait for submission: %v", err)
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
	current, ok := manager.Agent(child.ID)
	if !ok || current.Status != StatusRunning || current.TurnID == "" {
		t.Fatalf("accepted child lost after close timeout: %+v", current)
	}
	runtime.onCancel = func() error {
		return manager.Settle(Result{
			AgentID: child.ID, ThreadID: child.ThreadID, TurnID: current.TurnID,
			Status: StatusInterrupted,
		})
	}
	if err := manager.CloseContext(t.Context(), child.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMailboxMonotonicAndDrainOrder(t *testing.T) {
	box := NewMailbox()
	for i := 0; i < 5; i++ {
		if _, err := box.Deliver("a", "b", json.RawMessage(`{"i":`+strconv.Itoa(i)+`}`)); err != nil {
			t.Fatal(err)
		}
	}
	msgs := box.Drain("b")
	if len(msgs) != 5 {
		t.Fatalf("drain len = %d", len(msgs))
	}
	for i, msg := range msgs {
		if msg.Sequence != uint64(i+1) {
			t.Fatalf("sequence[%d]=%d", i, msg.Sequence)
		}
	}
	box.Close()
	if _, err := box.Deliver("a", "b", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected closed mailbox error")
	}
}

func TestMailboxSerializesPersistenceInSequenceOrder(t *testing.T) {
	mailbox := NewMailbox()
	firstPersisting := make(chan struct{})
	secondPersisting := make(chan struct{})
	releaseFirst := make(chan struct{})
	mailbox.persist = func(message Message) error {
		switch message.Sequence {
		case 1:
			close(firstPersisting)
			<-releaseFirst
		case 2:
			close(secondPersisting)
		}
		return nil
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := mailbox.Enqueue(Message{
			To: "agent", Body: json.RawMessage(`{"sequence":1}`),
		})
		firstDone <- err
	}()
	<-firstPersisting
	secondDone := make(chan error, 1)
	go func() {
		_, err := mailbox.Enqueue(Message{
			To: "agent", Body: json.RawMessage(`{"sequence":2}`),
		})
		secondDone <- err
	}()
	select {
	case <-secondPersisting:
		t.Fatal("second message persisted before the first committed")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	messages := mailbox.Pending("agent")
	if len(messages) != 2 ||
		messages[0].Sequence != 1 ||
		messages[1].Sequence != 2 {
		t.Fatalf("pending messages = %+v", messages)
	}
}

func TestMailboxCloseWaitsForPersistenceCommit(t *testing.T) {
	mailbox := NewMailbox()
	persisting := make(chan struct{})
	release := make(chan struct{})
	mailbox.persist = func(Message) error {
		close(persisting)
		<-release
		return nil
	}
	enqueued := make(chan error, 1)
	go func() {
		_, err := mailbox.Enqueue(Message{
			To: "agent", Body: json.RawMessage(`{"message":"before-close"}`),
		})
		enqueued <- err
	}()
	<-persisting
	closed := make(chan struct{})
	go func() {
		mailbox.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while persistence was in flight")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-enqueued; err != nil {
		t.Fatal(err)
	}
	<-closed
	if messages := mailbox.Pending("agent"); len(messages) != 1 {
		t.Fatalf("pending messages after Close = %d", len(messages))
	}
	if _, err := mailbox.Enqueue(Message{
		To: "agent", Body: json.RawMessage(`{}`),
	}); err == nil {
		t.Fatal("enqueue succeeded after Close")
	}
}

func TestMailboxDrainReturnsMessagesWhenAckFails(t *testing.T) {
	mailbox := NewMailbox()
	mailbox.deliver = func(Message) error {
		return errors.New("delivery persist failed")
	}
	if _, err := mailbox.Deliver("agent-1", SessionParentID, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	got := mailbox.Drain(SessionParentID)
	if len(got) != 1 || got[0].Kind != MessageContext {
		t.Fatalf("drain = %+v", got)
	}
	if pending := mailbox.Pending(SessionParentID); len(pending) != 1 {
		t.Fatalf("pending after failed ack = %+v", pending)
	}
}

func BenchmarkOR0ResidentAgents(b *testing.B) {
	root := b.TempDir()
	for _, count := range []int{8, 32} {
		b.Run(fmt.Sprintf("agents_%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				manager, err := Open(Options{
					Root: filepath.Join(
						root,
						fmt.Sprintf("%d-%d", count, iteration),
					),

					Gate: &fakeGate{}, Budget: Budget{
						MaxDepth: 5, MaxParallel: count,
						MaxResident: count, MaxTotal: count,
					}, Workspace: root, SessionID: "or0-baseline",
				})
				if err != nil {
					b.Fatal(err)
				}
				for index := 0; index < count; index++ {
					if _, err := manager.Spawn(
						"",
						RoleExplore,
						fmt.Sprintf("inspect-%d", index),
					); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
