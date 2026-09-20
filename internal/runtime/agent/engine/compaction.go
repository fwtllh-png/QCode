package engine

import (
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/platform/tokenestimate"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

const (
	// defaultMaxDigestEntries bounds the per-message running record. Past a
	// hundred-odd lines the record stops being read and starts being scrolled.
	defaultMaxDigestEntries = agentcontext.DefaultMaxDigestEntries
	// summaryLineBytes caps one flattened message.
	summaryLineBytes = 512
)

// buildCompactSummary assembles what a compaction keeps out of the ledgers the
// thread has been filling all along, plus a running record of the messages that
// are about to be dropped.
//
// Only the running record has to be carried across compactions. The other
// sections are read live from ledgers that already span the whole session, so
// regenerating them is not an approximation of the previous summary — it is the
// same answer, current as of now.
func (e *Engine) buildCompactSummary(removed []provider.Message) agentcontext.Summary {
	e.planMu.Lock()
	plan := e.plan.Clone()
	e.planMu.Unlock()
	return e.contextAuthority().BuildSummary(agentcontext.SummaryRequest{
		Plan: plan, Removed: removed, Turn: e.turn,
		WorkingSetLimit:  e.options.WorkingSetLimit,
		EvidenceLimit:    e.options.EvidenceLimit,
		MaxDigestEntries: e.options.MaxDigestEntries,
		SummaryLineBytes: summaryLineBytes,
	}).Summary
}

// summaryBudget is how many bytes a rendered summary may occupy.
//
// Summary rendering remains byte-bounded; only the decision to compact is token-native.
func (e *Engine) summaryBudget() int {
	if e.options.SummaryMaxBytes > 0 {
		return e.options.SummaryMaxBytes
	}
	return int(tokenestimate.BytesForTokens(
		e.contextCapacity().HardInputTokens,
	))
}
