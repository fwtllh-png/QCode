package subagent

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// ThreadIDFor is the deterministic thread a child agent runs on. Both the agent
// tool (which reports it to the model) and the child runtime host (which submits
// turns on it) derive it from the agent id, so no handshake is needed to agree.
func ThreadIDFor(agentID string) string {
	return "thread-" + agentID
}

// ResultUsage is the child's real token and cost spend. CostKnown separates
// "free" from "price unknown" — a missing price must never read as zero cost.
type ResultUsage struct {
	InputTokens     uint64 `json:"input_tokens"`
	OutputTokens    uint64 `json:"output_tokens"`
	ReasoningTokens uint64 `json:"reasoning_tokens,omitempty"`
	CachedTokens    uint64 `json:"cached_tokens,omitempty"`
	CostMicrounits  uint64 `json:"cost_microunits"`
	CostKnown       bool   `json:"cost_known"`
}

// Tokens is the spend the shared child budget is charged for. It matches the
// engine's own definition (input plus output) so the fleet ledger and the
// per-child engine budget cannot disagree about what a turn cost. The sum
// saturates: a receipt whose token counts overflow uint64 must read as the
// maximum possible spend, never as a small number that slips under a budget.
func (u ResultUsage) Tokens() uint64 {
	total := u.InputTokens + u.OutputTokens
	if total < u.InputTokens {
		return math.MaxUint64
	}
	return total
}

// CostUSD is zero when the model has no pricing metadata. Callers that need to
// tell "free" from "unpriced" must read CostKnown.
func (u ResultUsage) CostUSD() float64 {
	if !u.CostKnown {
		return 0
	}
	return float64(u.CostMicrounits) / 1e6
}

// Result is what a child agent hands back to its parent. Every partition is a
// projection of the child turn's own execution receipt, so the child cannot
// claim more than the runtime observed.
type Result struct {
	AgentID           string                       `json:"agent_id"`
	ThreadID          string                       `json:"thread_id"`
	TurnID            string                       `json:"turn_id"`
	Status            Status                       `json:"status"`
	Summary           string                       `json:"summary,omitempty"`
	Evidence          *protocol.ReceiptEvidence    `json:"evidence,omitempty"`
	Diff              []protocol.ReceiptChange     `json:"diff,omitempty"`
	Verification      protocol.ReceiptVerification `json:"verification"`
	Unresolved        []string                     `json:"unresolved,omitempty"`
	Usage             ResultUsage                  `json:"usage"`
	PermissionDigests []string                     `json:"permission_digests,omitempty"`
	Context           ContextReceipt               `json:"context"`
	ReasonCode        string                       `json:"reason_code,omitempty"`
	Retryable         bool                         `json:"retryable,omitempty"`
	SuggestedAction   string                       `json:"suggested_action,omitempty"`
}

// WritePaths lists the paths the child actually changed, which is what the
// parent needs for conflict detection and for merging a worktree back.
func (r Result) WritePaths() []string {
	if len(r.Diff) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(r.Diff))
	paths := make([]string, 0, len(r.Diff))
	for _, change := range r.Diff {
		path := strings.TrimSpace(change.Path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths
}

// Digest is the compact status message persisted with the agent edge. The full
// Result is stored durably; Digest is the bounded Agent Node summary.
func (r Result) Digest() string {
	parts := make([]string, 0, 4)
	if summary := strings.TrimSpace(r.Summary); summary != "" {
		parts = append(parts, firstLine(summary, 200))
	}
	if len(r.Diff) > 0 {
		parts = append(parts, fmt.Sprintf("changed %d file(s)", len(r.Diff)))
	}
	if verdict := strings.TrimSpace(r.Verification.Verify); verdict != "" &&
		verdict != protocol.ReceiptNotEvaluated {
		parts = append(parts, "verify="+verdict)
	}
	if reason := strings.TrimSpace(r.ReasonCode); reason != "" {
		parts = append(parts, reason)
	} else if len(r.Unresolved) > 0 {
		parts = append(parts, firstSettlementNote(r.Unresolved, "", "unresolved"))
	}
	if len(parts) == 0 {
		return string(r.Status)
	}
	return strings.Join(parts, " · ")
}

func firstLine(value string, limit int) string {
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		value = value[:index]
	}
	if len(value) > limit {
		return value[:limit] + "…"
	}
	return value
}

// Settle records a child's terminal result and moves it to the matching status.
// It is the single entry point the child runtime uses, so a child can never end
// up terminal without a result or with a result but still running.
func (m *Manager) Settle(result Result) error {
	agentID := strings.TrimSpace(result.AgentID)
	if agentID == "" {
		return errors.New("agent id is required")
	}
	if !isTerminal(result.Status) {
		return fmt.Errorf("result status %q is not terminal", result.Status)
	}
	m.mu.Lock()
	agent, ok := m.agents[agentID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("agent %s is unavailable", agentID)
	}
	if agent.Closed || agent.Status == StatusShutdown {
		m.mu.Unlock()
		return fmt.Errorf("agent %s is closed", agentID)
	}
	if agent.Result != nil && agent.Status == result.Status &&
		agent.Result.TurnID == result.TurnID {
		m.mu.Unlock()
		return nil
	}
	stored := result
	if stored.Context.Version == 0 && agent.Context != nil {
		stored.Context = cloneContextReceipt(*agent.Context)
	}
	for _, conflict := range m.claimLocked(agentID, result.WritePaths()) {
		stored.Unresolved = append(stored.Unresolved, conflict.String())
	}
	err := m.transitionLocked(
		agent, stored.Status, stored.TurnID, stored.Digest(),
		"child_runtime", "terminal result committed", &stored,
	)
	m.mu.Unlock()
	return err
}

// Result reports a settled child result, if the agent has one.
func (m *Manager) Result(agentID string) (Result, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[agentID]
	if !ok || agent.Result == nil {
		return Result{}, false
	}
	return *agent.Result, true
}

// IntegrationResult returns the most recent successful child result whose
// isolated writes remain eligible for parent preview. A later failed follow-up
// does not erase already verified work.
func (m *Manager) IntegrationResult(agentID string) (Result, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[agentID]
	if !ok || agent.IntegrationResult == nil {
		return Result{}, false
	}
	return *agent.IntegrationResult, true
}
