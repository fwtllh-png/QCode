package policy

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

type layerCase struct {
	name   string
	setup  func(*Runtime)
	call   func() invocationFixture
	action Action
	code   string
	layer  Layer
}

func runLayerCases(t *testing.T, cases []layerCase) {
	t.Helper()
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			runtime := DefaultRuntime(ModeAct, PermissionBypass)
			if test.setup != nil {
				test.setup(runtime)
			}
			decision := runtime.Decide(resolveFixture(test.call()))
			if decision.Action != test.action || decision.Code != test.code ||
				decision.Layer != test.layer {
				t.Fatalf("Decide() = %+v, want action=%s code=%q layer=%s",
					decision, test.action, test.code, test.layer)
			}
		})
	}
}

func readCall() invocationFixture {
	return invocation("file_read", "read-1", `{}`)
}

func writeCall(path string) invocationFixture {
	return invocation("file_write", "write-1", `{"path":"`+path+`"}`)
}

func networkReadCall(host string) invocationFixture {
	call := invocation("fetch_page", "net-1", `{}`)
	call.Capability, call.Access, call.Sandbox = CapabilityNetwork, tool.AccessRead, tool.SandboxNone
	call.Resources = []tool.Resource{{Kind: "url", ID: "https://" + host + "/", Access: tool.AccessRead}}
	return call
}

func TestDecideInputLayer(t *testing.T) {
	runLayerCases(t, []layerCase{
		{
			name: "valid input passes to later layers", call: readCall,
			action: ActionAllow, layer: LayerPosture,
		},
		{
			name: "missing call id",
			call: func() invocationFixture {
				call := readCall()
				call.CallID = ""
				return call
			},
			action: ActionDeny, code: "policy_invalid_invocation", layer: LayerInput,
		},
		{
			name: "unvalidated",
			call: func() invocationFixture {
				call := readCall()
				call.Validated = false
				return call
			},
			action: ActionDeny, code: "policy_unvalidated_invocation", layer: LayerInput,
		},
		{
			name: "unknown capability",
			call: func() invocationFixture {
				call := readCall()
				call.Capability = ""
				return call
			},
			action: ActionDeny, code: "policy_unknown_capability", layer: LayerInput,
		},
		{
			name: "unknown stage",
			call: func() invocationFixture {
				call := readCall()
				call.Stage = "later"
				return call
			},
			action: ActionDeny, code: "policy_invalid_invocation", layer: LayerInput,
		},
		{
			name: "unknown binding approval",
			call: func() invocationFixture {
				call := readCall()
				call.Effect.Approval = "sometimes"
				return call
			},
			action: ActionDeny, code: "policy_invalid_invocation", layer: LayerInput,
		},
		{
			name: "path write without workspace root",
			call: func() invocationFixture {
				call := writeCall("notes.txt")
				call.Workspace = ""
				return call
			},
			action: ActionDeny, code: "policy_invalid_invocation", layer: LayerInput,
		},
		{
			name: "input outranks hard constraints",
			call: func() invocationFixture {
				call := writeCall(".qcode/permissions.toml")
				call.Validated = false
				return call
			},
			action: ActionDeny, code: "policy_unvalidated_invocation", layer: LayerInput,
		},
	})
}

