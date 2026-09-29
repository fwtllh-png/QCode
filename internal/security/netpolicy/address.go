package netpolicy

import (
	"net"
	"net/netip"
	"strings"
)

// Reach classifies where traffic to an address is delivered. Classes are
// exclusive and checked in the order HostLocal, Private, Reserved, Public.
type Reach uint8

const (
	// HostLocal reaches this machine or its link: loopback, unspecified,
	// link local (including cloud metadata), and link-scoped multicast.
	HostLocal Reach = iota + 1
	// Private reaches a site network: RFC 1918, RFC 6598 shared address
	// space, and RFC 4193 unique local addresses.
	Private
	// Reserved is every other block the IANA special-purpose registries mark
	// not globally reachable, plus multicast and space outside 2000::/3.
	Reserved
	// Public is globally reachable unicast.
	Public
)

func (r Reach) String() string {
	switch r {
	case HostLocal:
		return "host_local"
	case Private:
		return "private"
	case Reserved:
		return "reserved"
	case Public:
		return "public"
	}
	return "unknown"
}

// Classify reports the reach of ip. An address that cannot be parsed is
// host-local so every caller fails closed.
func Classify(ip net.IP) Reach {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return HostLocal
	}
	return ClassifyAddr(addr)
}

// ClassifyAddr reports the reach of addr after unwrapping any IPv4
// destination embedded in an IPv6 form.
func ClassifyAddr(addr netip.Addr) Reach {
	if !addr.IsValid() {
		return HostLocal
	}
	addr = routedAddr(addr.WithZone(""))
	switch {
	case hostLocal(addr):
		return HostLocal
	case containsAddr(privateBlocks, addr):
		return Private
	case nonPublic(addr):
		return Reserved
	}
	return Public
}

// NamesHostLocal reports whether host itself names this machine or its link:
// a localhost name or an IP literal whose reach is HostLocal.
func NamesHostLocal(host string) bool {
	host = NormalizeHost(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && ClassifyAddr(addr) == HostLocal
}

// nonPublicIPv4 lists every IPv4 block the IANA IPv4 Special-Purpose Address
// Registry marks not globally reachable, plus multicast (RFC 5771).
var nonPublicIPv4 = mustPrefixes(
	"0.0.0.0/8",       // RFC 791 "this network"
	"10.0.0.0/8",      // RFC 1918 private
	"100.64.0.0/10",   // RFC 6598 shared address space (CGNAT)
	"127.0.0.0/8",     // RFC 1122 loopback
	"169.254.0.0/16",  // RFC 3927 link local, incl. cloud metadata
	"172.16.0.0/12",   // RFC 1918 private
	"192.0.0.0/24",    // RFC 6890 IETF protocol assignments
	"192.0.2.0/24",    // RFC 5737 TEST-NET-1
	"192.88.99.0/24",  // RFC 7526 deprecated 6to4 relay anycast
	"192.168.0.0/16",  // RFC 1918 private
	"198.18.0.0/15",   // RFC 2544 benchmarking
	"198.51.100.0/24", // RFC 5737 TEST-NET-2
	"203.0.113.0/24",  // RFC 5737 TEST-NET-3
	"224.0.0.0/4",     // RFC 5771 multicast
	"240.0.0.0/4",     // RFC 1112 reserved, incl. limited broadcast
)

// The IANA IPv6 Address Space registry allocates only 2000::/3 as global
// unicast; everything outside it (loopback, ULA, link local, site local,
// multicast, discard-only, local-use NAT64, IPv4-compatible) is non-public.
var globalUnicastIPv6 = netip.MustParsePrefix("2000::/3")

// nonPublicIPv6 lists special-purpose blocks inside 2000::/3 that the IANA
// IPv6 Special-Purpose Address Registry marks not globally reachable.
var nonPublicIPv6 = mustPrefixes(
	"2001::/23",     // RFC 2928 IETF protocol assignments, incl. Teredo
	"2001:db8::/32", // RFC 3849 documentation
	"3fff::/20",     // RFC 9637 documentation
)

// hostLocalIPv4 reaches this machine or its link.
var hostLocalIPv4 = mustPrefixes(
	"0.0.0.0/8",      // RFC 791; 0.0.0.0 connects to the local host
	"127.0.0.0/8",    // RFC 1122 loopback
	"169.254.0.0/16", // RFC 3927 link local, incl. cloud metadata
	"224.0.0.0/24",   // RFC 5771 local network control block
)

var privateBlocks = mustPrefixes(
	"10.0.0.0/8",     // RFC 1918
	"100.64.0.0/10",  // RFC 6598 shared address space (CGNAT)
	"172.16.0.0/12",  // RFC 1918
	"192.168.0.0/16", // RFC 1918
	"fc00::/7",       // RFC 4193 unique local
)

var (
	nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96") // RFC 6052
	sixToFour      = netip.MustParsePrefix("2002::/16")    // RFC 3056
)

func nonPublic(addr netip.Addr) bool {
	if addr.Is4() {
		return containsAddr(nonPublicIPv4, addr)
	}
	return !globalUnicastIPv6.Contains(addr) || containsAddr(nonPublicIPv6, addr)
}

func hostLocal(addr netip.Addr) bool {
	if addr.Is4() {
		return containsAddr(hostLocalIPv4, addr)
	}
	return addr.IsUnspecified() || addr.IsLoopback() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast()
}

// routedAddr returns the address traffic is actually delivered to. IPv4-mapped
// (RFC 4291), IPv4-compatible (RFC 4291), NAT64 well-known prefix, and 6to4
// forms carry an IPv4 destination that must be classified in its own right.
func routedAddr(addr netip.Addr) netip.Addr {
	addr = addr.Unmap()
	if addr.Is4() {
		return addr
	}
	raw := addr.As16()
	switch {
	case nat64WellKnown.Contains(addr):
		return netip.AddrFrom4([4]byte(raw[12:16]))
	case sixToFour.Contains(addr):
		return netip.AddrFrom4([4]byte(raw[2:6]))
	case [12]byte(raw[:12]) == [12]byte{} && !addr.IsUnspecified() && !addr.IsLoopback():
		return netip.AddrFrom4([4]byte(raw[12:16]))
	}
	return addr
}

func containsAddr(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func mustPrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefixes = append(prefixes, netip.MustParsePrefix(value))
	}
	return prefixes
}
