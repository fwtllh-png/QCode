package wire

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestWiredNetworkGrantsIsolateProviderWebAndProcess(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Method+" ok")
	})
	providerServer := httptest.NewTLSServer(handler)
	t.Cleanup(providerServer.Close)
	webServer := httptest.NewServer(handler)
	t.Cleanup(webServer.Close)
	t.Setenv("QCODE_WEB_SEARCH_URL", webServer.URL)
	workspace, providerID, modelID := t.TempDir(), "local", "egress-fixture"
	protocol, tools := string(model.ProtocolOpenAIChat), true
	var built *buildState
	modules := []buildModule{
		configModule{}, providerModule{}, persistenceModule{}, platformModule{},
		builtinToolsModule{}, securityModule{},
		buildModuleFunc{name: "capture", fn: func(_ context.Context, state *buildState) error {
			built = state
			return nil
		}},
	}
	session, err := newExec(t.Context(), withNonDurableTestJournal(t, ExecOptions{
		BaseURL:       providerServer.URL,
		Permission:    "suggest",
		ModelMetadata: ModelMetadataOptions{Descriptor: fixtureModel(modelID)},
		Skills:        SkillOptions{UserHome: t.TempDir(), DataDir: t.TempDir()},
		ConfigOverrides: config.Overrides{
			Workspace: &workspace, Provider: &providerID, Model: &modelID,
			Protocol: &protocol, Tools: &tools,
		},
	}), modules)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	providerClient := egress.WrapClient(providerServer.Client(), built.provider.egress)
	backendPolicy, ok := sandbox.BackendPolicy(built.platform.backend)
	if !ok || backendPolicy.ManagedProxyPort == 0 {
		t.Fatal("workspace has no managed network proxy")
	}
	proxyURL := &url.URL{Scheme: "http", Host: net.JoinHostPort(
		"127.0.0.1", strconv.Itoa(int(backendPolicy.ManagedProxyPort)),
	)}
	transport := providerServer.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	transport.DisableKeepAlives = true
	processClient := &http.Client{Transport: transport}
	t.Cleanup(processClient.CloseIdleConnections)
	t.Cleanup(providerClient.CloseIdleConnections)
	t.Cleanup(built.platform.web.HTTP.CloseIdleConnections)

	assertHTTP := func(client *http.Client, method, endpoint string, want int) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != want {
			t.Fatalf("%s %s: status=%d body=%q err=%v, want %d",
				method, endpoint, response.StatusCode, body, err, want)
		}
		if want == http.StatusOK && string(body) != method+" ok" {
			t.Fatalf("%s: upstream body=%q", method, body)
		}
	}
	assertDenied := func(gate *egress.Gate, target egress.Target) {
		t.Helper()
		if _, err := gate.Authorize(t.Context(), target, "test"); !errors.Is(err, egress.ErrDenied) {
			t.Fatalf("target=%+v: err=%v, want denied", target, err)
		}
	}
	targetOf := func(endpoint, method string) egress.Target {
		t.Helper()
		parsed, err := url.Parse(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		port, err := strconv.ParseUint(parsed.Port(), 10, 16)
		if err != nil {
			t.Fatal(err)
		}
		return egress.Target{
			Host: parsed.Hostname(), Protocol: parsed.Scheme, Port: uint16(port),
			Methods: []string{method}, AllowPrivate: true,
		}
	}

	assertHTTP(providerClient, http.MethodPost, providerServer.URL, http.StatusOK)
	assertHTTP(built.platform.web.HTTP, http.MethodGet, webServer.URL, http.StatusOK)
	assertHTTP(processClient, http.MethodGet, webServer.URL, http.StatusForbidden)
	assertDenied(built.platform.processEgress, targetOf(providerServer.URL, http.MethodConnect))
	assertDenied(built.platform.webEgress, targetOf(providerServer.URL, http.MethodGet))
	assertDenied(built.provider.egress, targetOf(webServer.URL, http.MethodGet))
	if response, err := processClient.Get(providerServer.URL); err == nil {
		response.Body.Close()
		t.Fatal("process inherited the provider's fixed CONNECT permission")
	}

	allow := built.security.guardFactory.onNetworkAllow
	allow(tool.CapabilityProcess, targetOf(providerServer.URL, http.MethodConnect))
	assertHTTP(processClient, http.MethodGet, providerServer.URL, http.StatusOK)
	// A process CONNECT grant must neither revoke provider POST nor grant direct POST.
	assertHTTP(providerClient, http.MethodPost, providerServer.URL, http.StatusOK)
	assertDenied(built.platform.processEgress, targetOf(providerServer.URL, http.MethodPost))
	assertDenied(built.platform.webEgress, targetOf(providerServer.URL, http.MethodGet))

	allow(tool.CapabilityNetwork, targetOf(providerServer.URL, http.MethodGet))
	assertDenied(built.platform.processEgress, targetOf(providerServer.URL, http.MethodGet))
	assertDenied(built.platform.webEgress, targetOf(providerServer.URL, http.MethodConnect))
	allow(tool.CapabilityProcess, targetOf(webServer.URL, http.MethodGet))
	assertHTTP(processClient, http.MethodGet, webServer.URL, http.StatusOK)
	assertHTTP(processClient, http.MethodDelete, webServer.URL, http.StatusForbidden)
	assertHTTP(built.platform.web.HTTP, http.MethodPost, webServer.URL, http.StatusOK)
	assertDenied(built.provider.egress, targetOf(webServer.URL, http.MethodGet))

	// A Guard call must not inherit either fixed backend or other call grants.
	ctx, closeScope := egress.WithScope(t.Context())
	defer closeScope()
	for _, endpoint := range []string{webServer.URL, providerServer.URL} {
		if _, err := built.platform.webEgress.Authorize(ctx, targetOf(endpoint, http.MethodGet), "web"); !errors.Is(err, egress.ErrDenied) {
			t.Fatalf("Web call inherited shared grant for %s: %v", endpoint, err)
		}
	}
	egress.AllowInScope(ctx, targetOf(webServer.URL, http.MethodGet))
	if _, err := built.platform.webEgress.Authorize(ctx, targetOf(webServer.URL, http.MethodGet), "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := built.provider.egress.Authorize(ctx, targetOf(providerServer.URL, http.MethodPost), "provider"); err != nil {
		t.Fatalf("Web scope affected provider: %v", err)
	}
}
