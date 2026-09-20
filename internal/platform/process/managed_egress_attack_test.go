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

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
