package policy

import (
	"testing"
	"time"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestPolicyConsumersRejectMissingAssessment(t *testing.T) {
	call := resolveFixture(readCall())
	call.Assessment = securitymodel.Assessment{}
	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	if got := runtime.Decide(call); got.Layer != LayerInput || got.Code != "policy_unassessed_invocation" {
		t.Fatalf("unassessed input was not rejected: %+v", got)
	}
	if _, ok := GrantForInvocation(call); ok {
		t.Fatal("unassessed invocation obtained a grant")
	}
	if _, err := NewApprovalRequest(call, time.Time{}); err == nil {
		t.Fatal("unassessed invocation obtained an approval request")
	}
}
