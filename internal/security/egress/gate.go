package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/common/environment"
	"github.com/fwtllh-png/QCode/internal/security/netpolicy"
)

const (
	reasonTargetNotGranted  = "target is not granted"
	reasonPrivateNotGranted = "local or private address is not granted"
	reasonDNSFailed         = "DNS resolution failed"
	reasonGateMissing       = "egress gate is not configured"
	reasonUnpinnable        = "egress transport cannot pin resolved addresses"
)

// ErrDenied is returned (via errors.Is) when RoundTrip targets a host the Gate
// has not granted. The string is stable for host and probe callers.
var ErrDenied = errors.New("egress denied")

type DeniedError struct {
	Host            string
	Protocol        string
	Port            uint16
	Method          string
	Reason          string
	Category        string
	RequiredAction  string
	ApprovalSettled bool
}

func (e *DeniedError) Error() string {
	if e == nil {
		return ErrDenied.Error()
	}
	if e.Host != "" {
		return fmt.Sprintf(
			"%s: host %s protocol %s is not granted",
			ErrDenied,
			e.Host,
			e.Protocol,
		)
	}
	if e.Reason != "" {
		return fmt.Sprintf("%s: %s", ErrDenied, e.Reason)
	}
	return ErrDenied.Error()
}

func (*DeniedError) Unwrap() error { return ErrDenied }

// DeniedTarget preserves the request destination and method through wrapped errors.
func DeniedTarget(err error) (DeniedError, bool) {
	var denied *DeniedError
	if !errors.As(err, &denied) || denied == nil || denied.Host == "" {
		return DeniedError{}, false
	}
	return *denied, true
}

// Gate is a host allowlist. A zero Gate is a static gate with no grants; it
// and a nil *Gate deny every target; there is no pass-through mode. The
// constructors fix what a gate consults, so no caller combines mode flags.
type Gate struct {
	mu   sync.RWMutex
	mode gateMode
	// LookupIP replaces the system resolver, for example in tests.
	LookupIP func(context.Context, string) ([]net.IP, error)
	allowed  map[string]targetGrant
	receipts []Receipt
	approver RuntimeApprover
	// approverGeneration identifies the latest BindRuntimeApprover call.
	approverGeneration uint64
	asks               askState
}

type gateMode uint8

const (
	// gateStatic consults only the gate's own grants.
	gateStatic gateMode = iota
	// gateCallScoped consults the Guard call scope carried by the request
	// context, falling back to its own grants outside a call.
	gateCallScoped
	// gateBrowser admits, without a grant, any target whose every resolved
	// address is public. Non-public destinations still need a grant with
	// private access.
	gateBrowser
)

// NewStaticGate returns a gate that allows exactly its grants: provider,
// process, and Process Session traffic.
func NewStaticGate(targets ...Target) *Gate {
	gate := &Gate{}
	for _, target := range targets {
		gate.AllowTarget(target)
	}
	return gate
}

// NewCallScopedGate returns the Web tool gate. Inside a Guard call it consults
// that call's dynamic grants; approvals discovered during the call join the
// call scope instead of the gate.
func NewCallScopedGate() *Gate {
	return &Gate{mode: gateCallScoped}
}

// NewBrowserGate returns the browser session gate: page subresources,
// redirects, and scripts reach the public web while the host network stays
// behind approval.
func NewBrowserGate() *Gate {
	return &Gate{mode: gateBrowser}
}

// Private access is attached to each method, never shared across methods.
type targetGrant struct {
	allMethods bool
	allPrivate bool
	methods    map[string]bool
}

type Target struct {
	Host         string
	Protocol     string
	Port         uint16
	Methods      []string
	AllowPrivate bool
}

type Receipt struct {
	At             time.Time `json:"at"`
	Source         string    `json:"source"`
	Host           string    `json:"host"`
	Protocol       string    `json:"protocol"`
	Port           uint16    `json:"port"`
	Method         string    `json:"method,omitempty"`
	Decision       string    `json:"decision"`
	Reason         string    `json:"reason,omitempty"`
	Category       string    `json:"error_category,omitempty"`
	RequiredAction string    `json:"required_action,omitempty"`
	ResolvedIPs    []string  `json:"resolved_ips,omitempty"`
}

// maxReceipts bounds the per-gate receipt log: receipts are diagnostic
// evidence for receipts()/egress receipts, and an unbounded log would grow
// with every authorized request for the workspace's lifetime. Older entries
// are dropped. Public contract constant.
const maxReceipts = 256

