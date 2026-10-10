package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/fwtllh-png/QCode/internal/security/guardian"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

// GuardianInput is supplied by the trusted Guard for this invocation only.
// It is evidence, never an approval-store grant.
type GuardianInput struct {
	Enabled   bool
	Candidate guardian.ReviewCandidate
	Evidence  *guardian.ReviewEvidence
}

func (r *Runtime) guardianEligible(i Invocation, d Decision) bool {
	if !i.GuardianRegistered || i.Source != securitymodel.SourceBuiltin || i.Stage != StageCall ||
		r.Permission != PermissionAuto || r.DisableAutoReview || d.Action != ActionAsk ||
		d.Layer != LayerPosture || d.Approval != ApprovalReusable {
		return false
	}
	// Judge the hypothetical Allow against surface restrictions too: a surface
	// Ask may otherwise be invisible while the posture already says Ask.
	if ApplySurfaceTightening(Decision{Action: ActionAllow}, ClassifySurface(i.Source, i.Capability()), r.Granular, i.Assessment.Effect()).Action != ActionAllow {
		return false
	}
	f, e := i.Assessment.Facets(), i.Assessment.Effect()
	return guardian.CheckEligibility(guardian.CandidateFacts{
		BuiltinBinding: true, RegisteredForReview: true, Capability: string(i.Capability()), Stage: "call",
		SandboxRequired: f.StrongSandbox, HostExecution: f.HostExecution,
		NetworkAccess: f.Network || f.LoopbackReach || f.HostLocalTarget,
		EffectRule:    i.Assessment.Rule(), EffectKind: string(e.Kind), EffectRisk: string(e.Risk),
		EffectReversibility: string(e.Reversibility), EvidenceComplete: true,
	}).Eligible
}

func (r *Runtime) applyGuardian(i Invocation, d Decision) Decision {
	d.GuardianEligible = r.guardianEligible(i, d)
	input := i.Guardian
	if !d.GuardianEligible || input == nil {
		return d
	}
	c := input.Candidate
	sum := sha256.Sum256(i.Arguments)
	resources, _ := json.Marshal(i.Assessment.Resources())
	resourceSum := sha256.Sum256(resources)
	if c.Identity.CallID != i.CallID || c.Binding.Tool != i.Tool ||
		c.Binding.ArgumentsDigest != hex.EncodeToString(sum[:]) ||
		c.Execution.AssessmentID != i.Assessment.Digest() || c.Execution.ResourcesDigest != hex.EncodeToString(resourceSum[:]) ||
		c.Versions.PolicyRevision != r.Revision || c.Versions.Permission != string(r.Permission) {
		return d
	}
	if guardian.Evaluate(input.Evidence, guardian.EvidenceInvalidation{}, guardian.PolicyContext{
		Candidate: &c, GuardianEnabled: input.Enabled, CurrentAction: string(d.Action),
		PermissionAuto: true,
	}) == guardian.OutcomeAllow {
		return Decision{Action: ActionAllow, Code: "guardian_allowed", Reason: "bounded operation has current Guardian evidence", Layer: LayerAutoReview}
	}
	return d
}
