package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type ManagedNetworkProxy struct {
	workspace *proxyChannel
	protocol  http.Handler
	mu        sync.Mutex
	sessions  map[uint16]*processSession
}

func StartManagedNetworkProxy(gate *Gate) (*ManagedNetworkProxy, error) {
	channel, err := listenProxyChannel(gate, nil)
	if err != nil {
		return nil, err
	}
	return &ManagedNetworkProxy{
		workspace: channel,
		sessions:  make(map[uint16]*processSession),
	}, nil
}

// BindProtocolHandler binds the protocol service on every channel the proxy
// will serve: the stable workspace channel and each session channel opened
// afterwards. The workspace gate stays empty (deny-all) for CONNECT — the
// service enforces its own scope on origin-form requests, and external
// CONNECT keeps going through per-command session gates.
func (p *ManagedNetworkProxy) BindProtocolHandler(handler http.Handler) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.protocol = handler
	p.mu.Unlock()
	if p.workspace != nil {
		p.workspace.setProtocol(handler)
	}
}

type managedBackend struct {
	sandbox.Backend
	proxy *ManagedNetworkProxy
}

func NewManagedBackend(
	gate *Gate,
	options sandbox.Options,
	build func(sandbox.Options) (sandbox.Backend, error),
) (sandbox.Backend, error) {
	if build == nil {
		return nil, errors.New("managed network backend factory is required")
	}
	proxy, err := StartManagedNetworkProxy(gate)
	if err != nil {
		return nil, err
	}
	options.AllowNetwork = false
	options.ManagedProxyPort = sandbox.ManagedNetworkProxyPort(proxy.Port())
	backend, err := build(options)
	if err != nil {
		_ = proxy.Close(context.Background())
		return nil, fmt.Errorf("create managed sandbox backend: %w", err)
	}
	return &managedBackend{Backend: backend, proxy: proxy}, nil
}

func (b *managedBackend) OpenProcessSession(targets []Target) (ProcessSession, error) {
	if b == nil || b.proxy == nil {
		return nil, ErrProcessSessionUnsupported
	}
	return b.proxy.OpenSession(targets)
}

func (b *managedBackend) BindProtocolHandler(handler http.Handler) {
	if b == nil || b.proxy == nil {
		return
	}
	b.proxy.BindProtocolHandler(handler)
}

func (b *managedBackend) InnerBackend() sandbox.Backend {
	if b == nil {
		return nil
	}
	return b.Backend
}

func (b *managedBackend) Close() error {
	if b == nil {
		return nil
	}
	return errors.Join(sandbox.CloseBackend(b.Backend), b.proxy.Close(context.Background()))
}

func (p *ManagedNetworkProxy) URL() string {
	if p == nil || p.workspace == nil || p.workspace.listener == nil {
		return ""
	}
	return "http://" + p.workspace.listener.Addr().String()
}

func (p *ManagedNetworkProxy) Port() uint16 {
	if p == nil || p.workspace == nil {
		return 0
	}
	return p.workspace.port()
}

func (p *ManagedNetworkProxy) OpenProcessSession(targets []Target) (ProcessSession, error) {
	return p.OpenSession(targets)
}

func (p *ManagedNetworkProxy) OpenSession(targets []Target) (ProcessSession, error) {
	if p == nil {
		return nil, ErrProcessSessionUnsupported
	}
	gate := &Gate{Enforce: true}
	for _, target := range targets {
		gate.AllowTarget(target)
	}
	p.mu.Lock()
	handler := p.protocol
	p.mu.Unlock()
	channel, err := listenProxyChannel(gate, handler)
	if err != nil {
		return nil, err
	}
	port := channel.port()
	if port == 0 {
		_ = channel.close()
		return nil, fmt.Errorf("%w: session port unavailable", ErrProcessSessionUnsupported)
	}
	session := &processSession{parent: p, channel: channel, port: port}
	p.mu.Lock()
	p.sessions[port] = session
	p.mu.Unlock()
	return session, nil
}

func (p *ManagedNetworkProxy) removeSession(port uint16) {
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.sessions, port)
	p.mu.Unlock()
}

func (p *ManagedNetworkProxy) Close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	sessions := make([]*processSession, 0, len(p.sessions))
	for _, session := range p.sessions {
		sessions = append(sessions, session)
	}
	p.sessions = map[uint16]*processSession{}
	p.mu.Unlock()
	var closeErr error
	for _, session := range sessions {
		closeErr = errors.Join(closeErr, session.channel.close())
	}
	if p.workspace != nil {
		closeErr = errors.Join(closeErr, p.workspace.close())
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return errors.Join(closeErr, ctx.Err())
		default:
		}
	}
	return closeErr
}

func pinnedDialer(
	ips []net.IP,
	host string,
	port uint16,
) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		if len(ips) == 0 {
			return nil, fmt.Errorf("no approved address for %s", host)
		}
		return dialResolved(ctx, ips, port)
	}
}

func dialResolved(ctx context.Context, ips []net.IP, port uint16) (net.Conn, error) {
	var failures []error
	dialer := net.Dialer{}
	for _, ip := range ips {
		conn, err := dialer.DialContext(
			ctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(int(port))),
		)
		if err == nil {
			return conn, nil
		}
		failures = append(failures, err)
	}
	return nil, errors.Join(failures...)
}

func splitAuthority(value string, fallback uint16) (string, uint16, error) {
	host, rawPort, err := net.SplitHostPort(value)
	if err != nil {
		if strings.Contains(err.Error(), "missing port") {
			return strings.Trim(value, "[]"), fallback, nil
		}
		return "", 0, err
	}
	port, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || port == 0 {
		return "", 0, errors.New("invalid authority port")
	}
	return strings.Trim(host, "[]"), uint16(port), nil
}

func sameAuthority(header, target, protocol string) bool {
	left, leftPort, err := splitAuthority(header, defaultPort(protocol))
	if err != nil {
		return false
	}
	right, rightPort, err := splitAuthority(target, defaultPort(protocol))
	return err == nil && strings.EqualFold(left, right) && leftPort == rightPort
}

func defaultPort(protocol string) uint16 {
	if strings.EqualFold(protocol, "http") {
		return 80
	}
	return 443
}

type deniedPayload struct {
	Error          string `json:"error"`
	ErrorCategory  string `json:"error_category,omitempty"`
	RequiredAction string `json:"required_action,omitempty"`
	Source         string `json:"source"`
	Host           string `json:"host,omitempty"`
	Protocol       string `json:"protocol,omitempty"`
	Port           uint16 `json:"port,omitempty"`
	Method         string `json:"method,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

func writeDenied(writer http.ResponseWriter, err error) {
	payload := deniedPayload{
		Error:  ErrDenied.Error(),
		Source: "process_proxy",
	}
	var denied *DeniedError
	if errors.As(err, &denied) && denied != nil {
		payload.ErrorCategory = denied.Category
		payload.RequiredAction = denied.RequiredAction
		payload.Host = denied.Host
		payload.Protocol = denied.Protocol
		payload.Port = denied.Port
		payload.Method = denied.Method
		payload.Reason = denied.Reason
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(writer).Encode(payload)
}

func removeHopHeaders(header http.Header) {
	for _, name := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive",
		"Proxy-Authenticate", "Proxy-Authorization", "TE",
		"Trailer", "Transfer-Encoding", "Upgrade",
	} {
		header.Del(name)
	}
}

func relay(left, right net.Conn) {
	defer left.Close()
	defer right.Close()
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, _ = io.Copy(left, right)
	}()
	go func() {
		defer wait.Done()
		_, _ = io.Copy(right, left)
	}()
	wait.Wait()
}
