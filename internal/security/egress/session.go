package egress

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fwtllh-png/QCode/internal/platform/environment"
	"github.com/fwtllh-png/QCode/internal/security/netpolicy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// ErrProcessSessionUnsupported is returned when this platform cannot bind a
// per-process Session channel. Callers must fail closed and keep the process
// net-denied; they must not fall back to the workspace shared gate.
var ErrProcessSessionUnsupported = errors.New("process session network channel is unsupported")

// Channel lifecycle ceilings. ReadHeaderTimeout bounds a half-open request;
// IdleTimeout reaps keep-alive connections whose session is long gone;
// channelCloseTimeout bounds the graceful drain before hijacked CONNECT
// tunnels are force-closed. maxChannelWrapperDepth bounds backend-wrapper
// unwrapping (the managed/session-bound/close-binding chain is three deep).
// Public contract constants; boundary tests pin the close timeout.
const (
	channelReadHeaderTimeout = 10 * time.Second
	channelIdleTimeout       = 30 * time.Second
	channelCloseTimeout      = 5 * time.Second

	maxChannelWrapperDepth = 8
)

// ProcessSession is one Process Session's loopback port, credential, and Gate.
type ProcessSession interface {
	Port() uint16
	Credential() string
	Gate() *Gate
	Close() error
}

// ProcessSessionOpener allocates a Session channel on the Workspace proxy process.
type ProcessSessionOpener interface {
	OpenProcessSession(targets []Target) (ProcessSession, error)
}

type processSession struct {
	parent  *ManagedNetworkProxy
	channel *proxyChannel
	port    uint16
}

func (s *processSession) Port() uint16 {
	if s == nil {
		return 0
	}
	return s.port
}

func (s *processSession) Credential() string {
	if s == nil || s.channel == nil {
		return ""
	}
	return s.channel.credential
}

func (s *processSession) Gate() *Gate {
	if s == nil || s.channel == nil {
		return nil
	}
	return s.channel.gate
}

func (s *processSession) Close() error {
	if s == nil || s.parent == nil {
		return nil
	}
	s.parent.removeSession(s.port)
	if s.channel == nil {
		return nil
	}
	return s.channel.close()
}

type proxyChannel struct {
	gate *Gate
	// credential is the channel's Basic password. Any local process can reach
	// the loopback port, so reachability grants nothing without it. Empty
	// only on the browser channel, whose client cannot present one.
	credential string
	listener   net.Listener
	server     *http.Server
	done       chan struct{}
	// protocol serves origin-form requests (the GOPROXY auth service). It
	// is an atomic pointer because the workspace channel is already
	// serving when the service is bound after startup.
	protocol atomic.Pointer[http.Handler]
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
}

func listenProxyChannel(
	gate *Gate,
	protocol http.Handler,
	authenticated bool,
) (*proxyChannel, error) {
	if gate == nil {
		return nil, errors.New("process session proxy requires an egress gate")
	}
	credential := ""
	if authenticated {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return nil, fmt.Errorf("generate proxy channel credential: %w", err)
		}
		credential = hex.EncodeToString(secret[:])
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProcessSessionUnsupported, err)
	}
	channel := &proxyChannel{
		gate: gate, credential: credential, listener: listener,
		done: make(chan struct{}), conns: make(map[net.Conn]struct{}),
	}
	channel.setProtocol(protocol)
	channel.server = &http.Server{
		Handler:           http.HandlerFunc(channel.serveHTTP),
		ReadHeaderTimeout: channelReadHeaderTimeout,
		IdleTimeout:       channelIdleTimeout,
	}
	go func() {
		_ = channel.server.Serve(listener)
		close(channel.done)
	}()
	return channel, nil
}

func (c *proxyChannel) setProtocol(handler http.Handler) {
	if c == nil || handler == nil {
		return
	}
	c.protocol.Store(&handler)
}

func (c *proxyChannel) protocolHandler() http.Handler {
	if c == nil {
		return nil
	}
	if handler := c.protocol.Load(); handler != nil {
		return *handler
	}
	return nil
}

func (c *proxyChannel) port() uint16 {
	if c == nil || c.listener == nil {
		return 0
	}
	address, _ := c.listener.Addr().(*net.TCPAddr)
	if address == nil || address.Port < 1 || address.Port > 65535 {
		return 0
	}
	return uint16(address.Port)
}

func (c *proxyChannel) track(conn net.Conn) {
	if c == nil || conn == nil {
		return
	}
	c.mu.Lock()
	c.conns[conn] = struct{}{}
	c.mu.Unlock()
}

