package policy

import (
	"encoding/json"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestAssessEffectAndRisk(t *testing.T) {
	tests := []struct {
		name string
		call invocationFixture
		kind securitymodel.EffectKind
		risk securitymodel.Risk
	}{
		{
			name: "journaled workspace edit",
			call: effectInvocation("file_apply", CapabilityWrite, tool.AccessTree, tool.SandboxNone,
				tool.Resource{Kind: "file", Path: "a.go", Access: tool.AccessWrite}),
			kind: securitymodel.WorkspaceEdit, risk: securitymodel.RiskLow,
		},
		{
			name: "strong sandbox read-only process",
			call: effectInvocation("run_command", CapabilityProcess, tool.AccessTree, tool.SandboxStrong,
				tool.Resource{Kind: "process", ID: "workspace", Access: tool.AccessRead}),
			kind: securitymodel.ProcessReadOnly, risk: securitymodel.RiskLow,
		},
		{
			name: "declared process write",
			call: effectInvocation("run_command", CapabilityProcess, tool.AccessRead, tool.SandboxStrong,
				tool.Resource{Kind: "file", Path: "a.go", Access: tool.AccessWrite}),
			kind: securitymodel.ProcessMutating, risk: securitymodel.RiskHigh,
		},
		{
			name: "agent message",
			call: effectInvocation("send_message", CapabilityWrite, tool.AccessWrite, tool.SandboxNone,
				tool.Resource{Kind: "agent", ID: "agent-1", Access: tool.AccessWrite}),
			kind: securitymodel.AgentMessage, risk: securitymodel.RiskLow,
		},
		{
			name: "session plan mutation",
			call: effectInvocation(
				"update_plan",
				CapabilityWrite,
				tool.AccessWrite,
				tool.SandboxNone,
				tool.Resource{
					Kind: "plan", ID: "session", Access: tool.AccessWrite,
				},
			),
			kind: securitymodel.SessionMutation, risk: securitymodel.RiskLow,
		},
		{
			name: "agent followup",
			call: effectInvocation("followup_task", CapabilityWrite, tool.AccessWrite, tool.SandboxNone,
				tool.Resource{Kind: "agent", ID: "agent-1", Access: tool.AccessWrite}),
			kind: securitymodel.AgentLifecycle, risk: securitymodel.RiskMedium,
		},
		{
			name: "network read",
			call: effectInvocation("web_fetch", CapabilityNetwork, tool.AccessRead, tool.SandboxNone,
				tool.Resource{Kind: "host", ID: "example.com", Access: tool.AccessRead}),
			kind: securitymodel.NetworkRead, risk: securitymodel.RiskMedium,
		},
		{
			name: "process with method-unbounded network target",
			call: effectInvocation("run_command", CapabilityProcess, tool.AccessRead, tool.SandboxStrong,
				tool.Resource{Kind: "process", ID: "workspace", Access: tool.AccessRead},
				tool.Resource{Kind: "host", ID: "example.com", Access: tool.AccessWrite}),
			kind: securitymodel.NetworkMutating, risk: securitymodel.RiskHigh,
		},
		{
			name: "process with https CONNECT tunnel",
			call: effectInvocation("run_command", CapabilityProcess, tool.AccessRead, tool.SandboxStrong,
				tool.Resource{Kind: "process", ID: "workspace", Access: tool.AccessRead},
				tool.Resource{
					Kind: "host", ID: "example.com", Access: tool.AccessWrite,
					Protocol: "https", Port: 443, Methods: []string{"CONNECT"},
				}),
			kind: securitymodel.NetworkMutating, risk: securitymodel.RiskHigh,
		},
		{
			name: "runtime-discovered CONNECT",
			call: effectInvocation("run_command", CapabilityProcess, tool.AccessRead, tool.SandboxStrong,
				tool.Resource{
					Kind: "host", ID: "example.com", Access: tool.AccessRead,
					Protocol: "https", Port: 443, Methods: []string{"CONNECT"},
				},
				tool.Resource{
					Kind: "url", ID: "https://example.com:443/", Access: tool.AccessRead,
					Methods: []string{"CONNECT"},
				}),
			kind: securitymodel.NetworkMutating, risk: securitymodel.RiskHigh,
		},
		{
			name: "process with read-only plaintext target",
			call: effectInvocation("run_command", CapabilityProcess, tool.AccessRead, tool.SandboxStrong,
				tool.Resource{Kind: "process", ID: "workspace", Access: tool.AccessRead},
				tool.Resource{
					Kind: "host", ID: "example.com", Access: tool.AccessWrite,
					Protocol: "http", Port: 80, Methods: []string{"GET", "HEAD"},
				}),
			kind: securitymodel.NetworkRead, risk: securitymodel.RiskMedium,
		},
		{
			name: "process with plaintext POST target",
			call: effectInvocation("run_command", CapabilityProcess, tool.AccessRead, tool.SandboxStrong,
				tool.Resource{
					Kind: "host", ID: "example.com", Access: tool.AccessWrite,
					Protocol: "http", Port: 80, Methods: []string{"GET", "POST"},
				}),
			kind: securitymodel.NetworkMutating, risk: securitymodel.RiskHigh,
		},
		{
			name: "process with network and file mutation",
			call: effectInvocation("run_command", CapabilityProcess, tool.AccessRead, tool.SandboxStrong,
				tool.Resource{Kind: "process", ID: "workspace", Access: tool.AccessRead},
				tool.Resource{Kind: "host", ID: "example.com", Access: tool.AccessWrite},
				tool.Resource{Kind: "file", Path: "result.json", Access: tool.AccessWrite}),
			kind: securitymodel.NetworkMutating, risk: securitymodel.RiskHigh,
		},
		{
			name: "external high",
			call: effectInvocation("external_call", CapabilityExternal, tool.AccessTree, tool.SandboxStrong),
			kind: securitymodel.ExternalMutation, risk: securitymodel.RiskHigh,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			eff := assessFixture(test.call).Effect()
			if eff.Kind != test.kind || eff.Risk != test.risk {
				t.Fatalf("effect = %+v, want %s/%s", eff, test.kind, test.risk)
			}
		})
	}
}

