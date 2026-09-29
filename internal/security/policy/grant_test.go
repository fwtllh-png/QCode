package policy

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestShellGrantBindsCommandCWDAndWriteSet(t *testing.T) {
	base := invocationFixture{
		Tool: "run_command", Capability: CapabilityProcess,
		Access: tool.AccessRead, Sandbox: tool.SandboxStrong, Validated: true,
		Arguments: json.RawMessage(`{"command":"go test ./...","cwd":"src"}`),
		Resources: []tool.Resource{
			{Kind: "process", ID: "workspace", Access: tool.AccessRead, Tree: true},
			{Kind: "file", Path: "report.txt", Access: tool.AccessWrite},
		},
	}
	grant, ok := GrantForInvocation(resolveFixture(base))
	if !ok || grant.Kind != "shell" || len(grant.Key) != 64 {
		t.Fatalf("grant = %+v ok=%v", grant, ok)
	}
	for name, mutate := range map[string]func(*invocationFixture){
		"command": func(call *invocationFixture) {
			call.Arguments = json.RawMessage(`{"command":"go test ./pkg","cwd":"src"}`)
		},
		"cwd": func(call *invocationFixture) {
			call.Arguments = json.RawMessage(`{"command":"go test ./...","cwd":"pkg"}`)
		},
		"write set": func(call *invocationFixture) {
			call.Resources[1].Path = "other.txt"
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			changed.Resources = append([]tool.Resource(nil), base.Resources...)
			mutate(&changed)
			other, ok := GrantForInvocation(resolveFixture(changed))
			if !ok || other.Key == grant.Key {
				t.Fatalf("changed grant = %+v", other)
			}
		})
	}
}

func TestShellGrantBindsLoopbackResourceSemantics(t *testing.T) {
	base := invocationFixture{
		Tool: "run_command", Capability: CapabilityProcess,
		Access: tool.AccessWrite, Sandbox: tool.SandboxStrong, Validated: true,
		Arguments: json.RawMessage(`{"command":"go test ./..."}`),
		Resources: []tool.Resource{{
			Kind: "host", ID: "localhost", Access: tool.AccessWrite,
			Protocol: "http", Port: 8080, Methods: []string{"GET"},
			AllowPrivate: true,
		}},
	}
	httpGrant, ok := GrantForInvocation(resolveFixture(base))
	if !ok {
		t.Fatal("HTTP invocation did not produce a grant")
	}
	loopback := base
	loopback.Resources = []tool.Resource{{
		Kind: "host", ID: "localhost", Access: tool.AccessWrite,
		Protocol: securitymodel.LoopbackProtocol, Methods: []string{"BIND", "CONNECT"},
		AllowPrivate: true,
	}}
	loopbackGrant, ok := GrantForInvocation(resolveFixture(loopback))
	if !ok || loopbackGrant.Key == httpGrant.Key {
		t.Fatalf("loopback grant = %+v, HTTP grant = %+v", loopbackGrant, httpGrant)
	}
}

func TestProcessPathGrantBindsExecutableAndArguments(t *testing.T) {
	base := invocationFixture{
		Tool: "fixture_host_process", Capability: CapabilityProcess,
		Access: tool.AccessRead, Sandbox: tool.SandboxNone, Validated: true,
		Arguments: json.RawMessage(
			`{"path":"target/App","args":["--smoke"]}`,
		),
		Resources: []tool.Resource{
			{Kind: "file", Path: "target/App", Access: tool.AccessRead},
			{Kind: "process", ID: "host", Access: tool.AccessWrite},
		},
	}
	grant, ok := GrantForInvocation(resolveFixture(base))
	if !ok || grant.Kind != "shell" {
		t.Fatalf("path grant = %+v, ok=%t", grant, ok)
	}
	changed := base
	changed.Arguments = json.RawMessage(
		`{"path":"target/App","args":["--other"]}`,
	)
	other, ok := GrantForInvocation(resolveFixture(changed))
	if !ok || other.Key == grant.Key {
		t.Fatalf("changed path grant = %+v, ok=%t", other, ok)
	}
}

