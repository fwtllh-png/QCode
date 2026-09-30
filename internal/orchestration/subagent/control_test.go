package subagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/common/tracecontext"
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
	manager, err := Open(Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime, Budget: Budget{MaxDepth: 3, MaxParallel: 4},
	})
	if err != nil {
		t.Fatal(err)
	}

	parent, err := manager.Spawn("", RoleGeneral, "root")
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Spawn(parent.ID, RoleExplore, "map")
	if err != nil {
		t.Fatal(err)
	}
	if child.Status != StatusPendingInit {
		t.Fatalf("spawn status = %q", child.Status)
	}

	listed := manager.List(ListFilter{})
	if len(listed) != 2 {
		t.Fatalf("list = %+v", listed)
	}
	children := manager.List(ListFilter{ParentID: parent.ID})
	if len(children) != 1 || children[0].ID != child.ID {
		t.Fatalf("parent filter = %+v", children)
	}

	turn, err := manager.Takeover(context.Background(), child.ID, "go")
	if err != nil || turn == "" {
		t.Fatalf("takeover=%q err=%v", turn, err)
	}
	snap, ok := manager.Agent(child.ID)
	if !ok || snap.Status != StatusRunning || snap.TurnID != turn {
		t.Fatalf("running snapshot = %+v ok=%v", snap, ok)
	}
	if _, err := manager.FollowUp(context.Background(), child.ID, "again"); err == nil {
		t.Fatal("follow-up while running should fail")
	}

	prev, err := manager.Interrupt(context.Background(), child.ID)
	if err != nil || prev != StatusRunning {
		t.Fatalf("interrupt prev=%q err=%v", prev, err)
	}
	if runtime.cancels != 1 {
		t.Fatalf("cancels = %d", runtime.cancels)
	}
	snap, ok = manager.Agent(child.ID)
	if !ok || snap.Status != StatusRunning || snap.Result != nil {
		t.Fatalf("cancel request prematurely settled child: %+v", snap)
	}
	if _, err := manager.FollowUp(t.Context(), child.ID, "too early"); err == nil {
		t.Fatal("follow-up accepted before cancellation settled")
	}
	if err := manager.Settle(Result{
		AgentID: child.ID, ThreadID: child.ThreadID, TurnID: turn,
		Status: StatusInterrupted,
	}); err != nil {
		t.Fatal(err)
	}
	snap, ok = manager.Agent(child.ID)
	if !ok || snap.Status != StatusInterrupted {
		t.Fatalf("interrupted = %+v", snap)
	}
	if _, err := os.Stat(filepath.Join(snap.Worktree, ".qcode-worktree")); err != nil {
		t.Fatalf("interrupt cleaned worktree: %v", err)
	}
	if len(manager.List(ListFilter{})) != 2 {
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
	done := make(chan WaitResult, 1)
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
	if result.TimedOut || len(result.Agents) != 1 || result.Agents[0].Status != StatusCompleted {
		t.Fatalf("wait result = %+v", result)
	}

	if err := manager.Close(child.ID); err != nil {
		t.Fatal(err)
	}
	if len(manager.List(ListFilter{})) != 1 {
		t.Fatalf("closed child should leave parent only: %+v", manager.List(ListFilter{}))
	}
	closed := manager.List(ListFilter{IncludeClosed: true})
	if len(closed) != 2 {
		t.Fatalf("include closed = %+v", closed)
	}
	if _, err := manager.FollowUp(context.Background(), child.ID, "nope"); err == nil {
		t.Fatal("follow-up on closed agent should fail")
	}
}

