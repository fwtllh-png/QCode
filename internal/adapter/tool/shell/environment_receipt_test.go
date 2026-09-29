package shell

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/platform/environment"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/goproxy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestFailedExecCommandSurfacesAuthBindReportFacts(t *testing.T) {
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(
		registry, t.TempDir(), manager, passthroughBackend{},
	); err != nil {
		t.Fatal(err)
	}
	report := &goproxy.BindReport{}
	report.Record(environment.Fact{
		Source:         goproxy.Source,
		Category:       environment.CategoryCredentialUnavailable,
		RequiredAction: environment.ActionBindCredential,
		Resource:       "goproxy.corp.example",
		Detail:         "host GOPROXY credential is not present in netrc",
	})
	ctx := goproxy.WithBindReport(t.Context(), report)
	result := executeProcessToolContext(
		t, ctx, registry, processTestThread, "exec_command", map[string]any{
			"command": "printf '401 Unauthorized\\n'; exit 1",
		},
	)
	if !result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if result.Metadata["error_category"] != environment.CategoryCredentialUnavailable {
		t.Fatalf("error_category = %v metadata=%v", result.Metadata["error_category"], result.Metadata)
	}
	if result.Metadata["required_action"] != environment.ActionBindCredential {
		t.Fatalf("required_action = %v", result.Metadata["required_action"])
	}
	if !strings.Contains(
		strings.TrimSpace(result.Metadata["environment_detail"].(string)),
		"netrc",
	) {
		t.Fatalf("environment_detail = %v", result.Metadata["environment_detail"])
	}
}

func TestFailedExecCommandWithoutFactsIsUnknown(t *testing.T) {
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(
		registry, t.TempDir(), manager, passthroughBackend{},
	); err != nil {
		t.Fatal(err)
	}
	result := executeProcessTool(t, registry, processTestThread, "exec_command", map[string]any{
		"command": "printf '%s\\n' '401 Unauthorized' 'permission denied' 'GOPROXY'; exit 1",
	})
	if !result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if result.Metadata["error_category"] != environment.CategoryUnknown {
		t.Fatalf("error_category = %v metadata=%v", result.Metadata["error_category"], result.Metadata)
	}
	if _, ok := result.Metadata["required_action"]; ok {
		t.Fatalf("unknown must not invent required_action: %v", result.Metadata)
	}
	if result.Metadata["retry_original"] != false {
		t.Fatalf("retry_original = %v", result.Metadata["retry_original"])
	}
	content := strings.ToLower(result.Content)
	if !strings.Contains(content, "401") || !strings.Contains(content, "permission denied") {
		t.Fatalf("output was not preserved: %q", result.Content)
	}
	for _, banned := range []string{
		environment.CategoryEnvironmentResourceUnavailable,
		environment.CategoryCredentialUnavailable,
		environment.CategoryCredentialRejected,
		environment.CategoryFilesystemAccessDenied,
		environment.CategoryNetworkTargetUnapproved,
	} {
		if result.Metadata["error_category"] == banned {
			t.Fatalf("classified %q from stderr", banned)
		}
	}
}

func TestFailedExecCommandUsesSessionGateReceipts(t *testing.T) {
	gate := &egress.Gate{}
	_, _ = gate.Authorize(t.Context(), egress.Target{
		Host: "code.byted.org", Protocol: "https", Port: 443,
		Methods: []string{"CONNECT"},
	}, environment.SourceProcessProxy)
	protocol := &commandProtocol{
		networks: map[string]egress.ProcessSession{
			"sess": stubProcessSession{gate: gate},
		},
	}
	result := tool.Result{
		Content:  "Forbidden\nmanaged egress denied",
		IsError:  true,
		Metadata: map[string]any{},
	}
	attachMissingCapability(
		&result,
		protocol.sessionEnvironmentFacts(t.Context(), "sess", nil),
	)
	if result.Metadata["error_category"] != environment.CategoryNetworkTargetUnapproved {
		t.Fatalf("error_category = %v metadata=%v", result.Metadata["error_category"], result.Metadata)
	}
	if result.Metadata["required_action"] != environment.ActionApproveNetworkTarget {
		t.Fatalf("required_action = %v", result.Metadata["required_action"])
	}
}

