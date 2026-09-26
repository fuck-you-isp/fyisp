package netinfo

import "net/netip"

// notPublic are IPv4 ranges that are never an ISP's public edge: RFC 1918,
// carrier-grade NAT (RFC 6598), loopback, link-local, "this network", the
// IETF protocol assignments block (DS-Lite/464XLAT use 192.0.0.0/29),
// benchmarking (sometimes used inside provider networks), multicast and
// reserved space.
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("224.0.0.0/3"), // multicast and 240/4 reserved
}

// IsPublic reports whether a is a globally routed IPv4 address, i.e. a hop
// that belongs to the ISP (or beyond) rather than to the home network or a
// provider's private/CGNAT space. IPv6 is not considered (probes are IPv4).
func IsPublic(a netip.Addr) bool {
	a = a.Unmap()
	if !a.Is4() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
