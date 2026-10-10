package engine

import (
	"sync"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

type modelBudgetReservation struct {
	usage  provider.Usage
	cost   float64
	turnID string
}

// All physical samples share this ledger. Pending estimates affect admission,
// never reported usage. Settlement replaces each reservation exactly once.
func (e *Engine) reserveAuxiliaryBudget(options Options, route model.ReadyRoute, input, output uint64, turnID string) (uint64, func(provider.Usage), error) {
	return e.reserveModelBudget(options, route, input, output, turnID, true)
}

func (e *Engine) reserveModelBudget(options Options, route model.ReadyRoute, input, output uint64, turnID string, auxiliary bool) (uint64, func(provider.Usage), error) {
	accounting := &e.titleState
	accounting.mu.Lock()
	defer accounting.mu.Unlock()
	usage := accounting.mainUsage
	usage.Add(accounting.usage)
	cost := accounting.mainCost + accounting.cost
	turn := accounting.turnUsage[turnID]
	if turnID != "" && turnID == accounting.mainTurnID {
		turn.Add(accounting.mainTurnUsage)
	}
	for pending := range accounting.pending {
		usage.Add(pending.usage)
		cost += pending.cost
		if turnID != "" && pending.turnID == turnID {
			turn.Add(pending.usage)
		}
	}
	request := agentcontext.BudgetRequest{
		ContextTokens: route.Model().Limits.ContextTokens, EstimatedInput: input, OutputReserve: output,
		SessionUsage: usage, MaxTokens: options.Budget.MaxTokens,
		SpentCostUSD: cost, MaxCostUSD: options.Budget.MaxCostUSD, Pricing: route.Model().Pricing, Scope: "session",
	}
	if turnID != "" && options.Budget.MaxTurnTokens != 0 {
		turnRequest := request
		turnRequest.SessionUsage, turnRequest.MaxTokens, turnRequest.MaxCostUSD, turnRequest.Scope = turn, options.Budget.MaxTurnTokens, 0, "turn:"+turnID
		var err error
		request.OutputReserve, err = agentcontext.CheckBudget(turnRequest)
		if err != nil {
			return 0, nil, err
		}
	}
	granted, err := agentcontext.CheckBudget(request)
	if err != nil {
		return 0, nil, err
	}
	reservation := &modelBudgetReservation{usage: provider.Usage{InputTokens: input, OutputTokens: granted}, turnID: turnID}
	reservation.cost = provider.EstimateCost(route.Model().Pricing, reservation.usage)
	if accounting.pending == nil {
		accounting.pending = make(map[*modelBudgetReservation]struct{})
	}
	accounting.pending[reservation] = struct{}{}
	var once sync.Once
	settle := func(actual provider.Usage) {
		once.Do(func() {
			accounting.mu.Lock()
			defer accounting.mu.Unlock()
			delete(accounting.pending, reservation)
			cost := provider.EstimateCost(route.Model().Pricing, actual)
			if auxiliary {
				accounting.usage.Add(actual)
				accounting.cost += cost
				if turnID != "" {
					if accounting.turnUsage == nil {
						accounting.turnUsage = make(map[string]provider.Usage)
					}
					usage := accounting.turnUsage[turnID]
					usage.Add(actual)
					accounting.turnUsage[turnID] = usage
				}
			} else {
				// Foreground durable accounting is committed by the existing turn
				// path. Publish it here first so concurrent auxiliary calls see it.
				accounting.mainUsage.Add(actual)
				accounting.mainCost += cost
				if accounting.mainTurnID == turnID {
					accounting.mainTurnUsage.Add(actual)
				}
			}
		})
	}
	return granted, settle, nil
}

// Called by foreground budget checks under the turn lock, using the leaf lock
// only to read auxiliary usage and reservations.
func (e *Engine) auxiliaryBudgetUsage(turnID string) (session, turn provider.Usage, cost float64) {
	accounting := &e.titleState
	accounting.mu.Lock()
	defer accounting.mu.Unlock()
	turn = accounting.turnUsage[turnID]
	for pending := range accounting.pending {
		session.Add(pending.usage)
		cost += pending.cost
		if pending.turnID == turnID {
			turn.Add(pending.usage)
		}
	}
	return
}
