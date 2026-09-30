//go:build capability && darwin

package process_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/envprep"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/common/environment"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestSandboxNodeUsesValidatedCertificateDependencies(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is unavailable")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	caPath := filepath.Join(t.TempDir(), "public-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: server.Certificate().Raw,
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_EXTRA_CA_CERTS", caPath)
	target, _ := url.Parse(server.URL)
	port, _ := strconv.ParseUint(target.Port(), 10, 16)
	gate := egress.NewStaticGate()
	gate.AllowTarget(egress.Target{
		Host: target.Hostname(), Protocol: "https", Port: uint16(port),
		Methods: []string{"CONNECT"}, AllowPrivate: true,
	})
	proxy, err := egress.StartManagedNetworkProxy(gate)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close(context.Background())
	prepared, err := envprep.Prepare(t.Context(), envprep.Options{
		Sandbox: sandbox.Options{
			EnvironmentProfile: environment.ProfileNative,
			WorkspaceRoot:      t.TempDir(), ManagedProxyPort: proxy.Port(),
			ManagedProxyCredential: proxy.Credential(),
			PrivateTemp:            t.TempDir(),
		},
		Declarations: []environment.ResourceRequest{declaredTLSConfig(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(prepared.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	defer sandbox.CloseBackend(backend)
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Fatalf("sandbox_unavailable: %v", err)
	}
	policy, _ := sandbox.BackendPolicy(backend)
	pinned, err := process.OpenPinnedDirectory(backend, policy.WorkspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	// Read all discovered files, then use a real CONNECT tunnel and verify TLS.
	// Neither the client nor the proxy disables certificate verification.
	files, _ := json.Marshal(policy.Toolchains.ReadFiles)
	script := `const fs=require('fs'),http=require('http'),tls=require('tls');
for(const file of JSON.parse(process.argv[1])) fs.readFileSync(file);
try { fs.writeFileSync(process.env.NODE_EXTRA_CA_CERTS,'modified'); process.exit(90); }
catch(e) { if(e.code!=='EPERM'&&e.code!=='EACCES') throw e; }
const target=new URL(process.argv[2]),proxy=new URL(process.env.HTTPS_PROXY);
const auth=Buffer.from(decodeURIComponent(proxy.username)+':'+decodeURIComponent(proxy.password)).toString('base64');
const req=http.request({hostname:proxy.hostname,port:proxy.port,method:'CONNECT',path:target.host,headers:{'Proxy-Authorization':'Basic '+auth}});
req.on('connect',(res,socket)=>{
 if(res.statusCode!==200) process.exit(91);
 const secure=tls.connect({socket,servername:'example.com'},()=>{
  if(!secure.authorized) process.exit(92);
  secure.end();console.log('verified');
 });
 secure.on('error',e=>{console.error(e.code);process.exitCode=1});
});req.on('error',e=>{console.error(e.code);process.exitCode=1});req.end();`
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	result, err := process.Run(ctx, process.Options{
		Path: node, Args: []string{"-e", script, string(files), server.URL},
		Dir: policy.WorkspaceRoot, DirFile: pinned, Sandbox: backend,
		RequireSandbox: true, WorkspaceReadOnly: true,
	})
	if err != nil || result.ExitCode != 0 || result.Stdout != "verified\n" {
		t.Fatalf("sandbox TLS verification: %+v %v", result, err)
	}
}

func TestDeclaredGoModuleCacheWritableInSandbox(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/t\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "probe_test.go"), []byte(
		"package probe\nimport (\"testing\"; \"strings\")\nfunc TestProbe(t *testing.T) { if strings.TrimSpace(\" ok \") != \"ok\" { t.Fatal(\"probe\") } }\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	toolchain := exec.Command("go", "env", "GOROOT")
	toolchain.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	goRoot, err := toolchain.Output()
	if err != nil {
		t.Fatalf("resolve fixture toolchain: %v", err)
	}
	backend, err := declaredToolBackend(t, root, []environment.ResourceRequest{
		{Name: "toolchain", Namespace: environment.NamespaceHostToolchain, Access: environment.AccessRead, Path: strings.TrimSpace(string(goRoot)), Env: "GOROOT"},
		{Name: "build", Namespace: environment.NamespaceCache, Access: environment.AccessWrite, Path: "sandbox-home/cache/build", Env: "GOCACHE", Tree: true},
		{Name: "modules", Namespace: environment.NamespaceCache, Access: environment.AccessWrite, Path: "sandbox-home/cache/modules", Env: "GOMODCACHE", Tree: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skip(err)
	}
	ws, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := ws.OpenDirectory(".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pinned.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := process.Run(ctx, process.Options{
		Dir: ws.Root(), DirFile: pinned,
		Command: `go env GOMODCACHE GOCACHE GOTMPDIR HOME && go list -m && go test ./... && go vet ./...`,
		Env:     []string{"GOPROXY=off", "GOTOOLCHAIN=local"},
		Sandbox: backend, RequireSandbox: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := result.Stdout + "\n" + result.Stderr
	if result.ExitCode != 0 {
		t.Fatalf("exit=%d out=%s", result.ExitCode, out)
	}
	if strings.Contains(out, "mkdir /var: file exists") ||
		strings.Contains(out, "could not create module cache") {
		t.Fatalf("module cache still broken: %s", out)
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "/var/") {
			t.Fatalf("cache path still /var symlink form: %q\nfull:\n%s", line, result.Stdout)
		}
	}
}

func TestDeclaredHostToolchainIsReusableInSandbox(t *testing.T) {
	root := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	rustup := os.Getenv("RUSTUP_HOME")
	if rustup == "" {
		rustup = filepath.Join(home, ".rustup")
	}
	backend, err := declaredToolBackend(t, root, []environment.ResourceRequest{
		{Name: "toolchain", Namespace: environment.NamespaceHostToolchain, Access: environment.AccessRead, Path: rustup, Env: "RUSTUP_HOME", Tree: true},
		{Name: "cache", Namespace: environment.NamespaceCache, Access: environment.AccessWrite, Path: "sandbox-home/cache/cargo", Env: "CARGO_HOME", Tree: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skip(err)
	}
	policy, ok := sandbox.BackendPolicy(backend)
	if !ok || !hasToolchainExecutable(policy.Toolchains.BinDirs, "cargo") {
		t.Skip("an exposed cargo toolchain is unavailable")
	}
	ws, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := ws.OpenDirectory(".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pinned.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := process.Run(ctx, process.Options{
		Dir: ws.Root(), DirFile: pinned,
		Command: `cargo --version && rustc --version`,
		Sandbox: backend, RequireSandbox: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf(
			"host toolchain was not reusable: exit=%d stdout=%s stderr=%s",
			result.ExitCode,
			result.Stdout,
			result.Stderr,
		)
	}
}

func TestDeclaredNodeRuntimeIsReusableInSandbox(t *testing.T) {
	root := t.TempDir()
	backend, err := declaredToolBackend(t, root, []environment.ResourceRequest{declaredTLSConfig(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skip(err)
	}
	policy, ok := sandbox.BackendPolicy(backend)
	if !ok || !hasToolchainExecutable(policy.Toolchains.BinDirs, "node") {
		t.Skip("an exposed Node.js runtime is unavailable")
	}
	ws, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := ws.OpenDirectory(".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pinned.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := process.Run(ctx, process.Options{
		Dir: ws.Root(), DirFile: pinned,
		Command: `node --version && npm --version && ` +
			`node -e 'const c=require("node:child_process");` +
			`const r=c.spawnSync("/bin/sh",["-c","exit 0"]);` +
			`if(r.error)throw r.error;process.exit(r.status??1)'`,
		Sandbox: backend, RequireSandbox: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf(
			"host Node.js runtime was not reusable: exit=%d stdout=%s stderr=%s",
			result.ExitCode,
			result.Stdout,
			result.Stderr,
		)
	}
}

func hasToolchainExecutable(directories []string, name string) bool {
	for _, directory := range directories {
		info, err := os.Stat(filepath.Join(directory, name))
		if err == nil && info.Mode().IsRegular() &&
			info.Mode().Perm()&0o111 != 0 {
			return true
		}
	}
	return false
}

func declaredToolBackend(t *testing.T, workspace string, declarations []environment.ResourceRequest) (sandbox.Backend, error) {
	t.Helper()
	private, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		return nil, err
	}
	prepared, err := envprep.Prepare(t.Context(), envprep.Options{
		Sandbox:      sandbox.Options{WorkspaceRoot: workspace, PrivateTemp: private, EnvironmentProfile: environment.ProfileIsolated},
		Declarations: declarations,
	})
	if err != nil {
		return nil, err
	}
	return sandbox.NewPlatformBackend(prepared.Sandbox)
}

// Runtime crypto configuration is an explicit file declaration, separate from
// automatically bound library files and validated public trust certificates.
func declaredTLSConfig(t *testing.T) environment.ResourceRequest {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crypto.cnf")
	if err := os.WriteFile(path, []byte("# test crypto configuration\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return environment.ResourceRequest{Name: "crypto-config", Namespace: environment.NamespaceHostConfig, Access: environment.AccessRead, Path: path, Env: "OPENSSL_CONF"}
}

func TestIsolatedSandboxEnvironmentBaseline(t *testing.T) {
	root := t.TempDir()
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot:       root,
		PrivateTemp:         t.TempDir(),
		AllowNetwork:        false,
		EnvironmentContract: "v1",
		EnvironmentProfile:  "isolated",
	})
	if err != nil {
		t.Fatalf("unavailable: construct sandbox: %v", err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Fatalf("unavailable: %v", err)
	}
	policy, ok := sandbox.BackendPolicy(backend)
	if !ok {
		t.Fatalf("unavailable: sandbox backend has no policy")
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatalf("unavailable: %v", err)
	}
	directory, err := workspace.OpenDirectory(".")
	if err != nil {
		t.Fatalf("unavailable: %v", err)
	}
	t.Cleanup(func() { _ = directory.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	temp, err := process.Run(ctx, process.Options{
		Dir: workspace.Root(), DirFile: directory,
		Command: `printf '%s\n' "$HOME" "$TMPDIR"; mktemp -d`,
		Sandbox: backend, RequireSandbox: true, WorkspaceReadOnly: true,
	})
	if err != nil {
		t.Fatalf("unavailable: run mktemp probe: %v", err)
	}
	if !strings.Contains(temp.Stdout, policy.PrivateTemp) {
		t.Fatalf(
			"isolated HOME/TMPDIR baseline drifted: stdout=%q private=%q",
			temp.Stdout,
			policy.PrivateTemp,
		)
	}
	if temp.ExitCode == 0 {
		t.Fatal("isolated mktemp -d succeeded; isolated contract should deny the Darwin user temp")
	}
	if !strings.Contains(temp.Stderr+temp.Stdout, "Operation not permitted") &&
		!strings.Contains(temp.Stderr+temp.Stdout, "not permitted") {
		t.Fatalf(
			"isolated mktemp failure is not the documented EPERM baseline: exit=%d stdout=%q stderr=%q",
			temp.ExitCode,
			temp.Stdout,
			temp.Stderr,
		)
	}

	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("unavailable: go toolchain: %v", err)
	}
	sandboxed, err := process.Run(ctx, process.Options{
		Dir: workspace.Root(), DirFile: directory,
		Command: `go env HOME GOENV`,
		Sandbox: backend, RequireSandbox: true, WorkspaceReadOnly: true,
	})
	if err != nil {
		t.Fatalf("unavailable: run go env probe: %v", err)
	}
	if sandboxed.ExitCode != 0 {
		t.Fatalf(
			"unavailable: go env failed in sandbox: stdout=%q stderr=%q",
			sandboxed.Stdout,
			sandboxed.Stderr,
		)
	}
	if !strings.Contains(sandboxed.Stdout, policy.PrivateTemp) {
		t.Fatalf("sandbox go env HOME/GOENV is not private temp: %q", sandboxed.Stdout)
	}
}

func TestV1NativeSharedUserTempAllowsMktemp(t *testing.T) {
	userTemp, err := envprep.UserTempDir()
	if err != nil {
		t.Fatalf("unavailable: resolve Darwin user temp: %v", err)
	}
	// Keep the workspace outside the shared temp root: granting a workspace's
	// parent as a host write root must remain forbidden.
	root, err := os.MkdirTemp("/private/tmp", "qcode-shared-temp-workspace-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	private, err := os.MkdirTemp("/private/tmp", "qcode-shared-temp-private-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(private) })
	prepared, err := envprep.Prepare(t.Context(), envprep.Options{
		Sandbox: sandbox.Options{
			WorkspaceRoot: root, PrivateTemp: private,
			EnvironmentContract: "v1", EnvironmentProfile: "native", SharedUserTemp: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(prepared.Sandbox)
	if err != nil {
		t.Fatalf("unavailable: construct sandbox: %v", err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Fatalf("unavailable: %v", err)
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatalf("unavailable: %v", err)
	}
	directory, err := workspace.OpenDirectory(".")
	if err != nil {
		t.Fatalf("unavailable: %v", err)
	}
	t.Cleanup(func() { _ = directory.Close() })

	existing, err := os.CreateTemp(userTemp, "qcode-shared-existing-")
	if err != nil {
		t.Fatal(err)
	}
	existingPath := existing.Name()
	if _, err := existing.WriteString("unrelated shared content"); err != nil {
		t.Fatal(err)
	}
	if err := existing.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(existingPath) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := process.Run(ctx, process.Options{
		Dir: workspace.Root(), DirFile: directory,
		Env:     []string{"EXISTING_SHARED_FILE=" + filepath.Clean(existingPath)},
		Command: `if cat "$EXISTING_SHARED_FILE" >/dev/null 2>&1; then exit 91; fi; printf '%s\n' "$TMPDIR"; created=$(mktemp -d) || exit 1; trap 'rmdir "$created"' EXIT; printf '%s\n' "$created"`,
		Sandbox: backend, RequireSandbox: true, WorkspaceReadOnly: true,
	})
	if err != nil {
		t.Fatalf("unavailable: run shared mktemp: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf(
			"native shared_user_temp mktemp -d failed: exit=%d stdout=%q stderr=%q",
			result.ExitCode, result.Stdout, result.Stderr,
		)
	}
	if !strings.Contains(result.Stdout, userTemp) {
		t.Fatalf("shared mktemp output %q does not use %q", result.Stdout, userTemp)
	}
}

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
	gate := egress.NewStaticGate()
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
				ManagedProxyCredential: proxy.Credential(),
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
			for _, grant := range []string{"loopback-only", "denied", "managed-and-loopback"} {
				if proxyPort == 0 && grant == "managed-and-loopback" {
					continue
				}
				t.Run(grant, func(t *testing.T) {
					resources := []tool.Resource{{
						Kind: "repo", Path: base.WorkspaceRoot, Access: tool.AccessRead, Tree: true,
					}}
					want := securitymodel.NetworkDenied
					if grant != "denied" {
						resources = append(resources, tool.Resource{
							Kind: "host", ID: "localhost", Protocol: securitymodel.LoopbackProtocol, Access: tool.AccessRead,
						})
						want = securitymodel.NetworkLoopbackAny
					}
					if grant == "managed-and-loopback" {
						resources = append(resources, tool.Resource{
							Kind: "host", ID: target.Host, Protocol: target.Protocol,
							Port: target.Port, Methods: target.Methods, AllowPrivate: true,
							Access: tool.AccessWrite,
						})
						want = securitymodel.NetworkProxyTargets
					}
					profile, err := compileTestProfile(authority.CompileInput{
						Runtime: policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
						Invocation: resolvePolicyFixture(policyInvocationFixture{
							CallID: "approved-loopback", Tool: "exec_command",
							Arguments: json.RawMessage(`{"command":"curl"}`), Validated: true,
							Capability: tool.CapabilityProcess, Access: tool.AccessRead,
							Sandbox: tool.SandboxStrong, Resources: resources,
						}),
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

// policyInvocationFixture keeps test declarations separate from the immutable assessment
// sent to policy. Mutating a fixture requires an explicit resolveAssessment call.
type policyInvocationFixture struct {
	CallID, Tool, Source string
	Arguments            json.RawMessage
	Resources            []tool.Resource
	Capability           tool.Capability
	Access               tool.AccessMode
	Sandbox              tool.SandboxRequirement
	Effect               tool.EffectContract
	Declared             securitymodel.Declared
	Journaled, Validated bool
	Workspace            string
	Stage                string
}

func (i policyInvocationFixture) resolveAssessment() securitymodel.Assessment {
	contract := i.Effect
	if i.Journaled {
		contract.WorkspaceTransaction = tool.TransactionBeforeImage
	} else {
		contract.WorkspaceTransaction = tool.TransactionNone
	}
	return tool.AssessResources(tool.TrustedBinding{
		Capability: i.Capability, AccessMode: i.Access, SandboxRequirement: i.Sandbox,
		Effect: contract,
	}, i.Declared, i.Resources)
}

func resolvePolicyFixture(i policyInvocationFixture) policy.Invocation {
	return policy.Invocation{CallID: i.CallID, Tool: i.Tool, Source: tool.CatalogSourceKind(i.Tool, i.Source), Arguments: i.Arguments,
		Assessment: i.resolveAssessment(), Approval: i.Effect.Approval, Validated: i.Validated,
		Workspace: i.Workspace, Stage: policy.Stage(i.Stage)}
}

// Isolated posture: HOME/TMPDIR stay in PrivateTemp so compiler outputs
// do not land on host /tmp. The shared-temp posture is
// TestV1NativeSharedUserTempAllowsMktemp.
func TestSandboxCompilerUsesPrivateTempAndHostTmpRemainsDenied(t *testing.T) {
	if err := exec.Command("/usr/bin/xcrun", "--find", "clang++").Run(); err != nil {
		t.Skipf("xcrun clang++ unavailable: %v", err)
	}
	t.Setenv("TMPDIR", "/var/folders/host/T")
	t.Setenv("TMP", "/tmp")
	t.Setenv("TEMP", "/private/tmp")
	root, err := os.MkdirTemp("/private/tmp", "qcode-compiler-workspace-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	private, err := os.MkdirTemp("/private/tmp", "qcode-compiler-private-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(private) })

	if err := os.WriteFile(
		filepath.Join(root, "probe.cc"),
		[]byte("#include <cassert>\n#include <vector>\nint main() { std::vector<int> v(1, 42); assert(v[0] == 42); }\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	prepared, err := envprep.Prepare(t.Context(), envprep.Options{
		Sandbox: sandbox.Options{
			WorkspaceRoot:       root,
			PrivateTemp:         private,
			AllowNetwork:        false,
			EnvironmentContract: "v1",
			EnvironmentProfile:  "isolated",
		},
		SourceEnv: os.Environ(),
	})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(prepared.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skipf("strong sandbox unavailable: %v", err)
	}
	policy, ok := sandbox.BackendPolicy(backend)
	if !ok {
		t.Fatal("sandbox backend has no policy")
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := workspace.OpenDirectory(".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })

	hostTmpTarget := fmt.Sprintf("/tmp/qcode-private-temp-%d", os.Getpid())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := process.Run(ctx, process.Options{
		Dir: workspace.Root(), DirFile: directory,
		Command: fmt.Sprintf(
			`printf '%%s\n' "$TMPDIR" "$TMP" "$TEMP"; `+
				`printf private > "$TMPDIR/probe"; `+
				`if printf escaped > %q; then exit 91; fi; `+
				`clang++ probe.cc -o "$TMPDIR/probe.o" && "$TMPDIR/probe.o"`,
			hostTmpTarget,
		),
		Sandbox: backend, RequireSandbox: true, WorkspaceReadOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf(
			"compiler failed: exit=%d stdout=%q stderr=%q",
			result.ExitCode,
			result.Stdout,
			result.Stderr,
		)
	}
	wantEnvironment := strings.Repeat(policy.PrivateTemp+"\n", 3)
	if result.Stdout != wantEnvironment {
		t.Fatalf("temporary environment = %q, want %q", result.Stdout, wantEnvironment)
	}
	for _, name := range []string{"probe", "probe.o"} {
		if _, err := os.Stat(filepath.Join(policy.PrivateTemp, name)); err != nil {
			t.Fatalf("private temp artifact %s: %v", name, err)
		}
	}
	baseline, baselineErr := process.Run(ctx, process.Options{
		Dir: workspace.Root(), DirFile: directory,
		Command: `unset SDKROOT
clang++ probe.cc -o "$TMPDIR/probe-without-sdk"`,
		Sandbox: backend, RequireSandbox: true, WorkspaceReadOnly: true,
	})
	t.Logf("without projected SDKROOT: exit=%d error=%v stderr=%s",
		baseline.ExitCode, baselineErr, baseline.Stderr)
	if _, err := os.Stat(hostTmpTarget); !os.IsNotExist(err) {
		t.Fatalf("host /tmp write escaped sandbox: %v", err)
	}
}

func TestRealSandboxAttackCorpus(t *testing.T) {
	if os.Getenv("QCODE_SANDBOX_STAGE") != "1" {
		t.Skip("real sandbox attack corpus runs in the required sandbox stage")
	}
	root := t.TempDir()
	external := t.TempDir()
	secretValue := "fixture-secret-never-host-data"
	secret := filepath.Join(external, "secret")
	if err := os.WriteFile(filepath.Join(root, "workspace"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte(secretValue), 0o600); err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sandbox.CloseBackend(backend)
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Fatal(err)
	}
	directory, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	root = directory.Root()
	pinned, err := directory.OpenDirectory(".")
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()

	run := func(tb testing.TB, command string) process.Result {
		tb.Helper()
		result, runErr := process.Run(tb.Context(), process.Options{
			Command: command, Dir: root, DirFile: pinned,
			Sandbox: backend, RequireSandbox: true,
		})
		if runErr != nil {
			tb.Fatal(runErr)
		}
		if strings.Contains(result.Stdout+result.Stderr, secretValue) {
			tb.Fatalf("fixture secret leaked for %q", command)
		}
		return result
	}
	if result := run(t, `cat workspace; test "$(cat <<'EOF'
heredoc
EOF
)" = heredoc; printf written > created; sh -c 'cat workspace'`); result.ExitCode != 0 {
		t.Fatalf("workspace/child command failed: %+v", result)
	}
	attacks := []struct {
		name    string
		command string
	}{
		{"external-read", "cat " + shellQuote(secret)},
		{"external-write", "printf escaped > " + shellQuote(filepath.Join(external, "escaped"))},
		{"host-temp-write", `printf escaped > "/private/tmp/qcode-sandbox-attack-$$"`},
		{"host-var-temp-lexical-write", `printf escaped > "/var/tmp/qcode-sandbox-attack-$$"`},
		{"host-var-temp-write", `printf escaped > "/private/var/tmp/qcode-sandbox-attack-$$"`},
		{"symlink-read", "ln -s " + shellQuote(external) + " link && cat link/secret"},
		{"network", "/usr/bin/nc -w 1 127.0.0.1 9"},
		{"environment", `test -z "$QCODE_ATTACK_SECRET"`},
	}
	for _, attack := range attacks {
		t.Run(attack.name, func(t *testing.T) {
			t.Setenv("QCODE_ATTACK_SECRET", secretValue)
			result := run(t, attack.command)
			if attack.name == "symlink-read" {
				_ = os.Remove(filepath.Join(root, "link"))
			}
			if attack.name == "environment" {
				if result.ExitCode != 0 {
					t.Fatalf("secret environment was inherited: %+v", result)
				}
				return
			}
			if result.ExitCode == 0 {
				t.Fatalf("attack unexpectedly succeeded: %+v", result)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(external, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("outside write created a file: %v", err)
	}
	hardlink := filepath.Join(root, "hardlink-secret")
	if err := os.Link(secret, hardlink); err == nil {
		if _, err := process.NewCommand(t.Context(), process.Options{
			Command: "cat hardlink-secret", Dir: root, DirFile: pinned,
			Sandbox: backend, RequireSandbox: true,
		}); err == nil {
			t.Fatal("hard-linked external fixture was accepted")
		}
	}
}
