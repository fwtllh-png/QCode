package guardian

import (
	"fmt"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

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
	// Candidate is freshly sampled by the trusted caller. Evidence may only
	// apply to this exact invocation, authorization and execution snapshot.
	Candidate       *ReviewCandidate
	GuardianEnabled bool

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

// CandidateFacts captures the §2.1 eligibility inputs from the trusted
// binding and resolved invocation. The caller (Runtime/Guard integration
// point in a later phase) constructs these from authoritative sources;
// the Guardian package only validates them as pure computation.
type CandidateFacts struct {
	// Binding eligibility is supplied by the trusted built-in catalog.
	BuiltinBinding      bool
	RegisteredForReview bool
	EffectRule          string
	PermissionExpansion bool
	ProtectedPaths      bool
	ControlPlaneWrite   bool

	// Capability must be CapabilityProcess (shell-family executor).
	Capability string
	// Stage must be StageCall (admission, not egress approval).
	Stage string
	// SandboxRequired is true when the binding declares Strong Sandbox.
	SandboxRequired bool
	// HostExecution is true when the resolved prepare requests host execution.
	HostExecution bool
	// NetworkAccess is true when the invocation has any network resources
	// or capabilities.
	NetworkAccess bool
	// MCPOrSkill is true when the tool source is an MCP server or skill.
	MCPOrSkill bool
	// EffectRisk is the original assessment risk (high, medium, etc.).
	EffectRisk string
	// EffectKind is the original assessment effect kind.
	EffectKind string
	// EffectReversibility is the original assessment reversibility.
	EffectReversibility string
	// EvidenceComplete is true when all required execution evidence
	// (content digests, environment identity) is present and bound.
	EvidenceComplete bool
}

// EligibilityResult reports whether a candidate is eligible for Guardian
// review, with the first failing condition for auditability.
type EligibilityResult struct {
	Eligible bool
	Reason   string // empty when eligible; names the failing condition otherwise
}

// CheckEligibility applies the §2.1 conditions as a pure function.
// All conditions must hold; the first failure determines the reason.
func CheckEligibility(facts CandidateFacts) EligibilityResult {
	if !facts.BuiltinBinding || !facts.RegisteredForReview {
		return EligibilityResult{false, "binding is not registered for review"}
	}
	if !facts.SandboxRequired {
		return EligibilityResult{false, "Strong Sandbox is required"}
	}
	if facts.PermissionExpansion || facts.ProtectedPaths || facts.ControlPlaneWrite {
		return EligibilityResult{false, "expanded or protected authority excluded"}
	}
	if facts.Capability != "process" {
		return EligibilityResult{false, "not CapabilityProcess"}
	}
	if facts.Stage != "call" {
		return EligibilityResult{false, "not StageCall"}
	}
	if facts.HostExecution {
		return EligibilityResult{false, "host execution excluded"}
	}
	if facts.NetworkAccess {
		return EligibilityResult{false, "network access excluded in first version"}
	}
	if facts.MCPOrSkill {
		return EligibilityResult{false, "MCP/Skill source excluded"}
	}
	// §2.2: the reviewable category is high/bounded from RuleProcessMutating.
	// Other high effects (Fixed, irreversible, network-mutating) and all
	// critical effects are not eligible.
	if facts.EffectRisk == "critical" {
		return EligibilityResult{false, "critical risk excluded"}
	}
	if facts.EffectRisk != "high" {
		return EligibilityResult{false, "only high-risk bounded process mutations are reviewable"}
	}
	if facts.EffectRule != securitymodel.RuleProcessMutating ||
		facts.EffectKind != string(securitymodel.ProcessMutating) ||
		facts.EffectReversibility != string(securitymodel.Bounded) {
		return EligibilityResult{false, "only RuleProcessMutating process.mutating/high/bounded is reviewable"}
	}
	if !facts.EvidenceComplete {
		return EligibilityResult{false, "execution evidence is incomplete"}
	}
	return EligibilityResult{Eligible: true}
}

// Evaluate applies the local decision table from design §6.2. It is a
// pure function; no side effects, no network, no provider calls.
// CurrentAction must be exactly "ask" for Guardian to potentially allow;
// any other value (including empty or unknown) falls through to the
// deterministic-rule or discard paths.
func Evaluate(
	evidence *ReviewEvidence,
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
	// Guard: CurrentAction must be exactly "ask"; empty or unknown
	// values cannot proceed to model-evidence evaluation.
	if policy.CurrentAction != "ask" {
		return OutcomeKeepAsk
	}
	// Row 5: current rules ask with non-eligible constraints → keep Ask.
	if policy.HasExplicitAsk || policy.HasFreshRequirement ||
		!policy.PermissionAuto || !policy.GuardianEnabled || policy.DisableAutoReview {
		return OutcomeKeepAsk
	}
	if policy.Candidate == nil || policy.Candidate.Validate() != nil ||
		!CheckEligibility(policy.Candidate.Facts).Eligible {
		return OutcomeKeepAsk
	}
	if evidence != nil && evidence.candidateDigest != policy.Candidate.Digest() {
		return OutcomeDiscard
	}
	// Row 6: model unavailable, invalid, or incomplete → Ask.
	// (The caller passes a nil assessment in this case.)
	if evidence == nil || evidence.candidateDigest == "" {
		return OutcomeKeepAsk
	}
	assessment := &evidence.assessment
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
