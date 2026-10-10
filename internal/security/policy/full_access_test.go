package policy

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestFullAccessVerificationPreservesExplicitRestrictions(t *testing.T) {
	call := planningInvocation("exec_command", tool.CapabilityProcess, []tool.Resource{{Kind: "process", ID: "workspace", Access: tool.AccessRead}})
	call.Declared = securitymodel.Declared{FullAccess: true}
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

func TestFullAccessOnceBindingPreservesRestrictions(t *testing.T) {
	for _, test := range []struct {
		name     string
		setup    func(*Runtime)
		action   Action
		code     string
		layer    Layer
		approval ApprovalRequirement
	}{
		{name: "preauthorized", action: ActionAllow, layer: LayerPosture},
		{name: "managed ask", setup: func(r *Runtime) {
			r.Grants = []Rule{{Tool: "*", Action: ActionAsk}}
		}, action: ActionAsk, code: "approval_required", layer: LayerHard, approval: ApprovalFreshOnce},
		{name: "repository ask", setup: func(r *Runtime) {
			r.Repository = []Rule{{Tool: "*", Action: ActionAsk}}
		}, action: ActionAsk, code: "approval_required", layer: LayerRepository, approval: ApprovalFreshOnce},
		{name: "user ask", setup: func(r *Runtime) {
			r.User = []Rule{{Tool: "*", Action: ActionAsk}}
		}, action: ActionAsk, code: "approval_required", layer: LayerUser, approval: ApprovalFreshOnce},
		{name: "surface ask", setup: func(r *Runtime) {
			r.Granular.Rules = SurfaceAsk
		}, action: ActionAsk, code: "granular_ask", layer: LayerSurface, approval: ApprovalFreshOnce},
		{name: "constitution deny", setup: func(r *Runtime) {
			r.Constitution = []Rule{{Tool: "*", Action: ActionDeny}}
		}, action: ActionDeny, code: "constitution_denied", layer: LayerHard},
		{name: "missing managed grant", setup: func(r *Runtime) {
			r.Grants = nil
		}, action: ActionDeny, code: "tool_grant_missing", layer: LayerHard},
		{name: "managed deny", setup: func(r *Runtime) {
			r.Grants = []Rule{{Tool: "*", Action: ActionDeny}}
		}, action: ActionDeny, code: "tool_grant_denied", layer: LayerHard},
		{name: "repository deny", setup: func(r *Runtime) {
			r.Repository = []Rule{{Tool: "*", Action: ActionDeny}}
		}, action: ActionDeny, code: "repository_rule_denied", layer: LayerRepository},
		{name: "repository hold", setup: func(r *Runtime) {
			r.Repository = []Rule{{Tool: "*", Action: ActionHold}}
		}, action: ActionDeny, code: "repository_hold", layer: LayerRepository},
		{name: "user deny", setup: func(r *Runtime) {
			r.User = []Rule{{Tool: "*", Action: ActionDeny}}
		}, action: ActionDeny, code: "user_rule_denied", layer: LayerUser},
		{name: "user hold", setup: func(r *Runtime) {
			r.User = []Rule{{Tool: "*", Action: ActionHold}}
		}, action: ActionDeny, code: "user_rule_denied", layer: LayerUser},
		{name: "surface deny", setup: func(r *Runtime) {
			r.Granular.Rules = SurfaceDeny
		}, action: ActionDeny, code: "granular_deny", layer: LayerSurface},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := DefaultRuntime(ModeAct, PermissionBypass)
			if test.setup != nil {
				test.setup(r)
			}
			got := r.Decide(resolveFixture(onceCall()))
			if got.Action != test.action || got.Code != test.code || got.Layer != test.layer || got.Approval != test.approval {
				t.Fatalf("decision = %+v, want %s/%s/%s/%s", got, test.action, test.code, test.layer, test.approval)
			}
		})
	}
}

func TestFullAccessOnceBindingStillRequiresExplicitEditReview(t *testing.T) {
	call := writeCall("notes.txt")
	call.Effect.Approval = tool.ApprovalPolicyOnce
	r := DefaultRuntime(ModeAct, PermissionBypass)
	r.ForceEditPlanApproval = true
	if got := r.Decide(resolveFixture(call)); got.Action != ActionAsk || got.Code != "edit_plan_required" || got.Approval != ApprovalFresh {
		t.Fatalf("explicit edit review = %+v", got)
	}
}
