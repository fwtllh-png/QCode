package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type fullAccessProfileRecorder struct {
	sandbox.Backend
	profile *string
}

func (b fullAccessProfileRecorder) Policy() sandbox.Policy {
	policy, _ := sandbox.BackendPolicy(b.Backend)
	return policy
}

func (b fullAccessProfileRecorder) Prepare(ctx context.Context, command sandbox.Command) (sandbox.Command, error) {
	prepared, err := b.Backend.Prepare(ctx, command)
	if err == nil && b.profile != nil && len(prepared.Args) > 2 {
		*b.profile = prepared.Args[2]
	}
	return prepared, err
}

func fullAccessSystemRunner(t *testing.T, root string, profile *string) func(map[string]any) tool.Result {
	t.Helper()
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: root, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	return fullAccessBackendRunner(t, root, fullAccessProfileRecorder{Backend: backend, profile: profile})
}

func fullAccessBackendRunner(t *testing.T, root string, backend sandbox.Backend) func(map[string]any) tool.Result {
	t.Helper()
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skip(err)
	}
	manager := process.NewSessionManager(64 * 1024)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, root, manager, backend); err != nil {
		t.Fatal(err)
	}
	guarded, err := toolguard.New(toolguard.Options{Registry: registry, Workspace: root,
		Policy:    policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		Approvals: func(context.Context, toolguard.ApprovalRequest) error { return fmt.Errorf("unexpected approval") },
	})
	if err != nil {
		t.Fatal(err)
	}
	sequence := 0
	return func(args map[string]any) tool.Result {
		t.Helper()
		sequence++
		args["yield_time_ms"] = 30000
		args["timeout_ms"] = 30000
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
		defer cancel()
		result, err := guarded.Execute(ctx, fmt.Sprintf("system-%d", sequence), "exec_command", raw)
		if err != nil {
			t.Fatalf("execute: %v\n%s", err, result.Content)
		}
		if result.Execution == nil || len(result.Execution.Attempts) != 1 || !result.Execution.Attempts[0].FullAccess {
			t.Fatalf("missing Full Access receipt: %+v", result.Execution)
		}
		return result
	}
}

type fixtureSessionResolver struct{}

type fixtureProxySession struct {
	*egress.ManagedNetworkProxy
	gate *egress.Gate
}

func (s fixtureProxySession) Gate() *egress.Gate { return s.gate }
func (s fixtureProxySession) Close() error       { return s.ManagedNetworkProxy.Close(context.Background()) }

func (fixtureSessionResolver) OpenProcessSession(targets []egress.Target) (egress.ProcessSession, error) {
	gate := egress.NewStaticGate(targets...)
	// Configure the fixture resolver before starting the serving goroutine.
	gate.LookupIP = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	proxy, err := egress.StartManagedNetworkProxy(gate)
	if err != nil {
		return nil, err
	}
	return fixtureProxySession{ManagedNetworkProxy: proxy, gate: gate}, nil
}

