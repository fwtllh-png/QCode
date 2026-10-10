package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func guardianAuditFixture() GuardianReviewData {
	digest := strings.Repeat("a", 64)
	return GuardianReviewData{ReviewID: "review", CallID: "call", Tool: "exec_command", Phase: "assessed", ReasonCode: "assessment_complete",
		OriginalEffect: "process.mutating", OriginalRisk: "high", Eligibility: "bounded_process", Provider: "judge", Model: "model",
		ConfigurationDigest: digest, RouteDigest: digest, PromptVersion: "v1", SchemaVersion: "v1",
		Candidate: &GuardianCandidateRecord{AttemptID: "attempt", WorkspaceID: "workspace", WorkspaceGeneration: 1, CatalogID: "catalog", CatalogGeneration: 1,
			BindingRevision: 1, SubjectDigest: digest, ArgumentsDigest: digest, CandidateDigest: digest, AuthorizationDigest: digest, ExecutionDigest: digest, PolicyRevision: 1, Permission: "auto"},
		Assessment: &GuardianAssessmentRecord{Risk: "low", Authorization: "supported", Recommendation: "allow", SourceIDs: []string{"user"}},
	}
}

func TestGuardianAuditContractRejectsUnboundAuthority(t *testing.T) {
	for _, name := range []string{"valid", "missing_candidate", "bad_digest", "missing_source", "missing_policy", "missing_approval", "usage", "phase"} {
		t.Run(name, func(t *testing.T) {
			d := guardianAuditFixture()
			switch name {
			case "missing_candidate":
				d.Candidate = nil
			case "bad_digest":
				d.Candidate.ArgumentsDigest = "command text"
			case "missing_source":
				d.Assessment.SourceIDs = nil
			case "missing_policy":
				d.Decision = &GuardianDecisionRecord{Action: "allow", Layer: "auto_review", Authority: "guardian", Code: "guardian_allowed"}
			case "missing_approval":
				d.Decision = &GuardianDecisionRecord{Action: "ask", Layer: "default", Authority: "human", PolicyRevision: 1}
			case "usage":
				d.Usage = &GuardianUsageRecord{InputTokens: 1, CachedTokens: 2}
			case "phase":
				d.Phase = "executed"
			}
			if err := d.validate(); (err == nil) != (name == "valid") {
				t.Fatalf("validation=%v", err)
			}
		})
	}
	d := guardianAuditFixture()
	event, err := NewEvent(EventMeta{Sequence: 1, OperationID: "op", ThreadID: "thread", TurnID: "turn", ItemID: "item"}, &d)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var restored Event
	if err := json.Unmarshal(body, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Kind != EventGuardianReview || restored.Data.(*GuardianReviewData).Assessment.Authorization != "supported" {
		t.Fatal("audit round trip lost facts")
	}
	traits, _ := Traits(EventGuardianReview)
	if !traits.Durability.Persisted() || traits.Terminal || traits.Correlation != "call" {
		t.Fatal("audit traits do not retain call-scoped facts")
	}
}
