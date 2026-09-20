package subagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
)

func TestCloseWaitsForRealSettlement(t *testing.T) {
	for _, terminal := range []subagent.Status{subagent.StatusInterrupted, subagent.StatusCompleted} {
		t.Run(string(terminal), func(t *testing.T) {
			requested, returnCancel := make(chan struct{}), make(chan struct{})
			runtime := &interruptRuntime{onCancel: func() error {
				close(requested)
				<-returnCancel
				return nil
			}}
			manager, err := subagent.Open(subagent.Options{
				Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
				Budget: subagent.Budget{MaxParallel: 1, MaxTokens: 1000},
			})
			if err != nil {
				t.Fatal(err)
			}
			child, err := manager.Spawn("", subagent.RoleGeneral, "work")
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
			if !ok || current.Closed || current.Status != subagent.StatusRunning ||
				current.ReservedTokens != before.ReservedTokens || current.Result != nil {
				t.Fatalf("close changed unsettled agent: %+v", current)
			}
			if _, err := os.Stat(child.Worktree); err != nil {
				t.Fatalf("close removed active worktree: %v", err)
			}
			if _, err := manager.ExecuteTool(ctx, child.ID, "late", "read", json.RawMessage(`{}`)); err == nil {
				t.Fatal("closing agent accepted tool execution")
			}
			if _, err := manager.Spawn(child.ID, subagent.RoleExplore, "late"); err == nil {
				t.Fatal("closing parent accepted delegation")
			}
			result := subagent.Result{
				AgentID: child.ID, ThreadID: child.ThreadID, TurnID: turn,
				Status: terminal, Summary: "real result",
				Usage: subagent.ResultUsage{InputTokens: 17, OutputTokens: 3},
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
			final := manager.List(subagent.ListFilter{IncludeClosed: true})[0]
			if !final.Closed || final.Status != subagent.StatusClosed ||
				final.SpentTokens != 20 || final.ReservedTokens != 0 ||
				final.Result == nil || final.Result.Status != terminal || final.Result.TurnID != turn {
				t.Fatalf("closed result = %+v", final)
			}
			if _, err := os.Stat(child.Worktree); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("worktree not removed: %v", err)
			}
			messages := manager.Mailbox().Receive(subagent.SessionParentID)
			if len(messages) != 1 || messages[0].Kind != subagent.MessageCompletion {
				t.Fatalf("completion messages = %+v", messages)
			}
			next, err := manager.Spawn("", subagent.RoleExplore, "next")
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
			manager, err := subagent.Open(subagent.Options{
				Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
			})
			if err != nil {
				t.Fatal(err)
			}
			child, err := manager.Spawn("", subagent.RoleGeneral, "work")
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
			if !ok || current.Status != subagent.StatusRunning || current.Result != nil {
				t.Fatalf("failed close changed agent: %+v", current)
			}
			if _, err := os.Stat(child.Worktree); err != nil {
				t.Fatal(err)
			}
			runtime.onCancel = func() error {
				return manager.Settle(subagent.Result{
					AgentID: child.ID, ThreadID: child.ThreadID, TurnID: turn,
					Status: subagent.StatusInterrupted,
				})
			}
			if err := manager.CloseContext(t.Context(), child.ID); err != nil {
				t.Fatalf("retry close: %v", err)
			}
		})
	}
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

func TestCloseWaitsForStartSubmission(t *testing.T) {
	runtime := &startingCloseRuntime{
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	manager, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Spawn("", subagent.RoleExplore, "work")
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
	if !ok || current.Status != subagent.StatusRunning || current.TurnID == "" {
		t.Fatalf("accepted child lost after close timeout: %+v", current)
	}
	runtime.onCancel = func() error {
		return manager.Settle(subagent.Result{
			AgentID: child.ID, ThreadID: child.ThreadID, TurnID: current.TurnID,
			Status: subagent.StatusInterrupted,
		})
	}
	if err := manager.CloseContext(t.Context(), child.ID); err != nil {
		t.Fatal(err)
	}
}
