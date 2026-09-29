package egress

import (
	"context"
	"net"

	"github.com/fwtllh-png/QCode/internal/security/netpolicy"
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
	host = netpolicy.NormalizeHost(host)
	if netpolicy.NamesHostLocal(host) {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return netpolicy.Classify(ip) != netpolicy.Public
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
		reach := netpolicy.Classify(ip)
		if reach == netpolicy.HostLocal {
			return false
		}
		private = private || reach != netpolicy.Public
	}
	return private
}
