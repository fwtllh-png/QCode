package guardian

import (
	"encoding/json"
	"testing"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func validCandidate() ReviewCandidate {
	d := digestValue("fixture")
	return ReviewCandidate{
		Identity: InvocationIdentity{ReviewID: "review", WorkspaceID: "workspace", WorkspaceGeneration: 1, SessionID: "session", ThreadID: "thread", TurnID: "turn", CallID: "call", AttemptID: "attempt"},
		Binding: BindingIdentity{Tool: "exec_command", CatalogID: "catalog", CatalogGeneration: 1, Revision: 1, Authority: 1, ArgumentsDigest: d,
			Subject: securitymodel.Subject{Kind: securitymodel.SubjectBuiltin, Trust: securitymodel.TrustBuiltin, ID: "shell", Digest: d, Generation: 1}},
		Facts: validCandidateFacts(),
		Execution: ExecutionSnapshot{ID: "snapshot", Root: "/private/copy", RootIdentity: "root-inode", WorkingDir: "/private/copy", WorkingDirIdentity: "cwd-inode", Command: "./build.sh", EnvironmentDigest: d, ResourcesDigest: d, AssessmentID: d, SandboxPolicyID: "sandbox", Settlement: "apply", Controls: securitymodel.RequiredControls{Network: securitymodel.NetworkDenied}, CoverageComplete: true,
			Content: []ContentEvidence{{Path: "build.sh", Identity: "inode", Digest: d, Mode: 0700, Size: 3}}},
		Authorization: AuthorizationSnapshot{WorkspaceID: "workspace", SessionID: "session", ThreadID: "thread", Revision: d, Complete: true,
			Sources: []AuthorizationSource{{ID: "src-1", ThreadID: "thread", TurnID: "turn", Role: "user", Version: 1, Digest: d}}},
		Versions: ReviewVersions{PolicyRevision: 1, Permission: "auto", ConfigurationDigest: d, RouteDigest: d, PromptVersion: "v2", SchemaVersion: "v1"},
	}
}

func TestEvidenceBindingRejectsDrift(t *testing.T) {
	a := validAssessment()
	e := testEvidence(a)
	if e == nil {
		t.Fatal("fixture evidence is invalid")
	}
	for name, mutate := range map[string]func(*ReviewCandidate){
		"workspace":   func(c *ReviewCandidate) { c.Identity.WorkspaceGeneration++ },
		"call":        func(c *ReviewCandidate) { c.Identity.CallID = "other" },
		"turn":        func(c *ReviewCandidate) { c.Identity.TurnID = "other" },
		"attempt":     func(c *ReviewCandidate) { c.Identity.AttemptID = "other" },
		"binding":     func(c *ReviewCandidate) { c.Binding.Authority++ },
		"parameters":  func(c *ReviewCandidate) { c.Binding.ArgumentsDigest = digestValue("changed") },
		"policy":      func(c *ReviewCandidate) { c.Versions.PolicyRevision++ },
		"config":      func(c *ReviewCandidate) { c.Versions.ConfigurationDigest = digestValue("changed") },
		"route":       func(c *ReviewCandidate) { c.Versions.RouteDigest = digestValue("changed") },
		"prompt":      func(c *ReviewCandidate) { c.Versions.PromptVersion = "v3" },
		"schema":      func(c *ReviewCandidate) { c.Versions.SchemaVersion = "v2" },
		"source":      func(c *ReviewCandidate) { c.Authorization.Sources[0].Digest = digestValue("changed") },
		"revocation":  func(c *ReviewCandidate) { c.Authorization.Sources[0].Revoked = true },
		"parent":      func(c *ReviewCandidate) { c.Authorization.ParentDigest = digestValue("parent tightened") },
		"copy":        func(c *ReviewCandidate) { c.Execution.ID = "new-copy" },
		"cwd":         func(c *ReviewCandidate) { c.Execution.WorkingDirIdentity = "replacement" },
		"environment": func(c *ReviewCandidate) { c.Execution.EnvironmentDigest = digestValue("changed") },
		"resources":   func(c *ReviewCandidate) { c.Execution.ResourcesDigest = digestValue("changed") },
		"write_scope": func(c *ReviewCandidate) { c.Execution.WritePaths = []string{"new-path"} },
		"script":      func(c *ReviewCandidate) { c.Execution.Content[0].Digest = digestValue("changed") },
		"sandbox":     func(c *ReviewCandidate) { c.Execution.SandboxPolicyID = "new-policy" },
	} {
		t.Run(name, func(t *testing.T) {
			c := validCandidate()
			mutate(&c)
			ctx := validContext()
			ctx.Candidate = &c
			if got := Evaluate(e, EvidenceInvalidation{}, ctx); got != OutcomeDiscard {
				t.Fatalf("stale evidence: %s", got)
			}
		})
	}
}

func TestEvidenceRequiresCompleteCandidateAndValidSources(t *testing.T) {
	data, _ := json.Marshal(validAssessment())
	for name, mutate := range map[string]func(*ReviewCandidate){
		"empty":              func(c *ReviewCandidate) { *c = ReviewCandidate{} },
		"coverage":           func(c *ReviewCandidate) { c.Execution.CoverageComplete = false },
		"missing_dependency": func(c *ReviewCandidate) { c.Execution.Missing = []string{"dynamic import"} },
		"write_traversal":    func(c *ReviewCandidate) { c.Execution.WritePaths = []string{"../escape"} },
		"write_absolute":     func(c *ReviewCandidate) { c.Execution.WritePaths = []string{"/private/outside"} },
		"role":               func(c *ReviewCandidate) { c.Authorization.Sources[0].Role = "assistant" },
		"revoked":            func(c *ReviewCandidate) { c.Authorization.Sources[0].Revoked = true },
		"foreign_thread":     func(c *ReviewCandidate) { c.Authorization.Sources[0].ThreadID = "other" },
		"host":               func(c *ReviewCandidate) { c.Facts.HostExecution = true },
		"fixed":              func(c *ReviewCandidate) { c.Facts.EffectRule = securitymodel.RuleFixed },
	} {
		t.Run(name, func(t *testing.T) {
			c := validCandidate()
			mutate(&c)
			if _, err := BindAssessment(c, data); err == nil {
				t.Fatal("invalid candidate bound")
			}
		})
	}
	ctx := validContext()
	if Evaluate(&ReviewEvidence{}, EvidenceInvalidation{}, ctx) == OutcomeAllow {
		t.Fatal("zero evidence allowed")
	}
	e := testEvidence(validAssessment())
	a := e.Assessment()
	a.AuthorizationSourceIDs[0] = "forged"
	if e.Assessment().AuthorizationSourceIDs[0] != "src-1" {
		t.Fatal("evidence is mutable through accessor")
	}
}

func TestCandidateEncodingPreservesFieldBoundaries(t *testing.T) {
	a, b := validCandidate(), validCandidate()
	a.Identity.CallID, a.Identity.AttemptID = "call\x00attempt", "x"
	b.Identity.CallID, b.Identity.AttemptID = "call", "attempt\x00x"
	if a.Digest() == b.Digest() {
		t.Fatal("field boundaries collided")
	}
}