func (c *proxyChannel) untrack(conn net.Conn) {
	if c == nil || conn == nil {
		return
	}
	c.mu.Lock()
	delete(c.conns, conn)
	c.mu.Unlock()
}

func (c *proxyChannel) close() error {
	if c == nil || c.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), channelCloseTimeout)
	defer cancel()
	err := c.server.Close()
	c.mu.Lock()
	for conn := range c.conns {
		_ = conn.Close()
	}
	c.conns = map[net.Conn]struct{}{}
	c.mu.Unlock()
	select {
	case <-c.done:
		return err
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
}

func (c *proxyChannel) serveHTTP(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if request.Method == http.MethodConnect {
		if !c.authenticate(writer, request, "Proxy-Authorization") {
			return
		}
		c.serveConnect(writer, request)
		return
	}
	if handler := c.protocolHandler(); handler != nil && isOriginForm(request) {
		// Origin-form clients (GOPROXY) send the URL userinfo as
		// Authorization; the header is ours and never reaches the service.
		if !c.authenticate(writer, request, "Authorization", "Proxy-Authorization") {
			return
		}
		request.Header.Del("Authorization")
		request.Header.Del("Proxy-Authorization")
		handler.ServeHTTP(writer, request)
		return
	}
	if !c.authenticate(writer, request, "Proxy-Authorization") {
		return
	}
	c.serveForward(writer, request)
}

