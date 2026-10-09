package policy

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestHostExecutionApprovalPreservesPolicyRestrictions(t *testing.T) {
	call := planningInvocation("exec_command", tool.CapabilityProcess, []tool.Resource{{Kind: "process", ID: "workspace", Access: tool.AccessRead}})
	call.Declared = securitymodel.Declared{Verification: true, HostExecution: true}
	for _, tc := range []struct {
		name  string
		setup func(*Runtime)
		want  Action
	}{
		{"auto", func(*Runtime) {}, ActionAsk},
		{"full access", func(r *Runtime) { r.SetPermission(PermissionBypass) }, ActionAllow},
		{"delegated", func(r *Runtime) { r.DisableHostExecution = true }, ActionDeny},
		{"read only", func(r *Runtime) { r.SetPermission(PermissionNever) }, ActionDeny},
		{"constitution", func(r *Runtime) { r.Constitution = []Rule{{Tool: "*", Action: ActionDeny}} }, ActionDeny},
		{"scoped managed allow", func(r *Runtime) { r.Grants = []Rule{{Tool: "*", Resource: "workspace", Action: ActionAllow}} }, ActionDeny},
		{"repository ask", func(r *Runtime) { r.Repository = []Rule{{Tool: "*", Resource: "/protected", Action: ActionAsk}} }, ActionAsk},
		{"user deny", func(r *Runtime) { r.User = []Rule{{Tool: "*", Resource: "/protected", Action: ActionDeny}} }, ActionDeny},
		{"surface ask", func(r *Runtime) { r.Granular.Sandbox = SurfaceAsk }, ActionAsk},
		{"user allow", func(r *Runtime) { r.User = []Rule{{Tool: "*", Action: ActionAllow}} }, ActionAsk},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := DefaultRuntime(ModeAct, PermissionAuto)
			r.ConfigurePlanning(PlanningRequired)
			tc.setup(r)
			got := r.Decide(resolveFixture(call))
			if got.Action != tc.want {
				t.Fatalf("host decision: %+v", got)
			}
			if got.Action == ActionAsk && got.Approval != ApprovalFreshOnce {
				t.Fatalf("host approval can be reused: %+v", got)
			}
		})
	}
}
