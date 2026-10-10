package protocol

import (
	"errors"
	"math"
)

const EventGuardianReview EventKind = "guardian.review"

// GuardianReviewData is an audit projection, never an approval or a lease.
// It deliberately excludes command/code/user text, environment values and the
// model's free-form rationale. Tool/approval events own the displayed operation.
type GuardianReviewData struct {
	ReviewID            string                    `json:"review_id"`
	CallID              string                    `json:"call_id"`
	Tool                string                    `json:"tool"`
	Phase               string                    `json:"phase"`
	ReasonCode          string                    `json:"reason_code"`
	OriginalEffect      string                    `json:"original_effect"`
	OriginalRisk        string                    `json:"original_risk"`
	Eligibility         string                    `json:"eligibility"`
	Provider            string                    `json:"provider"`
	Model               string                    `json:"model"`
	ConfigurationDigest string                    `json:"configuration_digest"`
	RouteDigest         string                    `json:"route_digest"`
	PromptVersion       string                    `json:"prompt_version"`
	SchemaVersion       string                    `json:"schema_version"`
	Candidate           *GuardianCandidateRecord  `json:"candidate,omitempty"`
	Assessment          *GuardianAssessmentRecord `json:"assessment,omitempty"`
	Decision            *GuardianDecisionRecord   `json:"decision,omitempty"`
	Usage               *GuardianUsageRecord      `json:"usage,omitempty"`
	ApprovalRequestID   string                    `json:"approval_request_id,omitempty"`
}

type GuardianCandidateRecord struct {
	AttemptID           string `json:"attempt_id"`
	WorkspaceID         string `json:"workspace_id"`
	WorkspaceGeneration uint64 `json:"workspace_generation"`
	CatalogID           string `json:"catalog_id"`
	CatalogGeneration   uint64 `json:"catalog_generation"`
	BindingRevision     uint64 `json:"binding_revision"`
	SubjectDigest       string `json:"subject_digest"`
	ArgumentsDigest     string `json:"arguments_digest"`
	CandidateDigest     string `json:"candidate_digest"`
	AuthorizationDigest string `json:"authorization_digest"`
	ExecutionDigest     string `json:"execution_digest"`
	PolicyRevision      uint64 `json:"policy_revision"`
	Permission          string `json:"permission"`
}

type GuardianAssessmentRecord struct {
	Risk           string   `json:"risk"`
	Authorization  string   `json:"authorization"`
	Recommendation string   `json:"recommendation"`
	SourceIDs      []string `json:"source_ids"`
}

type GuardianDecisionRecord struct {
	PolicyRevision uint64 `json:"policy_revision"`
	Action         string `json:"action"`
	Layer          string `json:"layer"`
	Code           string `json:"code,omitempty"`
	Authority      string `json:"authority"`
}

type GuardianUsageRecord struct {
	InputTokens     uint64  `json:"input_tokens"`
	OutputTokens    uint64  `json:"output_tokens"`
	CachedTokens    uint64  `json:"cached_tokens"`
	ReasoningTokens uint64  `json:"reasoning_tokens"`
	CostUSD         float64 `json:"cost_usd"`
	CostKnown       bool    `json:"cost_known"`
	Attempted       bool    `json:"attempted"`
	DurationMS      int64   `json:"duration_ms"`
}

func (*GuardianReviewData) eventKind() EventKind { return EventGuardianReview }

func (d *GuardianReviewData) validate() error {
	if d.ReviewID == "" || d.CallID == "" || d.Tool == "" || d.Provider == "" || d.Model == "" ||
		d.PromptVersion == "" || d.SchemaVersion == "" || !validSHA256(d.ConfigurationDigest) || !validSHA256(d.RouteDigest) ||
		!validApprovalEffect(d.OriginalEffect) || !validApprovalRisk(d.OriginalRisk) || d.Eligibility != "bounded_process" {
		return errors.New("Guardian review identity and trusted provenance are required")
	}
	switch d.Phase {
	case "started", "assessed", "failed", "canceled", "invalidated", "human_required", "human_resolved", "decided":
	default:
		return errors.New("invalid Guardian review phase")
	}
	if !validGuardianReason(d.ReasonCode) {
		return errors.New("invalid Guardian review reason")
	}
	if d.Phase == "started" || d.Phase == "assessed" {
		if d.Candidate == nil {
			return errors.New("Guardian phase requires a candidate")
		}
	}
	if d.Phase == "assessed" && d.Assessment == nil {
		return errors.New("Guardian assessment is required")
	}
	if d.Phase == "decided" && d.Decision == nil {
		return errors.New("Guardian policy decision is required")
	}
	if c := d.Candidate; c != nil {
		if c.AttemptID == "" || c.WorkspaceID == "" || c.CatalogID == "" || c.WorkspaceGeneration == 0 || c.CatalogGeneration == 0 || c.BindingRevision == 0 || c.PolicyRevision == 0 || c.Permission != "auto" {
			return errors.New("invalid Guardian candidate identity")
		}
		for _, digest := range []string{c.SubjectDigest, c.ArgumentsDigest, c.CandidateDigest, c.AuthorizationDigest, c.ExecutionDigest} {
			if !validSHA256(digest) {
				return errors.New("invalid Guardian evidence digest")
			}
		}
	}
	if a := d.Assessment; a != nil {
		if !validApprovalRisk(a.Risk) || (a.Authorization != "supported" && a.Authorization != "unknown" && a.Authorization != "conflicting") || (a.Recommendation != "allow" && a.Recommendation != "prompt") {
			return errors.New("invalid Guardian assessment")
		}
		if a.Authorization == "supported" && len(a.SourceIDs) == 0 {
			return errors.New("Guardian supported assessment requires user sources")
		}
		seen := make(map[string]bool)
		for _, id := range a.SourceIDs {
			if id == "" || seen[id] {
				return errors.New("invalid Guardian user source")
			}
			seen[id] = true
		}
	}
	if decision := d.Decision; decision != nil {
		switch decision.Action {
		case "allow", "ask", "deny", "hold":
		default:
			return errors.New("invalid Guardian policy action")
		}
		switch decision.Authority {
		case "policy", "guardian", "human", "none":
		default:
			return errors.New("invalid Guardian authority")
		}
		if decision.Layer == "" || decision.PolicyRevision == 0 {
			return errors.New("Guardian policy layer is required")
		}
		if decision.Authority == "guardian" && (decision.Action != "allow" || decision.Code != "guardian_allowed" || d.Assessment == nil || d.Candidate == nil) {
			return errors.New("Guardian authorization requires bound assessment and policy allow")
		}
		if decision.Authority == "human" && d.ApprovalRequestID == "" {
			return errors.New("Guardian human authority requires an approval reference")
		}
	}
	if u := d.Usage; u != nil {
		if u.CachedTokens > u.InputTokens || u.ReasoningTokens > u.OutputTokens || u.DurationMS < 0 || u.CostUSD < 0 || math.IsNaN(u.CostUSD) || math.IsInf(u.CostUSD, 0) {
			return errors.New("invalid Guardian usage")
		}
	}
	return nil
}

func validGuardianReason(code string) bool {
	switch code {
	case "review_started", "assessment_complete", "review_unavailable", "review_timed_out", "review_canceled", "evidence_invalidated", "execution_invalidated", "approval_required", "policy_decision", "human_approved", "human_denied", "human_canceled", "approval_expired", "audit_unavailable", "automatic_approval_disabled":
		return true
	}
	return false
}
