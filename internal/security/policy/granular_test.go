package policy

import (
	"encoding/json"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestGranularTightensAllowToAsk(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	runtime.Granular.MCP = SurfaceAsk
	call := invocationFixture{
		CallID: "c1", Tool: "mcp_github_list", Source: "mcp:github",
		Capability: CapabilityNetwork,
		Arguments:  json.RawMessage(`{}`), Validated: true,
	}
	decision := runtime.Decide(resolveFixture(call))
	if decision.Action != ActionAsk {
		t.Fatalf("decision = %+v, want ask from MCP surface", decision)
	}
}

func TestGranularAskKeepsReadOnlyProcessProbesFrictionless(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	runtime.Granular.Sandbox = SurfaceAsk
	probe := invocationFixture{
		CallID: "c1", Tool: "run_command", Capability: CapabilityProcess,
		Arguments: json.RawMessage(`{"command":"ls -la"}`), Validated: true,
		Resources: []tool.Resource{{
			Kind: "process", ID: "workspace", Access: tool.AccessRead, Tree: true,
		}},
		Access: tool.AccessRead, Sandbox: tool.SandboxStrong,
	}
	decision := runtime.Decide(resolveFixture(probe))
	if decision.Action != ActionAllow {
		t.Fatalf("read-only probe decision = %+v, want allow under sandbox ask", decision)
	}
	mutating := probe
	mutating.Resources = []tool.Resource{
		{Kind: "process", ID: "workspace", Access: tool.AccessRead, Tree: true},
		{Kind: "file", Path: "bin/app", Access: tool.AccessWrite},
	}
	mutating.Access, mutating.Workspace = tool.AccessWrite, "/workspace"
	mutating.Arguments = json.RawMessage(
		`{"command":"go build -o bin/app ./...","write_paths":["bin/app"]}`,
	)
	if decision := runtime.Decide(resolveFixture(mutating)); decision.Action != ActionAsk ||
		decision.Code != "granular_ask" {
		t.Fatalf("mutating decision = %+v, want granular ask", decision)
	}
	runtime.Granular.Sandbox = SurfaceDeny
	if decision := runtime.Decide(resolveFixture(probe)); decision.Action != ActionDeny {
		t.Fatalf("deny posture softened for read-only: %+v", decision)
	}
}

func TestGranularAllowDoesNotBypassAutoApproval(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionAuto)
	runtime.Granular.Sandbox = SurfaceAllow
	call := invocationFixture{
		CallID: "c1", Tool: "run_command", Capability: CapabilityProcess,
		Arguments: json.RawMessage(`{}`), Validated: true,
		Resources: []tool.Resource{{Kind: "process", ID: "shell", Access: tool.AccessWrite}},
	}
	decision := runtime.Decide(resolveFixture(call))
	if decision.Action != ActionAsk {
		t.Fatalf("decision = %+v, want ask preserved under act+auto", decision)
	}
}

func TestClassifySurface(t *testing.T) {
	for _, test := range []struct {
		name, source string
		capability   Capability
		want         Surface
	}{
		{"fixture", "mcp:fixture", CapabilityNetwork, SurfaceMCP},
		{"skills_read", "builtin:skills_read:1", CapabilityRead, SurfaceSkills},
		{"skills.list", "builtin:skills_list:4", CapabilityRead, SurfaceSkills},
		{"run_command", "builtin:run_command:2", CapabilityProcess, SurfaceSandbox},
		{"lookup", "dynamic:1", CapabilityRead, SurfaceRules},
		{"skills_read", "dynamic:5", CapabilityRead, SurfaceRules},
		{"file_write", "builtin:file_write:3", CapabilityWrite, SurfaceRules},
	} {
		if got := ClassifySurface(tool.CatalogSourceKind(test.name, test.source), test.capability); got != test.want {
			t.Fatalf("%s from %s = %s, want %s", test.name, test.source, got, test.want)
		}
	}
}

func TestGranularSurfaceCannotBeSpoofedByDynamicToolName(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	runtime.Granular.Rules = SurfaceDeny
	runtime.Granular.MCP = SurfaceAllow
	call := invocationFixture{
		CallID: "c1", Tool: "mcp_forged", Source: "dynamic:1",
		Capability: CapabilityNetwork, Arguments: json.RawMessage(`{}`),
		Validated: true,
	}
	if decision := runtime.Decide(resolveFixture(call)); decision.Action != ActionDeny {
		t.Fatalf("decision = %+v, want dynamic source governed by rules", decision)
	}
}

func TestUnknownInvocationSourceFailsClosed(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	call := resolveFixture(invocationFixture{
		CallID: "c1", Tool: "probe", Capability: CapabilityRead,
		Arguments: json.RawMessage(`{}`), Validated: true,
	})
	for _, source := range []string{"", "mcp:unparsed", "unknown"} {
		call.Source = securitymodel.SourceKind(source)
		if decision := runtime.Decide(call); decision.Action != ActionDeny || decision.Layer != LayerInput {
			t.Fatalf("unknown source %q was not rejected at input: %+v", source, decision)
		}
	}
}
