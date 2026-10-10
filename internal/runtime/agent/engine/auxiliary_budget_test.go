package engine

import (
	"sync"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func TestAuxiliaryBudgetReservationsAreAtomicAndSettleOnce(t *testing.T) {
	e := guardianTestEngine(t, &guardianTestProvider{})
	e.options.Budget.MaxTokens = 100
	var wg sync.WaitGroup
	settlers := make(chan func(provider.Usage), 2)
	for range 2 {
		wg.Go(func() {
			_, settle, err := e.reserveAuxiliaryBudget(e.options, e.options.Route, 60, 40, "turn")
			if err == nil {
				settlers <- settle
			}
		})
	}
	wg.Wait()
	close(settlers)
	if len(settlers) != 1 {
		t.Fatalf("%d requests consumed the same balance", len(settlers))
	}
	if usage, _ := e.Usage(); usage.Total() != 0 {
		t.Fatal("pending estimates were reported as actual usage")
	}
	if _, err := e.checkBudget(1, provider.Usage{}, provider.Usage{}, 1); err == nil {
		t.Fatal("main sampler ignored auxiliary reservation")
	}
	for settle := range settlers {
		settle(provider.Usage{InputTokens: 10, OutputTokens: 5})
		settle(provider.Usage{InputTokens: 10, OutputTokens: 5})
	}
	if usage, _ := e.Usage(); usage.Total() != 15 {
		t.Fatalf("actual=%+v", usage)
	}
	output, settle, err := e.reserveAuxiliaryBudget(e.options, e.options.Route, 10, 100, "turn")
	if err != nil || output != 75 {
		t.Fatalf("unused reservation not refunded: %d %v", output, err)
	}
	settle(provider.Usage{})
}

func TestAuxiliaryBudgetSeesForegroundInFlightAndSpentUsage(t *testing.T) {
	e := guardianTestEngine(t, &guardianTestProvider{})
	e.options.Budget.MaxTokens = 100
	_, finishMain, err := e.reserveModelBudget(e.options, e.options.Route, 60, 40, "turn", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.reserveAuxiliaryBudget(e.options, e.options.Route, 1, 1, "turn"); err == nil {
		t.Fatal("auxiliary bypassed main reservation")
	}
	finishMain(provider.Usage{InputTokens: 60, OutputTokens: 10})
	out, settle, err := e.reserveAuxiliaryBudget(e.options, e.options.Route, 10, 100, "turn")
	if err != nil || out != 20 {
		t.Fatalf("foreground spend missing: output=%d err=%v", out, err)
	}
	settle(provider.Usage{})
}

func TestAuxiliaryBudgetSharesTurnTokensAndSessionCost(t *testing.T) {
	for _, resource := range []string{"turn", "cost"} {
		t.Run(resource, func(t *testing.T) {
			e := guardianTestEngine(t, &guardianTestProvider{})
			if resource == "turn" {
				e.options.Budget.MaxTurnTokens = 100
			} else {
				e.options.Budget.MaxCostUSD = 0.0001
			}
			_, settle, err := e.reserveAuxiliaryBudget(e.options, e.options.Route, 60, 40, "turn")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := e.reserveAuxiliaryBudget(e.options, e.options.Route, 1, 1, "turn"); err == nil {
				t.Fatal("pending quota ignored")
			}
			settle(provider.Usage{InputTokens: 60, OutputTokens: 40})
			if _, _, err := e.reserveAuxiliaryBudget(e.options, e.options.Route, 1, 1, "turn"); err == nil {
				t.Fatal("spent quota ignored")
			}
			if resource == "turn" {
				_, settle, err := e.reserveAuxiliaryBudget(e.options, e.options.Route, 1, 1, "next-turn")
				if err != nil {
					t.Fatal("turn quota leaked into next turn", err)
				}
				settle(provider.Usage{})
				scope := attachTestScope(t, e)
				scope.spec.Identity.TurnID = "turn"
				if _, err := e.checkBudget(1, provider.Usage{}, provider.Usage{}, 1); err == nil {
					t.Fatal("foreground turn budget omitted Guardian usage")
				}
			}
		})
	}
}
