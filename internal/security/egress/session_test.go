package egress_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/goproxy"
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

	workspace := &egress.Gate{Enforce: true}
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

	assertProxyBody(t, sessionA.Port(), left.URL, http.StatusOK, "left")
	assertProxyStatus(t, sessionA.Port(), right.URL, http.StatusForbidden)
	assertProxyBody(t, sessionB.Port(), right.URL, http.StatusOK, "right")
	assertProxyStatus(t, sessionB.Port(), left.URL, http.StatusForbidden)
	assertProxyStatus(t, proxy.Port(), left.URL, http.StatusForbidden)
	assertProxyStatus(t, proxy.Port(), right.URL, http.StatusForbidden)
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
	proxy, err := egress.StartManagedNetworkProxy(&egress.Gate{Enforce: true})
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
	client, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(session.Port()))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err = fmt.Fprintf(
		client,
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n",
		authority,
		authority,
	); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
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
	workspace := &egress.Gate{Enforce: true}
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

	client, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(session.Port()))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err = fmt.Fprintf(
		client,
		"CONNECT goproxy.example:443 HTTP/1.1\r\nHost: goproxy.example:443\r\n\r\n",
	); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
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
	workspace := &egress.Gate{Enforce: true}
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
		t, session.Port(), "http://goproxy.example/", http.StatusForbidden,
	)
	receipts := session.Gate().Receipts()
	if len(receipts) == 0 || receipts[0].Category != "trust_validation_failed" {
		t.Fatalf("receipts = %+v", receipts)
	}
}

func TestProtocolHandlerServesOriginFormOnWorkspaceAndSessionChannels(t *testing.T) {
	workspace := &egress.Gate{Enforce: true}
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

	direct, err := http.Get(fmt.Sprintf(
		"http://127.0.0.1:%d/example.com/qcode/testmod/@v/v1.2.3.info",
		session.Port(),
	))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(direct.Body)
	direct.Body.Close()
	if err != nil || direct.StatusCode != http.StatusOK ||
		!strings.Contains(string(body), "v1.2.3") {
		t.Fatalf("origin-form status=%d body=%s err=%v", direct.StatusCode, body, err)
	}

	stable, err := http.Get(fmt.Sprintf(
		"http://127.0.0.1:%d/example.com/qcode/testmod/@v/v1.2.3.info",
		proxy.Port(),
	))
	if err != nil {
		t.Fatal(err)
	}
	stableBody, err := io.ReadAll(stable.Body)
	stable.Body.Close()
	if err != nil || stable.StatusCode != http.StatusOK ||
		!strings.Contains(string(stableBody), "v1.2.3") {
		t.Fatalf("stable channel status=%d body=%s err=%v", stable.StatusCode, stableBody, err)
	}

	assertProxyStatus(t, session.Port(), "http://goproxy.example/", http.StatusForbidden)
	if _, err := workspace.Authorize(t.Context(), egress.Target{
		Host: "goproxy.example", Protocol: "https", Port: 443,
		Methods: []string{http.MethodConnect},
	}, "test"); err == nil {
		t.Fatal("protocol session leaked a grant onto the workspace gate")
	}
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

func proxyClient(t *testing.T, port uint16) *http.Client {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(&url.URL{
			Scheme: "http",
			Host:   net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))),
		}),
		DisableKeepAlives: true,
	}}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func assertProxyBody(t *testing.T, port uint16, endpoint string, want int, body string) {
	t.Helper()
	response, err := proxyClient(t, port).Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != want || string(got) != body {
		t.Fatalf(
			"port %d %s: status=%d body=%q err=%v, want %d %q",
			port, endpoint, response.StatusCode, got, err, want, body,
		)
	}
}

func assertProxyStatus(t *testing.T, port uint16, endpoint string, want int) {
	t.Helper()
	response, err := proxyClient(t, port).Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("port %d %s: status=%d, want %d", port, endpoint, response.StatusCode, want)
	}
}

func TestProcessSessionDeniesTrailingDotConnectToBoundHost(t *testing.T) {
	workspace := &egress.Gate{Enforce: true}
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

	client, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(session.Port()))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	// FQDN trailing dot: same origin after normalization, spelling chosen to
	// sidestep a naive equality check.
	if _, err = fmt.Fprintf(
		client,
		"CONNECT goproxy.example.:443 HTTP/1.1\r\nHost: goproxy.example.:443\r\n\r\n",
	); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
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
	proxy, err := egress.StartManagedNetworkProxy(&egress.Gate{Enforce: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	proxy.BindProtocolHandler(service)

	response, err := http.Get(fmt.Sprintf(
		"http://127.0.0.1:%d/example.com/qcode/testmod/@v/v1.2.3.info",
		proxy.Port(),
	))
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