func TestEffectRiskDrivesApprovalWithoutToolNameExceptions(t *testing.T) {
	tests := []struct {
		name       string
		permission Permission
		call       invocationFixture
		want       Action
	}{
		{
			name: "suggest edit allows", permission: PermissionSuggest,
			call: effectInvocation("file_edit", CapabilityWrite, tool.AccessWrite, tool.SandboxNone,
				tool.Resource{Kind: "file", Path: "a.go", Access: tool.AccessWrite}),
			want: ActionAllow,
		},
		{
			name: "suggest verify allows", permission: PermissionSuggest,
			call: effectInvocation("run_command", CapabilityProcess, tool.AccessTree, tool.SandboxStrong,
				tool.Resource{Kind: "process", ID: "workspace", Access: tool.AccessRead}),
			want: ActionAllow,
		},
		{
			name: "suggest message allows", permission: PermissionSuggest,
			call: effectInvocation("send_message", CapabilityWrite, tool.AccessWrite, tool.SandboxNone,
				tool.Resource{Kind: "agent", ID: "agent-1", Access: tool.AccessWrite}),
			want: ActionAllow,
		},
		{
			name: "suggest session plan allows", permission: PermissionSuggest,
			call: effectInvocation(
				"update_plan",
				CapabilityWrite,
				tool.AccessWrite,
				tool.SandboxNone,
				tool.Resource{
					Kind: "plan", ID: "session", Access: tool.AccessWrite,
				},
			),
			want: ActionAllow,
		},
		{
			name: "suggest followup auto reviews", permission: PermissionSuggest,
			call: effectInvocation("followup_task", CapabilityWrite, tool.AccessWrite, tool.SandboxNone,
				tool.Resource{Kind: "agent", ID: "agent-1", Access: tool.AccessWrite}),
			want: ActionAllow,
		},
		{
			name: "suggest network read asks", permission: PermissionSuggest,
			call: effectInvocation("web_fetch", CapabilityNetwork, tool.AccessRead, tool.SandboxNone,
				tool.Resource{Kind: "host", ID: "example.com", Access: tool.AccessRead}),
			want: ActionAsk,
		},
		{
			name: "auto network read auto reviews", permission: PermissionAuto,
			call: effectInvocation("web_fetch", CapabilityNetwork, tool.AccessRead, tool.SandboxNone,
				tool.Resource{Kind: "host", ID: "example.com", Access: tool.AccessRead}),
			want: ActionAllow,
		},
		{
			name: "auto process write asks", permission: PermissionAuto,
			call: effectInvocation("run_command", CapabilityProcess, tool.AccessRead, tool.SandboxStrong,
				tool.Resource{Kind: "file", Path: "a.go", Access: tool.AccessWrite}),
			want: ActionAsk,
		},
		{
			name: "never read-only process allows", permission: PermissionNever,
			call: effectInvocation("run_command", CapabilityProcess, tool.AccessRead, tool.SandboxStrong,
				tool.Resource{Kind: "process", ID: "workspace", Access: tool.AccessRead}),
			want: ActionAllow,
		},
		{
			name: "never process write denies", permission: PermissionNever,
			call: effectInvocation("run_command", CapabilityProcess, tool.AccessRead, tool.SandboxStrong,
				tool.Resource{Kind: "file", Path: "a.go", Access: tool.AccessWrite}),
			want: ActionDeny,
		},
		{
			name:       "auto strong loopback fixture asks",
			permission: PermissionAuto,
			call: effectInvocation(
				"run_command",
				CapabilityProcess,
				tool.AccessTree,
				tool.SandboxStrong,
				tool.Resource{
					Kind: "host", ID: "localhost", Access: tool.AccessWrite,
					Protocol: securitymodel.LoopbackProtocol, Methods: []string{"BIND", "CONNECT"},
					AllowPrivate: true,
				},
			),
			want: ActionAsk,
		},
		{
			name: "external bypass allows", permission: PermissionBypass,
			call: effectInvocation("external_call", CapabilityExternal, tool.AccessTree, tool.SandboxStrong),
			want: ActionAllow,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := DefaultRuntime(ModeAct, test.permission).Decide(resolveFixture(test.call))
			if decision.Action != test.want {
				t.Fatalf("decision = %+v, want %s", decision, test.want)
			}
		})
	}
}

