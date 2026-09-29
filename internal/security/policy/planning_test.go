package policy

import (
	"encoding/json"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestRequiredPlanningGatesConsequentialEffects(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	runtime.ConfigurePlanning(PlanningRequired)
	write := planningInvocation("file_edit", tool.CapabilityWrite, []tool.Resource{{
		Kind: "file", Path: "parser.go", Access: tool.AccessWrite,
	}})
	if decision := runtime.Decide(resolveFixture(write)); decision.Code != "plan_required" {
		t.Fatalf("write decision = %+v", decision)
	}
	runtime.SubmitPlan()
	if decision := runtime.Decide(resolveFixture(write)); decision.Action != ActionAllow {
		t.Fatalf("submitted Plan decision = %+v", decision)
	}
}

func TestAdaptivePlanningUsesTrustedEffectInsteadOfFileCount(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	runtime.ConfigurePlanning(PlanningAdaptive)
	multiple := planningInvocation("file_edit", tool.CapabilityWrite, []tool.Resource{
		{Kind: "file", Path: "parser.go", Access: tool.AccessWrite},
		{Kind: "file", Path: "lexer.go", Access: tool.AccessWrite},
	})
	multiple.Effect = tool.EffectContract{
		Mode: tool.EffectFixed, Kind: tool.EffectWorkspaceEdit,
		Risk: tool.RiskLow, Reversibility: tool.Reversible,
		WorkspaceTransaction: tool.TransactionBeforeImage,
		Approval:             tool.ApprovalPolicyDefault,
	}
	if decision := runtime.Decide(resolveFixture(multiple)); decision.Action != ActionAllow {
		t.Fatalf("reversible multi-file edit decision = %+v", decision)
	}
	highRisk := multiple
	highRisk.Effect.Risk = tool.RiskHigh
	if decision := runtime.Decide(resolveFixture(highRisk)); decision.Code != "plan_required" {
		t.Fatalf("high-risk decision = %+v", decision)
	}
	irreversible := multiple
	irreversible.Effect.Reversibility = tool.Irreversible
	if decision := runtime.Decide(resolveFixture(irreversible)); decision.Code != "plan_required" {
		t.Fatalf("irreversible decision = %+v", decision)
	}
}

func TestPlanningStateIsResetBetweenTurns(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	runtime.ConfigurePlanning(PlanningRequired)
	runtime.SubmitPlan()
	runtime.ResetPlanState()
	write := planningInvocation("file_edit", tool.CapabilityWrite, []tool.Resource{{
		Kind: "file", Path: "parser.go", Access: tool.AccessWrite,
	}})
	if decision := runtime.Decide(resolveFixture(write)); decision.Code != "plan_required" {
		t.Fatalf("next turn decision = %+v", decision)
	}
}

func TestDeclaredVerificationDowngradesPlanGateToApproval(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	runtime.ConfigurePlanning(PlanningRequired)
	build := func(declared securitymodel.Declared) invocationFixture {
		invocation := planningInvocation("run_command", tool.CapabilityProcess, []tool.Resource{
			{Kind: "process", ID: "workspace", Access: tool.AccessRead, Tree: true},
			{Kind: "file", Path: "bin/app", Access: tool.AccessWrite},
		})
		invocation.Arguments = json.RawMessage(
			`{"command":"go build -o bin/app ./...","write_paths":["bin/app"]}`,
		)
		invocation.Declared = declared
		return invocation
	}
	verification := build(securitymodel.Declared{Verification: true})
	decision := runtime.Decide(resolveFixture(verification))
	if decision.Action != ActionAsk || decision.Code != "plan_verification" {
		t.Fatalf("declared verification decision = %+v", decision)
	}
	// Undeclared process commands still hit the hard plan gate.
	if decision := runtime.Decide(resolveFixture(build(securitymodel.Declared{}))); decision.Code != "plan_required" {
		t.Fatalf("undeclared process decision = %+v", decision)
	}
	runtime.SubmitPlan()
	if decision := runtime.Decide(resolveFixture(verification)); decision.Action == ActionAsk {
		t.Fatalf("submitted plan still asks: %+v", decision)
	}
}

func TestPlanningDoesNotExemptMutatingProcessesByToolName(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	runtime.ConfigurePlanning(PlanningRequired)
	for _, name := range []string{
		"run_command", "write_stdin", "fixture_host_process",
	} {
		invocation := planningInvocation(
			name,
			tool.CapabilityProcess,
			[]tool.Resource{{
				Kind: "host", ID: "localhost", Access: tool.AccessWrite,
			}},
		)
		if decision := runtime.Decide(resolveFixture(invocation)); decision.Code != "plan_required" {
			t.Fatalf("%s decision = %+v", name, decision)
		}
	}
}

func TestPlanningExemptEffectSkipsPlanGate(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	runtime.ConfigurePlanning(PlanningRequired)
	push := planningInvocation(
		"push_branch",
		tool.CapabilityExternal,
		[]tool.Resource{
			{Kind: "vcs", ID: ".", Access: tool.AccessWrite},
			{Kind: "vcs_remote", ID: "origin", Access: tool.AccessWrite},
			{Kind: "vcs_branch", ID: "main", Access: tool.AccessWrite},
		},
	)
	push.Effect = tool.EffectContract{
		Mode: tool.EffectFixed, Kind: tool.EffectExternalMutation,
		Risk: tool.RiskHigh, Reversibility: tool.Irreversible,
		WorkspaceTransaction: tool.TransactionNone,
		Approval:             tool.ApprovalPolicyOnce,
	}
	if decision := runtime.Decide(resolveFixture(push)); decision.Code != "plan_required" {
		t.Fatalf("undeclared exemption decision = %+v", decision)
	}
	push.Effect.Planning = tool.PlanningExempt
	if decision := runtime.Decide(resolveFixture(push)); decision.Code != "host_process_approval_required" ||
		decision.Layer != LayerBinding {
		t.Fatalf("planning-exempt decision = %+v, want the binding's one-time approval", decision)
	}
}

func TestAdaptivePlanningSkipsDeclaredReadOnlySpawn(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	runtime.ConfigurePlanning(PlanningAdaptive)
	spawn := readOnlySpawnInvocation()
	spawn.Declared.ReadOnly = true
	if decision := runtime.Decide(resolveFixture(spawn)); decision.Action != ActionAllow {
		t.Fatalf("read-only spawn decision = %+v", decision)
	}
	spawn.Declared.ReadOnly = false
	if decision := runtime.Decide(resolveFixture(spawn)); decision.Code != "plan_required" {
		t.Fatalf("writing spawn decision = %+v", decision)
	}
}

func TestDeclaredReadOnlySpawnSkipsApproval(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionSuggest)
	spawn := readOnlySpawnInvocation()
	spawn.Declared.ReadOnly = true
	if decision := runtime.Decide(resolveFixture(spawn)); decision.Action != ActionAllow {
		t.Fatalf("read-only spawn approval = %+v", decision)
	}
	spawn.Declared.ReadOnly = false
	if decision := runtime.Decide(resolveFixture(spawn)); decision.Action != ActionAsk {
		t.Fatalf("writing spawn approval = %+v", decision)
	}
}