func TestAttachMissingCapabilityIgnoresOutputText(t *testing.T) {
	result := tool.Result{
		Content:  "401 Unauthorized\npermission denied\nGOPROXY is not set",
		IsError:  true,
		Metadata: map[string]any{},
	}
	attachMissingCapability(&result, nil)
	if result.Metadata["error_category"] != environment.CategoryUnknown {
		t.Fatalf("empty facts = %v", result.Metadata["error_category"])
	}

	result = tool.Result{
		Content:  "401 Unauthorized",
		IsError:  true,
		Metadata: map[string]any{},
	}
	attachMissingCapability(&result, []environment.Fact{{
		Source:         environment.SourcePreparer,
		Category:       environment.CategoryEnvironmentResourceUnavailable,
		RequiredAction: environment.ActionApproveHostConfig,
		Detail:         "401 Unauthorized",
	}})
	if result.Metadata["error_category"] != environment.CategoryEnvironmentResourceUnavailable ||
		result.Metadata["required_action"] != environment.ActionApproveHostConfig {
		t.Fatalf("preparer fact overwritten by output: %v", result.Metadata)
	}

	result = tool.Result{
		Content:  "permission denied",
		IsError:  true,
		Metadata: map[string]any{},
	}
	attachMissingCapability(&result, []environment.Fact{{
		Source:         environment.SourceOSBackend,
		Category:       environment.CategoryFilesystemAccessDenied,
		RequiredAction: environment.ActionEnableSharedUserTemp,
		Detail:         "permission denied",
	}})
	if result.Metadata["error_category"] != environment.CategoryFilesystemAccessDenied {
		t.Fatalf("os backend fact = %v", result.Metadata["error_category"])
	}

	result = tool.Result{
		Metadata: map[string]any{
			"error_category":  "process_still_running",
			"required_action": "write_stdin",
		},
	}
	attachMissingCapability(&result, []environment.Fact{{
		Category:       environment.CategoryNetworkTargetUnapproved,
		RequiredAction: environment.ActionApproveNetworkTarget,
	}})
	if result.Metadata["error_category"] != "process_still_running" {
		t.Fatalf("running category replaced: %v", result.Metadata["error_category"])
	}
}

func TestExecCommandReportsUnsupportedSessionNetwork(t *testing.T) {
	if sandbox.SupportsManagedNetworkProxy() {
		t.Skip("this platform allocates process session channels")
	}
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(
		registry, t.TempDir(), manager, passthroughBackend{},
	); err != nil {
		t.Fatal(err)
	}
	result := executeProcessTool(t, registry, processTestThread, "exec_command", map[string]any{
		"command": "true",
		"network_targets": []map[string]any{{
			"host": "example.test", "protocol": "https", "port": 443,
			"methods": []string{"CONNECT"},
		}},
	})
	if !result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if result.Metadata["error_category"] != environment.CategoryBackendCapabilityUnsupported {
		t.Fatalf("error_category = %v metadata=%v", result.Metadata["error_category"], result.Metadata)
	}
	if manager.Count() != 0 {
		t.Fatalf("unsupported exec leaked a session: count=%d", manager.Count())
	}
}

func TestInheritedEnvironmentNetworkUsesUserGrantOnly(t *testing.T) {
	adapterOnly := inheritedEnvironmentNetwork(policyNetworkBackend{
		policy: sandbox.Policy{ID: "v1", EnvironmentContract: "v1"},
	})
	if len(adapterOnly) != 0 {
		t.Fatalf("empty inherited = %+v", adapterOnly)
	}
	targets := inheritedEnvironmentNetwork(policyNetworkBackend{
		policy: sandbox.Policy{
			ID:                  "v1",
			EnvironmentContract: "v1",
			EnvironmentNetwork: []sandbox.EnvironmentNetworkTarget{{
				Host: "declared.example", Protocol: "https", Port: 443,
				Methods: []string{"CONNECT"},
			}},
		},
	})
	if len(targets) != 1 || targets[0].Host != "declared.example" {
		t.Fatalf("v1 inherited = %+v", targets)
	}
}

func TestResolveProcessNetworkTargetsPrefersModelThenInherit(t *testing.T) {
	backend := policyNetworkBackend{
		policy: sandbox.Policy{
			ID:                  "v1",
			EnvironmentContract: "v1",
			EnvironmentNetwork: []sandbox.EnvironmentNetworkTarget{{
				Host: "declared.example", Protocol: "https", Port: 443,
				Methods: []string{"CONNECT"},
			}},
		},
	}
	inherited := resolveProcessNetworkTargets(backend, nil)
	if len(inherited) != 1 || inherited[0].Host != "declared.example" {
		t.Fatalf("inherit = %+v", inherited)
	}
	model := resolveProcessNetworkTargets(backend, []tool.DeclaredNetworkTarget{{
		Host: "model.example", Protocol: "https", Port: 443,
		Methods: []string{"CONNECT"},
	}})
	if len(model) != 1 || model[0].Host != "model.example" {
		t.Fatalf("model = %+v", model)
	}
}

