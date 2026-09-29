package host

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/fwtllh-png/QCode/internal/runtime/app"
)

const launchTestHost = "127.0.0.1:43210"

func newLaunchTestServer(t *testing.T) *Server {
	t.Helper()
	server, err := New(Options{
		Assets: fstest.MapFS{
			"index.html": &fstest.MapFile{
				Data: []byte("<main>QCode</main>"), Mode: fs.FileMode(0o444),
			},
		},
		ExpectedHost: launchTestHost, Origin: "http://" + launchTestHost,
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func serveLaunchTest(
	server *Server,
	method, target string,
	configure func(*http.Request),
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://"+launchTestHost+target, nil)
	request.Host = launchTestHost
	if configure != nil {
		configure(request)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func launchPath(t *testing.T, server *Server, rawURL string) string {
	t.Helper()
	launchURL, err := server.LaunchURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(launchURL, "http://"+launchTestHost)
}

func TestLaunchCodeExchangesOnceForStrictHttpOnlyCookie(t *testing.T) {
	server := newLaunchTestServer(t)
	target := launchPath(t, server, "http://"+launchTestHost+"/?workspace=root-1")
	if !strings.HasPrefix(target, "/?workspace=root-1&launch=") {
		t.Fatalf("launch URL = %q", target)
	}

	redeemed := serveLaunchTest(server, http.MethodGet, target, nil)
	if redeemed.Code != http.StatusSeeOther ||
		redeemed.Header().Get("Location") != "/?workspace=root-1" {
		t.Fatalf("redeem status=%d location=%q", redeemed.Code, redeemed.Header().Get("Location"))
	}
	if redeemed.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("redeem cache control = %q", redeemed.Header().Get("Cache-Control"))
	}
	cookies := redeemed.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %+v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != "qcode_session_43210" || !cookie.HttpOnly ||
		cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" ||
		cookie.Value == "" || cookie.Value == server.token {
		t.Fatalf("session cookie = %+v", cookie)
	}

	replayed := serveLaunchTest(server, http.MethodGet, target, nil)
	if replayed.Code != http.StatusSeeOther || len(replayed.Result().Cookies()) != 0 {
		t.Fatalf("replayed launch code status=%d cookies=%+v",
			replayed.Code, replayed.Result().Cookies())
	}

	withCookie := func(request *http.Request) { request.AddCookie(cookie) }
	authenticated := serveLaunchTest(server, http.MethodGet, "/api/v1/bootstrap", withCookie)
	var bootstrap bootstrapResponse
	if err := json.Unmarshal(authenticated.Body.Bytes(), &bootstrap); err != nil {
		t.Fatal(err)
	}
	if !bootstrap.Authenticated {
		t.Fatalf("cookie bootstrap = %s", authenticated.Body.String())
	}
	listWorkspaces := func(request *http.Request) {
		request.Header.Set("Content-Type", "application/json")
		request.Body = io.NopCloser(strings.NewReader(`{}`))
	}
	if response := serveLaunchTest(
		server, http.MethodPost, "/api/v1/workspace/list",
		func(request *http.Request) { listWorkspaces(request); withCookie(request) },
	); response.Code == http.StatusUnauthorized {
		t.Fatalf("cookie session was rejected: %s", response.Body.String())
	}
	if response := serveLaunchTest(
		server, http.MethodPost, "/api/v1/workspace/list", listWorkspaces,
	); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous unary status = %d", response.Code)
	}
}

func TestBootstrapWithoutSessionDisclosesNoSecretsOrWorkspace(t *testing.T) {
	server := newLaunchTestServer(t)
	response := serveLaunchTest(server, http.MethodGet, "/api/v1/bootstrap", nil)
	body := response.Body.String()
	if strings.Contains(body, server.token) || strings.Contains(body, server.session) {
		t.Fatalf("anonymous bootstrap leaked a credential: %s", body)
	}
	var bootstrap bootstrapResponse
	if err := json.Unmarshal(response.Body.Bytes(), &bootstrap); err != nil {
		t.Fatal(err)
	}
	if bootstrap.Authenticated || bootstrap.WorkspaceRoot != "" ||
		bootstrap.Workspace != nil || len(bootstrap.WorkspaceCatalog.Workspaces) != 0 {
		t.Fatalf("anonymous bootstrap = %+v", bootstrap)
	}
	forged := serveLaunchTest(server, http.MethodGet, "/api/v1/bootstrap",
		func(request *http.Request) {
			request.AddCookie(&http.Cookie{Name: server.sessionCookieName(), Value: server.token})
		})
	if err := json.Unmarshal(forged.Body.Bytes(), &bootstrap); err != nil {
		t.Fatal(err)
	}
	if bootstrap.Authenticated {
		t.Fatal("the capability token must not double as a session cookie")
	}
}

func TestLaunchCodeExpiresAtConfiguredTTL(t *testing.T) {
	server := newLaunchTestServer(t)
	issuedAt := time.Unix(1_700_000_000, 0)
	now := issuedAt
	server.now = func() time.Time { return now }
	ttl := time.Duration(defaultLaunchCodeTTLSeconds) * time.Second

	justInTime, err := server.IssueLaunchCode()
	if err != nil {
		t.Fatal(err)
	}
	expired, err := server.IssueLaunchCode()
	if err != nil {
		t.Fatal(err)
	}
	now = issuedAt.Add(ttl - time.Nanosecond)
	if !server.redeemLaunchCode(justInTime) {
		t.Fatal("launch code rejected before its TTL elapsed")
	}
	now = issuedAt.Add(ttl)
	if server.redeemLaunchCode(expired) {
		t.Fatal("launch code accepted at its TTL")
	}
	if _, err := server.IssueLaunchCode(); err != nil {
		t.Fatal(err)
	}
	server.launchMu.Lock()
	outstanding := len(server.launchCodes)
	server.launchMu.Unlock()
	if outstanding != 1 {
		t.Fatalf("expired launch codes were retained: %d outstanding", outstanding)
	}
}

func TestLaunchRedirectNeverLeavesTheHost(t *testing.T) {
	server := newLaunchTestServer(t)
	response := serveLaunchTest(server, http.MethodGet, "/deep/evil.example?launch=unknown", nil)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" {
		t.Fatalf("status=%d location=%q", response.Code, response.Header().Get("Location"))
	}
	if len(response.Result().Cookies()) != 0 {
		t.Fatal("unknown launch code set a cookie")
	}
}

func TestLaunchCodeEndpointRequiresCapabilityToken(t *testing.T) {
	server := newLaunchTestServer(t)
	request := func(configure func(*http.Request)) *httptest.ResponseRecorder {
		return serveLaunchTest(server, http.MethodPost, "/api/v1/auth/launch-code",
			func(request *http.Request) {
				request.Header.Set("Content-Type", "application/json")
				request.Body = io.NopCloser(strings.NewReader(`{}`))
				configure(request)
			})
	}
	granted := request(func(request *http.Request) {
		request.Header.Set("Authorization", "Bearer "+server.token)
	})
	var envelope struct {
		Result LaunchCodeResult `json:"result"`
	}
	if err := json.Unmarshal(granted.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if granted.Code != http.StatusOK || envelope.Result.Code == "" ||
		envelope.Result.ExpiresInSeconds != defaultLaunchCodeTTLSeconds {
		t.Fatalf("launch code status=%d body=%s", granted.Code, granted.Body.String())
	}
	if !server.redeemLaunchCode(envelope.Result.Code) {
		t.Fatal("issued launch code is not redeemable")
	}
	cookieOnly := request(func(request *http.Request) {
		request.AddCookie(&http.Cookie{Name: server.sessionCookieName(), Value: server.session})
	})
	if cookieOnly.Code == http.StatusOK {
		t.Fatalf("browser session minted a launch code: %s", cookieOnly.Body.String())
	}
}

func TestWebSocketAuthenticatesWithSessionCookie(t *testing.T) {
	runtime := app.NewRuntime(app.Options{})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	server, origin, _ := runningWebServer(t, runtime, Capacity{})
	launchURL, err := server.LaunchURL(origin + "/")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	redeemed, err := client.Get(launchURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = redeemed.Body.Close()
	if len(redeemed.Cookies()) != 1 {
		t.Fatalf("launch cookies = %+v", redeemed.Cookies())
	}
	workspaceID := server.dependencies.WorkspaceIdentity.RootID
	dial := func(header http.Header) (*websocket.Conn, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		connection, _, err := websocket.Dial(ctx, "ws"+origin[len("http"):]+"/api/v1/events",
			&websocket.DialOptions{HTTPHeader: header})
		if err != nil {
			return nil, err
		}
		if err := wsjson.Write(ctx, connection, authFrame{
			Type: "authenticate", WorkspaceID: workspaceID,
		}); err != nil {
			_ = connection.CloseNow()
			return nil, err
		}
		var hello eventFrame
		if err := wsjson.Read(ctx, connection, &hello); err != nil {
			_ = connection.CloseNow()
			return nil, err
		}
		return connection, nil
	}
	header := http.Header{}
	header.Set("Cookie", redeemed.Cookies()[0].String())
	connection, err := dial(header)
	if err != nil {
		t.Fatalf("cookie WebSocket authentication failed: %v", err)
	}
	_ = connection.CloseNow()
	if connection, err := dial(nil); err == nil {
		_ = connection.CloseNow()
		t.Fatal("WebSocket without cookie or token was accepted")
	}
}