func TestNeverPostureAllowsOnlyReadOnlyProcessEffects(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionNever)
	readOnly := effectInvocation(
		"run_command",
		CapabilityProcess,
		tool.AccessRead,
		tool.SandboxStrong,
		tool.Resource{Kind: "process", ID: "workspace", Access: tool.AccessRead},
	)
	if decision := runtime.Decide(resolveFixture(readOnly)); decision.Action != ActionAllow {
		t.Fatalf("read-only process decision = %+v, want allow", decision)
	}

	mutating := effectInvocation(
		"run_command",
		CapabilityProcess,
		tool.AccessRead,
		tool.SandboxStrong,
		tool.Resource{Kind: "file", Path: "a.go", Access: tool.AccessWrite},
	)
	decision := runtime.Decide(resolveFixture(mutating))
	if decision.Action != ActionDeny || decision.Code != "permission_denied" {
		t.Fatalf("mutating process decision = %+v, want permission_denied", decision)
	}
}

func TestAssessDeclaredReadOnlySpawnIsLowRisk(t *testing.T) {
	spawn := effectInvocation(
		"start_agent", CapabilityWrite, tool.AccessWrite, tool.SandboxNone,
	)
	spawn.Declared.ReadOnly = true
	got := assessFixture(spawn).Effect()
	if got.Kind != securitymodel.AgentLifecycle || got.Risk != securitymodel.RiskLow {
		t.Fatalf("read-only spawn effect = %+v", got)
	}
	spawn.Declared.ReadOnly = false
	got = assessFixture(spawn).Effect()
	if got.Kind != securitymodel.AgentLifecycle || got.Risk != securitymodel.RiskMedium {
		t.Fatalf("writing spawn effect = %+v", got)
	}
}

