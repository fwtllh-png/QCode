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

	workspace := egress.NewStaticGate()
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
	proxy, err := egress.StartManagedNetworkProxy(egress.NewStaticGate())
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

func TestProxyChannelsRejectOriginForm(t *testing.T) {
	proxy, err := egress.StartManagedNetworkProxy(egress.NewStaticGate())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(t.Context()) })
	session, err := proxy.OpenSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	for _, endpoint := range []proxyEndpoint{proxy, session} {
		for _, authenticated := range []bool{false, true} {
			request, err := http.NewRequest(http.MethodGet,
				fmt.Sprintf("http://127.0.0.1:%d/example/module/@v/list", endpoint.Port()), nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Host = "artifacts.example"
			// Origin Authorization cannot authenticate a local proxy channel.
			request.SetBasicAuth(sandbox.ManagedProxyUser, endpoint.Credential())
			want := http.StatusProxyAuthRequired
			if authenticated {
				request.Header.Set("Proxy-Authorization", request.Header.Get("Authorization"))
				want = http.StatusBadRequest
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != want {
				t.Fatalf("origin-form authenticated=%v status=%d want=%d", authenticated, response.StatusCode, want)
			}
		}
	}
	if len(session.Gate().Receipts()) != 0 {
		t.Fatal("origin-form request reached the network gate")
	}
}

func TestForwardProxySeparatesChannelAndUpstreamCredentials(t *testing.T) {
	var reached atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reached.Add(1)
		if request.Header.Get("Proxy-Authorization") != "" || request.Header.Get("Authorization") != "Bearer fixture-upstream" {
			t.Error("channel credential leaked or upstream Authorization changed")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(writer, "ok")
	}))
	t.Cleanup(upstream.Close)
	proxy, err := egress.StartManagedNetworkProxy(egress.NewStaticGate())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(t.Context()) })
	session, err := proxy.OpenSession([]egress.Target{httpTarget(t, upstream.URL, http.MethodGet)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	for _, endpoint := range []proxyEndpoint{proxy, foreignEndpoint{port: session.Port()}, session} {
		request, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer fixture-upstream")
		want := http.StatusOK
		if endpoint == proxy {
			want = http.StatusForbidden
		} else if endpoint.Credential() == "" {
			request.SetBasicAuth(sandbox.ManagedProxyUser, session.Credential())
			want = http.StatusProxyAuthRequired
		}
		response, err := proxyClient(t, endpoint).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("forward status=%d want=%d", response.StatusCode, want)
		}
	}
	if reached.Load() != 1 {
		t.Fatalf("upstream reached %d times, want only the granted session", reached.Load())
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
	proxy, err := egress.StartManagedNetworkProxy(egress.NewStaticGate())
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

func TestBrowserProxyDoesNotShareAdoptedGrantsWithForeignClients(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = io.WriteString(writer, "ok")
	}))
	t.Cleanup(upstream.Close)
	gate := egress.NewBrowserGate()
	ctx, release := egress.WithScope(t.Context())
	egress.AllowInScope(ctx, httpTarget(t, upstream.URL, http.MethodGet))
	gate.AdoptScope(ctx)
	release()
	proxy, err := egress.StartManagedNetworkProxy(gate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close(context.Background()) })
	if proxy.Credential() == "" {
		t.Fatal("browser proxy has no credential")
	}
	assertProxyStatus(t, foreignEndpoint{port: proxy.Port()}, upstream.URL, http.StatusProxyAuthRequired)
	assertProxyBody(t, proxy, upstream.URL, http.StatusOK, "ok")
}

func TestProxyCannotOpenSessionAfterClose(t *testing.T) {
	proxy, err := egress.StartManagedNetworkProxy(egress.NewStaticGate())
	if err != nil {
		t.Fatal(err)
	}
	if err := proxy.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if session, err := proxy.OpenSession(nil); err == nil || session != nil {
		t.Fatal("closed proxy allocated a new session")
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
