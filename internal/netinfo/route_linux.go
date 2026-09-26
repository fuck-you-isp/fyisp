//go:build linux

package netinfo

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// defaultRoute reads the main routing table from /proc/net/route (readable
// by every user).
func defaultRoute() (route, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return route{}, err
	}
	defer f.Close()
	return parseProcRoute(f)
}

const (
	rtfUp      = 0x1
	rtfGateway = 0x2
)

// parseProcRoute picks the default route with the lowest metric. Addresses
// are hex in host byte order (little-endian on every platform Linux runs
// fyisp on; /proc prints the raw __be32 as a native integer).
func parseProcRoute(r io.Reader) (route, error) {
	sc := bufio.NewScanner(r)
	best, bestMetric, found := route{}, uint64(0), false
	first := true
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if first {
			first = false
			if len(f) > 0 && f[0] == "Iface" {
				continue
			}
		}
		if len(f) < 8 {
			continue
		}
		dst, err1 := hexAddr(f[1])
		mask, err2 := hexAddr(f[7])
		flags, err3 := strconv.ParseUint(f[3], 16, 32)
		metric, err4 := strconv.ParseUint(f[6], 10, 32)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		if !dst.IsUnspecified() || !mask.IsUnspecified() || flags&rtfUp == 0 {
			continue
		}
		rt := route{Iface: f[0]}
		if flags&rtfGateway != 0 {
			if gw, err := hexAddr(f[2]); err == nil && !gw.IsUnspecified() {
				rt.Gateway = gw
			}
		}
		if !found || metric < bestMetric {
			best, bestMetric, found = rt, metric, true
		}
	}
	if err := sc.Err(); err != nil {
		return route{}, err
	}
	if !found {
		return route{}, errNoRoute
	}
	return best, nil
}

func hexAddr(s string) (netip.Addr, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 4 {
		return netip.Addr{}, fmt.Errorf("bad address %q", s)
	}
	var a [4]byte
	binary.BigEndian.PutUint32(a[:], binary.LittleEndian.Uint32(b))
	// /proc prints the in-memory (network order) value as a little-endian
	// integer: bytes come out reversed.
	return netip.AddrFrom4([4]byte{a[0], a[1], a[2], a[3]}), nil
}