func TestBoundedAutoReview(t *testing.T) {
	network := effectInvocation(
		"web_fetch", CapabilityNetwork, tool.AccessRead, tool.SandboxNone,
		tool.Resource{Kind: "host", ID: "example.com", Access: tool.AccessRead},
	)
	agent := effectInvocation(
		"start_agent", CapabilityWrite, tool.AccessWrite, tool.SandboxNone,
		tool.Resource{Kind: "agent", ID: "agent-1", Access: tool.AccessWrite},
	)
	high := effectInvocation(
		"run_command", CapabilityProcess, tool.AccessWrite, tool.SandboxStrong,
		tool.Resource{Kind: "file", Path: "a.go", Access: tool.AccessWrite},
	)
	untyped := effectInvocation(
		"unknown_write", CapabilityWrite, "", "",
	)
	suggest := DefaultRuntime(ModeAct, PermissionSuggest)
	if reviewed := suggest.Decide(resolveFixture(network)); reviewed.Action != ActionAsk {
		t.Fatalf("suggest network review = %+v", reviewed)
	}
	if reviewed := suggest.Decide(resolveFixture(agent)); reviewed.Action != ActionAllow ||
		reviewed.Code != "auto_review_allowed" {
		t.Fatalf("suggest agent review = %+v", reviewed)
	}
	auto := DefaultRuntime(ModeAct, PermissionAuto)
	if reviewed := auto.Decide(resolveFixture(network)); reviewed.Action != ActionAllow ||
		reviewed.Code != "auto_review_allowed" {
		t.Fatalf("auto network review = %+v", reviewed)
	}
	runtime := DefaultRuntime(ModeAct, PermissionSuggest)
	if reviewed := runtime.Decide(resolveFixture(high)); reviewed.Action != ActionAsk {
		t.Fatalf("high risk review = %+v", reviewed)
	}
	if reviewed := runtime.Decide(resolveFixture(untyped)); reviewed.Action != ActionAsk {
		t.Fatalf("untyped review = %+v", reviewed)
	}
	runtime.Repository = []Rule{{Tool: "web_fetch", Action: ActionAsk}}
	if reviewed := runtime.Decide(resolveFixture(network)); reviewed.Action != ActionAsk {
		t.Fatalf("repository ask review = %+v", reviewed)
	}
	runtime.Repository = nil
	runtime.DisableAutoReview = true
	if reviewed := runtime.Decide(resolveFixture(network)); reviewed.Action != ActionAsk {
		t.Fatalf("kill switch review = %+v", reviewed)
	}
}

func TestAutoReviewNeverCoversProcessTunnels(t *testing.T) {
	auto := DefaultRuntime(ModeAct, PermissionAuto)
	tunnel := effectInvocation(
		"run_command", CapabilityProcess, tool.AccessRead, tool.SandboxStrong,
		tool.Resource{
			Kind: "host", ID: "uploads.example.com", Access: tool.AccessRead,
			Protocol: "https", Port: 443, Methods: []string{"CONNECT"},
			AllowPrivate: true,
		},
		tool.Resource{
			Kind: "url", ID: "https://uploads.example.com:443/", Access: tool.AccessRead,
			Methods: []string{"CONNECT"},
		},
	)
	if reviewed := auto.Decide(resolveFixture(tunnel)); reviewed.Action != ActionAsk {
		t.Fatalf("auto review of process CONNECT = %+v, want ask", reviewed)
	}
	read := effectInvocation(
		"run_command", CapabilityProcess, tool.AccessRead, tool.SandboxStrong,
		tool.Resource{
			Kind: "host", ID: "mirror.example.com", Access: tool.AccessRead,
			Protocol: "http", Port: 80, Methods: []string{"GET"},
		},
	)
	if reviewed := auto.Decide(resolveFixture(read)); reviewed.Action != ActionAllow ||
		reviewed.Code != "auto_review_allowed" {
		t.Fatalf("auto review of plaintext GET = %+v, want auto_review_allowed", reviewed)
	}
}

