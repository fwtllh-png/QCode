package egress_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/goproxy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestProcessSessionsIsolateGrantedTargets(t *testing.T) {
	left := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = io.WriteString(writer, "left")
	}))
	t.Cleanup(left.Close)
	right := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = io.WriteString(writer, "right")
	}))
	t.Cleanup(right.Close)

	workspace := &egress.Gate{}
	proxy, err := egress.StartManagedNetworkProxy(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })

	sessionA, err := proxy.OpenSession([]egress.Target{httpTarget(t, left.URL, http.MethodGet)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessionA.Close() })
	sessionB, err := proxy.OpenSession([]egress.Target{httpTarget(t, right.URL, http.MethodGet)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessionB.Close() })

	assertProxyBody(t, sessionA, left.URL, http.StatusOK, "left")
	assertProxyStatus(t, sessionA, right.URL, http.StatusForbidden)
	assertProxyBody(t, sessionB, right.URL, http.StatusOK, "right")
	assertProxyStatus(t, sessionB, left.URL, http.StatusForbidden)
	assertProxyStatus(t, proxy, left.URL, http.StatusForbidden)
	assertProxyStatus(t, proxy, right.URL, http.StatusForbidden)
	if _, err := workspace.Authorize(
		t.Context(),
		httpTarget(t, left.URL, http.MethodGet),
		"test",
	); err == nil {
		t.Fatal("session grant leaked onto the workspace gate")
	}
}

