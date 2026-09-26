//go:build windows

package netinfo

import (
	"net"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
)

// defaultRoute reads the IPv4 routing table with GetIpForwardTable2
// (iphlpapi, no privileges) and picks the default route with the lowest
// effective metric (route metric + interface metric), as Windows does.
func defaultRoute() (route, error) {
	var tbl *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_INET, &tbl); err != nil {
		return route{}, err
	}
	defer windows.FreeMibTable(unsafe.Pointer(tbl))
	var (
		best       route
		bestMetric uint64
		found      bool
	)
	for _, r := range tbl.Rows() {
		if r.DestinationPrefix.PrefixLength != 0 || r.DestinationPrefix.Prefix.Family != windows.AF_INET || r.Loopback != 0 {
			continue
		}
		metric := uint64(r.Metric)
		ifr := windows.MibIpInterfaceRow{Family: windows.AF_INET, InterfaceLuid: r.InterfaceLuid, InterfaceIndex: r.InterfaceIndex}
		if err := windows.GetIpInterfaceEntry(&ifr); err == nil {
			if ifr.Connected == 0 {
				continue
			}
			metric += uint64(ifr.Metric)
		}
		rt := route{}
		if r.NextHop.Family == windows.AF_INET {
			sa := (*windows.RawSockaddrInet4)(unsafe.Pointer(&r.NextHop))
			if a := netip.AddrFrom4(sa.Addr); !a.IsUnspecified() {
				rt.Gateway = a
			}
		}
		if ifi, err := net.InterfaceByIndex(int(r.InterfaceIndex)); err == nil {
			rt.Iface = ifi.Name
		}
		if !found || metric < bestMetric || (metric == bestMetric && rt.Gateway.IsValid() && !best.Gateway.IsValid()) {
			best, bestMetric, found = rt, metric, true
		}
	}
	if !found {
		return route{}, errNoRoute
	}
	return best, nil
}
