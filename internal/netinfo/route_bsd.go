//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package netinfo

import (
	"net"
	"net/netip"
	"syscall"

	xroute "golang.org/x/net/route"
)

// defaultRoute dumps the kernel routing table with sysctl (no privileges)
// and picks the IPv4 default route. On macOS, interface-scoped default
// routes (one per interface, RTF_IFSCOPE) are skipped: the unscoped one is
// the route the system uses.
func defaultRoute() (route, error) {
	b, err := xroute.FetchRIB(syscall.AF_INET, xroute.RIBTypeRoute, 0)
	if err != nil {
		return route{}, err
	}
	msgs, err := xroute.ParseRIB(xroute.RIBTypeRoute, b)
	if err != nil {
		return route{}, err
	}
	var (
		best  routeCandidate
		found bool
	)
	for _, m := range msgs {
		rm, ok := m.(*xroute.RouteMessage)
		if !ok {
			continue
		}
		c, ok := candidate(rm)
		if !ok {
			continue
		}
		if !found || c.better(best) {
			best, found = c, true
		}
	}
	if !found {
		return route{}, errNoRoute
	}
	rt := route{Gateway: best.gw}
	if ifi, err := net.InterfaceByIndex(best.index); err == nil {
		rt.Iface = ifi.Name
	}
	return rt, nil
}

type routeCandidate struct {
	gw     netip.Addr
	index  int
	scoped bool
}

// better prefers unscoped routes, then routes with a gateway.
func (c routeCandidate) better(o routeCandidate) bool {
	if c.scoped != o.scoped {
		return !c.scoped
	}
	return c.gw.IsValid() && !o.gw.IsValid()
}

func candidate(rm *xroute.RouteMessage) (routeCandidate, bool) {
	const up = syscall.RTF_UP
	if rm.Flags&up == 0 || len(rm.Addrs) <= syscall.RTAX_NETMASK {
		return routeCandidate{}, false
	}
	dst, ok := rm.Addrs[syscall.RTAX_DST].(*xroute.Inet4Addr)
	if !ok || dst.IP != [4]byte{} {
		return routeCandidate{}, false
	}
	// A default route has an all-zero (or absent) netmask.
	if nm, ok := rm.Addrs[syscall.RTAX_NETMASK].(*xroute.Inet4Addr); ok && nm.IP != [4]byte{} {
		return routeCandidate{}, false
	}
	c := routeCandidate{index: rm.Index, scoped: rm.Flags&rtfIfscope != 0}
	if rm.Flags&syscall.RTF_GATEWAY != 0 {
		if gw, ok := rm.Addrs[syscall.RTAX_GATEWAY].(*xroute.Inet4Addr); ok && gw.IP != [4]byte{} {
			c.gw = netip.AddrFrom4(gw.IP)
		}
	}
	return c, true
}