func TestAutoReviewNeverCoversHostLocalTargets(t *testing.T) {
	auto := DefaultRuntime(ModeAct, PermissionAuto)
	for _, resource := range []tool.Resource{
		{Kind: "url", ID: "http://127.0.0.1:6732/api/v1/bootstrap"},
		{Kind: "url", ID: "http://169.254.169.254/latest/meta-data/"},
		{Kind: "url", ID: "http://[::1]:8080/"},
		{Kind: "url", ID: "http://[::ffff:127.0.0.1]/"},
		{Kind: "url", ID: "http://0.0.0.0/"},
		{Kind: "url", ID: "http://localhost:3000/"},
		{Kind: "url", ID: "http://app.localhost/"},
		{Kind: "host", ID: "localhost."},
	} {
		resource.Access = tool.AccessRead
		call := effectInvocation(
			"web_fetch", CapabilityNetwork, tool.AccessRead, tool.SandboxNone,
			resource,
		)
		if reviewed := auto.Decide(resolveFixture(call)); reviewed.Action != ActionAsk {
			t.Fatalf("auto review of %s = %+v", resource.ID, reviewed)
		}
	}
	for _, resource := range []tool.Resource{
		{Kind: "url", ID: "https://docs.example.com/guide"},
		{Kind: "url", ID: "http://10.1.2.3/wiki"},
	} {
		resource.Access = tool.AccessRead
		call := effectInvocation(
			"web_fetch", CapabilityNetwork, tool.AccessRead, tool.SandboxNone,
			resource,
		)
		if reviewed := auto.Decide(resolveFixture(call)); reviewed.Action != ActionAllow ||
			reviewed.Code != "auto_review_allowed" {
			t.Fatalf("auto review of %s = %+v", resource.ID, reviewed)
		}
	}
}

func effectInvocation(
	name string,
	capability Capability,
	access tool.AccessMode,
	sandbox tool.SandboxRequirement,
	resources ...tool.Resource,
) invocationFixture {
	arguments := json.RawMessage(`{}`)
	if capability == tool.CapabilityProcess {
		arguments = json.RawMessage(`{"command":"fixture"}`)
	}
	contract := tool.EffectContract{
		Mode:                 tool.EffectDerived,
		WorkspaceTransaction: tool.TransactionNone,
		Approval:             tool.ApprovalPolicyDefault,
	}
	switch name {
	case "send_message":
		contract = tool.EffectContract{
			Mode: tool.EffectFixed, Kind: tool.EffectAgentMessage,
			Risk: tool.RiskLow, Reversibility: tool.Reversible,
			WorkspaceTransaction: tool.TransactionNone,
			Approval:             tool.ApprovalPolicyDefault,
		}
	case "start_agent", "followup_task":
		contract = tool.EffectContract{
			Mode: tool.EffectFixed, Kind: tool.EffectAgentLifecycle,
			Risk: tool.RiskMedium, Reversibility: tool.Bounded,
			WorkspaceTransaction: tool.TransactionNone,
			Approval:             tool.ApprovalPolicyDefault,
		}
	case "file_write", "file_edit", "file_apply", "file_patch":
		contract = tool.EffectContract{
			Mode: tool.EffectFixed, Kind: tool.EffectWorkspaceEdit,
			Risk: tool.RiskLow, Reversibility: tool.Reversible,
			WorkspaceTransaction:   tool.TransactionBeforeImage,
			RequireReadBeforeWrite: true,
			Approval:               tool.ApprovalPolicyDefault,
		}
	}
	return invocationFixture{
		CallID: name, Tool: name, Arguments: arguments,
		Resources: resources, Capability: capability,
		Access: access, Sandbox: sandbox,
		Effect:    contract,
		Journaled: contract.WorkspaceTransaction == tool.TransactionBeforeImage,
		Validated: true, Workspace: "/workspace",
	}
}