// authenticate accepts the channel credential as Basic auth in any of the
// named headers and otherwise answers the matching challenge.
func (c *proxyChannel) authenticate(
	writer http.ResponseWriter,
	request *http.Request,
	headers ...string,
) bool {
	if c.credential == "" {
		return true
	}
	want := []byte(sandbox.ManagedProxyUser + ":" + c.credential)
	for _, header := range headers {
		scheme, encoded, ok := strings.Cut(strings.TrimSpace(request.Header.Get(header)), " ")
		if !ok || !strings.EqualFold(scheme, "Basic") {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err == nil && subtle.ConstantTimeCompare(decoded, want) == 1 {
			return true
		}
	}
	status, challenge := http.StatusProxyAuthRequired, "Proxy-Authenticate"
	if headers[0] == "Authorization" {
		status, challenge = http.StatusUnauthorized, "WWW-Authenticate"
	}
	writer.Header().Set(challenge, `Basic realm="qcode-process-proxy"`)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(deniedPayload{
		Error: ErrDenied.Error(), Source: environment.SourceProcessProxy,
		Reason: "process proxy credential is missing or invalid",
	})
	return false
}

type boundOrigin interface {
	BoundHost() string
}

func boundProtocolHost(handler http.Handler) string {
	if bound, ok := handler.(boundOrigin); ok {
		return strings.TrimSpace(bound.BoundHost())
	}
	return ""
}

// denyBoundOrigin compares hosts under the Gate's normalization so an
// authority spelling like "proxy.example.:443" cannot sidestep the denial.
func (c *proxyChannel) denyBoundOrigin(
	writer http.ResponseWriter,
	host, protocol string,
	port uint16,
	method string,
) bool {
	handler := c.protocolHandler()
	bound := netpolicy.NormalizeHost(boundProtocolHost(handler))
	host = netpolicy.NormalizeHost(host)
	if bound == "" || host == "" || host != bound {
		return false
	}
	if protocol == "" {
		protocol = "https"
	}
	target := Target{
		Host: host, Protocol: protocol, Port: port, Methods: []string{method},
	}
	denied := &DeniedError{
		Host:     host,
		Protocol: protocol,
		Port:     port,
		Method:   method,
		Reason: "this host is served by the bound GOPROXY auth service; " +
			"use GOPROXY, do not CONNECT or fetch the upstream origin",
		Category: environment.CategoryTrustValidationFailed,
	}
	if c.gate != nil {
		c.gate.recordDenied(environment.SourceProcessProxy, target, denied)
	}
	writeDenied(writer, denied)
	return true
}

func isOriginForm(request *http.Request) bool {
	if request == nil || request.URL == nil {
		return false
	}
	host := request.URL.Hostname()
	return host == "" || host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func (c *proxyChannel) serveConnect(
	writer http.ResponseWriter,
	request *http.Request,
) {
	host, port, err := netpolicy.SplitAuthority(request.Host, 443)
	if err != nil {
		http.Error(writer, "invalid CONNECT target", http.StatusBadRequest)
		return
	}
	if c.denyBoundOrigin(writer, host, "https", port, http.MethodConnect) {
		return
	}
	target, err := dialAuthorized(c.gate, request.Context(), Target{
		Host: host, Protocol: "https", Port: port,
		Methods: []string{http.MethodConnect},
	})
	if err != nil {
		writeDenied(writer, err)
		return
	}
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		target.Close()
		http.Error(writer, "CONNECT is unavailable", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		target.Close()
		return
	}
	if _, err = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err == nil {
		err = buffered.Flush()
	}
	if err != nil {
		client.Close()
		target.Close()
		return
	}
	c.track(client)
	c.track(target)
	go func() {
		relay(client, target)
		c.untrack(client)
		c.untrack(target)
	}()
}

func (c *proxyChannel) serveForward(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if request.URL == nil || request.URL.Hostname() == "" {
		http.Error(writer, "absolute proxy URL is required", http.StatusBadRequest)
		return
	}
	if request.Host != "" &&
		!sameAuthority(request.Host, request.URL.Host, request.URL.Scheme) {
		http.Error(writer, "proxy host mismatch", http.StatusBadRequest)
		return
	}
	port, err := netpolicy.URLPort(request.URL)
	if err != nil {
		http.Error(writer, "invalid target port", http.StatusBadRequest)
		return
	}
	if c.denyBoundOrigin(
		writer,
		request.URL.Hostname(),
		request.URL.Scheme,
		port,
		request.Method,
	) {
		return
	}
	ips, err := c.gate.AuthorizeBeforeConnect(request.Context(), Target{
		Host: request.URL.Hostname(), Protocol: request.URL.Scheme, Port: port,
		Methods: []string{request.Method},
	}, environment.SourceProcessProxy)
	if err != nil {
		writeDenied(writer, err)
		return
	}
	outbound := request.Clone(request.Context())
	outbound.RequestURI = ""
	removeHopHeaders(outbound.Header)
	transport := &http.Transport{
		Proxy:             nil,
		DialContext:       pinnedDialer(ips, request.URL.Hostname(), port),
		ForceAttemptHTTP2: false,
	}
	defer transport.CloseIdleConnections()
	response, err := transport.RoundTrip(outbound)
	if err != nil {
		http.Error(writer, "managed egress upstream failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	removeHopHeaders(response.Header)
	for name, values := range response.Header {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	_, _ = io.Copy(writer, response.Body)
}

func dialAuthorized(
	gate *Gate,
	ctx context.Context,
	target Target,
) (net.Conn, error) {
	ips, err := gate.AuthorizeBeforeConnect(ctx, target, environment.SourceProcessProxy)
	if err != nil {
		return nil, err
	}
	return dialResolved(ctx, ips, target.Port)
}

// BindSessionOpener exposes a Workspace proxy's Session allocator on a
// child or other backend that was not constructed by NewManagedBackend.
func BindSessionOpener(backend sandbox.Backend, opener ProcessSessionOpener) sandbox.Backend {
	if backend == nil || opener == nil {
		return backend
	}
	return &sessionBoundBackend{Backend: backend, opener: opener}
}

type sessionBoundBackend struct {
	sandbox.Backend
	opener ProcessSessionOpener
}

func (b *sessionBoundBackend) OpenProcessSession(targets []Target) (ProcessSession, error) {
	if b == nil || b.opener == nil {
		return nil, ErrProcessSessionUnsupported
	}
	return b.opener.OpenProcessSession(targets)
}

func (b *sessionBoundBackend) InnerBackend() sandbox.Backend {
	if b == nil {
		return nil
	}
	return b.Backend
}

func BindProtocolHandler(backend sandbox.Backend, handler http.Handler) {
	current := backend
	for range maxChannelWrapperDepth {
		if current == nil {
			return
		}
		if binder, ok := current.(interface{ BindProtocolHandler(http.Handler) }); ok {
			binder.BindProtocolHandler(handler)
			return
		}
		wrapper, ok := current.(interface{ InnerBackend() sandbox.Backend })
		if !ok {
			return
		}
		next := wrapper.InnerBackend()
		if next == nil || next == current {
			return
		}
		current = next
	}
}

func LookupProcessSessionOpener(backend sandbox.Backend) (ProcessSessionOpener, bool) {
	current := backend
	for range maxChannelWrapperDepth {
		if current == nil {
			return nil, false
		}
		if opener, ok := current.(ProcessSessionOpener); ok {
			return opener, true
		}
		wrapper, ok := current.(interface{ InnerBackend() sandbox.Backend })
		if !ok {
			return nil, false
		}
		next := wrapper.InnerBackend()
		if next == nil || next == current {
			return nil, false
		}
		current = next
	}
	return nil, false
}
