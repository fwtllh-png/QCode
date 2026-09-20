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
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestRealLoopbackAuthorityWithAndWithoutManagedProxy(t *testing.T) {
	if os.Getenv("QCODE_SANDBOX_STAGE") != "1" {
		t.Skip("loopback authority test requires the staged macOS sandbox")
	}
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is unavailable")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("loopback-ok"))
	}))
	defer upstream.Close()
	targetURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(targetURL.Port(), 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	target := egress.Target{
		Host: targetURL.Hostname(), Protocol: "http", Port: uint16(port),
		Methods: []string{http.MethodGet}, AllowPrivate: true,
	}
	gate := &egress.Gate{Enforce: true}
	gate.AllowTarget(target)
	proxy, err := egress.StartManagedNetworkProxy(gate)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close(context.Background())

	for _, proxyPort := range []uint16{0, proxy.Port()} {
		t.Run("proxy-"+strconv.Itoa(int(proxyPort)), func(t *testing.T) {
			backend, err := sandbox.NewPlatformBackend(sandbox.Options{
				WorkspaceRoot: t.TempDir(), ManagedProxyPort: proxyPort,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer sandbox.CloseBackend(backend)
			capability := backend.Capability()
			if !capability.Available || !capability.ManagedProxy {
				t.Fatalf("sandbox_unavailable: expected probed Seatbelt proxy support: %+v", capability)
			}
			base, _ := sandbox.BackendPolicy(backend)
			pinned, err := process.OpenPinnedDirectory(backend, base.WorkspaceRoot)
			if err != nil {
				t.Fatal(err)
			}
			defer pinned.Close()
			for _, grant := range []string{"loopback", "denied", "managed-and-loopback"} {
				if proxyPort == 0 && grant == "managed-and-loopback" {
					continue
				}
				t.Run(grant, func(t *testing.T) {
					resources := []tool.Resource{{
						Kind: "repo", Path: base.WorkspaceRoot, Access: tool.AccessRead, Tree: true,
					}}
					want := controlmatrix.NetworkDenied
					if grant != "denied" {
						resources = append(resources, tool.Resource{
							Kind: "host", ID: "localhost", Protocol: "loopback", Access: tool.AccessRead,
						})
						want = controlmatrix.NetworkLoopbackExact
					}
					if grant == "managed-and-loopback" {
						resources = append(resources, tool.Resource{
							Kind: "host", ID: target.Host, Protocol: target.Protocol,
							Port: target.Port, Methods: target.Methods, AllowPrivate: true,
							Access: tool.AccessWrite,
						})
						want = controlmatrix.NetworkProxyTargets
					}
					profile, err := authority.Compile(authority.CompileInput{
						Runtime: policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
						Invocation: policy.Invocation{
							CallID: "approved-loopback", Tool: "exec_command",
							Arguments: json.RawMessage(`{"command":"curl"}`), Validated: true,
							Capability: tool.CapabilityProcess, Access: tool.AccessRead,
							Sandbox: tool.SandboxStrong, Resources: resources,
						},
						Authorized: true, Decision: policy.Decision{Action: policy.ActionAsk},
						Revision: 1, Enforcement: "strong",
						Capability: capability, SandboxPolicy: base,
					})
					if err != nil {
						t.Fatal(err)
					}
					if profile.Controls.Network != want {
						t.Fatalf("compiled controls = %+v, want network %s", profile.Controls, want)
					}
					execution := profile.ExecutionAuthorityFor(authority.ExecutionOperation{
						Required: authority.RequiredControls{Network: want},
					})
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					ctx, err = sandbox.WithExecutionAuthority(ctx, execution)
					if err != nil {
						t.Fatal(err)
					}
					command, err := process.NewCommand(ctx, process.Options{
						Path: curl, Args: []string{"-fsS", "--connect-timeout", "2", upstream.URL},
						Dir: base.WorkspaceRoot, DirFile: pinned, Sandbox: backend,
						RequireSandbox: true, WorkspaceReadOnly: true,
					})
					if err != nil {
						t.Fatalf("approved %s command was rejected before launch: %v", grant, err)
					}
					hasProxy := false
					for _, entry := range command.Env {
						name, _, _ := strings.Cut(entry, "=")
						switch strings.ToUpper(name) {
						case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
							hasProxy = true
						}
					}
					if hasProxy != (grant == "managed-and-loopback") {
						t.Fatalf("%s proxy environment present = %t", grant, hasProxy)
					}
					output, runErr := command.CombinedOutput()
					if grant == "denied" {
						if runErr == nil {
							t.Fatalf("unapproved loopback connection succeeded: %s", output)
						}
					} else if runErr != nil || string(output) != "loopback-ok" {
						t.Fatalf("%s request: %v, output=%s", grant, runErr, output)
					}
					if current, _ := sandbox.BackendPolicy(backend); current.ManagedProxyPort != proxyPort {
						t.Fatal("command changed shared backend proxy policy")
					}
				})
			}
		})
	}
}
