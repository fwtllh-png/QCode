//go:build capability && darwin

package process_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestRealManagedProxyBlocksDirectEgress(t *testing.T) {
	if os.Getenv("QCODE_SANDBOX_STAGE") != "1" {
		t.Skip("managed proxy attack test requires the staged macOS sandbox")
	}
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is unavailable")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = writer.Write([]byte("managed-ok"))
	}))
	defer upstream.Close()
	targetURL, _ := url.Parse(upstream.URL)
	portValue, _ := strconv.ParseUint(targetURL.Port(), 10, 16)
	gate := egress.NewStaticGate()
	gate.AllowTarget(egress.Target{
		Host: targetURL.Hostname(), Protocol: "http", Port: uint16(portValue),
		Methods: []string{http.MethodGet}, AllowPrivate: true,
	})
	proxy, err := egress.StartManagedNetworkProxy(gate)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close(context.Background())
	root := t.TempDir()
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: root, ManagedProxyPort: proxy.Port(),
		ManagedProxyCredential: proxy.Credential(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sandbox.CloseBackend(backend)
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	root = workspace.Root()
	pinned, err := workspace.OpenDirectory(".")
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	sandboxPolicy, _ := sandbox.BackendPolicy(backend)
	profile, err := compileTestProfile(authority.CompileInput{
		Runtime: policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Invocation: resolvePolicyFixture(policyInvocationFixture{
			CallID: "approved-network", Tool: "exec_command",
			Arguments: json.RawMessage(`{"command":"curl"}`), Validated: true,
			Capability: tool.CapabilityProcess, Access: tool.AccessRead,
			Sandbox: tool.SandboxStrong,
			Resources: []tool.Resource{
				{Kind: "repo", Path: root, Access: tool.AccessRead, Tree: true},
				{Kind: "host", ID: targetURL.Hostname(), Protocol: "http",
					Port: uint16(portValue), Methods: []string{"GET"}, AllowPrivate: true,
					Access: tool.AccessWrite},
			},
		}),
		Authorized: true, Decision: policy.Decision{Action: policy.ActionAsk},
		Revision: 1, Enforcement: "strong",
		Capability: backend.Capability(), SandboxPolicy: sandboxPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if profile.Controls.Network != securitymodel.NetworkProxyTargets || profile.Network.ProxyPort != proxy.Port() {
		t.Fatalf("approved target lost proxy authority: %+v", profile.Network)
	}
	execution := profile.ExecutionAuthorityFor(authority.ExecutionOperation{
		Required: authority.RequiredControls{Network: securitymodel.NetworkProxyTargets},
	})
	ctx, err := sandbox.WithExecutionAuthority(t.Context(), execution)
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := process.Run(ctx, process.Options{
		Command: shellQuote(curl) + " -fsS --noproxy '' " + shellQuote(upstream.URL),
		Dir:     root, DirFile: pinned, Sandbox: backend,
		RequireSandbox: true, WorkspaceReadOnly: true,
	})
	if err != nil || allowed.Stdout != "managed-ok" {
		t.Fatalf("managed request = %+v error=%v", allowed, err)
	}
	undeclared, err := process.Run(ctx, process.Options{
		Command: shellQuote(curl) + " -fsS --noproxy '' -X POST " + shellQuote(upstream.URL),
		Dir:     root, DirFile: pinned, Sandbox: backend,
		RequireSandbox: true, WorkspaceReadOnly: true,
	})
	if err != nil || undeclared.ExitCode == 0 {
		t.Fatalf("undeclared method = %+v error=%v", undeclared, err)
	}
	direct, err := process.Run(ctx, process.Options{
		Command: "/usr/bin/nc -w 1 127.0.0.1 " + targetURL.Port(),
		Dir:     root, DirFile: pinned, Sandbox: backend,
		RequireSandbox: true, WorkspaceReadOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if direct.ExitCode == 0 {
		t.Fatalf("direct egress succeeded: %+v", direct)
	}
}

func TestRealSessionProxyIsolatesSiblingPorts(t *testing.T) {
	if os.Getenv("QCODE_SANDBOX_STAGE") != "1" {
		t.Skip("session isolation attack test requires the staged macOS sandbox")
	}
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is unavailable")
	}
	left := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = writer.Write([]byte("left-ok"))
	}))
	defer left.Close()
	right := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = writer.Write([]byte("right-ok"))
	}))
	defer right.Close()
	root := t.TempDir()
	backend, err := egress.NewManagedBackend(
		egress.NewStaticGate(),
		sandbox.Options{WorkspaceRoot: root},
		sandbox.NewPlatformBackend,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer sandbox.CloseBackend(backend)
	opener, ok := egress.LookupProcessSessionOpener(backend)
	if !ok {
		t.Fatal("workspace proxy has no session allocator")
	}
	leftTarget := httpTarget(t, left.URL, http.MethodGet)
	rightTarget := httpTarget(t, right.URL, http.MethodGet)
	sessionA, err := opener.OpenProcessSession([]egress.Target{leftTarget})
	if err != nil {
		t.Fatal(err)
	}
	defer sessionA.Close()
	sessionB, err := opener.OpenProcessSession([]egress.Target{rightTarget})
	if err != nil {
		t.Fatal(err)
	}
	defer sessionB.Close()
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	root = workspace.Root()
	pinned, err := workspace.OpenDirectory(".")
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	ctx := sandboxedProxyContext(t, backend, root, left.URL, right.URL)

	allowed, err := process.Run(ctx, process.Options{
		Command: shellQuote(curl) + " -fsS --noproxy '' " + shellQuote(left.URL),
		Dir:     root, DirFile: pinned, Sandbox: backend,
		RequireSandbox: true, WorkspaceReadOnly: true,
		SessionProxyPort: sessionA.Port(), SessionProxyCredential: sessionA.Credential(),
	})
	if err != nil || allowed.Stdout != "left-ok" {
		t.Fatalf("session A granted request = %+v error=%v", allowed, err)
	}
	crossed, err := process.Run(ctx, process.Options{
		Command: shellQuote(curl) + " -fsS --noproxy '' " + shellQuote(right.URL),
		Dir:     root, DirFile: pinned, Sandbox: backend,
		RequireSandbox: true, WorkspaceReadOnly: true,
		SessionProxyPort: sessionA.Port(), SessionProxyCredential: sessionA.Credential(),
	})
	if err != nil || crossed.ExitCode == 0 {
		t.Fatalf("session A consumed sibling grant = %+v error=%v", crossed, err)
	}
	sibling, err := process.Run(ctx, process.Options{
		Command: shellQuote(curl) + " -fsS --noproxy '' -x " +
			shellQuote("http://127.0.0.1:"+strconv.Itoa(int(sessionB.Port()))) +
			" " + shellQuote(left.URL),
		Dir: root, DirFile: pinned, Sandbox: backend,
		RequireSandbox: true, WorkspaceReadOnly: true,
		SessionProxyPort: sessionA.Port(), SessionProxyCredential: sessionA.Credential(),
	})
	if err != nil || sibling.ExitCode == 0 {
		t.Fatalf("session A reached sibling port = %+v error=%v", sibling, err)
	}

	// allow_loopback opens every local port, so the proxy channels
	// themselves are reachable; only their credentials keep them apart.
	loopbackCtx := sandboxedLoopbackContext(t, backend, root)
	workspacePolicy, _ := sandbox.BackendPolicy(backend)
	for name, proxyURL := range map[string]string{
		"sibling session without credential": sandbox.ManagedProxyURL(sessionB.Port(), ""),
		"sibling session with own credential": sandbox.ManagedProxyURL(
			sessionB.Port(), sessionA.Credential(),
		),
		"workspace channel without credential": sandbox.ManagedProxyURL(
			workspacePolicy.ManagedProxyPort, "",
		),
		"workspace channel with session credential": sandbox.ManagedProxyURL(
			workspacePolicy.ManagedProxyPort, sessionA.Credential(),
		),
	} {
		probe, err := process.Run(loopbackCtx, process.Options{
			Command: shellQuote(curl) + " -sS -o /dev/null -w '%{http_code}' --noproxy '' -x " +
				shellQuote(proxyURL) + " " + shellQuote(right.URL),
			Dir: root, DirFile: pinned, Sandbox: backend,
			RequireSandbox: true, WorkspaceReadOnly: true,
		})
		if err != nil || probe.Stdout != "407" {
			t.Fatalf("%s: loopback command through proxy = %+v error=%v", name, probe, err)
		}
	}
}