func key(target Target) string {
	return target.Protocol + "://" + net.JoinHostPort(
		target.Host,
		strconv.Itoa(int(target.Port)),
	)
}

// Allow grants host+protocol for subsequent RoundTrips.
func (g *Gate) Allow(host, protocol string) {
	target := Target{
		Host: host, Protocol: protocol,
		AllowPrivate: true,
	}
	g.AllowTarget(target)
}

// AllowTarget adds a grant without revoking earlier grants for the same origin.
// An empty Methods list grants all methods at this target's private-access level.
func (g *Gate) AllowTarget(target Target) {
	if g == nil {
		return
	}
	target, err := normalizeTarget(target)
	if err != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.allowed == nil {
		g.allowed = make(map[string]targetGrant)
	}
	origin := key(target)
	grant := g.allowed[origin]
	if len(target.Methods) == 0 {
		grant.allMethods = true
		grant.allPrivate = grant.allPrivate || target.AllowPrivate
	} else {
		if grant.methods == nil {
			grant.methods = make(map[string]bool)
		}
		for _, method := range target.Methods {
			grant.methods[method] = grant.methods[method] || target.AllowPrivate
		}
	}
	g.allowed[origin] = grant
}

// AllowURL grants the host+scheme of a URL or bare endpoint string.
func (g *Gate) AllowURL(raw string) bool {
	parsed, err := netpolicy.ParseTarget(raw)
	if err != nil {
		return false
	}
	target := Target{
		Host: parsed.Host, Protocol: parsed.Scheme, Port: parsed.Port,
		AllowPrivate: true,
	}
	g.AllowTarget(target)
	return true
}

