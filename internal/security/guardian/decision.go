package guardian

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ReviewCandidate is the immutable input bound to one authorization attempt
// (design §5.1). It carries the invocation identity, tool identity, execution
// facts, and the policy snapshot that produced the Ask. The candidate is
// constructed by the trusted Runtime/Guard integration point, never from
// untrusted tool JSON or model output.
type ReviewCandidate struct {
	// ReviewID uniquely identifies this review attempt.
	ReviewID string
	// CallID, Tool identify the invocation under review.
	CallID string
	Tool   string
	// Command is the normalized shell command for process tools.
	Command string
	// WorkingDir is the resolved cwd.
	WorkingDir string
	// OriginalEffect is the pre-review securitymodel effect classification,
	// preserved for audit; the Guardian does not mutate it.
	OriginalEffectKind     string
	OriginalEffectRisk     string
	OriginalEffectReversib string
	// AuthorizationDigest fingerprints the user authorization context that
	// was current when the review began. A mismatch on re-evaluation
	// invalidates the evidence.
	AuthorizationDigest string
	// CatalogVersion identifies the tool binding catalog revision.
	CatalogVersion string
	// ContentDigests fingerprint the execution evidence (scripts, configs)
	// that was reviewed. A mismatch at execution time invalidates the allow.
	ContentDigests []string
}

// Digest returns a stable identity for deduplication and evidence binding.
func (c ReviewCandidate) Digest() string {
	h := sha256.New()
	for _, s := range []string{
		c.ReviewID, c.CallID, c.Tool, c.Command, c.WorkingDir,
		c.OriginalEffectKind, c.OriginalEffectRisk, c.OriginalEffectReversib,
		c.AuthorizationDigest, c.CatalogVersion,
	} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	for _, d := range c.ContentDigests {
		h.Write([]byte(d))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// EvidenceInvalidation captures what may have changed between the model
// review and the re-evaluation (design §7). Any non-zero field means
// the evidence is stale and must not justify an automatic Allow.
type EvidenceInvalidation struct {
	// Cancelled is true when the call, turn, or session has ended.
	Cancelled bool
	// AuthorizationChanged is true when the user authorization context
	// (permissions, restrictions, revocations) differs from what was
	// captured in the candidate.
	AuthorizationChanged bool
	// CatalogChanged is true when the tool binding catalog was updated.
	CatalogChanged bool
	// ContentChanged is true when any execution evidence (script, config,
	// environment) differs from what was reviewed.
	ContentChanged bool
	// SurfaceTightened is true when a surface posture now requires an
	// approval that was not present at review time.
	SurfaceTightened bool
}

// Invalid reports whether any invalidation condition holds.
func (e EvidenceInvalidation) Invalid() bool {
	return e.Cancelled || e.AuthorizationChanged || e.CatalogChanged ||
		e.ContentChanged || e.SurfaceTightened
}

// PolicyContext captures the current policy state for the local decision
// table (design §6.2). It is a pure snapshot; the decision table is a
// pure function of (Assessment, EvidenceInvalidation, PolicyContext).
type PolicyContext struct {
	// CurrentAction is the policy decision before Guardian evidence
	// is applied ("allow", "ask", "deny", "hold").
	CurrentAction string
	// HasExplicitAsk is true when any layer other than Posture
	// (Constitution, Repository, User, Surface, Binding, Planning)
	// contributed an approval requirement.
	HasExplicitAsk bool
	// HasFreshRequirement is true when the binding requires Fresh or
	// FreshOnce approval.
	HasFreshRequirement bool
	// PermissionAuto is true when the session posture is Auto.
	PermissionAuto bool
	// DisableAutoReview is true when auto review is disabled.
	DisableAutoReview bool
}

// Outcome is the result of applying the local decision table. It tells
// the policy layer what to do with the model evidence.
type Outcome string

const (
	// OutcomeDiscard means the evidence is no longer relevant; the policy
	// should proceed with its own decision without the Guardian result.
	OutcomeDiscard Outcome = "discard"
	// OutcomePreserveDeny means deterministic rules already denied; the
	// model cannot override.
	OutcomePreserveDeny Outcome = "preserve_deny"
	// OutcomeKeepAsk means the Ask must remain; the Guardian evidence
	// does not satisfy the approval requirement.
	OutcomeKeepAsk Outcome = "keep_ask"
	// OutcomeAllow means all conditions are met for an automatic Allow.
	OutcomeAllow Outcome = "allow"
)

// Evaluate applies the local decision table from design §6.2. It is a
// pure function; no side effects, no network, no provider calls.
func Evaluate(
	assessment *Assessment,
	invalid EvidenceInvalidation,
	policy PolicyContext,
) Outcome {
	// Row 1: cancelled or session ended → discard.
	if invalid.Cancelled {
		return OutcomeDiscard
	}
	// Row 2: version or evidence changed → discard (re-evaluate or ask).
	if invalid.Invalid() {
		return OutcomeDiscard
	}
	// Row 3: current deterministic rules deny or hold → preserve.
	if policy.CurrentAction == "deny" || policy.CurrentAction == "hold" {
		return OutcomePreserveDeny
	}
	// Row 4: current rules already allow → discard model evidence.
	if policy.CurrentAction == "allow" {
		return OutcomeDiscard
	}
	// Row 5: current rules ask with non-eligible constraints → keep Ask.
	if policy.HasExplicitAsk || policy.HasFreshRequirement ||
		!policy.PermissionAuto || policy.DisableAutoReview {
		return OutcomeKeepAsk
	}
	// Row 6: model unavailable, invalid, or incomplete → Ask.
	// (The caller passes a nil assessment in this case.)
	if assessment == nil {
		return OutcomeKeepAsk
	}
	// Row 7: model risk high/critical, auth unknown/conflicting, or
	// recommendation prompt → Ask.
	if assessment.RiskLevel == RiskHigh || assessment.RiskLevel == RiskCritical {
		return OutcomeKeepAsk
	}
	if assessment.Authorization == AuthUnknown || assessment.Authorization == AuthConflicting {
		return OutcomeKeepAsk
	}
	if assessment.Recommendation == RecommendPrompt {
		return OutcomeKeepAsk
	}
	// Row 8: eligible, valid, low/medium risk, supported, allow → Allow.
	if assessment.RiskLevel == RiskLow || assessment.RiskLevel == RiskMedium {
		if assessment.Authorization == AuthSupported && assessment.Recommendation == RecommendAllow {
			return OutcomeAllow
		}
	}
	// Defensive: unhandled combination → Ask.
	return OutcomeKeepAsk
}

// Reason returns a human-readable explanation for audit.
func (o Outcome) Reason(assessment *Assessment) string {
	switch o {
	case OutcomeDiscard:
		return "guardian evidence is no longer relevant"
	case OutcomePreserveDeny:
		return "deterministic rules already denied this invocation"
	case OutcomeKeepAsk:
		if assessment == nil {
			return "guardian review did not complete; human approval is required"
		}
		return fmt.Sprintf("guardian assessment: risk=%s auth=%s rec=%s; human approval is required",
			assessment.RiskLevel, assessment.Authorization, assessment.Recommendation)
	case OutcomeAllow:
		if assessment != nil {
			return assessment.Rationale
		}
		return "guardian allowed this operation"
	}
	return "unknown guardian outcome"
}