func sandboxedLoopbackContext(t *testing.T, backend sandbox.Backend, root string) context.Context {
	t.Helper()
	sandboxPolicy, _ := sandbox.BackendPolicy(backend)
	profile, err := compileTestProfile(authority.CompileInput{
		Runtime: policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Invocation: resolvePolicyFixture(policyInvocationFixture{
			CallID: "loopback-probe", Tool: "exec_command",
			Arguments: json.RawMessage(`{"command":"curl"}`), Validated: true,
			Capability: tool.CapabilityProcess, Access: tool.AccessRead,
			Sandbox: tool.SandboxStrong,
			Resources: []tool.Resource{
				{Kind: "repo", Path: root, Access: tool.AccessRead, Tree: true},
				{Kind: "host", ID: "localhost", Protocol: securitymodel.LoopbackProtocol, Access: tool.AccessRead},
			},
		}),
		Authorized: true, Decision: policy.Decision{Action: policy.ActionAsk},
		Revision: 1, Enforcement: "strong",
		Capability: backend.Capability(), SandboxPolicy: sandboxPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	execution := profile.ExecutionAuthorityFor(authority.ExecutionOperation{
		Required: authority.RequiredControls{Network: securitymodel.NetworkLoopbackAny},
	})
	ctx, err := sandbox.WithExecutionAuthority(t.Context(), execution)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func sandboxedProxyContext(
	t *testing.T,
	backend sandbox.Backend,
	root string,
	endpoints ...string,
) context.Context {
	t.Helper()
	sandboxPolicy, _ := sandbox.BackendPolicy(backend)
	resources := []tool.Resource{{
		Kind: "repo", Path: root, Access: tool.AccessRead, Tree: true,
	}}
	for _, endpoint := range endpoints {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		portValue, err := strconv.ParseUint(parsed.Port(), 10, 16)
		if err != nil {
			t.Fatal(err)
		}
		resources = append(resources, tool.Resource{
			Kind: "host", ID: parsed.Hostname(), Protocol: parsed.Scheme,
			Port: uint16(portValue), Methods: []string{"GET"}, AllowPrivate: true,
			Access: tool.AccessWrite,
		})
	}
	profile, err := compileTestProfile(authority.CompileInput{
		Runtime: policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Invocation: resolvePolicyFixture(policyInvocationFixture{
			CallID: "session-isolation", Tool: "exec_command",
			Arguments: json.RawMessage(`{"command":"curl"}`), Validated: true,
			Capability: tool.CapabilityProcess, Access: tool.AccessRead,
			Sandbox: tool.SandboxStrong, Resources: resources,
		}),
		Authorized: true, Decision: policy.Decision{Action: policy.ActionAsk},
		Revision: 1, Enforcement: "strong",
		Capability: backend.Capability(), SandboxPolicy: sandboxPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	execution := profile.ExecutionAuthorityFor(authority.ExecutionOperation{
		Required: authority.RequiredControls{Network: securitymodel.NetworkProxyTargets},
	})
	ctx, err := sandbox.WithExecutionAuthority(t.Context(), execution)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func httpTarget(t *testing.T, endpoint, method string) egress.Target {
	t.Helper()
	parsed, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	portValue, err := strconv.ParseUint(parsed.Port(), 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	return egress.Target{
		Host: parsed.Hostname(), Protocol: parsed.Scheme, Port: uint16(portValue),
		Methods: []string{method}, AllowPrivate: true,
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func compileTestProfile(input authority.CompileInput) (authority.EffectivePermissionProfile, error) {
	invocation := input.Invocation
	prepared := tool.PreparedInvocation{
		CallID: invocation.CallID, Tool: invocation.Tool,
		Ref: tool.ToolRef{
			Name: invocation.Tool, Source: "builtin:" + invocation.Tool,
			CatalogID: "catalog", Generation: 1, Revision: 1, Authority: 1,
		},
		Arguments: invocation.Arguments,
		Descriptor: tool.Descriptor{
			Name: invocation.Tool, Capability: invocation.Capability(),
			AccessMode: invocation.Access(), SandboxRequirement: fixtureSandboxRequirement(invocation),
		},
		Source: tool.InvocationSourceModel,
	}
	prepared.Binding = tool.TrustedBindingFromDescriptor(prepared.Descriptor)
	prepared.Assessment = input.Invocation.Assessment
	resolved, err := prepared.SecurityInvocation()
	if err != nil {
		return authority.EffectivePermissionProfile{}, err
	}
	input.Prepared = resolved
	compiled, err := authority.Compile(input)
	return compiled.Profile, err
}

func fixtureSandboxRequirement(invocation policy.Invocation) tool.SandboxRequirement {
	if invocation.StrongSandbox() {
		return tool.SandboxStrong
	}
	return tool.SandboxNone
}
