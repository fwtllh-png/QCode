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
	"time"

	"github.com/fwtllh-png/QCode/internal/environment"
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
// tunnels are force-closed. Public contract constants; boundary tests pin the
// close timeout.
const (
	channelReadHeaderTimeout = 10 * time.Second
	channelIdleTimeout       = 30 * time.Second
	channelCloseTimeout      = 5 * time.Second
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
	// the loopback port, so reachability grants nothing without it.
	credential string
	listener   net.Listener
	server     *http.Server
	done       chan struct{}
	mu         sync.Mutex
	conns      map[net.Conn]struct{}
	closed     bool
	closeOnce  sync.Once
	closeErr   error
}

func listenProxyChannel(gate *Gate) (*proxyChannel, error) {
	if gate == nil {
		return nil, errors.New("process session proxy requires an egress gate")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, fmt.Errorf("generate proxy channel credential: %w", err)
	}
	credential := hex.EncodeToString(secret[:])
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProcessSessionUnsupported, err)
	}
	channel := &proxyChannel{
		gate: gate, credential: credential, listener: listener,
		done: make(chan struct{}), conns: make(map[net.Conn]struct{}),
	}
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

func (c *proxyChannel) track(conns ...net.Conn) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		return false
	}
	for _, conn := range conns {
		c.conns[conn] = struct{}{}
	}
	c.mu.Unlock()
	return true
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
	c.closeOnce.Do(func() { c.closeErr = c.closeChannel() })
	return c.closeErr
}

func (c *proxyChannel) closeChannel() error {
	ctx, cancel := context.WithTimeout(context.Background(), channelCloseTimeout)
	defer cancel()
	c.mu.Lock()
	c.closed = true
	conns := c.conns
	c.conns = nil
	c.mu.Unlock()
	err := c.server.Close()
	for conn := range conns {
		_ = conn.Close()
	}
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
		if !c.authenticate(writer, request) {
			return
		}
		c.serveConnect(writer, request)
		return
	}
	if !c.authenticate(writer, request) {
		return
	}
	c.serveForward(writer, request)
}

// authenticate accepts only the proxy channel credential. Origin Authorization
// belongs to the requested upstream and cannot authenticate a local channel.
func (c *proxyChannel) authenticate(writer http.ResponseWriter, request *http.Request) bool {
	if c.credential == "" {
		http.Error(writer, "proxy credential unavailable", http.StatusProxyAuthRequired)
		return false
	}
	want := []byte(sandbox.ManagedProxyUser + ":" + c.credential)
	scheme, encoded, ok := strings.Cut(strings.TrimSpace(request.Header.Get("Proxy-Authorization")), " ")
	if ok && strings.EqualFold(scheme, "Basic") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err == nil && subtle.ConstantTimeCompare(decoded, want) == 1 {
			return true
		}
	}
	writer.Header().Set("Proxy-Authenticate", `Basic realm="qcode-process-proxy"`)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusProxyAuthRequired)
	_ = json.NewEncoder(writer).Encode(deniedPayload{
		Error: ErrDenied.Error(), Source: environment.SourceProcessProxy,
		Reason: "process proxy credential is missing or invalid",
	})
	return false
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
	// Registration and revocation share one lock. Once HTTP relinquishes
	// ownership, the channel owns both sockets even while the 200 is blocked.
	if !c.track(client, target) {
		return
	}
	if _, err = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err == nil {
		err = buffered.Flush()
	}
	if err != nil {
		client.Close()
		target.Close()
		c.untrack(client)
		c.untrack(target)
		return
	}
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

// LookupProcessSessionOpener reports the Session allocator a composed
// backend carries. Backends are composed explicitly, so this is a direct
// capability check rather than a walk through wrappers.
func LookupProcessSessionOpener(backend sandbox.Backend) (ProcessSessionOpener, bool) {
	opener, ok := backend.(ProcessSessionOpener)
	return opener, ok && opener != nil
}
