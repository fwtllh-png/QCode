package subagent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
)

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
		for _, terminal := range []subagent.Status{subagent.StatusInterrupted, subagent.StatusCompleted} {
			t.Run(timing+"/"+string(terminal), func(t *testing.T) {
				runtime := &interruptRuntime{}
				manager, err := subagent.Open(subagent.Options{
					Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
					Budget: subagent.Budget{MaxParallel: 1, MaxTokens: 1000, MaxCostUSD: 1},
				})
				if err != nil {
					t.Fatal(err)
				}
				child, err := manager.Spawn("", subagent.RoleExplore, "inspect")
				if err != nil {
					t.Fatal(err)
				}
				turn, err := manager.Takeover(t.Context(), child.ID, "inspect")
				if err != nil {
					t.Fatal(err)
				}
				running, _ := manager.Agent(child.ID)
				result := subagent.Result{
					AgentID: child.ID, ThreadID: child.ThreadID, TurnID: turn,
					Status: terminal, Summary: "runtime result",
					Usage: subagent.ResultUsage{
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
				if err != nil || prev != subagent.StatusRunning {
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
					if snap.Status != subagent.StatusRunning || snap.TurnID != followup {
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
				messages := manager.Mailbox().PendingSession(child.SessionID, subagent.SessionParentID)
				if len(messages) != 1 || messages[0].Kind != subagent.MessageCompletion {
					t.Fatalf("completion must be delivered once: %+v", messages)
				}
			})
		}
	}
}

func TestInterruptSubmissionFailureKeepsActiveTurn(t *testing.T) {
	rejected := errors.New("cancel rejected")
	runtime := &interruptRuntime{onCancel: func() error { return rejected }}
	manager, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: &fakeGate{}, Runtime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Spawn("", subagent.RoleExplore, "inspect")
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
	manager, err := subagent.Open(subagent.Options{Root: t.TempDir(), Gate: &fakeGate{}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Spawn("", subagent.RoleExplore, "inspect")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Interrupt(t.Context(), child.ID); err != nil {
		t.Fatal(err)
	}
	if result, ok := manager.Result(child.ID); !ok || result.Status != subagent.StatusInterrupted {
		t.Fatalf("synthetic result = %+v, %t", result, ok)
	}
	if _, err := manager.Interrupt(t.Context(), child.ID); err != nil {
		t.Fatal(err)
	}
	if messages := manager.Mailbox().PendingSession(child.SessionID, subagent.SessionParentID); len(messages) != 1 {
		t.Fatalf("synthetic completion = %+v", messages)
	}
}