func TestDecideHardConstraintLayer(t *testing.T) {
	runLayerCases(t, []layerCase{
		{
			name: "control-plane read is not a write", call: func() invocationFixture {
				call := readCall()
				call.Resources = []tool.Resource{{Kind: "file", Path: ".qcode/permissions.toml", Access: tool.AccessRead}}
				return call
			},
			action: ActionAllow, layer: LayerPosture,
		},
		{
			name: "control-plane write", call: func() invocationFixture { return writeCall(".qcode/permissions.toml") },
			action: ActionDeny, code: "control_plane_protected", layer: LayerHard,
		},
		{
			name: "unbounded workspace tree write", call: func() invocationFixture {
				call := writeCall("")
				call.Resources = []tool.Resource{{Kind: "workspace", Path: "/workspace", Access: tool.AccessWrite}}
				return call
			},
			action: ActionDeny, code: "control_plane_protected", layer: LayerHard,
		},
		{
			name: "write outside workspace", call: func() invocationFixture { return writeCall("/elsewhere/a.txt") },
			action: ActionDeny, code: "control_plane_protected", layer: LayerHard,
		},
		{
			name: "constitution hold",
			setup: func(r *Runtime) {
				r.Constitution = []Rule{{Tool: "*", Resource: "secrets", RequireWrite: true, Action: ActionHold, Code: "constitution_hold:repo"}}
			},
			call:   func() invocationFixture { return writeCall("secrets/token") },
			action: ActionDeny, code: "constitution_hold:repo", layer: LayerHard,
		},
		{
			name:   "managed grant missing",
			setup:  func(r *Runtime) { r.Grants = []Rule{{Tool: "file_read", Action: ActionAllow}} },
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionDeny, code: "tool_grant_missing", layer: LayerHard,
		},
		{
			name: "managed deny",
			setup: func(r *Runtime) {
				r.Grants = []Rule{{Tool: "*", Action: ActionAllow}, {Tool: "file_write", Action: ActionDeny}}
			},
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionDeny, code: "tool_grant_denied", layer: LayerHard,
		},
		{
			name: "managed ask is attributed to the hard layer",
			setup: func(r *Runtime) {
				r.Grants = []Rule{{Tool: "*", Action: ActionAsk}}
			},
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionAsk, code: "approval_required", layer: LayerHard,
		},
		{
			name: "control plane outranks a user allow",
			setup: func(r *Runtime) {
				r.User = []Rule{{Tool: "file_write", Action: ActionAllow}}
			},
			call:   func() invocationFixture { return writeCall(".git/config") },
			action: ActionDeny, code: "control_plane_protected", layer: LayerHard,
		},
		{
			name: "constitution outranks a repository ask",
			setup: func(r *Runtime) {
				r.Constitution = []Rule{{Tool: "file_write", Action: ActionDeny, Code: "constitution_deny:repo"}}
				r.Repository = []Rule{{Tool: "file_write", Action: ActionAsk}}
			},
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionDeny, code: "constitution_deny:repo", layer: LayerHard,
		},
	})
	t.Run("control-plane denial names the resource", func(t *testing.T) {
		decision := DefaultRuntime(ModeAct, PermissionBypass).Decide(resolveFixture(writeCall(".git/index")))
		if decision.Resource != "/workspace/.git/index" {
			t.Fatalf("Decision.Resource = %q", decision.Resource)
		}
	})
	t.Run("constitution rejects allow and ask rules", func(t *testing.T) {
		for _, action := range []Action{ActionAllow, ActionAsk} {
			if err := ValidateRules(SourceConstitution, []Rule{{Tool: "*", Action: action}}); err == nil {
				t.Fatalf("constitution %s rule was accepted", action)
			}
		}
	})
}

func TestDecideRepositoryLayer(t *testing.T) {
	runLayerCases(t, []layerCase{
		{
			name:   "no repository rule allows",
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionAllow, layer: LayerPosture,
		},
		{
			name:   "deny",
			setup:  func(r *Runtime) { r.Repository = []Rule{{Tool: "file_write", Action: ActionDeny}} },
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionDeny, code: "repository_rule_denied", layer: LayerRepository,
		},
		{
			name: "hold carries its code",
			setup: func(r *Runtime) {
				r.Repository = []Rule{{Tool: "file_write", Action: ActionHold, Code: "release_hold"}}
			},
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionDeny, code: "release_hold", layer: LayerRepository,
		},
		{
			name:   "ask",
			setup:  func(r *Runtime) { r.Repository = []Rule{{Tool: "file_write", Action: ActionAsk}} },
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionAsk, code: "approval_required", layer: LayerRepository,
		},
		{
			name:   "malformed allow",
			setup:  func(r *Runtime) { r.Repository = []Rule{{Tool: "file_write", Action: ActionAllow}} },
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionDeny, code: "repository_source_invalid", layer: LayerRepository,
		},
		{
			name: "repository deny outranks user allow",
			setup: func(r *Runtime) {
				r.Repository = []Rule{{Tool: "file_write", Action: ActionDeny}}
				r.User = []Rule{{Tool: "file_write", Action: ActionAllow}}
			},
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionDeny, code: "repository_rule_denied", layer: LayerRepository,
		},
	})
}

