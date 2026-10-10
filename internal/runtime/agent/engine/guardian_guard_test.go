package engine

import (
	"context"
	"math"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func TestGuardianGuardChargesTurnOnceWithoutAuxiliaryDoubleCount(t *testing.T) {
	p := &guardianTestProvider{}
	e := guardianTestEngine(t, p)
	attachTestScope(t, e)
	review, err := e.PrepareGuardianReview()
	if err != nil {
		t.Fatal(err)
	}
	review.toolSpend = true
	candidate, authorization, content := guardianTestInput(t, review)
	call := &guardianCall{GuardianReview: review, authorization: authorization}
	p.call = func(context.Context, provider.ModelRequest) (provider.Stream, error) {
		return guardianSuccess(guardianResponse(authorization.Texts()[0].Source.ID)), nil
	}
	if _, err := call.Review(t.Context(), candidate, content); err != nil {
		t.Fatal(err)
	}
	if _, err := call.Review(t.Context(), candidate, content); err == nil {
		t.Fatal("same attempt was charged twice")
	}
	spend := e.drainToolSpend()
	if spend.usage.Total() != 15 || spend.samples != 1 {
		t.Fatalf("turn spend=%+v", spend)
	}
	if again := e.drainToolSpend(); again.samples != 0 {
		t.Fatalf("turn spend repeated: %+v", again)
	}
	accounting := &e.titleState
	accounting.mu.Lock()
	defer accounting.mu.Unlock()
	if accounting.mainUsage.Total() != 15 || accounting.usage.Total() != 0 || len(accounting.pending) != 0 || accounting.turnUsage[candidate.Identity.TurnID].Total() != 0 {
		t.Fatalf("duplicate or missing spend: main=%+v auxiliary=%+v pending=%d", accounting.mainUsage, accounting.usage, len(accounting.pending))
	}
	if call.AutomaticApprovalReady() {
		t.Fatal("automatic approval enabled without a durable sink and authorization source")
	}
}

func TestGuardianGuardKeepsJudgePriceAfterMainBudgetResample(t *testing.T) {
	e := guardianTestEngine(t, &guardianTestProvider{})
	scope := attachTestScope(t, e)
	scope.spec.Identity.TurnID = "turn"
	usage := provider.Usage{InputTokens: 10, OutputTokens: 5}
	const paid = 0.25
	e.recordGuardianCost("turn", GuardianReviewResult{Usage: usage, CostUSD: paid, CostKnown: true})
	e.options.Budget.MaxCostUSD = 0.2
	if _, err := e.checkBudget(1, usage, provider.Usage{}, 1); err == nil {
		t.Fatal("main budget repriced the expensive judge call as act tokens")
	}
	accounting := &e.titleState
	accounting.mu.Lock()
	defer accounting.mu.Unlock()
	if math.Abs(accounting.mainCost-paid) > 1e-12 {
		t.Fatalf("shared ledger lost judge price: %g", accounting.mainCost)
	}
}

func TestGuardianReviewBindsActiveDurableSessionInsteadOfProcessSeed(t *testing.T) {
	p := &guardianTestProvider{}
	e := guardianTestEngine(t, p)
	e.options.SessionID = "process-runtime"
	e.syncSessionTitleState(provider.Usage{})
	scope := attachTestScope(t, e)
	scope.spec.Identity = TurnIdentity{SessionID: "session", ThreadID: "thread", TurnID: "turn"}
	r, err := e.PrepareGuardianReview()
	if err != nil {
		t.Fatal(err)
	}
	c, a, content := guardianTestInput(t, r)
	p.call = func(context.Context, provider.ModelRequest) (provider.Stream, error) {
		return guardianSuccess(guardianResponse(a.Texts()[0].Source.ID)), nil
	}
	if _, err := r.Review(t.Context(), c, a, content); err != nil || p.attempts.Load() != 1 {
		t.Fatalf("durable session did not reach provider: calls=%d err=%v", p.attempts.Load(), err)
	}
}

func TestGuardianReviewFreezesInvocationSessionAndRejectsAnotherSession(t *testing.T) {
	for _, active := range []string{"session", "different-session"} {
		t.Run(active, func(t *testing.T) {
			p := &guardianTestProvider{}
			e := guardianTestEngine(t, p)
			e.options.SessionID = "process-runtime"
			e.syncSessionTitleState(provider.Usage{})
			ctx := tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{SessionID: active, ThreadID: "thread", TurnID: "turn"})
			r, err := e.prepareGuardianReview(ctx)
			if err != nil {
				t.Fatal(err)
			}
			c, a, content := guardianTestInput(t, r)
			// Starting a later scope must not change this already prepared attempt.
			scope := attachTestScope(t, e)
			scope.spec.Identity = TurnIdentity{SessionID: "later-session", ThreadID: "later-thread", TurnID: "later-turn"}
			p.call = func(context.Context, provider.ModelRequest) (provider.Stream, error) {
				return guardianSuccess(guardianResponse(a.Texts()[0].Source.ID)), nil
			}
			_, err = r.Review(t.Context(), c, a, content)
			if active == "session" {
				if err != nil || p.attempts.Load() != 1 {
					t.Fatalf("frozen identity lost: calls=%d err=%v", p.attempts.Load(), err)
				}
			} else if err == nil || p.attempts.Load() != 0 {
				t.Fatalf("cross-session candidate reached provider: calls=%d err=%v", p.attempts.Load(), err)
			}
		})
	}
}