func TestWaitTimeoutAndEmptyIDs(t *testing.T) {
	manager, err := Open(Options{
		Root: t.TempDir(), Gate: &fakeGate{},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := manager.Spawn("", RoleGeneral, "one")
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
	manager, err := Open(Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := manager.Spawn("", RoleGeneral, "write")
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
	if waiting.Status != StatusWaiting ||
		waiting.Revision != running.Revision+1 {
		t.Fatalf("waiting agent = %+v, running = %+v", waiting, running)
	}
	if err := manager.ResumeApproval(agent.ID, "approval-1"); err != nil {
		t.Fatal(err)
	}
	resumed, _ := manager.Agent(agent.ID)
	if resumed.Status != StatusRunning ||
		resumed.Revision != waiting.Revision+1 {
		t.Fatalf("resumed agent = %+v, waiting = %+v", resumed, waiting)
	}
}

func TestTakeoverFailureMarksErrored(t *testing.T) {
	runtime := &recordingRuntime{failOnce: true}
	manager, err := Open(Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := manager.Spawn("", RoleGeneral, "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Takeover(context.Background(), agent.ID, "boom"); err == nil {
		t.Fatal("expected takeover failure")
	}
	snap, ok := manager.Agent(agent.ID)
	if !ok || snap.Status != StatusErrored {
		t.Fatalf("errored = %+v ok=%v", snap, ok)
	}
}

type interruptRuntime struct {
	recordingRuntime
	onCancel func() error
}

func (r *interruptRuntime) CancelTurn(ctx context.Context, agentID, turnID string) error {
	if err := r.recordingRuntime.CancelTurn(ctx, agentID, turnID); err != nil {
		return err
	}
	if r.onCancel != nil {
		return r.onCancel()
	}
	return nil
}

func TestInterruptSettlesOnlyFromRuntimeResult(t *testing.T) {
	for _, timing := range []string{"after_return", "before_return", "followup_before_return"} {
		for _, terminal := range []Status{StatusInterrupted, StatusCompleted} {
			t.Run(timing+"/"+string(terminal), func(t *testing.T) {
				runtime := &interruptRuntime{}
				manager, err := Open(Options{
					Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
					Budget: Budget{MaxParallel: 1, MaxTokens: 1000, MaxCostUSD: 1},
				})
				if err != nil {
					t.Fatal(err)
				}
				child, err := manager.Spawn("", RoleExplore, "inspect")
				if err != nil {
					t.Fatal(err)
				}
				turn, err := manager.Takeover(t.Context(), child.ID, "inspect")
				if err != nil {
					t.Fatal(err)
				}
				running, _ := manager.Agent(child.ID)
				result := Result{
					AgentID: child.ID, ThreadID: child.ThreadID, TurnID: turn,
					Status: terminal, Summary: "runtime result",
					Usage: ResultUsage{
						InputTokens: 17, OutputTokens: 3, CostKnown: true, CostMicrounits: 100,
					},
				}
				settle := func() error {
					if err := manager.Settle(result); err != nil {
						return err
					}
					// Retried delivery must not duplicate accounting or completion.
					return manager.Settle(result)
				}
				var followup string
				if timing != "after_return" {
					runtime.onCancel = func() error {
						if err := settle(); err != nil {
							return err
						}
						if timing == "followup_before_return" {
							var err error
							followup, err = manager.FollowUp(t.Context(), child.ID, "continue")
							return err
						}
						return nil
					}
				}
				prev, err := manager.Interrupt(t.Context(), child.ID)
				if err != nil || prev != StatusRunning {
					t.Fatalf("Interrupt = %s, %v", prev, err)
				}
				if timing == "after_return" {
					if _, err := manager.Interrupt(t.Context(), child.ID); err != nil {
						t.Fatal(err)
					}
					pending, _ := manager.Agent(child.ID)
					if pending.Status != running.Status || pending.Revision != running.Revision ||
						pending.Result != nil || pending.ReservedTokens != running.ReservedTokens ||
						pending.ReservedMicros != running.ReservedMicros {
						t.Fatalf("cancel request changed settlement: %+v", pending)
					}
					// A canceled context returns immediately only if Wait has no
					// terminal fact, so no timing-based sleeps are needed.
					ctx, cancel := context.WithCancel(t.Context())
					cancel()
					if _, err := manager.Wait(ctx, []string{child.ID}, 0); !errors.Is(err, context.Canceled) {
						t.Fatalf("Wait returned before result: %v", err)
					}
					if err := settle(); err != nil {
						t.Fatal(err)
					}
				}
				snap, _ := manager.Agent(child.ID)
				if snap.Result == nil || snap.Result.TurnID != turn ||
					snap.Result.Summary != result.Summary ||
					snap.SpentTokens != 20 || snap.SpentMicros != 100 {
					t.Fatalf("result or accounting missing/duplicated: %+v", snap)
				}
				if timing == "followup_before_return" {
					if snap.Status != StatusRunning || snap.TurnID != followup {
						t.Fatalf("late cancel return overwrote follow-up: %+v", snap)
					}
				} else {
					if snap.Status != terminal || snap.ReservedTokens != 0 || snap.ReservedMicros != 0 ||
						snap.Revision != running.Revision+1 {
						t.Fatalf("terminal transition = %+v", snap)
					}
					waited, err := manager.Wait(t.Context(), []string{child.ID}, 0)
					if err != nil || len(waited.Agents) != 1 || waited.Agents[0].Result == nil {
						t.Fatalf("Wait missed result: %+v, %v", waited, err)
					}
					cancels := runtime.cancels
					if prev, err := manager.Interrupt(t.Context(), child.ID); err != nil || prev != terminal ||
						runtime.cancels != cancels {
						t.Fatalf("terminal interrupt = %s, %v; cancels %d -> %d", prev, err, cancels, runtime.cancels)
					}
					if _, err := manager.FollowUp(t.Context(), child.ID, "continue"); err != nil {
						t.Fatalf("follow-up after settlement: %v", err)
					}
				}
				messages := manager.Mailbox().PendingSession(child.SessionID, SessionParentID)
				if len(messages) != 1 || messages[0].Kind != MessageCompletion {
					t.Fatalf("completion must be delivered once: %+v", messages)
				}
			})
		}
	}
}

func TestInterruptSubmissionFailureKeepsActiveTurn(t *testing.T) {
	rejected := errors.New("cancel rejected")
	runtime := &interruptRuntime{onCancel: func() error { return rejected }}
	manager, err := Open(Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Spawn("", RoleExplore, "inspect")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Takeover(t.Context(), child.ID, "inspect"); err != nil {
		t.Fatal(err)
	}
	before, _ := manager.Agent(child.ID)
	if _, err := manager.Interrupt(t.Context(), child.ID); !errors.Is(err, rejected) {
		t.Fatalf("submission error = %v", err)
	}
	after, _ := manager.Agent(child.ID)
	if after.Status != before.Status || after.Revision != before.Revision || after.Result != nil {
		t.Fatalf("rejected cancel mutated turn: %+v", after)
	}
}

func TestInterruptWithoutRuntimePublishesSyntheticResult(t *testing.T) {
	manager, err := Open(Options{Root: t.TempDir(), Gate: &fakeGate{}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Spawn("", RoleExplore, "inspect")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Interrupt(t.Context(), child.ID); err != nil {
		t.Fatal(err)
	}
	if result, ok := manager.Result(child.ID); !ok || result.Status != StatusInterrupted {
		t.Fatalf("synthetic result = %+v, %t", result, ok)
	}
	if _, err := manager.Interrupt(t.Context(), child.ID); err != nil {
		t.Fatal(err)
	}
	if messages := manager.Mailbox().PendingSession(child.SessionID, SessionParentID); len(messages) != 1 {
		t.Fatalf("synthetic completion = %+v", messages)
	}
}

func TestParentCompletionWakeP95(t *testing.T) {
	const samples = 64
	manager, err := Open(Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Budget: Budget{
			MaxParallel: 1, MaxResident: 1, MaxTotal: samples,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	latencies := make([]time.Duration, 0, samples)
	for index := 0; index < samples; index++ {
		agent, err := manager.Spawn("", RoleExplore, "wake")
		if err != nil {
			t.Fatal(err)
		}
		ready := make(chan struct{})
		done := make(chan error, 1)
		go func(agentID string) {
			close(ready)
			_, waitErr := manager.Wait(
				context.Background(),
				[]string{agentID},
				time.Second,
			)
			done <- waitErr
		}(agent.ID)
		<-ready
		time.Sleep(time.Millisecond)
		started := time.Now()
		if err := manager.Complete(agent.ID, "done"); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		latencies = append(latencies, time.Since(started))
	}
	sort.Slice(latencies, func(left, right int) bool {
		return latencies[left] < latencies[right]
	})
	p95 := latencies[(samples*95+99)/100-1]
	if p95 > 50*time.Millisecond {
		t.Fatalf("parent completion wake p95 = %s, want <= 50ms", p95)
	}
	t.Logf("parent completion wake p95 = %s", p95)
}
