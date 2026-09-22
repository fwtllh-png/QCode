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
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/egress"
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
	gate := &egress.Gate{Enforce: true}
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
	profile, err := authority.Compile(authority.CompileInput{
		Runtime: policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Invocation: policy.Invocation{
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
		},
		Authorized: true, Decision: policy.Decision{Action: policy.ActionAsk},
		Revision: 1, Enforcement: "strong",
		Capability: backend.Capability(), SandboxPolicy: sandboxPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if profile.Network.Mode != "managed" || profile.Network.ProxyPort != proxy.Port() {
		t.Fatalf("approved target lost proxy authority: %+v", profile.Network)
	}
	execution := profile.ExecutionAuthorityFor(authority.ExecutionOperation{
		Required: authority.RequiredControls{Network: controlmatrix.NetworkProxyTargets},
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
		&egress.Gate{Enforce: true},
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
		SessionProxyPort: sessionA.Port(),
	})
	if err != nil || allowed.Stdout != "left-ok" {
		t.Fatalf("session A granted request = %+v error=%v", allowed, err)
	}
	crossed, err := process.Run(ctx, process.Options{
		Command: shellQuote(curl) + " -fsS --noproxy '' " + shellQuote(right.URL),
		Dir:     root, DirFile: pinned, Sandbox: backend,
		RequireSandbox: true, WorkspaceReadOnly: true,
		SessionProxyPort: sessionA.Port(),
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
		SessionProxyPort: sessionA.Port(),
	})
	if err != nil || sibling.ExitCode == 0 {
		t.Fatalf("session A reached sibling port = %+v error=%v", sibling, err)
	}
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
	profile, err := authority.Compile(authority.CompileInput{
		Runtime: policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Invocation: policy.Invocation{
			CallID: "session-isolation", Tool: "exec_command",
			Arguments: json.RawMessage(`{"command":"curl"}`), Validated: true,
			Capability: tool.CapabilityProcess, Access: tool.AccessRead,
			Sandbox: tool.SandboxStrong, Resources: resources,
		},
		Authorized: true, Decision: policy.Decision{Action: policy.ActionAsk},
		Revision: 1, Enforcement: "strong",
		Capability: backend.Capability(), SandboxPolicy: sandboxPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	execution := profile.ExecutionAuthorityFor(authority.ExecutionOperation{
		Required: authority.RequiredControls{Network: controlmatrix.NetworkProxyTargets},
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
