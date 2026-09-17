package subagent_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/observability/tracecontext"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
)

type recordingRuntime struct {
	mu       sync.Mutex
	starts   int
	cancels  int
	lastTurn string
	failOnce bool
	trace    tracecontext.Link
}

func (r *recordingRuntime) StartTurn(ctx context.Context, agentID, prompt string) (string, error) {
	link, _ := tracecontext.Current(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts++
	r.trace = link
	if r.failOnce {
		r.failOnce = false
		return "", errors.New("turn failed")
	}
	r.lastTurn = "turn:" + agentID + ":" + prompt
	return r.lastTurn, nil
}

func (r *recordingRuntime) traceLink() tracecontext.Link {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.trace
}

func (r *recordingRuntime) CancelTurn(_ context.Context, _, turnID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels++
	r.lastTurn = turnID
	return nil
}

func TestListFollowUpInterruptWaitContract(t *testing.T) {
	runtime := &recordingRuntime{}
	manager, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime, Budget: subagent.Budget{MaxDepth: 3, MaxParallel: 4},
	})
	if err != nil {
		t.Fatal(err)
	}

	parent, err := manager.Spawn("", subagent.RoleGeneral, "root")
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Spawn(parent.ID, subagent.RoleExplore, "map")
	if err != nil {
		t.Fatal(err)
	}
	if child.Status != subagent.StatusPendingInit {
		t.Fatalf("spawn status = %q", child.Status)
	}

	listed := manager.List(subagent.ListFilter{})
	if len(listed) != 2 {
		t.Fatalf("list = %+v", listed)
	}
	children := manager.List(subagent.ListFilter{ParentID: parent.ID})
	if len(children) != 1 || children[0].ID != child.ID {
		t.Fatalf("parent filter = %+v", children)
	}

	turn, err := manager.Takeover(context.Background(), child.ID, "go")
	if err != nil || turn == "" {
		t.Fatalf("takeover=%q err=%v", turn, err)
	}
	snap, ok := manager.Agent(child.ID)
	if !ok || snap.Status != subagent.StatusRunning || snap.TurnID != turn {
		t.Fatalf("running snapshot = %+v ok=%v", snap, ok)
	}
	if _, err := manager.FollowUp(context.Background(), child.ID, "again"); err == nil {
		t.Fatal("follow-up while running should fail")
	}

	prev, err := manager.Interrupt(context.Background(), child.ID)
	if err != nil || prev != subagent.StatusRunning {
		t.Fatalf("interrupt prev=%q err=%v", prev, err)
	}
	if runtime.cancels != 1 {
		t.Fatalf("cancels = %d", runtime.cancels)
	}
	snap, ok = manager.Agent(child.ID)
	if !ok || snap.Status != subagent.StatusRunning || snap.Result != nil {
		t.Fatalf("cancel request prematurely settled child: %+v", snap)
	}
	if _, err := manager.FollowUp(t.Context(), child.ID, "too early"); err == nil {
		t.Fatal("follow-up accepted before cancellation settled")
	}
	if err := manager.Settle(subagent.Result{
		AgentID: child.ID, ThreadID: child.ThreadID, TurnID: turn,
		Status: subagent.StatusInterrupted,
	}); err != nil {
		t.Fatal(err)
	}
	snap, ok = manager.Agent(child.ID)
	if !ok || snap.Status != subagent.StatusInterrupted {
		t.Fatalf("interrupted = %+v", snap)
	}
	if _, err := os.Stat(filepath.Join(snap.Worktree, ".qcode-worktree")); err != nil {
		t.Fatalf("interrupt cleaned worktree: %v", err)
	}
	if len(manager.List(subagent.ListFilter{})) != 2 {
		t.Fatal("interrupted agent should remain listable")
	}

	follow, err := manager.FollowUp(context.Background(), child.ID, "resume")
	if err != nil || follow == "" {
		t.Fatalf("follow-up=%q err=%v", follow, err)
	}
	if runtime.starts != 2 {
		t.Fatalf("starts = %d", runtime.starts)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan subagent.WaitResult, 1)
	errs := make(chan error, 1)
	go func() {
		result, waitErr := manager.Wait(waitCtx, []string{child.ID}, 0)
		done <- result
		errs <- waitErr
	}()
	time.Sleep(20 * time.Millisecond)
	if err := manager.Complete(child.ID, "finished"); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if result.TimedOut || len(result.Agents) != 1 || result.Agents[0].Status != subagent.StatusCompleted {
		t.Fatalf("wait result = %+v", result)
	}

	if err := manager.Close(child.ID); err != nil {
		t.Fatal(err)
	}
	if len(manager.List(subagent.ListFilter{})) != 1 {
		t.Fatalf("closed child should leave parent only: %+v", manager.List(subagent.ListFilter{}))
	}
	closed := manager.List(subagent.ListFilter{IncludeClosed: true})
	if len(closed) != 2 {
		t.Fatalf("include closed = %+v", closed)
	}
	if _, err := manager.FollowUp(context.Background(), child.ID, "nope"); err == nil {
		t.Fatal("follow-up on closed agent should fail")
	}
}

func TestWaitTimeoutAndEmptyIDs(t *testing.T) {
	manager, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: &fakeGate{},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := manager.Spawn("", subagent.RoleGeneral, "one")
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Wait(context.Background(), []string{agent.ID}, 30*time.Millisecond)
	if err != nil || !result.TimedOut {
		t.Fatalf("timeout result = %+v err=%v", result, err)
	}

	if err := manager.Complete(agent.ID, "done"); err != nil {
		t.Fatal(err)
	}
	result, err = manager.Wait(context.Background(), nil, 50*time.Millisecond)
	if err != nil || result.TimedOut || len(result.Agents) != 1 {
		t.Fatalf("empty wait = %+v err=%v", result, err)
	}
}

func TestChildApprovalTransitionsThroughWaiting(t *testing.T) {
	runtime := &recordingRuntime{}
	manager, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := manager.Spawn("", subagent.RoleGeneral, "write")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Takeover(t.Context(), agent.ID, "write"); err != nil {
		t.Fatal(err)
	}
	running, _ := manager.Agent(agent.ID)
	if err := manager.AwaitApproval(agent.ID, "approval-1"); err != nil {
		t.Fatal(err)
	}
	waiting, _ := manager.Agent(agent.ID)
	if waiting.Status != subagent.StatusWaiting ||
		waiting.Revision != running.Revision+1 {
		t.Fatalf("waiting agent = %+v, running = %+v", waiting, running)
	}
	if err := manager.ResumeApproval(agent.ID, "approval-1"); err != nil {
		t.Fatal(err)
	}
	resumed, _ := manager.Agent(agent.ID)
	if resumed.Status != subagent.StatusRunning ||
		resumed.Revision != waiting.Revision+1 {
		t.Fatalf("resumed agent = %+v, waiting = %+v", resumed, waiting)
	}
}

func TestTakeoverFailureMarksErrored(t *testing.T) {
	runtime := &recordingRuntime{failOnce: true}
	manager, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := manager.Spawn("", subagent.RoleGeneral, "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Takeover(context.Background(), agent.ID, "boom"); err == nil {
		t.Fatal("expected takeover failure")
	}
	snap, ok := manager.Agent(agent.ID)
	if !ok || snap.Status != subagent.StatusErrored {
		t.Fatalf("errored = %+v ok=%v", snap, ok)
	}
}