func TestSessionGrantMatchesOnlyTypedKey(t *testing.T) {
	now := time.Now()
	cache := NewApprovalCache()
	base := invocationFixture{
		CallID: "one", Tool: "run_command", Capability: CapabilityProcess,
		Access: tool.AccessRead, Sandbox: tool.SandboxStrong, Validated: true,
		Arguments: json.RawMessage(`{"cwd":".","command":"go test ./..."}`),
		Resources: []tool.Resource{{
			Kind: "process", ID: "workspace", Access: tool.AccessRead, Tree: true,
		}},
	}
	request, err := NewApprovalRequestForScope(resolveFixture(base), ApprovalSession, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Add(request, ApprovalSession); err != nil {
		t.Fatal(err)
	}
	reordered := base
	reordered.CallID = "two"
	reordered.Arguments = json.RawMessage(`{"command":"go test ./...","cwd":"."}`)
	if !cache.MatchInvocation(resolveFixture(reordered), now) {
		t.Fatal("same typed grant did not match")
	}
	changed := base
	changed.CallID = "three"
	changed.Arguments = json.RawMessage(`{"cwd":".","command":"go env"}`)
	if cache.MatchInvocation(resolveFixture(changed), now) {
		t.Fatal("different command reused session grant")
	}
}

func TestShellGrantPrefixBindsEffectAndFacets(t *testing.T) {
	now := time.Now()
	cache := NewApprovalCache()
	base := invocationFixture{
		CallID: "one", Tool: "run_command", Capability: CapabilityProcess,
		Access: tool.AccessRead, Sandbox: tool.SandboxStrong, Validated: true,
		Arguments: json.RawMessage(`{"cwd":".","command":"go test"}`),
		Resources: []tool.Resource{{
			Kind: "process", ID: "workspace", Access: tool.AccessRead, Tree: true,
		}},
	}
	request, err := NewApprovalRequestForScope(resolveFixture(base), ApprovalSession, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Add(request, ApprovalSession); err != nil {
		t.Fatal(err)
	}
	extended := base
	extended.CallID = "two"
	extended.Arguments = json.RawMessage(`{"cwd":".","command":"go test ./pkg"}`)
	if !cache.MatchInvocation(resolveFixture(extended), now) {
		t.Fatal("argv extension in the same scope did not match")
	}
	for name, mutate := range map[string]func(*invocationFixture){
		"facets": func(call *invocationFixture) { call.Declared.Verification = true },
		"effect": func(call *invocationFixture) { call.Sandbox = tool.SandboxNone },
	} {
		t.Run(name, func(t *testing.T) {
			changed := extended
			changed.CallID = "three"
			mutate(&changed)
			if cache.MatchInvocation(resolveFixture(changed), now) {
				t.Fatal("argv prefix crossed an effect or facet boundary")
			}
			same := base
			mutate(&same)
			left, _ := GrantForInvocation(resolveFixture(base))
			right, ok := GrantForInvocation(resolveFixture(same))
			if !ok || left.Key != right.Key {
				t.Fatalf("exact key changed with %s, so deny rules would stop matching: %+v", name, right)
			}
		})
	}
}

func TestReusableGrantRejectsUnscopedInvocation(t *testing.T) {
	call := invocationFixture{
		CallID: "one", Tool: "external_mutation", Capability: CapabilityExternal,
		Access: tool.AccessTree, Sandbox: tool.SandboxNone,
		Arguments: json.RawMessage(`{}`), Validated: true,
	}
	request, err := NewApprovalRequestForScope(resolveFixture(call), ApprovalSession, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if request.Grant != nil {
		t.Fatalf("unexpected grant = %+v", request.Grant)
	}
	if err := NewApprovalCache().Add(request, ApprovalSession); err == nil {
		t.Fatal("unscoped session grant was accepted")
	}
}
