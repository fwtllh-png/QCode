package web_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/fwtllh-png/QCode/internal/adapter/mcp"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	gittool "github.com/fwtllh-png/QCode/internal/adapter/tool/git"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	threadstate "github.com/fwtllh-png/QCode/internal/host/runtimeapi/thread"
	webhost "github.com/fwtllh-png/QCode/internal/host/runtimeapi/web"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/platform/workspacequery"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	apppersistence "github.com/fwtllh-png/QCode/internal/runtime/app/persistence"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/internal/security/vcsbroker"
	"github.com/fwtllh-png/QCode/internal/security/workspacebroker"
)

func TestBootstrapIsLoopbackFencedAndDoesNotCacheToken(t *testing.T) {
	server := newTestServer(t, "127.0.0.1:43210")
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:43210/api/v1/bootstrap", nil)
	request.Host = "127.0.0.1:43210"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var value struct {
		Token string `json:"token"`
		Ready bool   `json:"ready"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if len(value.Token) < 40 || value.Ready {
		t.Fatalf("bootstrap = %+v", value)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache control = %q", response.Header().Get("Cache-Control"))
	}
	if policy := response.Header().Get("Content-Security-Policy"); !strings.Contains(
		policy,
		"img-src 'self' data: blob: https:",
	) {
		t.Fatalf("content security policy = %q", policy)
	}
	if response.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("referrer policy = %q", response.Header().Get("Referrer-Policy"))
	}

	rebound := httptest.NewRequest(http.MethodGet, "http://evil.test/api/v1/bootstrap", nil)
	rebound.Host = "evil.test"
	rejected := httptest.NewRecorder()
	server.Handler().ServeHTTP(rejected, rebound)
	if rejected.Code != http.StatusForbidden {
		t.Fatalf("rebound status = %d", rejected.Code)
	}
}

func TestSetupIsCapabilityFencedBeforeRuntimeActivation(t *testing.T) {
	identity, err := protocol.NewWorkspaceIdentity(
		"file:///workspace",
		"/workspace",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	var applied webhost.SetupRequest
	server, err := webhost.New(webhost.Options{
		Assets: fstest.MapFS{
			"index.html": {Data: []byte("<main>QCode</main>")},
		},
		ExpectedHost: "127.0.0.1:43210",
		Origin:       "http://127.0.0.1:43210",
		Token:        "setup-token",
		Setup: &webhost.SetupOptions{
			WorkspaceRoot:     "/workspace",
			WorkspaceIdentity: identity,
			Apply: func(_ context.Context, request webhost.SetupRequest) error {
				applied = request
				return nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	health := httptest.NewRecorder()
	server.Handler().ServeHTTP(health, httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:43210/healthz",
		nil,
	))
	if health.Code != http.StatusOK ||
		!strings.Contains(health.Body.String(), `"status":"setup_required"`) {
		t.Fatalf("health = %d %s", health.Code, health.Body.String())
	}

	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:43210/api/v1/setup/apply",
		strings.NewReader(`{"model":"deepseek-chat","base_url":"https://api.deepseek.com/v1"}`),
	))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized setup status = %d", unauthorized.Code)
	}

	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:43210/api/v1/setup/apply",
		strings.NewReader(
			`{"model":"deepseek-chat","api_key":"secret-value","base_url":"https://api.deepseek.com/v1"}`,
		),
	)
	request.Header.Set("Authorization", "Bearer setup-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "setup-once")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("setup status = %d body=%s", response.Code, response.Body.String())
	}
	if applied.APIKey != "secret-value" || applied.Model != "deepseek-chat" {
		t.Fatalf("applied setup = %+v", applied)
	}
	if strings.Contains(response.Body.String(), "secret-value") {
		t.Fatal("setup response echoed the API key")
	}
}

func TestUnaryRequiresTokenAndRuntimeReadiness(t *testing.T) {
	server := newTestServer(t, "127.0.0.1:43210")
	request := httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:43210/api/v1/system/describe",
		strings.NewReader(`{}`),
	)
	request.Host = "127.0.0.1:43210"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", response.Code)
	}

	token := bootstrapToken(t, server, "127.0.0.1:43210")
	request = httptest.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:43210/api/v1/system/describe",
		strings.NewReader(`{}`),
	)
	request.Host = "127.0.0.1:43210"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestBootFailureAndBootstrapScriptAreNotCached(t *testing.T) {
	server := newTestServerWithAssets(t, "127.0.0.1:43210", fstest.MapFS{
		"index.html":         &fstest.MapFile{Data: []byte("<main>QCode</main>"), Mode: fs.FileMode(0o444)},
		"theme-bootstrap.js": &fstest.MapFile{Data: []byte("void 0"), Mode: fs.FileMode(0o444)},
	})
	server.FailBoot(errors.New("configuration is invalid"))

	health := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:43210/healthz", nil)
	health.Host = "127.0.0.1:43210"
	healthResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(healthResponse, health)
	if healthResponse.Code != http.StatusServiceUnavailable ||
		!strings.Contains(healthResponse.Body.String(), `"boot_failed"`) {
		t.Fatalf("health status=%d body=%s", healthResponse.Code, healthResponse.Body.String())
	}

	asset := httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:43210/theme-bootstrap.js",
		nil,
	)
	asset.Host = "127.0.0.1:43210"
	assetResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(assetResponse, asset)
	if got := assetResponse.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("theme bootstrap cache control = %q", got)
	}
}

func TestStaticAssetsRejectTraversalAndSupportETag(t *testing.T) {
	const host = "127.0.0.1:43210"
	server := newTestServerWithAssets(t, host, fstest.MapFS{
		"index.html": &fstest.MapFile{
			Data: []byte("<main>QCode</main>"), Mode: fs.FileMode(0o444),
		},
		"assets/app.js": &fstest.MapFile{
			Data: []byte("void 0"), Mode: fs.FileMode(0o444),
		},
		"assets/app.js.br": &fstest.MapFile{
			Data: []byte("brotli"), Mode: fs.FileMode(0o444),
		},
		"assets/app.js.gz": &fstest.MapFile{
			Data: []byte("gzip"), Mode: fs.FileMode(0o444),
		},
	})
	request := httptest.NewRequest(
		http.MethodGet,
		"http://"+host+"/assets/app.js",
		nil,
	)
	request.Host = host
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("ETag") == "" {
		t.Fatalf("asset status=%d headers=%v", response.Code, response.Header())
	}

	cached := httptest.NewRequest(
		http.MethodGet,
		"http://"+host+"/assets/app.js",
		nil,
	)
	cached.Host = host
	cached.Header.Set("If-None-Match", response.Header().Get("ETag"))
	cachedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(cachedResponse, cached)
	if cachedResponse.Code != http.StatusNotModified {
		t.Fatalf("cached status = %d", cachedResponse.Code)
	}

	compressed := httptest.NewRequest(
		http.MethodGet,
		"http://"+host+"/assets/app.js",
		nil,
	)
	compressed.Host = host
	compressed.Header.Set("Accept-Encoding", "gzip, br")
	compressedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(compressedResponse, compressed)
	if compressedResponse.Code != http.StatusOK ||
		compressedResponse.Header().Get("Content-Encoding") != "br" ||
		compressedResponse.Header().Get("Vary") != "Accept-Encoding" ||
		compressedResponse.Header().Get("Content-Type") != "text/javascript; charset=utf-8" ||
		compressedResponse.Body.String() != "brotli" {
		t.Fatalf(
			"compressed status=%d headers=%v body=%q",
			compressedResponse.Code,
			compressedResponse.Header(),
			compressedResponse.Body.String(),
		)
	}

	gzipOnly := httptest.NewRequest(
		http.MethodGet,
		"http://"+host+"/assets/app.js",
		nil,
	)
	gzipOnly.Host = host
	gzipOnly.Header.Set("Accept-Encoding", "*;q=1, br;q=0")
	gzipResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(gzipResponse, gzipOnly)
	if gzipResponse.Header().Get("Content-Encoding") != "gzip" ||
		gzipResponse.Body.String() != "gzip" {
		t.Fatalf(
			"gzip headers=%v body=%q",
			gzipResponse.Header(),
			gzipResponse.Body.String(),
		)
	}

	directCompressed := httptest.NewRequest(
		http.MethodGet,
		"http://"+host+"/assets/app.js.br",
		nil,
	)
	directCompressed.Host = host
	directCompressedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(directCompressedResponse, directCompressed)
	if directCompressedResponse.Code != http.StatusNotFound {
		t.Fatalf("direct compressed status = %d", directCompressedResponse.Code)
	}

	traversal := httptest.NewRequest(
		http.MethodGet,
		"http://"+host+"/%2e%2e/secret",
		nil,
	)
	traversal.Host = host
	traversalResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(traversalResponse, traversal)
	if traversalResponse.Code != http.StatusNotFound {
		t.Fatalf("traversal status = %d", traversalResponse.Code)
	}
}

func TestCapacityAppliesExactBodyIdentityAndConnectionLimits(t *testing.T) {
	const host = "127.0.0.1:43210"
	server := newTestServerWithOptions(t, webhost.Options{
		Assets: fstest.MapFS{
			"index.html": &fstest.MapFile{
				Data: []byte("<main>QCode</main>"), Mode: fs.FileMode(0o444),
			},
		},
		ExpectedHost: host,
		Origin:       "http://" + host,
		Capacity: webhost.Capacity{
			MaxJSONBodyBytes:       2,
			MaxWebSocketFrameBytes: 256,
			MaxReplayEvents:        10,
			MaxConnections:         1,
			MaxIdentityBytes:       4,
		},
	})
	runtime := app.NewRuntime(app.Options{})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	identity, err := protocol.NewWorkspaceIdentity("file:///workspace", "/workspace", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Activate(webhost.Dependencies{
		Runtime: runtime, WorkspaceRoot: "/workspace",
		WorkspaceIdentity: identity,
	}); err != nil {
		t.Fatal(err)
	}
	token := bootstrapToken(t, server, host)

	exact := postWeb(t, server, host, token, "system/describe", `{}`)
	if exact.Code != http.StatusOK {
		t.Fatalf("exact body status=%d body=%s", exact.Code, exact.Body.String())
	}
	over := postWeb(t, server, host, token, "system/describe", "{} ")
	if over.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over body status=%d body=%s", over.Code, over.Body.String())
	}

	longID := httptest.NewRequest(
		http.MethodPost,
		"http://"+host+"/api/v1/system/describe",
		strings.NewReader(`{}`),
	)
	longID.Host = host
	longID.Header.Set("Content-Type", "application/json")
	longID.Header.Set("Authorization", "Bearer "+token)
	longID.Header.Set("X-QCode-Request-ID", "12345")
	longIDResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(longIDResponse, longID)
	if longIDResponse.Code != http.StatusBadRequest {
		t.Fatalf("long identity status = %d", longIDResponse.Code)
	}
}

func TestRequestPanicIsContainedAndRedacted(t *testing.T) {
	const host = "127.0.0.1:43210"
	server := newTestServer(t, host)
	runtime := app.NewRuntime(app.Options{})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	identity, err := protocol.NewWorkspaceIdentity("file:///workspace", "/workspace", "")
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics strings.Builder
	if err := server.Activate(webhost.Dependencies{
		Runtime: runtime, WorkspaceRoot: "/workspace",
		WorkspaceIdentity: identity,
		Diagnostics:       &diagnostics,
		MCPHealth: func() []mcp.HealthSnapshot {
			panic("secret panic detail")
		},
	}); err != nil {
		t.Fatal(err)
	}
	response := postWeb(
		t,
		server,
		host,
		bootstrapToken(t, server, host),
		"system/diagnostics",
		`{}`,
	)
	if response.Code != http.StatusInternalServerError ||
		strings.Contains(response.Body.String(), "secret panic detail") ||
		strings.Contains(diagnostics.String(), "secret panic detail") {
		t.Fatalf(
			"panic status=%d body=%q diagnostics=%q",
			response.Code,
			response.Body.String(),
			diagnostics.String(),
		)
	}
}

func TestTraceQueryRejectsRequestsAboveCapacityBeforeRepositoryAccess(t *testing.T) {
	const host = "127.0.0.1:43212"
	server := newTestServerWithOptions(t, webhost.Options{
		Assets: fstest.MapFS{
			"index.html": &fstest.MapFile{
				Data: []byte("<main>QCode</main>"),
				Mode: fs.FileMode(0o444),
			},
		},
		ExpectedHost: host,
		Origin:       "http://" + host,
		Capacity:     webhost.Capacity{MaxTraceTurns: 2},
	})
	runtime := app.NewRuntime(app.Options{})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	identity, err := protocol.NewWorkspaceIdentity(
		"file:///workspace",
		"/workspace",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Activate(webhost.Dependencies{
		Runtime: runtime, WorkspaceRoot: "/workspace",
		WorkspaceIdentity: identity,
	}); err != nil {
		t.Fatal(err)
	}

	response := postWeb(
		t,
		server,
		host,
		bootstrapToken(t, server, host),
		"trace/query",
		`{"session_id":"session","turn_ids":["one","two","three"],"through_sequence":1}`,
	)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
}

func TestSystemDiagnosticsReportsAuthoritativeRuntimeHealth(t *testing.T) {
	const host = "127.0.0.1:43211"
	server := newTestServer(t, host)
	runtime := app.NewRuntime(app.Options{})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	identity, err := protocol.NewWorkspaceIdentity(
		"file:///workspace",
		"/workspace",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Activate(webhost.Dependencies{
		Runtime: runtime, WorkspaceRoot: "/workspace",
		WorkspaceIdentity: identity,
	}); err != nil {
		t.Fatal(err)
	}
	response := postWeb(
		t,
		server,
		host,
		bootstrapToken(t, server, host),
		"system/diagnostics",
		`{}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("diagnostics status=%d body=%s", response.Code, response.Body)
	}
	var envelope struct {
		Result struct {
			RuntimeHealth struct {
				ActiveTurns         int `json:"active_turns"`
				ActiveProviderCalls int `json:"active_provider_calls"`
				ActiveTools         int `json:"active_tool_executions"`
				Goroutines          int `json:"goroutines"`
				Trace               struct {
					DurableSource string `json:"durable_source"`
					RawAuthority  bool   `json:"raw_spans_table_authoritative"`
				} `json:"trace"`
			} `json:"runtime_health"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	health := envelope.Result.RuntimeHealth
	if health.ActiveTurns != 0 ||
		health.ActiveProviderCalls != 0 ||
		health.ActiveTools != 0 ||
		health.Goroutines == 0 ||
		health.Trace.DurableSource != "terminal_measurement" ||
		health.Trace.RawAuthority {
		t.Fatalf("runtime health = %+v", health)
	}
}

func TestRoutesSessionsAndEventsByWorkspace(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := listener.Addr().String()
	server := newTestServer(t, host)
	store, err := state.Open(t.Context(), state.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	repositories, err := apppersistence.NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	rootA := t.TempDir()
	rootB := t.TempDir()
	rootA, err = filepath.EvalSymlinks(rootA)
	if err != nil {
		t.Fatal(err)
	}
	rootB, err = filepath.EvalSymlinks(rootB)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(rootA, "README.md"),
		[]byte("Workspace A"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(rootB, "README.md"),
		[]byte("Workspace B"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{rootA, rootB} {
		if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
			t.Fatal(err)
		}
		if err := exec.Command("git", "-C", root, "add", "README.md").Run(); err != nil {
			t.Fatal(err)
		}
	}
	queryA, err := workspacequery.New(rootA, webTestBackend{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	queryB, err := workspacequery.New(rootB, webTestBackend{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	eventStoreA := app.NewMemoryEventStore(16)
	eventStoreB := app.NewMemoryEventStore(16)
	runtimeA := app.NewRuntime(app.Options{
		WorkspaceRoot: rootA, SessionLifecycle: repositories.Sessions,
		DefaultProfile: protocol.SessionProfile{Provider: "fixture", Model: "fixture"},
		EventStore:     eventStoreA,
	})
	runtimeB := app.NewRuntime(app.Options{
		WorkspaceRoot: rootB, SessionLifecycle: repositories.Sessions,
		DefaultProfile: protocol.SessionProfile{Provider: "fixture", Model: "fixture"},
		EventStore:     eventStoreB,
	})
	t.Cleanup(func() {
		_ = runtimeA.Close(context.Background())
		_ = runtimeB.Close(context.Background())
	})
	identityA, err := protocol.NewWorkspaceIdentity(
		(&url.URL{Scheme: "file", Path: rootA}).String(),
		rootA,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	identityB, err := protocol.NewWorkspaceIdentity(
		(&url.URL{Scheme: "file", Path: rootB}).String(),
		rootB,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Activate(webhost.Dependencies{
		Runtime: runtimeA, WorkspaceRoot: rootA, WorkspaceIdentity: identityA,
		Workspace: queryA,
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.AddWorkspace(webhost.Dependencies{
		Runtime: runtimeB, WorkspaceRoot: rootB, WorkspaceIdentity: identityB,
		Workspace: queryB,
	}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []struct {
		root      string
		sessionID string
		threadID  protocol.ThreadID
	}{
		{rootA, "session-a", "thread-a"},
		{rootB, "session-b", "thread-b"},
	} {
		if err := repositories.Sessions.EnsureSeed(
			t.Context(),
			value.sessionID,
			value.root,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := repositories.Threads.Create(
			t.Context(),
			threadstate.Thread{
				ID: value.threadID, SessionID: value.sessionID,
				Title: value.sessionID, Status: threadstate.ThreadOpen,
			},
		); err != nil {
			t.Fatal(err)
		}
	}
	for _, fixture := range []struct {
		store    *app.MemoryEventStore
		threadID protocol.ThreadID
		turnID   protocol.TurnID
		text     string
	}{
		{eventStoreA, "thread-a", "turn-a", "event-a"},
		{eventStoreB, "thread-b", "turn-b", "event-b"},
	} {
		event, err := protocol.NewEvent(protocol.EventMeta{
			Sequence: 1, OperationID: "operation-" + protocol.OperationID(fixture.turnID),
			ThreadID: fixture.threadID, TurnID: fixture.turnID,
			ItemID: "item-" + protocol.ItemID(fixture.turnID),
		}, &protocol.TurnCompletedData{Text: fixture.text})
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.store.Append(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}

	httpServer := &http.Server{Handler: server.Handler()}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Shutdown(context.Background()) })
	token := fetchBootstrapToken(t, "http://"+host)

	assertWorkspaceSessions := func(
		workspaceID string,
		wantSession string,
		unwantedSession string,
	) {
		t.Helper()
		response := postWebWorkspace(
			t,
			server,
			host,
			token,
			workspaceID,
			"session/list",
			`{"limit":10}`,
		)
		if response.Code != http.StatusOK ||
			!strings.Contains(response.Body.String(), wantSession) ||
			strings.Contains(response.Body.String(), unwantedSession) {
			t.Fatalf(
				"Workspace %s sessions status=%d body=%s",
				workspaceID,
				response.Code,
				response.Body.String(),
			)
		}
	}
	assertWorkspaceSessions(identityA.RootID, "session-a", "session-b")
	assertWorkspaceSessions(identityB.RootID, "session-b", "session-a")
	unscopedCreate := postWebWorkspace(
		t,
		server,
		host,
		token,
		"",
		"session/create",
		`{"session_id":"unscoped","title":"Unscoped"}`,
	)
	if unscopedCreate.Code != http.StatusBadRequest ||
		!strings.Contains(unscopedCreate.Body.String(), "select a ready workspace") {
		t.Fatalf(
			"unscoped session create status=%d body=%s",
			unscopedCreate.Code,
			unscopedCreate.Body,
		)
	}
	unknown := postWebWorkspace(
		t,
		server,
		host,
		token,
		"unknown-workspace",
		"session/list",
		`{"limit":10}`,
	)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown Workspace status=%d body=%s", unknown.Code, unknown.Body)
	}
	resource := postWebWorkspace(
		t,
		server,
		host,
		token,
		identityA.RootID,
		"workspace/resource",
		`{"path":"README.md"}`,
	)
	if resource.Code != http.StatusOK {
		t.Fatalf("Workspace resource status=%d body=%s", resource.Code, resource.Body)
	}
	var resourceEnvelope struct {
		Result struct {
			ContentHandle string `json:"content_handle"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resource.Body.Bytes(), &resourceEnvelope); err != nil {
		t.Fatal(err)
	}
	ownContentRequest := httptest.NewRequest(
		http.MethodGet,
		"http://"+host+"/api/v1/content/"+resourceEnvelope.Result.ContentHandle,
		nil,
	)
	ownContentRequest.Host = host
	ownContentRequest.Header.Set("Authorization", "Bearer "+token)
	ownContentRequest.Header.Set(
		"X-QCode-Workspace-ID",
		identityA.RootID,
	)
	ownContentResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(ownContentResponse, ownContentRequest)
	if ownContentResponse.Code != http.StatusOK ||
		ownContentResponse.Body.String() != "Workspace A" {
		t.Fatalf(
			"Workspace content status=%d body=%s",
			ownContentResponse.Code,
			ownContentResponse.Body,
		)
	}
	contentRequest := httptest.NewRequest(
		http.MethodGet,
		"http://"+host+"/api/v1/content/"+resourceEnvelope.Result.ContentHandle,
		nil,
	)
	contentRequest.Host = host
	contentRequest.Header.Set("Authorization", "Bearer "+token)
	contentRequest.Header.Set(
		"X-QCode-Workspace-ID",
		identityB.RootID,
	)
	contentResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(contentResponse, contentRequest)
	if contentResponse.Code != http.StatusForbidden {
		t.Fatalf(
			"cross-Workspace content status=%d body=%s",
			contentResponse.Code,
			contentResponse.Body,
		)
	}
	foreignTrace := postWebWorkspace(
		t,
		server,
		host,
		token,
		identityB.RootID,
		"trace/query",
		`{"session_id":"session-a","turn_ids":["turn-a"],"through_sequence":0}`,
	)
	if foreignTrace.Code != http.StatusConflict {
		t.Fatalf(
			"cross-Workspace trace status=%d body=%s",
			foreignTrace.Code,
			foreignTrace.Body,
		)
	}

	assertWorkspaceEvent := func(workspaceID, wantText string) {
		t.Helper()
		connection, _, err := websocket.Dial(
			t.Context(),
			"ws://"+host+"/api/v1/events",
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.CloseNow()
		if err := wsjson.Write(t.Context(), connection, map[string]any{
			"type": "authenticate", "token": token,
			"workspace_id": workspaceID, "cursor": 0,
		}); err != nil {
			t.Fatal(err)
		}
		var hello map[string]any
		if err := wsjson.Read(t.Context(), connection, &hello); err != nil {
			t.Fatal(err)
		}
		var frame struct {
			Event protocol.Event `json:"event"`
		}
		if err := wsjson.Read(t.Context(), connection, &frame); err != nil {
			t.Fatal(err)
		}
		completed, ok := frame.Event.Data.(*protocol.TurnCompletedData)
		if !ok || completed.Text != wantText {
			t.Fatalf("Workspace %s event = %+v", workspaceID, frame.Event)
		}
	}
	assertWorkspaceEvent(identityA.RootID, "event-a")
	assertWorkspaceEvent(identityB.RootID, "event-b")
}

func TestWebSocketDownlinkConcurrencyAndShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := listener.Addr().String()
	server := newTestServer(t, host)
	runtime := app.NewRuntime(app.Options{})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	identity, err := protocol.NewWorkspaceIdentity(
		"file:///workspace",
		"/workspace",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Activate(webhost.Dependencies{
		Runtime: runtime, WorkspaceRoot: "/workspace",
		WorkspaceIdentity: identity,
	}); err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: server.Handler()}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Shutdown(context.Background()) })

	token := fetchBootstrapToken(t, "http://"+host)
	connection, _, err := websocket.Dial(
		t.Context(),
		"ws://"+host+"/api/v1/events",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.CloseNow() })
	if err := wsjson.Write(
		t.Context(),
		connection,
		map[string]any{
			"type": "authenticate", "token": token,
			"workspace_id": identity.RootID, "cursor": 0,
		},
	); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var hello struct {
		Type string `json:"type"`
	}
	if err := wsjson.Read(ctx, connection, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.Type != "hello" {
		t.Fatalf("frame = %+v", hello)
	}
	if err := wsjson.Write(
		t.Context(),
		connection,
		map[string]any{"type": "unexpected"},
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := connection.Read(ctx); websocket.CloseStatus(err) !=
		websocket.StatusPolicyViolation {
		t.Fatalf("post-auth client frame close = %v", err)
	}
}

func TestArtifactRouteUsesTypedValidation(t *testing.T) {
	const host = "127.0.0.1:43210"
	server := newTestServer(t, host)
	runtime := app.NewRuntime(app.Options{})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	identity, err := protocol.NewWorkspaceIdentity("file:///workspace", "/workspace", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Activate(webhost.Dependencies{
		Runtime: runtime, WorkspaceRoot: "/workspace",
		WorkspaceIdentity: identity,
	}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"http://"+host+"/api/v1/checkpoint/get",
		strings.NewReader(`{"session_id":"session"}`),
	)
	request.Host = host
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+bootstrapToken(t, server, host))
	request.Header.Set("X-QCode-Workspace-ID", identity.RootID)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "checkpoint_id is required") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestWorkspaceRoutesUseBoundedWorkspaceQuery(t *testing.T) {
	const host = "127.0.0.1:43210"
	root := t.TempDir()
	command := exec.Command("git", "init", "-q")
	command.Dir = root
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := os.WriteFile(
		filepath.Join(root, "main.go"),
		[]byte("package main\n\nfunc hello() {}\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("fixture")...)
	if err := os.WriteFile(filepath.Join(root, "diagram.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, root, "add", ".")
	runWorkspaceGit(t, root, "commit", "-m", "initial")
	runWorkspaceGit(t, root, "branch", "feature")
	vcs, err := vcsbroker.New(
		root,
		authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}),
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	query, err := workspacequery.New(root, webTestBackend{}, vcs)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := gittool.RegisterMutations(registry, root, &workspacebroker.Runtime{VCS: vcs}); err != nil {
		t.Fatal(err)
	}
	runtime := app.NewRuntime(app.Options{GitControl: &app.GitControl{
		Workspace: query,
		NewGuard: func(context.Context, protocol.SessionProfile) (*guard.Guard, error) {
			return guard.New(guard.Options{
				Registry: registry, Workspace: root,
				Policy: policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
			})
		},
	}})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	identity, err := protocol.NewWorkspaceIdentity("file://"+root, root, "")
	if err != nil {
		t.Fatal(err)
	}
	server, err := webhost.New(webhost.Options{
		Assets: fstest.MapFS{
			"index.html": &fstest.MapFile{
				Data: []byte("<main>QCode</main>"),
				Mode: fs.FileMode(0o444),
			},
		},
		ExpectedHost: host,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Activate(webhost.Dependencies{
		Runtime: runtime, WorkspaceRoot: root,
		WorkspaceIdentity: identity, Workspace: query,
	}); err != nil {
		t.Fatal(err)
	}
	token := bootstrapToken(t, server, host)
	missingKey := postWeb(t, server, host, token, "workspace/git-action", `{}`)
	if missingKey.Code != http.StatusBadRequest {
		t.Fatalf("Git action without idempotency key status=%d", missingKey.Code)
	}
	branchRequest := httptest.NewRequest(
		http.MethodPost,
		"http://"+host+"/api/v1/workspace/git-switch",
		strings.NewReader(`{"branch":"feature"}`),
	)
	branchRequest.Host = host
	branchRequest.Header.Set("Content-Type", "application/json")
	branchRequest.Header.Set("Authorization", "Bearer "+token)
	branchRequest.Header.Set("X-QCode-Workspace-ID", identity.RootID)
	branchRequest.Header.Set("Idempotency-Key", "switch-feature")
	branchResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(branchResponse, branchRequest)
	if branchResponse.Code != http.StatusOK ||
		!strings.Contains(branchResponse.Body.String(), `"branch":"feature"`) {
		t.Fatalf(
			"branch switch status=%d body=%s",
			branchResponse.Code,
			branchResponse.Body.String(),
		)
	}
	response := postWeb(t, server, host, token, "workspace/search", `{"query":"hello"}`)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"path":"main.go"`) {
		t.Fatalf("search status=%d body=%s", response.Code, response.Body.String())
	}
	gitStatus := postWeb(t, server, host, token, "workspace/git-status", `{}`)
	if gitStatus.Code != http.StatusOK ||
		!strings.Contains(gitStatus.Body.String(), `"files":[]`) ||
		!strings.Contains(gitStatus.Body.String(), `"branch":"feature"`) {
		t.Fatalf("Git overview status=%d body=%s", gitStatus.Code, gitStatus.Body.String())
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc changed() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitPatch := postWeb(t, server, host, token, "workspace/git-diff", `{"path":"main.go","staged":false}`)
	if gitPatch.Code != http.StatusOK || !strings.Contains(gitPatch.Body.String(), "+func changed()") {
		t.Fatalf("Git patch status=%d body=%s", gitPatch.Code, gitPatch.Body.String())
	}
	invalidPatch := postWeb(t, server, host, token, "workspace/git-diff", `{"path":"../outside","staged":false}`)
	if invalidPatch.Code == http.StatusOK {
		t.Fatal("Git diff exposed an out-of-workspace path")
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc hello() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	response = postWeb(
		t,
		server,
		host,
		token,
		"workspace/resource",
		`{"path":"main.go"}`,
	)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"uri":"file://`) ||
		!strings.Contains(response.Body.String(), `"document_version":1`) ||
		!strings.Contains(response.Body.String(), `"content_handle":"`) ||
		strings.Contains(response.Body.String(), `"digest":"sha256:`) {
		t.Fatalf("resource status=%d body=%s", response.Code, response.Body.String())
	}
	resourceBody := append([]byte(nil), response.Body.Bytes()...)
	response = postWeb(
		t,
		server,
		host,
		token,
		"workspace/open",
		`{"path":"main.go"}`,
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("removed workspace/open route status=%d body=%s", response.Code, response.Body.String())
	}
	var resourceEnvelope struct {
		Result struct {
			ContentHandle string `json:"content_handle"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resourceBody, &resourceEnvelope); err != nil {
		t.Fatal(err)
	}
	contentRequest := httptest.NewRequest(
		http.MethodGet,
		"http://"+host+"/api/v1/content/"+resourceEnvelope.Result.ContentHandle,
		nil,
	)
	contentRequest.Host = host
	contentRequest.Header.Set("Authorization", "Bearer "+token)
	contentRequest.Header.Set("X-QCode-Workspace-ID", identity.RootID)
	contentResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(contentResponse, contentRequest)
	if contentResponse.Code != http.StatusOK ||
		contentResponse.Body.String() != "package main\n\nfunc hello() {}\n" ||
		!strings.Contains(
			contentResponse.Header().Get("Content-Disposition"),
			"main.go",
		) ||
		contentResponse.Header().Get("ETag") == "" {
		t.Fatalf(
			"content status=%d headers=%v body=%q",
			contentResponse.Code,
			contentResponse.Header(),
			contentResponse.Body.String(),
		)
	}
	unauthorized := httptest.NewRequest(
		http.MethodGet,
		"http://"+host+"/api/v1/content/"+resourceEnvelope.Result.ContentHandle,
		nil,
	)
	unauthorized.Host = host
	unauthorizedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized content status=%d", unauthorizedResponse.Code)
	}
	imageResponse := postWeb(
		t,
		server,
		host,
		token,
		"workspace/image",
		`{"path":"diagram.png"}`,
	)
	if imageResponse.Code != http.StatusOK ||
		!strings.Contains(imageResponse.Body.String(), `"media_type":"image/png"`) ||
		!strings.Contains(imageResponse.Body.String(), `"content_handle":"`) {
		t.Fatalf("image status=%d body=%s", imageResponse.Code, imageResponse.Body.String())
	}
	var imageEnvelope struct {
		Result struct {
			ContentHandle string `json:"content_handle"`
		} `json:"result"`
	}
	if err := json.Unmarshal(imageResponse.Body.Bytes(), &imageEnvelope); err != nil {
		t.Fatal(err)
	}
	imageRequest := httptest.NewRequest(
		http.MethodGet,
		"http://"+host+"/api/v1/content/"+imageEnvelope.Result.ContentHandle,
		nil,
	)
	imageRequest.Host = host
	imageRequest.Header.Set("Authorization", "Bearer "+token)
	imageRequest.Header.Set("X-QCode-Workspace-ID", identity.RootID)
	imageContent := httptest.NewRecorder()
	server.Handler().ServeHTTP(imageContent, imageRequest)
	if imageContent.Code != http.StatusOK ||
		imageContent.Header().Get("Content-Type") != "image/png" ||
		imageContent.Body.String() != string(png) {
		t.Fatalf(
			"image content status=%d headers=%v body=%q",
			imageContent.Code,
			imageContent.Header(),
			imageContent.Body.Bytes(),
		)
	}
	tampered := httptest.NewRequest(
		http.MethodGet,
		"http://"+host+"/api/v1/content/"+resourceEnvelope.Result.ContentHandle+"0",
		nil,
	)
	tampered.Host = host
	tampered.Header.Set("Authorization", "Bearer "+token)
	tamperedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(tamperedResponse, tampered)
	if tamperedResponse.Code != http.StatusBadRequest {
		t.Fatalf("tampered handle status=%d", tamperedResponse.Code)
	}
	if err := os.WriteFile(
		filepath.Join(root, "main.go"),
		[]byte("package changed\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	staleResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(staleResponse, contentRequest)
	if staleResponse.Code != http.StatusConflict {
		t.Fatalf("stale handle status=%d body=%s", staleResponse.Code, staleResponse.Body.String())
	}
	response = postWeb(
		t,
		server,
		host,
		token,
		"workspace/resource",
		`{"path":"../secret"}`,
	)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("escape status=%d body=%s", response.Code, response.Body.String())
	}
	response = postWeb(
		t,
		server,
		host,
		token,
		"workspace/open",
		`{"path":"../secret"}`,
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("removed workspace/open route status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestUnaryRejectsDeclaredOversizedBody(t *testing.T) {
	const host = "127.0.0.1:43210"
	server := newTestServerWithOptions(t, webhost.Options{
		Assets: fstest.MapFS{
			"index.html": &fstest.MapFile{
				Data: []byte("<main>QCode</main>"), Mode: fs.FileMode(0o444),
			},
		},
		ExpectedHost: host,
		Origin:       "http://" + host,
		Capacity:     webhost.Capacity{MaxJSONBodyBytes: 2},
	})
	runtime := app.NewRuntime(app.Options{})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	identity, err := protocol.NewWorkspaceIdentity("file:///workspace", "/workspace", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Activate(webhost.Dependencies{
		Runtime: runtime, WorkspaceRoot: "/workspace",
		WorkspaceIdentity: identity,
	}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"http://"+host+"/api/v1/system/describe",
		strings.NewReader(`{}`),
	)
	request.Host = host
	request.ContentLength = 3
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+bootstrapToken(t, server, host))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func newTestServer(t *testing.T, host string) *webhost.Server {
	t.Helper()
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<main>QCode</main>"), Mode: fs.FileMode(0o444)},
	}
	return newTestServerWithAssets(t, host, assets)
}

func newTestServerWithAssets(
	t *testing.T,
	host string,
	assets fs.FS,
) *webhost.Server {
	t.Helper()
	return newTestServerWithOptions(t, webhost.Options{
		Assets: assets, ExpectedHost: host, Origin: "http://" + host,
	})
}

func newTestServerWithOptions(
	t *testing.T,
	options webhost.Options,
) *webhost.Server {
	t.Helper()
	server, err := webhost.New(options)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func bootstrapToken(t *testing.T, server *webhost.Server, host string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/v1/bootstrap", nil)
	request.Host = host
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var value struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value.Token
}

func runWorkspaceGit(t *testing.T, root string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = root
	command.Env = append(
		os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root,
		"GIT_AUTHOR_NAME=QCode", "GIT_AUTHOR_EMAIL=fixture@invalid",
		"GIT_COMMITTER_NAME=QCode", "GIT_COMMITTER_EMAIL=fixture@invalid",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

func fetchBootstrapToken(t *testing.T, origin string) string {
	t.Helper()
	response, err := http.Get(origin + "/api/v1/bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var value struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value.Token
}

func postWeb(
	t *testing.T,
	server *webhost.Server,
	host, token, route, body string,
) *httptest.ResponseRecorder {
	return postWebWorkspace(
		t,
		server,
		host,
		token,
		bootstrapWorkspaceID(t, server, host),
		route,
		body,
	)
}

func bootstrapWorkspaceID(
	t *testing.T,
	server *webhost.Server,
	host string,
) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/v1/bootstrap", nil)
	request.Host = host
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var value struct {
		WorkspaceCatalog webhost.WorkspaceCatalog `json:"workspace_catalog"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	for _, workspace := range value.WorkspaceCatalog.Workspaces {
		if workspace.Ready {
			return workspace.ID
		}
	}
	return ""
}

func postWebWorkspace(
	t *testing.T,
	server *webhost.Server,
	host, token, workspaceID, route, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://"+host+"/api/v1/"+route,
		strings.NewReader(body),
	)
	request.Host = host
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	if workspaceID != "" {
		request.Header.Set("X-QCode-Workspace-ID", workspaceID)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

type webTestBackend struct{}

func (webTestBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "test", Backend: "passthrough",
		Available: true,
		Effective: controlmatrix.Matrix{FilesystemRead: controlmatrix.FilesystemReadDeclaredRoots,

			FilesystemWrite: controlmatrix.
				FilesystemWriteExactPaths,

			Network: controlmatrix.
				NetworkDenied,

			ProcessTree: controlmatrix.ProcessTreeGroupKill,

			CrossProcess: controlmatrix.
				CrossProcessUnrestricted, Syscall: controlmatrix.SyscallDenyDangerous, IPC: controlmatrix.
				IPCUnrestricted, PathIdentity:     controlmatrix.PathIdentityDescriptorRelative,
			ArtifactOrigin: controlmatrix.ArtifactOriginUnverifiedPath, DurableRecovery: controlmatrix.DurableRecoveryMemoryOnly},
	}
}

func (webTestBackend) Prepare(
	_ context.Context,
	command sandbox.Command,
) (sandbox.Command, error) {
	command.PreparedReadOnly = command.WorkspaceReadOnly
	command.PreparedReadPaths = append(
		[]string(nil), command.AdditionalReadPaths...,
	)
	command.PreparedWritePaths = append(
		[]string(nil), command.WorkspaceWritePaths...,
	)
	command.PreparedNetworkDenied = command.DenyNetwork
	return command, nil
}