func TestDecideUserLayer(t *testing.T) {
	suggest := func(r *Runtime) { r.Permission = PermissionSuggest }
	runLayerCases(t, []layerCase{
		{
			name: "allow overrides a posture ask",
			setup: func(r *Runtime) {
				suggest(r)
				r.User = []Rule{{Tool: "fetch_page", Action: ActionAllow}}
			},
			call:   func() invocationFixture { return networkReadCall("example.com") },
			action: ActionAllow, layer: LayerUser,
		},
		{
			name:   "deny",
			setup:  func(r *Runtime) { r.User = []Rule{{Tool: "file_write", Action: ActionDeny}} },
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionDeny, code: "user_rule_denied", layer: LayerUser,
		},
		{
			name:   "ask",
			setup:  func(r *Runtime) { r.User = []Rule{{Tool: "file_write", Action: ActionAsk}} },
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionAsk, code: "approval_required", layer: LayerUser,
		},
		{
			name: "user deny outranks planning",
			setup: func(r *Runtime) {
				r.PlanningPolicy = PlanningRequired
				r.User = []Rule{{Tool: "file_write", Action: ActionDeny}}
			},
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionDeny, code: "user_rule_denied", layer: LayerUser,
		},
	})
}

func TestDecideModeLayer(t *testing.T) {
	runLayerCases(t, []layerCase{
		{
			name: "submitted plan passes",
			setup: func(r *Runtime) {
				r.PlanningPolicy, r.PlanSubmitted = PlanningRequired, true
			},
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionAllow, layer: LayerPosture,
		},
		{
			name:   "unknown mode",
			setup:  func(r *Runtime) { r.Mode = "plan" },
			call:   readCall,
			action: ActionDeny, code: "mode_unknown", layer: LayerMode,
		},
		{
			name:   "malformed planning policy",
			setup:  func(r *Runtime) { r.PlanningPolicy = "sometimes" },
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionDeny, code: "planning_policy_invalid", layer: LayerMode,
		},
		{
			name:   "plan required holds even under bypass",
			setup:  func(r *Runtime) { r.PlanningPolicy = PlanningRequired },
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionHold, code: "plan_required", layer: LayerMode,
		},
		{
			name:  "declared verification asks",
			setup: func(r *Runtime) { r.Permission, r.PlanningPolicy = PermissionAuto, PlanningRequired },
			call: func() invocationFixture {
				call := invocation("run_command", "v-1", `{"command":"go test ./..."}`)
				call.Resources = []tool.Resource{{Kind: "file", Path: "coverage.out", Access: tool.AccessWrite}}
				return call
			},
			action: ActionHold, code: "plan_required", layer: LayerMode,
		},
		{
			name: "planning outranks the never posture",
			setup: func(r *Runtime) {
				r.Permission, r.PlanningPolicy = PermissionNever, PlanningRequired
			},
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionHold, code: "plan_required", layer: LayerMode,
		},
	})
}

