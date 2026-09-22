package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/environment"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

const (
	reasonTargetNotGranted  = "target is not granted"
	reasonPrivateNotGranted = "local or private address is not granted"
	reasonDNSFailed         = "DNS resolution failed"
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

// Gate is a session-scoped host allowlist. A zero Gate denies everything once
// Enforce is true; with Enforce false (or a nil *Gate on WrapClient) traffic
// passes through unchanged so unit tests that never wired a broker keep working.
type Gate struct {
	mu      sync.RWMutex
	Enforce bool
	// UseCallScope binds Web tool traffic to Guard's dynamic call grants.
	// Provider and process gates retain their independently configured grants.
	UseCallScope bool
	LookupIP     func(context.Context, string) ([]net.IP, error)
	allowed      map[string]targetGrant
	receipts     []Receipt
	approver     RuntimeApprover
	asks         askState
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
	parsed, ok := policy.ParseNetworkTarget(raw)
	if !ok {
		return false
	}
	target := Target{
		Host: parsed.Host, Protocol: parsed.Protocol, Port: parsed.Port,
		AllowPrivate: true,
	}
	g.AllowTarget(target)
	return true
}

// Allowed reports whether host+protocol is on the allowlist.
func (g *Gate) Allowed(host, protocol string) bool {
	if g == nil {
		return true
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.Enforce {
		return true
	}
	target, err := normalizeTarget(Target{Host: host, Protocol: protocol})
	if err != nil {
		return false
	}
	_, ok := g.allowed[key(target)]
	return ok
}

// Check returns ErrDenied when Enforce is on and the request URL is not granted.
func (g *Gate) Check(req *http.Request) error {
	if g == nil || !g.Enforce {
		return nil
	}
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
	port, err := requestPort(req.URL)
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
	if g == nil || !g.Enforce {
		return nil, nil
	}
	scoped, allowed, allowPrivate := false, false, false
	if g.UseCallScope {
		scoped, allowed, allowPrivate = scopedPermissions(ctx, request)
	}
	if !scoped {
		g.mu.RLock()
		grant := g.allowed[key(request)]
		allowed, allowPrivate = grant.permissions(request.Methods)
		g.mu.RUnlock()
	}
	var resolved []net.IP
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
				if nonPublicIP(ip) {
					granted.AllowPrivate = true
					break
				}
			}
			resolved = ips
			if g.UseCallScope {
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
		if err != nil {
			denied := deniedTarget(request, reasonDNSFailed)
			g.recordDenied(source, request, denied)
			return nil, denied
		}
	}
	for _, ip := range ips {
		if nonPublicIP(ip) && !allowPrivate {
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

// RoundTripper wraps base and refuses ungranted hosts when Enforce is on.
func (g *Gate) RoundTripper(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{gate: g, base: base}
}

// WrapClient returns a shallow clone of client whose Transport is gated.
// A nil gate returns client unchanged.
func WrapClient(client *http.Client, gate *Gate) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	if gate == nil {
		return client
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
	port, err := requestPort(req.URL)
	if err != nil {
		return nil, err
	}
	ips, err := t.gate.AuthorizeBeforeConnect(req.Context(), Target{
		Host: req.URL.Hostname(), Protocol: req.URL.Scheme, Port: port,
		Methods: []string{req.Method},
	}, "http")
	if err != nil {
		return nil, err
	}
	if base, ok := t.base.(*http.Transport); ok && len(ips) != 0 {
		pinned := base.Clone()
		pinned.Proxy = nil
		pinned.DisableKeepAlives = true
		pinned.DialContext = pinnedDialer(ips, req.URL.Hostname(), port)
		return pinned.RoundTrip(req)
	}
	return t.base.RoundTrip(req)
}

// HostOf is a small helper for callers that already have a parsed URL.
func HostOf(u *url.URL) (host, protocol string, ok bool) {
	if u == nil || u.Hostname() == "" {
		return "", "", false
	}
	protocol = strings.ToLower(u.Scheme)
	if protocol == "" {
		protocol = "https"
	}
	return strings.ToLower(u.Hostname()), protocol, true
}

func normalizeTarget(target Target) (Target, error) {
	target.Host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(target.Host), "."))
	target.Protocol = strings.ToLower(strings.TrimSpace(target.Protocol))
	if target.Protocol == "" {
		target.Protocol = "https"
	}
	if target.Host == "" || strings.ContainsAny(target.Host, "/\\@") {
		return Target{}, errors.New("network target requires one host")
	}
	if target.Port == 0 {
		switch target.Protocol {
		case "http":
			target.Port = 80
		case "https":
			target.Port = 443
		default:
			return Target{}, errors.New("network target requires one port")
		}
	}
	if target.Protocol != "http" && target.Protocol != "https" {
		return Target{}, errors.New("network target protocol is invalid")
	}
	methods := make([]string, 0, len(target.Methods))
	for _, method := range target.Methods {
		method = strings.ToUpper(strings.TrimSpace(method))
		if method == "" {
			continue
		}
		if strings.ContainsAny(method, " \t\r\n") {
			return Target{}, errors.New("network target method is invalid")
		}
		methods = append(methods, method)
	}
	slices.Sort(methods)
	target.Methods = slices.Compact(methods)
	return target, nil
}

func requestPort(value *url.URL) (uint16, error) {
	if value == nil {
		return 0, errors.New("missing request URL")
	}
	if raw := value.Port(); raw != "" {
		port, err := strconv.ParseUint(raw, 10, 16)
		if err != nil || port == 0 {
			// An explicit :0 is not a defaultable port; silently mapping it
			// to the scheme default would authorize a different endpoint
			// than the URL spelled.
			return 0, errors.New("request port is invalid")
		}
		return uint16(port), nil
	}
	switch strings.ToLower(value.Scheme) {
	case "http":
		return 80, nil
	case "https":
		return 443, nil
	default:
		return 0, errors.New("request protocol is invalid")
	}
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

func nonPublicIP(ip net.IP) bool {
	return ip == nil || ip.IsUnspecified() || ip.IsLoopback() ||
		ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || inCGNATRange(ip) || inBenchmarkRange(ip) ||
		inReservedRange(ip)
}

// inCGNATRange covers 100.64.0.0/10 (RFC 6598 carrier-grade NAT): shared
// address space an approved public origin must not be able to rebind into.
func inCGNATRange(ip net.IP) bool {
	address := ip.To4()
	return address != nil && address[0] == 100 &&
		address[1] >= 64 && address[1] <= 127
}

// inBenchmarkRange covers 198.18.0.0/15 (RFC 2544 benchmarking): never a
// legitimate egress destination from a build.
func inBenchmarkRange(ip net.IP) bool {
	address := ip.To4()
	return address != nil && address[0] == 198 &&
		(address[1] == 18 || address[1] == 19)
}

// inReservedRange covers 240.0.0.0/4 (reserved, incl. broadcast): Go's IP
// helpers have no predicate for it.
func inReservedRange(ip net.IP) bool {
	address := ip.To4()
	return address != nil && address[0] >= 240
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
