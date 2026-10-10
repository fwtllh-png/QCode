package policy

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestGuardianEligibilityCannotOverrideExplicitPolicy(t *testing.T) {
	for _, kind := range []string{"eligible", "unregistered", "readonly", "bypass", "disabled", "managed_ask", "user_ask", "repository_ask", "surface_ask", "deny", "fresh_once", "egress"} {
		t.Run(kind, func(t *testing.T) {
			r := DefaultRuntime(ModeAct, PermissionAuto)
			fixture := shellCommand("printf result > output.txt")
			fixture.Resources = []tool.Resource{{Kind: "file", Path: "/workspace/output.txt", Access: tool.AccessWrite}}
			if kind == "fresh_once" {
				fixture.Effect.Approval = securitymodel.ApprovalOnce
			}
			i := resolveFixture(fixture)
			i.GuardianRegistered = true
			switch kind {
			case "unregistered":
				i.GuardianRegistered = false
			case "readonly":
				r.SetPermission(PermissionNever)
			case "bypass":
				r.SetPermission(PermissionBypass)
			case "disabled":
				r.SetDisableAutoReview(true)
			case "managed_ask":
				r.Grants = []Rule{{Tool: i.Tool, Action: ActionAsk}}
			case "user_ask":
				r.User = []Rule{{Tool: i.Tool, Action: ActionAsk}}
			case "repository_ask":
				r.Repository = []Rule{{Tool: i.Tool, Action: ActionAsk}}
			case "surface_ask":
				r.SetGranular(Granular{Sandbox: SurfaceAsk})
			case "deny":
				r.User = []Rule{{Tool: i.Tool, Action: ActionDeny}}
			case "egress":
				i.Stage = StageEgress
			}
			d := r.Decide(i)
			if d.GuardianEligible != (kind == "eligible") {
				t.Fatalf("eligibility=%t decision=%+v rule=%s", d.GuardianEligible, d, i.Assessment.Rule())
			}
			if d.Code == "guardian_allowed" {
				t.Fatal("eligibility alone authorized execution")
			}
		})
	}
}