func TestDecidePostureLayer(t *testing.T) {
	runLayerCases(t, []layerCase{
		{
			name:   "bypass allows",
			call:   func() invocationFixture { return invocation("run_command", "p-1", `{"command":"make"}`) },
			action: ActionAllow, layer: LayerPosture,
		},
		{
			name:   "suggest asks for network reads",
			setup:  func(r *Runtime) { r.Permission = PermissionSuggest },
			call:   func() invocationFixture { return networkReadCall("example.com") },
			action: ActionAsk, code: "approval_required", layer: LayerPosture,
		},
		{
			name:   "never denies side effects",
			setup:  func(r *Runtime) { r.Permission = PermissionNever },
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionDeny, code: "permission_denied", layer: LayerPosture,
		},
		{
			name:   "malformed permission",
			setup:  func(r *Runtime) { r.Permission = "sometimes" },
			call:   readCall,
			action: ActionDeny, code: "permission_unknown", layer: LayerPosture,
		},
		{
			name:  "posture deny outranks surface allow",
			setup: func(r *Runtime) { r.Permission, r.Granular.Rules = PermissionNever, SurfaceAllow },
			call:  func() invocationFixture { return writeCall("notes.txt") },

			action: ActionDeny, code: "permission_denied", layer: LayerPosture,
		},
	})
}

func TestDecideSurfaceLayer(t *testing.T) {
	runLayerCases(t, []layerCase{
		{
			name:   "surface allow keeps the posture decision",
			setup:  func(r *Runtime) { r.Granular.Sandbox = SurfaceAllow },
			call:   func() invocationFixture { return invocation("run_command", "s-1", `{"command":"make"}`) },
			action: ActionAllow, layer: LayerPosture,
		},
		{
			name:   "surface deny",
			setup:  func(r *Runtime) { r.Granular.Sandbox = SurfaceDeny },
			call:   func() invocationFixture { return invocation("run_command", "s-2", `{"command":"make"}`) },
			action: ActionDeny, code: "granular_deny", layer: LayerSurface,
		},
		{
			name:   "surface ask tightens allow",
			setup:  func(r *Runtime) { r.Granular.Rules = SurfaceAsk },
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionAsk, code: "granular_ask", layer: LayerSurface,
		},
		{
			name: "surface ask stops auto review",
			setup: func(r *Runtime) {
				r.Permission, r.Granular.Rules = PermissionAuto, SurfaceAsk
			},
			call:   func() invocationFixture { return networkReadCall("example.com") },
			action: ActionAsk, code: "granular_ask", layer: LayerSurface,
		},
		{
			name: "surface deny outranks a user allow",
			setup: func(r *Runtime) {
				r.Granular.Rules = SurfaceDeny
				r.User = []Rule{{Tool: "file_write", Action: ActionAllow}}
			},
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionDeny, code: "granular_deny", layer: LayerSurface,
		},
	})
}

func onceCall() invocationFixture {
	call := networkReadCall("example.com")
	call.Effect.Approval = tool.ApprovalPolicyOnce
	return call
}

func TestDecideBindingLayer(t *testing.T) {
	runLayerCases(t, []layerCase{
		{
			name: "default binding keeps the decision", call: func() invocationFixture {
				call := networkReadCall("example.com")
				call.Effect.Approval = tool.ApprovalPolicyDefault
				return call
			},
			action: ActionAllow, layer: LayerPosture,
		},
		{
			name: "once binding is preauthorized under bypass", call: onceCall,
			action: ActionAllow, layer: LayerPosture,
		},
		{
			name:   "once binding cannot soften a posture deny",
			setup:  func(r *Runtime) { r.Permission = PermissionNever },
			call:   onceCall,
			action: ActionDeny, code: "permission_denied", layer: LayerPosture,
		},
		{
			name:   "forced edit review asks for journaled writes",
			setup:  func(r *Runtime) { r.ForceEditPlanApproval = true },
			call:   func() invocationFixture { return writeCall("notes.txt") },
			action: ActionAsk, code: "edit_plan_required", layer: LayerBinding,
		},
		{
			name: "egress target decisions skip binding approval",
			call: func() invocationFixture {
				call := onceCall()
				call.Stage = string(StageEgress)
				return call
			},
			action: ActionAllow, layer: LayerPosture,
		},
		{
			name: "once binding outranks auto review",
			setup: func(r *Runtime) {
				r.Permission = PermissionAuto
			},
			call:   onceCall,
			action: ActionAsk, code: "tool_approval_required", layer: LayerBinding,
		},
	})
	t.Run("approval reuse", func(t *testing.T) {
		runtime := DefaultRuntime(ModeAct, PermissionAuto)
		if got := runtime.Decide(resolveFixture(onceCall())).Approval; got != ApprovalFreshOnce {
			t.Fatalf("once binding approval = %q", got)
		}
		runtime.Permission = PermissionSuggest
		call := networkReadCall("example.com")
		if got := runtime.Decide(resolveFixture(call)).Approval; got != ApprovalReusable {
			t.Fatalf("posture ask approval = %q", got)
		}
		runtime.ForceEditPlanApproval = true
		if got := runtime.Decide(resolveFixture(call)).Approval; got != ApprovalFresh {
			t.Fatalf("forced review ask approval = %q", got)
		}
	})
}

