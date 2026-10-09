package policy

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestFullAccessVerificationPreservesExplicitRestrictions(t *testing.T) {
	call := planningInvocation("exec_command", tool.CapabilityProcess, []tool.Resource{{Kind: "process", ID: "workspace", Access: tool.AccessRead}})
	call.Declared = securitymodel.Declared{Verification: true, FullAccess: true}
	r := DefaultRuntime(ModeAct, PermissionBypass)
	r.ConfigurePlanning(PlanningRequired)
	if got := r.Decide(resolveFixture(call)); got.Action != ActionAllow {
		t.Fatalf("verification: %+v", got)
	}
	for _, action := range []Action{ActionDeny, ActionAsk, ActionHold} {
		r.User = []Rule{{Tool: "*", Resource: "/outside/protected", Action: action}}
		got := r.Decide(resolveFixture(call))
		if action == ActionAsk && got.Action != ActionAsk || action != ActionAsk && got.Action != ActionDeny {
			t.Fatalf("restriction %s: %+v", action, got)
		}
	}
	r.User = nil
	r.Permission = PermissionNever
	if got := r.Decide(resolveFixture(call)); got.Action == ActionAllow {
		t.Fatal("Read only accepted full access")
	}
}

func TestFullAccessDoesNotExpandScopedManagedGrant(t *testing.T) {
	call := planningInvocation("exec_command", tool.CapabilityProcess, []tool.Resource{{Kind: "process", ID: "workspace", Access: tool.AccessRead}})
	call.Declared.FullAccess = true
	r := DefaultRuntime(ModeAct, PermissionBypass)
	r.Grants = []Rule{{Tool: "*", Resource: "workspace", Action: ActionAllow}}
	if got := r.Decide(resolveFixture(call)); got.Code != "tool_grant_missing" {
		t.Fatalf("scoped managed grant widened to Full Access: %+v", got)
	}
	call.Declared.FullAccess = false
	if got := r.Decide(resolveFixture(call)); got.Action != ActionAllow {
		t.Fatalf("scoped process lost its original grant: %+v", got)
	}
}
