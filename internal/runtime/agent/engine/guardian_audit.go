package engine

import (
	"context"
	"errors"

	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (r *guardianCall) Report(_ context.Context, fact toolguard.GuardianFact) error {
	if r.emit == nil {
		return errors.New("Guardian durable audit sink is unavailable")
	}
	record := r.record
	record.ReviewID, record.CallID, record.Tool = r.ID(), fact.CallID, fact.Tool
	record.Phase, record.ReasonCode = fact.Phase, fact.ReasonCode
	record.OriginalEffect, record.OriginalRisk = fact.OriginalEffect, fact.OriginalRisk
	record.Eligibility = "bounded_process"
	record.Provider, record.Model = r.route.ProviderID(), r.route.Model().ID
	v := r.Versions()
	record.ConfigurationDigest, record.RouteDigest = v.ConfigurationDigest, v.RouteDigest
	record.PromptVersion, record.SchemaVersion = v.PromptVersion, v.SchemaVersion
	if c := fact.Candidate; c != nil {
		record.Candidate = &protocol.GuardianCandidateRecord{AttemptID: c.Identity.AttemptID, WorkspaceID: c.Identity.WorkspaceID,
			WorkspaceGeneration: c.Identity.WorkspaceGeneration, CatalogID: c.Binding.CatalogID, CatalogGeneration: c.Binding.CatalogGeneration,
			BindingRevision: c.Binding.Revision, SubjectDigest: c.Binding.Subject.Digest, ArgumentsDigest: c.Binding.ArgumentsDigest,
			CandidateDigest: c.Digest(), AuthorizationDigest: c.Authorization.Digest(), ExecutionDigest: guardianReviewDigest(c.Execution),
			PolicyRevision: c.Versions.PolicyRevision, Permission: c.Versions.Permission}
	}
	if d := fact.Decision; d != nil {
		record.Decision = &protocol.GuardianDecisionRecord{Action: string(d.Action), Layer: string(d.Layer), Code: d.Code, Authority: fact.Authority, PolicyRevision: fact.PolicyRevision}
	}
	if fact.ApprovalRequestID != "" {
		record.ApprovalRequestID = fact.ApprovalRequestID
	}
	// Keep a value snapshot for the next transition. Never retain caller slices
	// or mutate data already handed to the durable event sink.
	r.record = record
	return r.emit(Event{Guardian: &record})
}

func (r *guardianCall) reportAssessment(ctx context.Context, result GuardianReviewResult, reviewErr error) error {
	r.record.Usage = &protocol.GuardianUsageRecord{InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens,
		CachedTokens: result.Usage.CachedTokens, ReasoningTokens: result.Usage.ReasoningTokens, CostUSD: result.CostUSD, CostKnown: result.CostKnown,
		Attempted: result.Attempted, DurationMS: result.Duration.Milliseconds()}
	phase, reason := "assessed", "assessment_complete"
	if result.Evidence != nil && reviewErr == nil {
		a := result.Evidence.Assessment()
		r.record.Assessment = &protocol.GuardianAssessmentRecord{Risk: string(a.RiskLevel), Authorization: string(a.Authorization), Recommendation: string(a.Recommendation), SourceIDs: a.AuthorizationSourceIDs}
	} else {
		r.record.Assessment = nil
		phase, reason = "failed", "review_unavailable"
		if errors.Is(reviewErr, context.DeadlineExceeded) {
			reason = "review_timed_out"
		}
		if errors.Is(reviewErr, context.Canceled) {
			phase, reason = "canceled", "review_canceled"
		}
	}
	return r.Report(ctx, toolguard.GuardianFact{CallID: r.record.CallID, Tool: r.record.Tool, OriginalEffect: r.record.OriginalEffect, OriginalRisk: r.record.OriginalRisk, Phase: phase, ReasonCode: reason})
}
