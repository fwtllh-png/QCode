package egress

import (
	"context"
	"net"
	"strings"

	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// GrantPrivate decides private reach for an approved URL target, as it
// resolves at grant time. A host that names a non-public destination (an IP
// literal or localhost) is granted as written. A hostname qualifies only when
// it resolves to a private network and to no host-local address, so a public
// origin that later rebinds, or a name pointing at loopback services or cloud
// metadata, stays denied.
func GrantPrivate(
	ctx context.Context,
	lookup func(context.Context, string) ([]net.IP, error),
	host string,
) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if policy.NamesHostLocal(host) {
		return true
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return nonPublicIP(ip)
	}
	if host == "" {
		return false
	}
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	ips, err := lookup(ctx, host)
	if err != nil {
		return false
	}
	private := false
	for _, ip := range ips {
		if hostLocalIP(ip) {
			return false
		}
		private = private || nonPublicIP(ip)
	}
	return private
}

// hostLocalIP covers addresses that reach this machine or its link. Cloud
// metadata (169.254.169.254) is link-local.
func hostLocalIP(ip net.IP) bool {
	return ip == nil || ip.IsUnspecified() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}