func TestDeclaredNetworkTargetsGrantHTTPSByTunnelEndpoint(t *testing.T) {
	targets := declaredNetworkTargets([]tool.DeclaredNetworkTarget{
		{
			Host: "secure.example", Protocol: "https", Port: 443,
			Methods: []string{"GET", "POST"}, AllowPrivate: true,
		},
		{
			Host: "plain.example", Protocol: "http", Port: 80,
			Methods: []string{"GET"}, AllowPrivate: false,
		},
	})
	if len(targets) != 2 {
		t.Fatalf("targets = %+v", targets)
	}
	if targets[0].Protocol == "https" && len(targets[0].Methods) != 0 {
		t.Fatalf(
			"https target methods = %v, tunnel-endpoint grants must be method-agnostic",
			targets[0].Methods,
		)
	}
	if !targets[0].AllowPrivate {
		t.Fatal("https target lost allow_private")
	}
	if len(targets[1].Methods) != 1 || targets[1].Methods[0] != "GET" {
		t.Fatalf("http target methods = %v, per-method grants must persist", targets[1].Methods)
	}
}

func TestOpenProcessNetworkInheritsDeclaredGrant(t *testing.T) {
	if !sandbox.SupportsManagedNetworkProxy() {
		t.Skip("session channels are unsupported")
	}
	workspace := &egress.Gate{}
	proxy, err := egress.StartManagedNetworkProxy(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(t.Context()) })
	target := sandbox.EnvironmentNetworkTarget{
		Host: "declared.example", Protocol: "https", Port: 443,
		Methods: []string{"CONNECT"}, AllowPrivate: true,
	}
	backend := policyNetworkBackend{
		sessionOpeningPassthrough: sessionOpeningPassthrough{opener: proxy},
		policy: sandbox.Policy{
			ID: "v1", EnvironmentContract: "v1",
			EnvironmentNetwork: []sandbox.EnvironmentNetworkTarget{target},
		},
	}
	session, err := openProcessNetwork(
		backend,
		false,
		inheritedEnvironmentNetwork(backend),
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if _, err := workspace.Authorize(t.Context(), egress.Target{
		Host: target.Host, Protocol: target.Protocol, Port: target.Port,
		Methods: target.Methods, AllowPrivate: target.AllowPrivate,
	}, "test"); err == nil {
		t.Fatal("inherited grant leaked onto the workspace gate")
	}
}

type fixedSessionOpener struct{ session egress.ProcessSession }

func (o fixedSessionOpener) OpenProcessSession([]egress.Target) (egress.ProcessSession, error) {
	return o.session, nil
}

func TestBackgroundProcessCannotAskAfterItsCallReturns(t *testing.T) {
	if !sandbox.SupportsManagedNetworkProxy() {
		t.Skip("session channels are unsupported")
	}
	gate := &egress.Gate{
		LookupIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		},
	}
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	backend := &envCaptureBackend{sessionOpeningPassthrough: sessionOpeningPassthrough{
		opener: fixedSessionOpener{session: stubProcessSession{gate: gate}},
	}}
	if err := RegisterWithManagerAndBackend(registry, t.TempDir(), manager, backend); err != nil {
		t.Fatal(err)
	}
	var asked atomic.Int32
	ctx := egress.WithRuntimeApprover(t.Context(), func(context.Context, egress.Target) error {
		asked.Add(1)
		return nil
	})
	result := executeProcessToolContext(t, ctx, registry, processTestThread, "exec_command", map[string]any{
		"command":       "sleep 5",
		"yield_time_ms": 50,
		"network_targets": []map[string]any{{
			"host": "declared.example", "protocol": "https", "port": 443,
			"methods": []string{"CONNECT"}, "allow_private": false,
		}},
	})
	if result.Metadata["running"] != true {
		t.Fatalf("fixture must outlive its call: %+v", result)
	}
	if _, err := gate.AuthorizeBeforeConnect(context.Background(), egress.Target{
		Host: "exfil.example", Protocol: "https", Port: 443,
		Methods: []string{"CONNECT"},
	}, "test"); err == nil {
		t.Fatal("background process gained a target after its call returned")
	}
	if asked.Load() != 0 {
		t.Fatalf("approver asked %d times after the call returned", asked.Load())
	}
}