func TestProcessSessionCloseRecyclesCONNECT(t *testing.T) {
	accepted := make(chan net.Conn, 1)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			accepted <- nil
			return
		}
		accepted <- conn
	}()
	address := listener.Addr().(*net.TCPAddr)
	proxy, err := egress.StartManagedNetworkProxy(&egress.Gate{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	session, err := proxy.OpenSession([]egress.Target{{
		Host: address.IP.String(), Protocol: "https", Port: uint16(address.Port),
		Methods: []string{http.MethodConnect}, AllowPrivate: true,
	}})
	if err != nil {
		t.Fatal(err)
	}

	authority := net.JoinHostPort(address.IP.String(), strconv.Itoa(address.Port))
	client, reader, status := dialConnect(t, session, authority)
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q", status)
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	upstream := <-accepted
	if upstream == nil {
		t.Fatal("upstream did not accept CONNECT")
	}
	t.Cleanup(func() { _ = upstream.Close() })
	if _, err = client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err = client.Read(make([]byte, 8)); err == nil {
		t.Fatal("hijacked CONNECT survived session close")
	}
}

type boundOriginHandler struct {
	host string
	http.Handler
}

func (h boundOriginHandler) BoundHost() string { return h.host }

func TestProcessSessionDeniesConnectToBoundGoproxyHost(t *testing.T) {
	workspace := &egress.Gate{}
	proxy, err := egress.StartManagedNetworkProxy(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	proxy.BindProtocolHandler(boundOriginHandler{
		host: "goproxy.example",
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "protocol should not see CONNECT", http.StatusBadRequest)
		}),
	})
	session, err := proxy.OpenSession([]egress.Target{{
		Host: "goproxy.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodConnect}, AllowPrivate: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	_, _, status := dialConnect(t, session, "goproxy.example:443")
	if !strings.Contains(status, "403") {
		t.Fatalf("CONNECT status = %q", status)
	}
	receipts := session.Gate().Receipts()
	if len(receipts) == 0 || receipts[0].Category != "trust_validation_failed" {
		t.Fatalf("receipts = %+v", receipts)
	}
	if strings.Contains(status, "401") {
		t.Fatal("bound-origin CONNECT returned an upstream 401")
	}
}

func TestProcessSessionDeniesAbsoluteFormToBoundGoproxyHost(t *testing.T) {
	workspace := &egress.Gate{}
	proxy, err := egress.StartManagedNetworkProxy(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	proxy.BindProtocolHandler(boundOriginHandler{host: "goproxy.example"})
	session, err := proxy.OpenSession([]egress.Target{{
		Host: "goproxy.example", Protocol: "http", Port: 80,
		Methods: []string{http.MethodGet}, AllowPrivate: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	assertProxyStatus(
		t, session, "http://goproxy.example/", http.StatusForbidden,
	)
	receipts := session.Gate().Receipts()
	if len(receipts) == 0 || receipts[0].Category != "trust_validation_failed" {
		t.Fatalf("receipts = %+v", receipts)
	}
}

func TestProtocolHandlerServesOriginFormOnWorkspaceAndSessionChannels(t *testing.T) {
	workspace := &egress.Gate{}
	proxy, err := egress.StartManagedNetworkProxy(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	// Binding after startup is the production order: the wire binds the
	// GOPROXY auth service once the workspace channel is already serving.
	// The stable channel must serve it from that moment on, or every
	// origin-form module fetch through GOPROXY dies at the workspace port.
	proxy.BindProtocolHandler(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/example.com/qcode/testmod/@v/v1.2.3.info" {
			http.Error(writer, "unexpected path", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(writer, `{"Version":"v1.2.3"}`)
	}))

	session, err := proxy.OpenSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	direct, err := http.Get(originFormURL(session))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(direct.Body)
	direct.Body.Close()
	if err != nil || direct.StatusCode != http.StatusOK ||
		!strings.Contains(string(body), "v1.2.3") {
		t.Fatalf("origin-form status=%d body=%s err=%v", direct.StatusCode, body, err)
	}

	stable, err := http.Get(originFormURL(proxy))
	if err != nil {
		t.Fatal(err)
	}
	stableBody, err := io.ReadAll(stable.Body)
	stable.Body.Close()
	if err != nil || stable.StatusCode != http.StatusOK ||
		!strings.Contains(string(stableBody), "v1.2.3") {
		t.Fatalf("stable channel status=%d body=%s err=%v", stable.StatusCode, stableBody, err)
	}

	assertProxyStatus(t, session, "http://goproxy.example/", http.StatusForbidden)
	if _, err := workspace.Authorize(t.Context(), egress.Target{
		Host: "goproxy.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodConnect},
	}, "test"); err == nil {
		t.Fatal("protocol session leaked a grant onto the workspace gate")
	}
}

func TestProxyChannelsRequireTheirOwnCredential(t *testing.T) {
	var reached atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		reached.Add(1)
		_, _ = io.WriteString(writer, "ok")
	}))
	t.Cleanup(upstream.Close)
	proxy, err := egress.StartManagedNetworkProxy(&egress.Gate{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	target := httpTarget(t, upstream.URL, http.MethodGet)
	target.Methods = []string{http.MethodGet, http.MethodConnect}
	sessionA, err := proxy.OpenSession([]egress.Target{target})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessionA.Close() })
	sessionB, err := proxy.OpenSession([]egress.Target{target})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessionB.Close() })
	credentials := map[string]bool{
		proxy.Credential(): true, sessionA.Credential(): true, sessionB.Credential(): true,
	}
	if len(credentials) != 3 || credentials[""] {
		t.Fatalf("channel credentials must be distinct and non-empty")
	}
	if !strings.Contains(proxy.URL(), proxy.Credential()+"@127.0.0.1:") {
		t.Fatal("workspace proxy URL does not carry its credential")
	}

	authority := strings.TrimPrefix(upstream.URL, "http://")
	for name, endpoint := range map[string]proxyEndpoint{
		"no credential":             foreignEndpoint{port: sessionB.Port()},
		"sibling session":           foreignEndpoint{sessionB.Port(), sessionA.Credential()},
		"session on workspace port": foreignEndpoint{proxy.Port(), sessionA.Credential()},
		"workspace on session port": foreignEndpoint{sessionA.Port(), proxy.Credential()},
		"forged credential":         foreignEndpoint{sessionB.Port(), "not-the-credential"},
	} {
		response, err := proxyClient(t, endpoint).Get(upstream.URL)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusProxyAuthRequired ||
			!strings.HasPrefix(response.Header.Get("Proxy-Authenticate"), "Basic ") {
			t.Fatalf("%s: forward status=%d challenge=%q", name,
				response.StatusCode, response.Header.Get("Proxy-Authenticate"))
		}
		if _, _, status := dialConnect(t, endpoint, authority); !strings.Contains(status, "407") {
			t.Fatalf("%s: CONNECT status = %q", name, status)
		}
	}
	if reached.Load() != 0 {
		t.Fatalf("unauthenticated clients reached the upstream %d times", reached.Load())
	}
	assertProxyBody(t, sessionA, upstream.URL, http.StatusOK, "ok")
	assertProxyBody(t, sessionB, upstream.URL, http.StatusOK, "ok")
}

func TestProtocolHandlerRequiresChannelCredential(t *testing.T) {
	var served atomic.Int32
	var leaked atomic.Bool
	proxy, err := egress.StartManagedNetworkProxy(&egress.Gate{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	proxy.BindProtocolHandler(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		served.Add(1)
		if request.Header.Get("Authorization") != "" ||
			request.Header.Get("Proxy-Authorization") != "" {
			leaked.Store(true)
		}
		_, _ = io.WriteString(writer, `{"Version":"v1.2.3"}`)
	}))
	session, err := proxy.OpenSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	for name, endpoint := range map[string]proxyEndpoint{
		"no credential":   foreignEndpoint{port: session.Port()},
		"workspace creds": foreignEndpoint{session.Port(), proxy.Credential()},
	} {
		response, err := http.Get(originFormURL(endpoint))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized ||
			!strings.HasPrefix(response.Header.Get("WWW-Authenticate"), "Basic ") {
			t.Fatalf("%s: origin-form status=%d", name, response.StatusCode)
		}
	}
	if served.Load() != 0 {
		t.Fatal("protocol handler served an unauthenticated request")
	}

	request, err := http.NewRequest(http.MethodGet, originFormURL(foreignEndpoint{port: session.Port()}), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString(
		[]byte(sandbox.ManagedProxyUser+":"+session.Credential()),
	))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || served.Load() != 1 {
		t.Fatalf("Proxy-Authorization origin-form status=%d served=%d", response.StatusCode, served.Load())
	}
	if leaked.Load() {
		t.Fatal("channel credential reached the protocol handler")
	}
}

func TestUnauthenticatedProxyAcceptsClientsWithoutCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = io.WriteString(writer, "ok")
	}))
	t.Cleanup(upstream.Close)
	proxy, err := egress.StartUnauthenticatedNetworkProxy(&egress.Gate{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	if proxy.Credential() != "" || strings.Contains(proxy.URL(), "@") {
		t.Fatalf("unauthenticated proxy URL = %q", proxy.URL())
	}
	session, err := proxy.OpenSession([]egress.Target{httpTarget(t, upstream.URL, http.MethodGet)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	assertProxyBody(t, session, upstream.URL, http.StatusOK, "ok")
}

func httpTarget(t *testing.T, endpoint, method string) egress.Target {
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

// proxyEndpoint is a proxy channel as a client sees it: a loopback port and
// the credential its owner was handed.
type proxyEndpoint interface {
	Port() uint16
	Credential() string
}

// foreignEndpoint presents one channel's port with another's credential.
type foreignEndpoint struct {
	port       uint16
	credential string
}

func (e foreignEndpoint) Port() uint16       { return e.port }
func (e foreignEndpoint) Credential() string { return e.credential }

func proxyClient(t *testing.T, endpoint proxyEndpoint) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse(sandbox.ManagedProxyURL(endpoint.Port(), endpoint.Credential()))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:             http.ProxyURL(proxyURL),
		DisableKeepAlives: true,
	}}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func originFormURL(endpoint proxyEndpoint) string {
	return sandbox.ManagedProxyURL(endpoint.Port(), endpoint.Credential()) +
		"/example.com/qcode/testmod/@v/v1.2.3.info"
}

func dialConnect(t *testing.T, endpoint proxyEndpoint, authority string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	client, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(endpoint.Port()))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	auth := ""
	if endpoint.Credential() != "" {
		auth = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString(
			[]byte(sandbox.ManagedProxyUser+":"+endpoint.Credential()),
		) + "\r\n"
	}
	if _, err = fmt.Fprintf(
		client, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", authority, authority, auth,
	); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return client, reader, status
}

func assertProxyBody(t *testing.T, endpoint proxyEndpoint, target string, want int, body string) {
	t.Helper()
	response, err := proxyClient(t, endpoint).Get(target)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != want || string(got) != body {
		t.Fatalf(
			"port %d %s: status=%d body=%q err=%v, want %d %q",
			endpoint.Port(), target, response.StatusCode, got, err, want, body,
		)
	}
}

func assertProxyStatus(t *testing.T, endpoint proxyEndpoint, target string, want int) {
	t.Helper()
	response, err := proxyClient(t, endpoint).Get(target)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("port %d %s: status=%d, want %d", endpoint.Port(), target, response.StatusCode, want)
	}
}