// Allowed reports whether host+protocol is on the allowlist.
func (g *Gate) Allowed(host, protocol string) bool {
	if g == nil {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	target, err := normalizeTarget(Target{Host: host, Protocol: protocol})
	if err != nil {
		return false
	}
	_, ok := g.allowed[key(target)]
	return ok
}

// Check returns ErrDenied when the request URL is not granted.
func (g *Gate) Check(req *http.Request) error {
	if req == nil || req.URL == nil {
		return &DeniedError{Reason: "missing request URL"}
	}
	host := req.URL.Hostname()
	if host == "" {
		host = strings.TrimSpace(req.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
	}
	protocol := strings.ToLower(req.URL.Scheme)
	if protocol == "" {
		protocol = "https"
	}
	port, err := netpolicy.URLPort(req.URL)
	if err != nil {
		return &DeniedError{Host: host, Protocol: protocol, Reason: err.Error()}
	}
	_, err = g.Authorize(req.Context(), Target{
		Host: host, Protocol: protocol, Port: port, Methods: []string{req.Method},
	}, "http")
	return err
}

func (g *Gate) Authorize(
	ctx context.Context,
	request Target,
	source string,
) ([]net.IP, error) {
	return g.authorize(ctx, request, source, false)
}

// AuthorizeBeforeConnect asks the current execution's Approval path for an
// ungranted target and only then resolves or dials. Probe callers must keep
// using Authorize so they cannot mint grants.
func (g *Gate) AuthorizeBeforeConnect(
	ctx context.Context,
	request Target,
	source string,
) ([]net.IP, error) {
	return g.authorize(ctx, request, source, true)
}

func (g *Gate) authorize(
	ctx context.Context,
	request Target,
	source string,
	discover bool,
) ([]net.IP, error) {
	request, err := normalizeTarget(request)
	if err != nil {
		g.record(Receipt{
			At: time.Now().UTC(), Source: source, Decision: "deny",
			Reason: err.Error(),
		})
		return nil, &DeniedError{Reason: err.Error()}
	}
	if g == nil {
		return nil, deniedTarget(request, reasonGateMissing)
	}
	scoped, allowed, allowPrivate := false, false, false
	if g.mode == gateCallScoped {
		scoped, allowed, allowPrivate = scopedPermissions(ctx, request)
	}
	if !scoped {
		g.mu.RLock()
		grant := g.allowed[key(request)]
		allowed, allowPrivate = grant.permissions(request.Methods)
		g.mu.RUnlock()
	}
	var resolved []net.IP
	if !allowed && g.mode == gateBrowser {
		ips, resolveErr := g.resolve(ctx, request.Host)
		if resolveErr != nil {
			denied := deniedTarget(request, reasonDNSFailed)
			g.recordDenied(source, request, denied)
			return nil, denied
		}
		if len(ips) == 0 || slices.ContainsFunc(ips, func(ip net.IP) bool {
			return netpolicy.Classify(ip) != netpolicy.Public
		}) {
			denied := deniedTarget(request, reasonPrivateNotGranted)
			g.recordDenied(source, request, denied)
			return nil, denied
		}
		resolved, allowed = ips, true
	}
	if !allowed && discover {
		if err := g.discover(ctx, request); err == nil {
			// The approval covers the target as it resolves right here, and
			// this one resolution also serves the request: private dialing
			// joins the grant only when the approved target actually
			// resolved to a private address, so a later DNS rebinding of a
			// public-approved origin to a private address stays denied by
			// the per-request resolution check below.
			granted := request
			granted.AllowPrivate = false
			ips, resolveErr := g.resolve(ctx, request.Host)
			if resolveErr != nil {
				denied := deniedTarget(request, reasonDNSFailed)
				g.recordDenied(source, request, denied)
				return nil, denied
			}
			for _, ip := range ips {
				if netpolicy.Classify(ip) != netpolicy.Public {
					granted.AllowPrivate = true
					break
				}
			}
			resolved = ips
			if g.mode == gateCallScoped {
				AllowInScope(ctx, granted)
				scoped, allowed, allowPrivate = scopedPermissions(ctx, request)
			} else {
				g.AllowTarget(granted)
				g.mu.RLock()
				grant := g.allowed[key(request)]
				allowed, allowPrivate = grant.permissions(request.Methods)
				g.mu.RUnlock()
			}
		} else if !errors.Is(err, errNoRuntimeApprover) {
			denied := settledDenied(request, err)
			g.recordDenied(source, request, denied)
			return nil, denied
		}
	}
	if !allowed {
		err := deniedTarget(request, reasonTargetNotGranted)
		g.recordDenied(source, request, err)
		return nil, err
	}
	ips := resolved
	if ips == nil {
		ips, err = g.resolve(ctx, request.Host)
	}
	if err != nil || len(ips) == 0 {
		denied := deniedTarget(request, reasonDNSFailed)
		g.recordDenied(source, request, denied)
		return nil, denied
	}
	for _, ip := range ips {
		// Model-driven Web traffic (a Guard call scope or the browser)
		// reaches host-local addresses only through a host that names them;
		// a hostname granted for an intranet address must not rebind onto
		// loopback services or cloud metadata.
		modelDriven := scoped || g.mode == gateBrowser
		reach := netpolicy.Classify(ip)
		hostLocalByName := modelDriven && reach == netpolicy.HostLocal &&
			!netpolicy.NamesHostLocal(request.Host)
		if (reach != netpolicy.Public && !allowPrivate) || hostLocalByName {
			denied := deniedTarget(request, reasonPrivateNotGranted)
			g.recordDenied(source, request, denied)
			return nil, denied
		}
	}
	g.record(Receipt{
		At: time.Now().UTC(), Source: source,
		Host: request.Host, Protocol: request.Protocol, Port: request.Port,
		Method: firstMethod(request.Methods), Decision: "allow",
		ResolvedIPs: ipStrings(ips),
	})
	return ips, nil
}

func (g *Gate) Receipts() []Receipt {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]Receipt, len(g.receipts))
	copy(out, g.receipts)
	for index := range out {
		out[index].ResolvedIPs = append([]string(nil), out[index].ResolvedIPs...)
	}
	return out
}

// PinnedTransport is a base RoundTripper that sends a request only to the
// addresses the Gate resolved and approved for it.
type PinnedTransport interface {
	RoundTripPinned(req *http.Request, addresses []net.IP) (*http.Response, error)
}

// RoundTripper wraps base and refuses ungranted hosts. Every request goes only
// to the addresses the Gate resolved and approved: an *http.Transport base
// (nil selects http.DefaultTransport) is cloned with a pinned dialer, a
// PinnedTransport receives the addresses, and any other RoundTripper is
// refused because it could re-resolve the host.
func (g *Gate) RoundTripper(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{gate: g, base: base}
}

// WrapClient returns a shallow clone of client whose Transport is gated.
// A nil gate denies every request.
func WrapClient(client *http.Client, gate *Gate) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	clone.Transport = gate.RoundTripper(client.Transport)
	return &clone
}

