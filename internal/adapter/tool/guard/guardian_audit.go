package guard

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/security/guardian"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// GuardianFact is a transient handoff to Runtime. Only Runtime projects the
// safe audit fields; neither candidate text nor provider rationale is persisted.
type GuardianFact struct {
	CallID, Tool, OriginalEffect, OriginalRisk string
	Phase, ReasonCode                          string
	Candidate                                  *guardian.ReviewCandidate
	Decision                                   *policy.Decision
	Authority, ApprovalRequestID               string
	PolicyRevision                             uint64
}

func (a *guardianAttempt) report(ctx context.Context, phase, reason string, decision *policy.Decision, authority string) error {
	if a == nil || a.reviewer == nil {
		return nil
	}
	fact := GuardianFact{CallID: a.invocation.CallID, Tool: a.invocation.Tool,
		OriginalEffect: string(a.invocation.Assessment.Effect().Kind), OriginalRisk: string(a.invocation.Assessment.Effect().Risk),
		Phase: phase, ReasonCode: reason, Decision: decision, Authority: authority, ApprovalRequestID: a.approvalID, PolicyRevision: a.policyRevision}
	if a.candidate.Identity.ReviewID != "" {
		c := a.candidate
		fact.Candidate = &c
	}
	return a.reviewer.Report(ctx, fact)
}

func (a *guardianAttempt) id() string {
	if a == nil {
		return ""
	}
	if a.reviewer != nil {
		return a.reviewer.ID()
	}
	return a.recoveredReviewID
}

func (g *Guard) recoveredGuardian(ctx context.Context, callID string) bool {
	g.mu.Lock()
	request, ok := g.recovered[callID]
	g.mu.Unlock()
	if !ok {
		return false
	}
	if a := guardianAttemptFrom(ctx); a != nil {
		a.tried = true
		a.recoveredReviewID = request.GuardianReviewID
		a.reasonCode = request.GuardianReasonCode
		a.approvalID = request.RequestID
	}
	return true
}
