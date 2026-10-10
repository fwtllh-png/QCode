package engine

import (
	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func (e *Engine) recordGuardianCost(turnID string, result GuardianReviewResult) {
	accounting := &e.titleState
	accounting.mu.Lock()
	defer accounting.mu.Unlock()
	if accounting.guardianSpent == nil {
		accounting.guardianSpent = make(map[string]toolSpend)
	}
	spent := accounting.guardianSpent[turnID]
	spent.usage.Add(result.Usage)
	spent.cost += result.CostUSD
	accounting.guardianSpent[turnID] = spent
}

// Turn Usage includes Guardian tokens for durable accounting. Replace their
// hypothetical act-route price with the observed judge-route cost at admission.
// This adjustment moves no tokens or costs between session accounting ledgers.
func guardianCostAdjustment(spent toolSpend, pricing model.Pricing) float64 {
	return spent.cost - provider.EstimateCost(pricing, spent.usage)
}

func (e *Engine) guardianTurnCostAdjustment(turnID string, pricing model.Pricing) float64 {
	accounting := &e.titleState
	accounting.mu.Lock()
	defer accounting.mu.Unlock()
	return guardianCostAdjustment(accounting.guardianSpent[turnID], pricing)
}