type transport struct {
	gate *Gate
	base http.RoundTripper
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, &DeniedError{Reason: "missing request URL"}
	}
	port, err := netpolicy.URLPort(req.URL)
	if err != nil {
		return nil, err
	}
	base, native := t.base.(*http.Transport)
	custom, declared := t.base.(PinnedTransport)
	if !native && !declared {
		return nil, &DeniedError{
			Host: req.URL.Hostname(), Protocol: req.URL.Scheme, Port: port,
			Method: req.Method, Reason: reasonUnpinnable,
		}
	}
	ips, err := t.gate.AuthorizeBeforeConnect(req.Context(), Target{
		Host: req.URL.Hostname(), Protocol: req.URL.Scheme, Port: port,
		Methods: []string{req.Method},
	}, "http")
	if err != nil {
		return nil, err
	}
	if !native {
		return custom.RoundTripPinned(req, slices.Clone(ips))
	}
	pinned := base.Clone()
	pinned.Proxy = nil
	pinned.DialContext = pinnedDialer(ips, req.URL.Hostname(), port)
	response, err := pinned.RoundTrip(req)
	if err != nil {
		pinned.CloseIdleConnections()
		return nil, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		response.Body = &releasingBody{ReadCloser: response.Body, transport: pinned}
	}
	return response, nil
}

// releasingBody closes the per-request transport's pooled connection once the
// caller is done. DisableKeepAlives would avoid the pool, but it also stops
// request cancellation from closing an in-flight connection that carries a
// request body.
type releasingBody struct {
	io.ReadCloser
	transport *http.Transport
}

func (b *releasingBody) Close() error {
	err := b.ReadCloser.Close()
	b.transport.CloseIdleConnections()
	return err
}

func normalizeTarget(target Target) (Target, error) {
	target.Host = netpolicy.NormalizeHost(target.Host)
	target.Protocol = strings.ToLower(strings.TrimSpace(target.Protocol))
	if target.Protocol == "" {
		target.Protocol = "https"
	}
	if target.Host == "" || strings.ContainsAny(target.Host, "/\\@") {
		return Target{}, errors.New("network target requires one host")
	}
	defaultPort, known := netpolicy.DefaultPort(target.Protocol)
	if !known {
		return Target{}, errors.New("network target protocol is invalid")
	}
	if target.Port == 0 {
		target.Port = defaultPort
	}
	methods, err := netpolicy.NormalizeMethods(target.Methods)
	if err != nil {
		return Target{}, err
	}
	target.Methods = methods
	return target, nil
}

// permissions runs under Gate.mu so Authorize retains only immutable decisions
// while resolving DNS. A request without methods requires an unrestricted grant.
func (g targetGrant) permissions(requested []string) (allowed, allowPrivate bool) {
	if len(requested) == 0 {
		return g.allMethods, g.allPrivate
	}
	allowPrivate = true
	for _, method := range requested {
		private, exists := g.methods[method]
		if !g.allMethods && !exists {
			return false, false
		}
		allowPrivate = allowPrivate && (g.allPrivate || private)
	}
	return true, allowPrivate
}

func firstMethod(methods []string) string {
	if len(methods) == 0 {
		return ""
	}
	return methods[0]
}

func (g *Gate) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return []net.IP{ip}, nil
	}
	if g.LookupIP != nil {
		return g.LookupIP(ctx, host)
	}
	addresses, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	return addresses, nil
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}

func deniedTarget(target Target, reason string) *DeniedError {
	denied := &DeniedError{
		Host: target.Host, Protocol: target.Protocol,
		Port: target.Port, Method: firstMethod(target.Methods),
		Reason: reason,
	}
	switch reason {
	case reasonTargetNotGranted, reasonPrivateNotGranted:
		denied.Category = environment.CategoryNetworkTargetUnapproved
		denied.RequiredAction = environment.ActionApproveNetworkTarget
	}
	return denied
}

func (g *Gate) recordDenied(source string, target Target, denied *DeniedError) {
	if denied == nil {
		return
	}
	g.record(Receipt{
		At: time.Now().UTC(), Source: source,
		Host: target.Host, Protocol: target.Protocol, Port: target.Port,
		Method: firstMethod(target.Methods), Decision: "deny", Reason: denied.Reason,
		Category: denied.Category, RequiredAction: denied.RequiredAction,
	})
}

func (g *Gate) record(receipt Receipt) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.receipts = append(g.receipts, receipt)
	if len(g.receipts) > maxReceipts {
		copy(g.receipts, g.receipts[len(g.receipts)-maxReceipts:])
		g.receipts = g.receipts[:maxReceipts]
	}
}
