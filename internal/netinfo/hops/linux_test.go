//go:build linux

package hops

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

func TestParseExtendedErr(t *testing.T) {
	mk := func(origin, typ uint8, addr string) []byte {
		b := make([]byte, 32)
		b[4], b[5] = origin, typ
		b[16], b[17] = 2, 0 // AF_INET, native endian (little on test hosts)
		a := netip.MustParseAddr(addr).As4()
		copy(b[20:24], a[:])
		return b
	}
	if h, ok := parseExtendedErr(mk(2, 11, "10.99.1.1")); !ok || h != (Hop{Addr: netip.MustParseAddr("10.99.1.1")}) {
		t.Errorf("time exceeded: %+v %v", h, ok)
	}
	if h, ok := parseExtendedErr(mk(2, 3, "198.51.100.2")); !ok || !h.Unreach || h.Addr != netip.MustParseAddr("198.51.100.2") {
		t.Errorf("unreachable: %+v %v", h, ok)
	}
	if _, ok := parseExtendedErr(mk(1, 11, "10.0.0.1")); ok {
		t.Error("local origin accepted")
	}
	if _, ok := parseExtendedErr(make([]byte, 10)); ok {
		t.Error("short buffer accepted")
	}
}

// TestTransportLoopback: TCP and UDP probes to a closed loopback port reach
// the destination (a RST; port unreachable through the error queue),
// without privileges.
func TestTransportLoopback(t *testing.T) {
	dst := netip.MustParseAddr("127.0.0.1")
	for _, tp := range []Transport{TCP, UDP} {
		h, err := errqTransport(context.Background(), tp, netip.AddrPortFrom(dst, 9), 8, 2*time.Second)
		if err != nil || h.Addr != dst || !h.Reached || h.RTT <= 0 {
			t.Errorf("%s: %+v, %v", tp, h, err)
		}
	}
}