func TestProcessSessionDeniesTrailingDotConnectToBoundHost(t *testing.T) {
	workspace := &egress.Gate{}
	proxy, err := egress.StartManagedNetworkProxy(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	proxy.BindProtocolHandler(boundOriginHandler{host: "goproxy.example"})
	session, err := proxy.OpenSession([]egress.Target{{
		Host: "goproxy.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodConnect}, AllowPrivate: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	// FQDN trailing dot: same origin after normalization, spelling chosen to
	// sidestep a naive equality check.
	_, _, status := dialConnect(t, session, "goproxy.example.:443")
	if !strings.Contains(status, "403") {
		t.Fatalf("trailing-dot CONNECT status = %q", status)
	}
	receipts := session.Gate().Receipts()
	if len(receipts) == 0 || receipts[0].Category != "trust_validation_failed" {
		t.Fatalf("receipts = %+v", receipts)
	}
}

func TestGoproxyServiceServesStableWorkspaceChannel(t *testing.T) {
	// End-to-end A1 reproduction: a real GOPROXY auth service bound after
	// startup, fetched in origin form through the stable workspace channel —
	// the exact shape `GOPROXY=http://127.0.0.1:<managed port>` produces.
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seenAuth = request.Header.Get("Authorization")
		if request.URL.Path != "/example.com/qcode/testmod/@v/v1.2.3.info" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"Version":"v1.2.3"}`))
	}))
	t.Cleanup(upstream.Close)
	service, err := goproxy.New(goproxy.Binding{
		Upstream:   upstream.URL,
		Prefixes:   []string{"example.com/qcode/"},
		Credential: goproxy.CredentialRef{Kind: "env", Name: "TEST_GOPROXY_TOKEN"},
	}, func(context.Context, string, string) (string, error) {
		return "user:secret-token", nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := egress.StartManagedNetworkProxy(&egress.Gate{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	proxy.BindProtocolHandler(service)

	response, err := http.Get(originFormURL(proxy))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK ||
		!strings.Contains(string(body), "v1.2.3") {
		t.Fatalf("stable channel status=%d body=%s err=%v", response.StatusCode, body, err)
	}
	if seenAuth == "" {
		t.Fatal("auth service did not reach the upstream")
	}
}
