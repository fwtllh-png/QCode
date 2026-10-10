package guardian

import (
	"encoding/json"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"testing"
)

func validAssessment() Assessment {
	return Assessment{
		RiskLevel:              RiskLow,
		Authorization:          AuthSupported,
		AuthorizationSourceIDs: []string{"src-1"},
		Recommendation:         RecommendAllow,
		Rationale:              "routine operation",
	}
}

func validContext() PolicyContext {
	c := validCandidate()
	return PolicyContext{
		Candidate: &c, GuardianEnabled: true,
		CurrentAction:  "ask",
		PermissionAuto: true,
	}
}

func validEvidence() EvidenceInvalidation {
	return EvidenceInvalidation{}
}

func TestDecisionTableRow1Cancelled(t *testing.T) {
	inv := EvidenceInvalidation{Cancelled: true}
	a := validAssessment()
	if got := Evaluate(testEvidence(a), inv, validContext()); got != OutcomeDiscard {
		t.Fatalf("cancelled = %s, want discard", got)
	}
}

func TestDecisionTableRow2VersionChanged(t *testing.T) {
	for name, inv := range map[string]EvidenceInvalidation{
		"authorization": {AuthorizationChanged: true},
		"catalog":       {CatalogChanged: true},
		"content":       {ContentChanged: true},
		"surface":       {SurfaceTightened: true},
	} {
		a := validAssessment()
		if got := Evaluate(testEvidence(a), inv, validContext()); got != OutcomeDiscard {
			t.Fatalf("%s changed = %s, want discard", name, got)
		}
	}
}

func TestDecisionTableRow3PreserveDeny(t *testing.T) {
	a := validAssessment()
	ctx := validContext()
	ctx.CurrentAction = "deny"
	if got := Evaluate(testEvidence(a), validEvidence(), ctx); got != OutcomePreserveDeny {
		t.Fatalf("deny = %s, want preserve_deny", got)
	}
	ctx.CurrentAction = "hold"
	if got := Evaluate(testEvidence(a), validEvidence(), ctx); got != OutcomePreserveDeny {
		t.Fatalf("hold = %s, want preserve_deny", got)
	}
}

func TestDecisionTableRow4AlreadyAllowed(t *testing.T) {
	a := validAssessment()
	ctx := validContext()
	ctx.CurrentAction = "allow"
	if got := Evaluate(testEvidence(a), validEvidence(), ctx); got != OutcomeDiscard {
		t.Fatalf("allow = %s, want discard", got)
	}
}

func TestDecisionTableRow5ExplicitConstraints(t *testing.T) {
	for name, mutate := range map[string]func(*PolicyContext){
		"explicit_ask":      func(c *PolicyContext) { c.HasExplicitAsk = true },
		"fresh":             func(c *PolicyContext) { c.HasFreshRequirement = true },
		"not_auto":          func(c *PolicyContext) { c.PermissionAuto = false },
		"auto_disabled":     func(c *PolicyContext) { c.DisableAutoReview = true },
		"guardian_disabled": func(c *PolicyContext) { c.GuardianEnabled = false },
	} {
		ctx := validContext()
		mutate(&ctx)
		if got := Evaluate(testEvidence(validAssessment()), validEvidence(), ctx); got != OutcomeKeepAsk {
			t.Fatalf("%s = %s, want keep_ask", name, got)
		}
	}
}

func TestDecisionTableRow6ModelUnavailable(t *testing.T) {
	if got := Evaluate(nil, validEvidence(), validContext()); got != OutcomeKeepAsk {
		t.Fatalf("nil assessment = %s, want keep_ask", got)
	}
}