func readOnlySpawnInvocation() invocationFixture {
	spawn := planningInvocation("start_agent", tool.CapabilityWrite, nil)
	spawn.Access = tool.AccessWrite
	spawn.Effect = tool.EffectContract{
		Mode: tool.EffectFixed, Kind: tool.EffectAgentLifecycle,
		Risk: tool.RiskMedium, Reversibility: tool.Bounded,
		Approval:     tool.ApprovalPolicyDefault,
		ReadOnlyWhen: &tool.ArgumentMatch{Field: "role", Values: []string{"review"}},
	}
	spawn.Arguments = []byte(`{"role":"review","task_name":"audit"}`)
	return spawn
}

func TestUnknownPlanningPolicyFailsClosed(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	runtime.ConfigurePlanning("unknown")
	write := planningInvocation("file_edit", tool.CapabilityWrite, []tool.Resource{{
		Kind: "file", Path: "parser.go", Access: tool.AccessWrite,
	}})
	if decision := runtime.Decide(resolveFixture(write)); decision.Code != "planning_policy_invalid" {
		t.Fatalf("unknown policy decision = %+v", decision)
	}
	if err := Validate(runtime); err == nil {
		t.Fatal("unknown planning policy passed validation")
	}
}

func planningInvocation(
	name string,
	capability tool.Capability,
	resources []tool.Resource,
) invocationFixture {
	return invocationFixture{
		CallID: "call-1", Tool: name, Capability: capability,
		Resources: resources, Access: tool.AccessWrite,
		Sandbox: tool.SandboxStrong, Journaled: true, Validated: true,
		Workspace: "/workspace",
	}
}
