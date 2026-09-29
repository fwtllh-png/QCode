package subagent

import (
	"errors"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// Reasons attached to resource_exhausted Problems for child capacity that is
// not a token or cost budget.
const (
	ReasonConcurrencyExhausted = "concurrency_capacity_exhausted"
	ReasonSpawnExhausted       = "spawn_capacity_exhausted"
)

// SessionBudget is the Session Agent Tree's ledger. The Manager is the only
// child budget authority: admission, reservation, and spend all read and
// write the same Agent facts that the durable graph projects.
func (m *Manager) SessionBudget(sessionID string) BudgetLedger {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionBudgetLocked(sessionID)
}

// sessionBudgetLocked folds the ledger from Agent state instead of keeping
// running counters, so a path that forgets to adjust a counter cannot leak a
// slot or reservation.
func (m *Manager) sessionBudgetLocked(sessionID string) BudgetLedger {
	var ledger BudgetLedger
	for _, agent := range m.agents {
		if agent == nil || agent.SessionID != sessionID {
			continue
		}
		ledger.SpentTokens += agent.SpentTokens
		ledger.SpentMicros += agent.SpentMicros
		ledger.ReservedTokens += agent.ReservedTokens
		ledger.ReservedMicros += agent.ReservedMicros
		if occupiesSlot(agent.Status) {
			ledger.ReservedSlots++
		}
		if chargesSpawnBudget(agent) {
			ledger.TotalSpawned++
		}
	}
	for _, draft := range m.provisioning {
		if draft.SessionID == sessionID {
			ledger.TotalSpawned++
		}
	}
	return ledger
}

// chargesSpawnBudget reports whether an Agent counts toward max_total. A
// delegation closed before any turn was admitted never ran, so a rejected
// admission does not permanently consume the tree's spawn capacity.
func chargesSpawnBudget(agent *Agent) bool {
	closed := agent.Closed || agent.Status == StatusClosed
	return !closed || agent.TurnID != ""
}

// reserveTurnLocked admits one turn for agent against the Session tree and the
// Agent's lifetime budget, returning the tokens and microunits it reserves.
func (m *Manager) reserveTurnLocked(agent *Agent) (uint64, uint64, error) {
	ledger := m.sessionBudgetLocked(agent.SessionID)
	tree := treeScope(agent.SessionID)
	if ledger.ReservedSlots >= m.budget.MaxParallel {
		return 0, 0, protocol.NewProblemWithDetails(
			protocol.CodeResourceExhausted,
			"child Agent concurrency capacity is exhausted",
			true,
			protocol.ProblemDetails{
				Reason: ReasonConcurrencyExhausted, ResourceID: tree,
			},
			fmt.Errorf(
				"%d of %d child turns are active", ledger.ReservedSlots, m.budget.MaxParallel,
			),
		)
	}
	var reserveTokens, reserveMicros uint64
	if limit := agent.Budget.MaxTokens; limit > 0 {
		if agent.SpentTokens >= limit {
			return 0, 0, budgetExhausted(
				protocol.BudgetResourceTokens, agentScope(agent.ID),
				agent.SpentTokens, limit, false,
			)
		}
		reserveTokens = limit - agent.SpentTokens
	}
	if limit := costMicrounits(agent.Budget.MaxCostUSD); limit > 0 {
		if agent.SpentMicros >= limit {
			return 0, 0, budgetExhausted(
				protocol.BudgetResourceCostMicrounits, agentScope(agent.ID),
				agent.SpentMicros, limit, false,
			)
		}
		reserveMicros = limit - agent.SpentMicros
	}
	if err := m.admitTreeLocked(ledger, tree, reserveTokens, reserveMicros); err != nil {
		return 0, 0, err
	}
	return reserveTokens, reserveMicros, nil
}

// admitTreeLocked refuses work the Session tree cannot pay for. Spend already
// at the limit is reported as used; reservations that would cross it are
// reported as projected, so reserved capacity is never shown as spent.
func (m *Manager) admitTreeLocked(
	ledger BudgetLedger, tree string, tokens, micros uint64,
) error {
	if limit := m.budget.MaxTokens; limit > 0 {
		if ledger.SpentTokens >= limit {
			return budgetExhausted(protocol.BudgetResourceTokens, tree, ledger.SpentTokens, limit, false)
		}
		if projected := ledger.SpentTokens + ledger.ReservedTokens + tokens; projected > limit {
			return budgetExhausted(protocol.BudgetResourceTokens, tree, projected, limit, true)
		}
	}
	if limit := costMicrounits(m.budget.MaxCostUSD); limit > 0 {
		if ledger.SpentMicros >= limit {
			return budgetExhausted(protocol.BudgetResourceCostMicrounits, tree, ledger.SpentMicros, limit, false)
		}
		if projected := ledger.SpentMicros + ledger.ReservedMicros + micros; projected > limit {
			return budgetExhausted(protocol.BudgetResourceCostMicrounits, tree, projected, limit, true)
		}
	}
	return nil
}

func (m *Manager) admitSpawnLocked(ledger BudgetLedger, sessionID string) error {
	if ledger.TotalSpawned < m.budget.MaxTotal {
		return nil
	}
	return protocol.NewProblemWithDetails(
		protocol.CodeResourceExhausted,
		"child Agent spawn capacity is exhausted",
		false,
		protocol.ProblemDetails{
			Reason: ReasonSpawnExhausted, ResourceID: treeScope(sessionID),
		},
		fmt.Errorf("%d of %d child Agents were spawned", ledger.TotalSpawned, m.budget.MaxTotal),
	)
}

func budgetExhausted(
	resource protocol.BudgetResource,
	scope string,
	used, limit uint64,
	projected bool,
) error {
	return protocol.NewBudgetExhausted(protocol.BudgetExhaustion{
		Resource: resource, Scope: scope,
		Used: used, Limit: limit, Projected: projected,
	}, errors.New("child Agent budget"))
}

func treeScope(sessionID string) string { return "agent_tree:" + sessionID }

func agentScope(agentID string) string { return "agent:" + agentID }

func costMicrounits(costUSD float64) uint64 {
	if costUSD <= 0 {
		return 0
	}
	return uint64(costUSD * 1e6)
}