func TestFullAccessManagedNetworkKeepsFileGrants(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "proxy-ok") }))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	proxy, err := egress.StartManagedNetworkProxy(egress.NewStaticGate())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	base, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: root, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true,
		ManagedProxyPort: sandbox.ManagedNetworkProxyPort(proxy.Port()), ManagedProxyCredential: proxy.Credential(),
	})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := egress.NewSessionBackend(base, fixtureSessionResolver{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	run := fullAccessBackendRunner(t, root, backend)
	targets := []map[string]any{{"host": "allowed.test", "protocol": "http", "port": port, "methods": []string{"GET"}, "allow_private": true}}
	result := run(map[string]any{
		"command":         fmt.Sprintf("mkdir -p results && /usr/bin/curl --noproxy '' --fail --silent --max-time 5 http://allowed.test:%d > results/download", port),
		"network_targets": targets,
	})
	if result.IsError || result.Execution.Attempts[0].NetworkMode != "proxy_targets" {
		t.Fatalf("managed network: %s; %+v", result.Content, result.Execution)
	}
	if content, err := os.ReadFile(filepath.Join(root, "results/download")); err != nil || string(content) != "proxy-ok" {
		t.Fatalf("managed download: %q %v", content, err)
	}
	for _, command := range []string{
		fmt.Sprintf("/usr/bin/curl --noproxy '' --fail --silent --max-time 5 http://undeclared.test:%d", port),
		"/usr/bin/curl --noproxy '*' --fail --silent --max-time 5 " + server.URL,
	} {
		if result := run(map[string]any{"command": command, "network_targets": targets}); !result.IsError {
			t.Fatalf("managed network escaped its scope: %s", command)
		}
	}
}

func TestFullAccessSystemFacilitiesAndNestedProtection(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt system facilities require macOS")
	}
	root := t.TempDir()
	var profile string
	run := fullAccessSystemRunner(t, root, &profile)
	// Exercise the exact IOKit operation Chromium crashed in, PTY allocation,
	// and credential-service denial without reading a real credential.
	source := `#include <IOKit/IOKitLib.h>
#include <servers/bootstrap.h>
#include <util.h>
#include <unistd.h>
#include <stdio.h>
int main(void) {
  int master, slave;
  if (openpty(&master, &slave, NULL, NULL, NULL)) return 1;
  close(master); close(slave);
  IONotificationPortRef port = IONotificationPortCreate(kIOMainPortDefault);
  if (!port || !IONotificationPortGetRunLoopSource(port)) return 2;
  IONotificationPortDestroy(port);
  mach_port_t service;
  if (bootstrap_look_up(bootstrap_port, "com.apple.SecurityServer", &service) == KERN_SUCCESS) return 3;
  puts("system-facilities-ok");
  return 0;
}`
	path := filepath.Join(root, "probe.c")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(t.Context(), "/usr/bin/cc", path, "-framework", "IOKit", "-framework", "CoreFoundation", "-o", filepath.Join(root, "probe")).CombinedOutput(); err != nil {
		t.Fatalf("build system probe: %v\n%s", err, output)
	}
	result := run(map[string]any{"command": "./probe"})
	if result.IsError || !strings.Contains(result.Content, "system-facilities-ok") {
		t.Fatalf("system facilities: %s", result.Content)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	// macOS permits reapplication only if the new profile retains every
	// existing restriction. It does not intersect a weaker child profile.
	if err := os.WriteFile(filepath.Join(root, "child.sb"), []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	result = run(map[string]any{"command": `/usr/bin/sandbox-exec -f child.sb /bin/sh -c 'printf nested > nested-output; if printf forbidden > .git/probe; then exit 1; fi'`})
	if result.IsError {
		t.Fatalf("nested sandbox: %s", result.Content)
	}
	if content, err := os.ReadFile(filepath.Join(root, "nested-output")); err != nil || string(content) != "nested" {
		t.Fatalf("nested output: %q %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git/probe")); !os.IsNotExist(err) {
		t.Fatalf("nested sandbox bypassed control protection: %v", err)
	}
	result = run(map[string]any{"command": `/usr/bin/sandbox-exec -p '(version 1)(allow default)' /usr/bin/true`})
	if !result.IsError {
		t.Fatal("child sandbox removed the parent's protections")
	}
}

func TestFullAccessPlaywright(t *testing.T) {
	module := os.Getenv("QCODE_TEST_PLAYWRIGHT_MODULE")
	if module == "" {
		t.Skip("set QCODE_TEST_PLAYWRIGHT_MODULE to the installed playwright package for browser acceptance")
	}
	if !filepath.IsAbs(module) {
		t.Fatal("QCODE_TEST_PLAYWRIGHT_MODULE must be an absolute package path")
	}
	root := t.TempDir()
	run := fullAccessSystemRunner(t, root, nil)
	encoded, _ := json.Marshal(module)
	script := `const { chromium } = require(` + string(encoded) + `);
const http = require('node:http');
const fs = require('node:fs/promises');
(async () => {
  const server = http.createServer((req, res) => res.end('<h1>full-access-e2e</h1>'));
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  let browser;
  try {
    browser = await chromium.launch({timeout: 15000});
    const page = await browser.newPage();
    await page.goto('http://127.0.0.1:' + server.address().port);
    if (await page.textContent('h1') !== 'full-access-e2e') throw Error('page mismatch');
    await fs.mkdir('results/browser', {recursive: true});
    await page.screenshot({path: 'results/browser/page.png'});
    console.log('browser-e2e-ok');
  } finally {
    if (browser) await browser.close();
    await new Promise(resolve => server.close(resolve));
  }
})().catch(err => {console.error(err); process.exitCode = 1;});`
	for _, loopback := range []bool{false, true} {
		t.Run(fmt.Sprintf("loopback=%t", loopback), func(t *testing.T) {
			result := run(map[string]any{"command": "node <<'JS'\n" + script + "\nJS", "allow_loopback": loopback})
			if result.IsError || !strings.Contains(result.Content, "browser-e2e-ok") {
				t.Fatalf("Playwright: %s", result.Content)
			}
			wantNetwork := "direct"
			if loopback {
				wantNetwork = "loopback_any"
			}
			if result.Execution.Attempts[0].NetworkMode != wantNetwork {
				t.Fatalf("network mode = %s", result.Execution.Attempts[0].NetworkMode)
			}
			info, err := os.Stat(filepath.Join(root, "results/browser/page.png"))
			if err != nil || info.Size() == 0 {
				t.Fatalf("missing screenshot: %v", err)
			}
		})
	}
}
