// Package netpolicy is the single vocabulary for network endpoints: target
// parsing, host and port normalization, and address reach classification.
package netpolicy

import (
	"errors"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Target is one network endpoint. Host is normalized by NormalizeHost and
// Scheme is lowercase.
type Target struct {
	Scheme string
	Host   string
	Port   uint16
}

// Key is the canonical scheme://host:port identity of the target.
func (t Target) Key() string {
	return t.Scheme + "://" + net.JoinHostPort(t.Host, strconv.Itoa(int(t.Port)))
}

// ParseTarget parses a URL or a bare host[:port] endpoint. A bare endpoint is
// https and must name an IP literal, localhost, or a dotted hostname, so free
// text is not mistaken for a host. A URL without a port takes its scheme's
// default; schemes without a known default require an explicit port.
func ParseTarget(raw string) (Target, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return Target{}, errors.New("network target is empty")
	}
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return Target{}, err
		}
		return URLTarget(parsed)
	}
	if strings.ContainsAny(value, " \t\r\n/\\?#@") {
		return Target{}, errors.New("network target is not a host")
	}
	host, port := value, uint16(443)
	if h, rawPort, err := net.SplitHostPort(value); err == nil {
		parsed, err := ParsePort(rawPort)
		if err != nil {
			return Target{}, err
		}
		host, port = h, parsed
	}
	host = NormalizeHost(host)
	if host == "" {
		return Target{}, errors.New("network target requires a host")
	}
	if net.ParseIP(host) == nil && host != "localhost" && !strings.Contains(host, ".") {
		return Target{}, errors.New("network target is not a host")
	}
	return Target{Scheme: "https", Host: host, Port: port}, nil
}

// URLTarget returns the endpoint a parsed URL addresses. An empty scheme is
// https.
func URLTarget(value *url.URL) (Target, error) {
	if value == nil {
		return Target{}, errors.New("network target URL is missing")
	}
	host := NormalizeHost(value.Hostname())
	if host == "" {
		return Target{}, errors.New("network target requires a host")
	}
	scheme := strings.ToLower(value.Scheme)
	if scheme == "" {
		scheme = "https"
	}
	port, err := schemePort(scheme, value.Port())
	if err != nil {
		return Target{}, err
	}
	return Target{Scheme: scheme, Host: host, Port: port}, nil
}

// URLPort returns the explicit port of value or its scheme's default.
func URLPort(value *url.URL) (uint16, error) {
	if value == nil {
		return 0, errors.New("missing request URL")
	}
	return schemePort(strings.ToLower(value.Scheme), value.Port())
}

func schemePort(scheme, raw string) (uint16, error) {
	if raw != "" {
		return ParsePort(raw)
	}
	port, ok := DefaultPort(scheme)
	if !ok {
		return 0, errors.New("network target requires a port for scheme " + strconv.Quote(scheme))
	}
	return port, nil
}

// DefaultPort reports the port a scheme implies when a URL omits one.
func DefaultPort(scheme string) (uint16, bool) {
	switch strings.ToLower(scheme) {
	case "http":
		return 80, true
	case "https":
		return 443, true
	}
	return 0, false
}

// ParsePort parses a decimal TCP port. Zero is rejected: an explicit :0 is
// not defaultable, and mapping it to a scheme default would authorize a
// different endpoint than the one spelled.
func ParsePort(raw string) (uint16, error) {
	port, err := strconv.ParseUint(raw, 10, 16)
	if err != nil || port == 0 {
		return 0, errors.New("network port is invalid")
	}
	return uint16(port), nil
}

// SplitAuthority splits host[:port], applying fallback when the port is
// absent. The host is returned without brackets but otherwise as written.
func SplitAuthority(value string, fallback uint16) (string, uint16, error) {
	host, rawPort, err := net.SplitHostPort(value)
	if err != nil {
		var addrErr *net.AddrError
		if errors.As(err, &addrErr) && addrErr.Err == "missing port in address" {
			return strings.Trim(value, "[]"), fallback, nil
		}
		return "", 0, err
	}
	port, err := ParsePort(rawPort)
	if err != nil {
		return "", 0, err
	}
	return strings.Trim(host, "[]"), port, nil
}

// NormalizeHost lowercases host and strips surrounding whitespace, IPv6
// brackets, and a trailing root dot.
func NormalizeHost(host string) string {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// NormalizeMethods uppercases, sorts, and deduplicates HTTP methods, dropping
// empty entries.
func NormalizeMethods(methods []string) ([]string, error) {
	out := make([]string, 0, len(methods))
	for _, method := range methods {
		method = strings.ToUpper(strings.TrimSpace(method))
		if method == "" {
			continue
		}
		if strings.ContainsAny(method, " \t\r\n") {
			return nil, errors.New("network method is invalid")
		}
		out = append(out, method)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