func TestDecisionTableRow7HighRiskOrUnknownAuthOrPrompt(t *testing.T) {
	for name, a := range map[string]Assessment{
		"high_risk":      {RiskLevel: RiskHigh, Authorization: AuthSupported, AuthorizationSourceIDs: []string{"s"}, Recommendation: RecommendAllow},
		"critical_risk":  {RiskLevel: RiskCritical, Authorization: AuthSupported, AuthorizationSourceIDs: []string{"s"}, Recommendation: RecommendAllow},
		"unknown_auth":   {RiskLevel: RiskLow, Authorization: AuthUnknown, Recommendation: RecommendAllow},
		"conflicting":    {RiskLevel: RiskLow, Authorization: AuthConflicting, Recommendation: RecommendAllow},
		"prompt_rec":     {RiskLevel: RiskLow, Authorization: AuthSupported, AuthorizationSourceIDs: []string{"s"}, Recommendation: RecommendPrompt},
		"high_and_allow": {RiskLevel: RiskHigh, Authorization: AuthSupported, AuthorizationSourceIDs: []string{"s"}, Recommendation: RecommendAllow},
	} {
		a.Rationale = "risk decision fixture"
		if a.Authorization == AuthSupported {
			a.AuthorizationSourceIDs = []string{"src-1"}
		}
		if got := Evaluate(testEvidence(a), validEvidence(), validContext()); got != OutcomeKeepAsk {
			t.Fatalf("%s = %s, want keep_ask", name, got)
		}
	}
}

func TestDecisionTableRow8Allow(t *testing.T) {
	for _, risk := range []RiskLevel{RiskLow, RiskMedium} {
		a := validAssessment()
		a.RiskLevel = risk
		if got := Evaluate(testEvidence(a), validEvidence(), validContext()); got != OutcomeAllow {
			t.Fatalf("risk=%s = %s, want allow", risk, got)
		}
	}
}

func TestParseAssessmentValid(t *testing.T) {
	data := []byte(`{
		"risk_level": "low",
		"authorization": "supported",
		"authorization_source_ids": ["src-1", "src-2"],
		"recommendation": "allow",
		"rationale": "safe operation"
	}`)
	sources := map[string]bool{"src-1": true, "src-2": true}
	a, err := ParseAssessment(data, sources)
	if err != nil {
		t.Fatalf("valid assessment failed: %v", err)
	}
	if a.RiskLevel != RiskLow || a.Authorization != AuthSupported ||
		a.Recommendation != RecommendAllow || a.Rationale != "safe operation" {
		t.Fatalf("parsed incorrectly: %+v", a)
	}
	if len(a.AuthorizationSourceIDs) != 2 {
		t.Fatalf("expected 2 source IDs, got %d", len(a.AuthorizationSourceIDs))
	}
}

func TestParseAssessmentRejectsUnknownField(t *testing.T) {
	data := []byte(`{"risk_level":"low","authorization":"unknown","recommendation":"prompt","extra_field":"x"}`)
	if _, err := ParseAssessment(data, nil); err == nil {
		t.Fatal("unknown field was accepted")
	}
}

func TestParseAssessmentRejectsMissingFields(t *testing.T) {
	for _, data := range []string{
		`{"authorization":"unknown","recommendation":"prompt"}`,
		`{"risk_level":"low","recommendation":"prompt"}`,
		`{"risk_level":"low","authorization":"unknown"}`,
	} {
		if _, err := ParseAssessment([]byte(data), nil); err == nil {
			t.Fatalf("missing field accepted: %s", data)
		}
	}
}

func TestParseAssessmentRejectsUnknownEnums(t *testing.T) {
	for _, data := range []string{
		`{"risk_level":"extreme","authorization":"unknown","recommendation":"prompt"}`,
		`{"risk_level":"low","authorization":"maybe","recommendation":"prompt"}`,
		`{"risk_level":"low","authorization":"unknown","recommendation":"deny"}`,
	} {
		if _, err := ParseAssessment([]byte(data), nil); err == nil {
			t.Fatalf("unknown enum accepted: %s", data)
		}
	}
}

func TestParseAssessmentSupportedRequiresSources(t *testing.T) {
	data := []byte(`{"risk_level":"low","authorization":"supported","recommendation":"allow","rationale":"r"}`)
	if _, err := ParseAssessment(data, nil); err == nil {
		t.Fatal("supported without sources was accepted")
	}
}

func TestParseAssessmentRejectsInvalidSourceReference(t *testing.T) {
	data := []byte(`{"risk_level":"low","authorization":"supported","authorization_source_ids":["revoked"],"recommendation":"allow"}`)
	valid := map[string]bool{"active": true}
	if _, err := ParseAssessment(data, valid); err == nil {
		t.Fatal("revoked source was accepted")
	}
}

func TestParseAssessmentNonSupportedMustNotCiteSources(t *testing.T) {
	data := []byte(`{"risk_level":"low","authorization":"unknown","authorization_source_ids":["src"],"recommendation":"prompt"}`)
	if _, err := ParseAssessment(data, nil); err == nil {
		t.Fatal("unknown auth with sources was accepted")
	}
}