func TestOpenProcessNetworkFailsClosedWithoutOpener(t *testing.T) {
	if !sandbox.SupportsManagedNetworkProxy() {
		t.Skip("session channels are unsupported")
	}
	_, err := openProcessNetwork(
		managedProxyPassthrough{},
		false,
		[]egress.Target{{
			Host: "example.test", Protocol: "https", Port: 443,
			Methods: []string{"CONNECT"},
		}},
		false,
	)
	if !errors.Is(err, egress.ErrProcessSessionUnsupported) {
		t.Fatalf("openProcessNetwork() error = %v", err)
	}
	if !strings.Contains(err.Error(), environment.CategoryBackendCapabilityUnsupported) {
		t.Fatalf("unsupported error omitted category: %v", err)
	}
}

func TestOpenProcessNetworkBindsSessionGate(t *testing.T) {
	if !sandbox.SupportsManagedNetworkProxy() {
		t.Skip("session channels are unsupported")
	}
	workspace := &egress.Gate{}
	proxy, err := egress.StartManagedNetworkProxy(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(t.Context()) })
	target := egress.Target{
		Host: "example.test", Protocol: "https", Port: 443,
		Methods: []string{"CONNECT"}, AllowPrivate: true,
	}
	session, err := openProcessNetwork(
		sessionOpeningPassthrough{opener: proxy},
		false,
		[]egress.Target{target},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if session.Port() == 0 || session.Port() == proxy.Port() {
		t.Fatalf("session port = %d workspace=%d", session.Port(), proxy.Port())
	}
	if _, err := workspace.Authorize(t.Context(), target, "test"); err == nil {
		t.Fatal("session grant leaked onto the workspace gate")
	}
}

func TestOpenProcessNetworkOpensLoopbackSessionForAuthService(t *testing.T) {
	if !sandbox.SupportsManagedNetworkProxy() {
		t.Skip("session channels are unsupported")
	}
	workspace := &egress.Gate{}
	proxy, err := egress.StartManagedNetworkProxy(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(t.Context()) })
	session, err := openProcessNetwork(
		sessionOpeningPassthrough{opener: proxy},
		true,
		nil,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if session.Port() == 0 {
		t.Fatal("auth service session port was not allocated")
	}
	if _, err := workspace.Authorize(t.Context(), egress.Target{
		Host: "goproxy.example", Protocol: "https", Port: 443,
		Methods: []string{"CONNECT"},
	}, "test"); err == nil {
		t.Fatal("auth session granted the real GOPROXY host")
	}
}

func TestExecCommandRewritesGoproxyToSessionAndKeepsSecretOut(t *testing.T) {
	if !sandbox.SupportsManagedNetworkProxy() {
		t.Skip("session channels are unsupported")
	}
	workspace := &egress.Gate{}
	proxy, err := egress.StartManagedNetworkProxy(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(t.Context()) })
	backend := &envCaptureBackend{sessionOpeningPassthrough: sessionOpeningPassthrough{opener: proxy}}
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, t.TempDir(), manager, backend); err != nil {
		t.Fatal(err)
	}
	service, err := goproxy.New(goproxy.Binding{
		Upstream:   "http://127.0.0.1:9",
		Prefixes:   []string{"example.com/qcode/"},
		Credential: goproxy.CredentialRef{Kind: "env", Name: "QCODE_TEST_GOPROXY_TOKEN"},
	}, func(context.Context, string, string) (string, error) {
		return "user:secret-token", nil
	}, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	result := executeProcessToolContext(t, goproxy.WithService(t.Context(), service), registry, processTestThread, "exec_command", map[string]any{
		"command": "true",
		"env": map[string]string{
			"GOPROXY": "https://user:secret-token@proxy.example|direct",
		},
	})
	if result.IsError {
		t.Fatalf("result = %+v", result)
	}
	var rewritten string
	for _, entry := range backend.env {
		if strings.HasPrefix(entry, "GOPROXY=") {
			rewritten = entry
		}
	}
	endpoint, err := url.Parse(strings.TrimPrefix(rewritten, "GOPROXY="))
	if err != nil || endpoint.Scheme != "http" || endpoint.Hostname() != "127.0.0.1" ||
		endpoint.User.Username() != sandbox.ManagedProxyUser {
		t.Fatalf("GOPROXY = %q env=%v", rewritten, backend.env)
	}
	if password, _ := endpoint.User.Password(); password == "" {
		t.Fatalf("GOPROXY carries no session channel credential: %q", rewritten)
	}
	if strings.Contains(rewritten, "secret-token") ||
		strings.Contains(rewritten, "|direct") ||
		strings.Contains(rewritten, "proxy.example") {
		t.Fatalf("secret or upstream leaked into GOPROXY: %q", rewritten)
	}
}

