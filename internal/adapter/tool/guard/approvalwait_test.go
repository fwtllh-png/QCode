package guard

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// waitClock is the guard's clock under test. The wait has to be measured by the
// guard because it is the only place both ends of it are visible, so the test
// moves the clock at the point a human would have been thinking.
type waitClock struct {
	mu sync.Mutex
	at time.Time
}

func newWaitClock() *waitClock {
	return &waitClock{at: time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)}
}

func (c *waitClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *waitClock) advance(step time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(step)
}

type waitObserver struct {
	mu    sync.Mutex
	waits []ApprovalWait
}

func (o *waitObserver) observe(wait ApprovalWait) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.waits = append(o.waits, wait)
}

func (o *waitObserver) snapshot() []ApprovalWait {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]ApprovalWait(nil), o.waits...)
}

// TestApprovalWaitIsTheTimeSpentWaiting pins what the observer reports: the
// stretch between raising the request and hearing back, and not the tool's own
// work, which happens after the decision.
func TestApprovalWaitIsTheTimeSpentWaiting(t *testing.T) {
	clock := newWaitClock()
	observer := &waitObserver{}
	executor := testExecutor{descriptor: writeDescriptor()}
	registry := newTestRegistry(t, nil, &executor)
	requests := make(chan ApprovalRequest, 1)
	guard, err := New(Options{
		Registry:  registry,
		Policy:    policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Workspace: t.TempDir(), Now: clock.now,
		Approvals: func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	guard.SetApprovalWaitObserver(observer.observe)

	done := make(chan error, 1)
	go func() {
		_, execErr := guard.Execute(context.Background(), "call-1", "write",
			json.RawMessage(`{"path":"a","value":"x"}`))
		done <- execErr
	}()
	request := <-requests
	clock.advance(90 * time.Second)
	mustDecide(t, guard, request, policy.ApprovalOnce, nil)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	waits := observer.snapshot()
	if len(waits) != 1 {
		t.Fatalf("waits = %+v, want one", waits)
	}
	got := waits[0]
	if got.Waited != 90*time.Second {
		t.Fatalf("waited = %s, want 90s", got.Waited)
	}
	if got.Outcome != ApprovalWaitDecided {
		t.Fatalf("outcome = %q, want %q", got.Outcome, ApprovalWaitDecided)
	}
	if got.CallID != "call-1" || got.Tool != "write" ||
		got.RequestID != request.RequestID {
		t.Fatalf("wait does not name what it waited for: %+v", got)
	}
}

// TestApprovalWaitIsReportedWhenNobodyAnswers covers the other endings. A wait
// that expired still cost the turn that time, so it is reported rather than
// dropped, and it says why it ended.
func TestApprovalWaitIsReportedWhenNobodyAnswers(t *testing.T) {
	observer := &waitObserver{}
	executor := testExecutor{descriptor: writeDescriptor()}
	registry := newTestRegistry(t, nil, &executor)
	guard, err := New(Options{
		Registry:  registry,
		Policy:    policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Workspace: t.TempDir(), ApprovalTTL: 20 * time.Millisecond,
		Approvals: func(context.Context, ApprovalRequest) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	guard.SetApprovalWaitObserver(observer.observe)

	if _, err := guard.Execute(t.Context(), "call-1", "write",
		json.RawMessage(`{"path":"a","value":"x"}`)); err == nil {
		t.Fatal("an unanswered approval should fail the call")
	}
	waits := observer.snapshot()
	if len(waits) != 1 || waits[0].Outcome != ApprovalWaitExpired {
		t.Fatalf("waits = %+v, want one expired wait", waits)
	}
	if waits[0].Waited <= 0 {
		t.Fatalf("expired wait = %s, want the time it spent waiting", waits[0].Waited)
	}
}

func TestApprovalWaitWithoutConfiguredTimeoutFollowsContext(t *testing.T) {
	observer := &waitObserver{}
	executor := testExecutor{descriptor: writeDescriptor()}
	registry := newTestRegistry(t, nil, &executor)
	requests := make(chan ApprovalRequest, 1)
	guard, err := New(Options{
		Registry: registry,
		Policy: policy.DefaultRuntime(
			policy.ModeAct,
			policy.PermissionSuggest,
		),
		Workspace: t.TempDir(),
		Approvals: func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	guard.SetApprovalWaitObserver(observer.observe)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, executeErr := guard.Execute(
			ctx,
			"call-no-timeout",
			"write",
			json.RawMessage(`{"path":"a","value":"x"}`),
		)
		done <- executeErr
	}()
	request := <-requests
	if !request.ExpiresAt.IsZero() {
		t.Fatalf("default approval expiry = %s, want none", request.ExpiresAt)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("approval cancellation error = %v", err)
	}
	waits := observer.snapshot()
	if len(waits) != 1 || waits[0].Outcome != ApprovalWaitCanceled {
		t.Fatalf("waits = %+v, want one canceled wait", waits)
	}
}

func TestC5GuardRestoresApprovalWaitWithoutDuplicateEmission(t *testing.T) {
	executor := testExecutor{descriptor: writeDescriptor()}
	registry := newTestRegistry(t, nil, &executor)
	var emissions int
	guard, err := New(Options{
		Registry: registry,
		Policy: policy.DefaultRuntime(
			policy.ModeAct,
			policy.PermissionSuggest,
		),
		Workspace: t.TempDir(),
		Approvals: func(context.Context, ApprovalRequest) error {
			emissions++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var request ApprovalRequest
	guard.SetApprovalHandler(func(_ context.Context, current ApprovalRequest) error {
		request = current
		return errors.New("simulate process exit while awaiting approval")
	})
	_, _ = guard.Execute(t.Context(), "call-restored", "write", json.RawMessage(`{"path":"a","value":"x"}`))
	if request.RequestID == "" || request.BindingDigest == "" {
		t.Fatal("missing real approval request")
	}
	guard.SetApprovalHandler(func(context.Context, ApprovalRequest) error { emissions++; return nil })
	if err := guard.RestoreApproval(request); err != nil {
		t.Fatal(err)
	}
	var restores int
	guard.SetApprovalRecoveryHandler(func(restored ApprovalRequest) error {
		if restored.RequestID != request.RequestID ||
			restored.CallID != request.CallID {
			t.Fatalf("restored request = %+v, want %+v", restored, request)
		}
		restores++
		return nil
	})
	done := make(chan error, 1)
	go func() {
		_, executeErr := guard.Execute(
			context.Background(),
			request.CallID,
			request.Tool,
			request.Arguments,
		)
		done <- executeErr
	}()
	deadline := time.Now().Add(time.Second)
	for {
		err := guard.StageDecision(ApprovalDecision{
			RequestID: request.RequestID,
			Approved:  true,
			Scope:     policy.ApprovalOnce,
		})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if err := guard.Resume(request.RequestID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if emissions != 0 || restores != 1 {
		t.Fatalf(
			"restored approval emissions = %d, restores = %d",
			emissions, restores,
		)
	}
}