func TestDecideAutoReviewLayer(t *testing.T) {
	auto := func(r *Runtime) { r.Permission = PermissionAuto }
	runLayerCases(t, []layerCase{
		{
			name: "public network read is auto reviewed", setup: auto,
			call:   func() invocationFixture { return networkReadCall("example.com") },
			action: ActionAllow, code: "auto_review_allowed", layer: LayerAutoReview,
		},
		{
			name: "auto review disabled",
			setup: func(r *Runtime) {
				auto(r)
				r.DisableAutoReview = true
			},
			call:   func() invocationFixture { return networkReadCall("example.com") },
			action: ActionAsk, code: "approval_required", layer: LayerPosture,
		},
		{
			name: "host-local target", setup: auto,
			call:   func() invocationFixture { return networkReadCall("127.0.0.1") },
			action: ActionAsk, code: "approval_required", layer: LayerPosture,
		},
		{
			name: "suggest posture does not auto review network reads",
			setup: func(r *Runtime) {
				r.Permission = PermissionSuggest
			},
			call:   func() invocationFixture { return networkReadCall("example.com") },
			action: ActionAsk, code: "approval_required", layer: LayerPosture,
		},
		{
			name: "repository ask is never auto reviewed",
			setup: func(r *Runtime) {
				auto(r)
				r.Repository = []Rule{{Tool: "fetch_page", Action: ActionAsk}}
			},
			call:   func() invocationFixture { return networkReadCall("example.com") },
			action: ActionAsk, code: "approval_required", layer: LayerRepository,
		},
		{
			name: "untyped resources have no exact grant", setup: auto,
			call: func() invocationFixture {
				call := networkReadCall("example.com")
				call.Resources = nil
				return call
			},
			action: ActionAsk, code: "approval_required", layer: LayerPosture,
		},
	})
}

func TestDecisionLayersMatchSecurityDocument(t *testing.T) {
	data, err := os.ReadFile("../../../docs/zh-CN/security.md")
	if err != nil {
		t.Fatal(err)
	}
	section := string(data)
	start := strings.Index(section, "## 决策分层")
	if start < 0 {
		t.Fatal("security document has no decision layer section")
	}
	section = section[start+1:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	row := regexp.MustCompile("(?m)^\\| L[0-8] \\| `([a-z_]+)` \\|")
	var documented []Layer
	for _, match := range row.FindAllStringSubmatch(section, -1) {
		documented = append(documented, Layer(match[1]))
	}
	layers := Layers()
	if len(documented) != len(layers) {
		t.Fatalf("documented layers = %v, code layers = %v", documented, layers)
	}
	for index := range layers {
		if documented[index] != layers[index] {
			t.Fatalf("layer %d: documented %q, code %q", index, documented[index], layers[index])
		}
	}
	conditions := regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|")
	var documentedConditions []string
	for _, match := range conditions.FindAllStringSubmatch(section, -1) {
		documentedConditions = append(documentedConditions, match[1])
	}
	if strings.Join(documentedConditions, ",") != strings.Join(AutoReviewConditions(), ",") {
		t.Fatalf("documented auto-review conditions = %v, code = %v",
			documentedConditions, AutoReviewConditions())
	}
}