func TestExecCommandAuthServiceFailsClosedWithoutSession(t *testing.T) {
	if sandbox.SupportsManagedNetworkProxy() {
		t.Skip("this platform allocates process session channels")
	}
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(
		registry, t.TempDir(), manager, passthroughBackend{},
	); err != nil {
		t.Fatal(err)
	}
	service, err := goproxy.New(goproxy.Binding{
		Upstream:   "http://127.0.0.1:9",
		Prefixes:   []string{"example.com/qcode/"},
		Credential: goproxy.CredentialRef{Kind: "env", Name: "QCODE_TEST_GOPROXY_TOKEN"},
	}, func(context.Context, string, string) (string, error) {
		return "user:secret-token", nil
	}, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	result := executeProcessToolContext(t, goproxy.WithService(t.Context(), service), registry, processTestThread, "exec_command", map[string]any{
		"command": "true",
	})
	if !result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if result.Metadata["error_category"] != environment.CategoryBackendCapabilityUnsupported {
		t.Fatalf("error_category = %v metadata=%v", result.Metadata["error_category"], result.Metadata)
	}
	if manager.Count() != 0 {
		t.Fatalf("unsupported auth exec leaked a session: count=%d", manager.Count())
	}
}

type policyNetworkBackend struct {
	sessionOpeningPassthrough
	policy sandbox.Policy
}

func (b policyNetworkBackend) Policy() sandbox.Policy { return b.policy }

type managedProxyPassthrough struct{ passthroughBackend }

func (managedProxyPassthrough) Capability() sandbox.Capability {
	capability := passthroughBackend{}.Capability()
	capability.ManagedProxy = true
	return capability
}

type sessionOpeningPassthrough struct {
	passthroughBackend
	opener egress.ProcessSessionOpener
}

func (sessionOpeningPassthrough) Capability() sandbox.Capability {
	capability := passthroughBackend{}.Capability()
	capability.ManagedProxy = true
	return capability
}

func (b sessionOpeningPassthrough) OpenProcessSession(
	targets []egress.Target,
) (egress.ProcessSession, error) {
	return b.opener.OpenProcessSession(targets)
}

type envCaptureBackend struct {
	sessionOpeningPassthrough
	env []string
}

func (b *envCaptureBackend) Prepare(
	ctx context.Context,
	command sandbox.Command,
) (sandbox.Command, error) {
	prepared, err := b.sessionOpeningPassthrough.Prepare(ctx, command)
	if err != nil {
		return sandbox.Command{}, err
	}
	prepared.PreparedProxyPort = command.SessionProxyPort
	b.env = append([]string(nil), prepared.Env...)
	return prepared, nil
}

type stubProcessSession struct {
	gate *egress.Gate
}

func (s stubProcessSession) Port() uint16       { return 1 }
func (s stubProcessSession) Credential() string { return "" }
func (s stubProcessSession) Gate() *egress.Gate { return s.gate }
func (s stubProcessSession) Close() error       { return nil }

func TestExecCommandRoutesGoproxyToStableWorkspaceChannel(t *testing.T) {
	if !sandbox.SupportsManagedNetworkProxy() {
		t.Skip("managed network proxy is unsupported")
	}
	root := t.TempDir()
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	backend, err := egress.NewManagedBackend(
		&egress.Gate{},
		sandbox.Options{
			WorkspaceRoot: root, PrivateTemp: t.TempDir(),
			SkipPATHReadRoots: true,
		},
		sandbox.NewPlatformBackend,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	workspacePort := sandbox.BackendManagedProxyPort(backend)
	if workspacePort == 0 {
		t.Fatal("managed backend exposes no workspace proxy port")
	}
	service, err := goproxy.New(goproxy.Binding{
		Upstream: "http://127.0.0.1:9",
		Prefixes: []string{"*"},
		Credential: goproxy.CredentialRef{
			Kind: goproxy.CredentialKindHost, Name: "fixture",
		},
	}, func(context.Context, string, string) (string, error) {
		return "token", nil
	}, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	egress.BindProtocolHandler(backend, service)

	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, root, manager, backend); err != nil {
		t.Fatal(err)
	}
	// No network_targets and no allow_loopback: the bound auth service keeps
	// module fetches online and GOPROXY must point at the pre-authorized
	// workspace channel, not a per-command session port.
	result := executeProcessToolContext(
		t, goproxy.WithService(t.Context(), service), registry,
		processTestThread, "exec_command", map[string]any{
			"command": `printf '%s' "$GOPROXY"`,
		},
	)
	if result.IsError {
		t.Fatalf("result = %+v", result)
	}
	want := sandbox.ManagedProxyURL(workspacePort, sandbox.BackendManagedProxyCredential(backend))
	if result.Content != want || !strings.Contains(want, "@") {
		t.Fatalf("GOPROXY = %q, want %q", result.Content, want)
	}
}