func TestParseAssessmentRejectsNonObject(t *testing.T) {
	for _, data := range []string{
		`"just a string"`,
		`[1,2,3]`,
		`null`,
		``,
	} {
		if _, err := ParseAssessment([]byte(data), nil); err == nil {
			t.Fatalf("non-object accepted: %q", data)
		}
	}
}

// TestParseAssessmentRejectsDuplicateKeys verifies that a JSON object
// with the same key appearing twice is rejected, even if both values
// are individually valid.
func TestParseAssessmentRejectsDuplicateKeys(t *testing.T) {
	data := []byte(`{"risk_level":"critical","risk_level":"low","authorization":"unknown","recommendation":"prompt","rationale":"r"}`)
	if _, err := ParseAssessment(data, nil); err == nil {
		t.Fatal("duplicate risk_level accepted (critical then low would silently win)")
	}
	data = []byte(`{"risk_level":"low","authorization":"unknown","recommendation":"allow","recommendation":"prompt","rationale":"r"}`)
	if _, err := ParseAssessment(data, nil); err == nil {
		t.Fatal("duplicate recommendation accepted")
	}
}

// TestParseAssessmentRejectsTrailingContent verifies that extra JSON
// objects, plain text, or unclosed JSON after the primary object are
// all rejected.
func TestParseAssessmentRejectsTrailingContent(t *testing.T) {
	for _, data := range []string{
		`{"risk_level":"low","authorization":"unknown","recommendation":"prompt","rationale":"r"}{"risk_level":"high"}`,
		`{"risk_level":"low","authorization":"unknown","recommendation":"prompt","rationale":"r"} extra text`,
		`{"risk_level":"low","authorization":"unknown","recommendation":"prompt","rationale":"r"`,
	} {
		if _, err := ParseAssessment([]byte(data), nil); err == nil {
			t.Fatalf("trailing content accepted: %.40s...", data)
		}
	}
}

// TestParseAssessmentRejectsNullRationale verifies that a null or missing
// rationale is rejected.
func TestParseAssessmentRejectsNullRationale(t *testing.T) {
	data := []byte(`{"risk_level":"low","authorization":"unknown","recommendation":"prompt","rationale":null}`)
	if _, err := ParseAssessment(data, nil); err == nil {
		t.Fatal("null rationale accepted")
	}
	data = []byte(`{"risk_level":"low","authorization":"unknown","recommendation":"prompt"}`)
	if _, err := ParseAssessment(data, nil); err == nil {
		t.Fatal("missing rationale accepted")
	}
}

// TestParseAssessmentNilSourcesRejectsAllCitations verifies that a nil
// validSourceIDs map (no known sources) rejects every citation, not
// just skips validation.
func TestParseAssessmentNilSourcesRejectsAllCitations(t *testing.T) {
	data := []byte(`{"risk_level":"low","authorization":"supported","authorization_source_ids":["invented-user-source"],"recommendation":"allow","rationale":"r"}`)
	if _, err := ParseAssessment(data, nil); err == nil {
		t.Fatal("invented source accepted with nil validSourceIDs")
	}
}

// TestEvaluateRejectsUnknownCurrentAction verifies that an empty or
// unrecognized CurrentAction falls through to KeepAsk rather than
// potentially allowing.
func TestEvaluateRejectsUnknownCurrentAction(t *testing.T) {
	a := validAssessment()
	for _, action := range []string{"", "unknown", "ALLOW", "Ask", "allow "} {
		ctx := validContext()
		ctx.CurrentAction = action
		if got := Evaluate(testEvidence(a), validEvidence(), ctx); got != OutcomeKeepAsk {
			t.Fatalf("CurrentAction=%q produced %s, want keep_ask", action, got)
		}
	}
}

func TestEvidenceInvalidationInvalid(t *testing.T) {
	if (EvidenceInvalidation{}).Invalid() {
		t.Fatal("zero invalidation should be valid")
	}
	for _, inv := range []EvidenceInvalidation{
		{Cancelled: true},
		{AuthorizationChanged: true},
		{CatalogChanged: true},
		{ContentChanged: true},
		{SurfaceTightened: true},
	} {
		if !inv.Invalid() {
			t.Fatalf("invalidation %+v should be invalid", inv)
		}
	}
}

func TestCandidateDigestStable(t *testing.T) {
	c1, c2 := validCandidate(), validCandidate()
	if c1.Digest() != c2.Digest() {
		t.Fatal("identical candidates differ")
	}
	c2.Execution.Command = "echo bye"
	if c1.Digest() == c2.Digest() {
		t.Fatal("different commands share a digest")
	}
}

func testEvidence(a Assessment) *ReviewEvidence {
	data, _ := json.Marshal(a)
	evidence, _ := BindAssessment(validCandidate(), data)
	return evidence
}

// validCandidateFacts returns facts satisfying all §2.1 conditions.
func validCandidateFacts() CandidateFacts {
	return CandidateFacts{
		BuiltinBinding: true, RegisteredForReview: true, EffectRule: securitymodel.RuleProcessMutating,
		Capability:          "process",
		Stage:               "call",
		SandboxRequired:     true,
		HostExecution:       false,
		NetworkAccess:       false,
		MCPOrSkill:          false,
		EffectRisk:          "high",
		EffectKind:          "process.mutating",
		EffectReversibility: "bounded",
		EvidenceComplete:    true,
	}
}

func TestCheckEligibilityValid(t *testing.T) {
	result := CheckEligibility(validCandidateFacts())
	if !result.Eligible {
		t.Fatalf("valid candidate rejected: %s", result.Reason)
	}
}

func TestCheckEligibilityMatrix(t *testing.T) {
	for name, mutate := range map[string]func(*CandidateFacts){
		"no_strong_sandbox":     func(f *CandidateFacts) { f.SandboxRequired = false },
		"fixed":                 func(f *CandidateFacts) { f.EffectRule = securitymodel.RuleFixed },
		"missing_rule":          func(f *CandidateFacts) { f.EffectRule = "" },
		"wrong_kind":            func(f *CandidateFacts) { f.EffectKind = "external.mutation" },
		"empty_kind":            func(f *CandidateFacts) { f.EffectKind = "" },
		"empty_reversibility":   func(f *CandidateFacts) { f.EffectReversibility = "" },
		"unknown_reversibility": func(f *CandidateFacts) { f.EffectReversibility = "unknown" },
		"reversible":            func(f *CandidateFacts) { f.EffectReversibility = "reversible" },
		"empty_stage":           func(f *CandidateFacts) { f.Stage = "" },
		"unregistered":          func(f *CandidateFacts) { f.RegisteredForReview = false },
		"external_binding":      func(f *CandidateFacts) { f.BuiltinBinding = false },
		"permission_expansion":  func(f *CandidateFacts) { f.PermissionExpansion = true },
		"protected_paths":       func(f *CandidateFacts) { f.ProtectedPaths = true },
		"control_plane":         func(f *CandidateFacts) { f.ControlPlaneWrite = true },
		"not_process":           func(f *CandidateFacts) { f.Capability = "read" },
		"egress_stage":          func(f *CandidateFacts) { f.Stage = "egress_target" },
		"host_execution":        func(f *CandidateFacts) { f.HostExecution = true },
		"network_access":        func(f *CandidateFacts) { f.NetworkAccess = true },
		"mcp_or_skill":          func(f *CandidateFacts) { f.MCPOrSkill = true },
		"critical_risk":         func(f *CandidateFacts) { f.EffectRisk = "critical" },
		"low_risk":              func(f *CandidateFacts) { f.EffectRisk = "low" },
		"medium_risk":           func(f *CandidateFacts) { f.EffectRisk = "medium" },
		"irreversible":          func(f *CandidateFacts) { f.EffectReversibility = "irreversible" },
		"evidence_incomplete":   func(f *CandidateFacts) { f.EvidenceComplete = false },
	} {
		facts := validCandidateFacts()
		mutate(&facts)
		result := CheckEligibility(facts)
		if result.Eligible {
			t.Fatalf("%s: expected ineligible, got eligible", name)
		}
		if result.Reason == "" {
			t.Fatalf("%s: rejection reason must not be empty", name)
		}
	}
}

func TestOutcomeReason(t *testing.T) {
	a := validAssessment()
	if OutcomeAllow.Reason(&a) != "routine operation" {
		t.Fatalf("allow reason = %q, want rationale", OutcomeAllow.Reason(&a))
	}
	if OutcomeKeepAsk.Reason(nil) == "" {
		t.Fatal("keep_ask reason should not be empty for nil assessment")
	}
}
