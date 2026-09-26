//go:build linux

package netinfo

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

const procHeader = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"

func TestParseProcRoute(t *testing.T) {
	cases := []struct {
		name, table string
		want        route
		err         error
	}{
		{"wifi", procHeader +
			"wlan0\t00000000\t0101A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
			"wlan0\t0001A8C0\t00000000\t0001\t0\t0\t600\t00FFFFFF\t0\t0\t0\n",
			route{Gateway: netip.MustParseAddr("192.168.1.1"), Iface: "wlan0"}, nil},
		{"lowest metric wins", procHeader +
			"wlan0\t00000000\t0101A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
			"eth0\t00000000\t010A0A0A\t0003\t0\t0\t100\t00000000\t0\t0\t0\n",
			route{Gateway: netip.MustParseAddr("10.10.10.1"), Iface: "eth0"}, nil},
		{"netns lab", procHeader +
			"c0\t00000000\t0101630A\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
			"c0\t0001630A\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n",
			route{Gateway: netip.MustParseAddr("10.99.1.1"), Iface: "c0"}, nil},
		{"point-to-point default without gateway", procHeader +
			"wg0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n",
			route{Iface: "wg0"}, nil},
		{"route down is ignored", procHeader +
			"eth0\t00000000\t0101A8C0\t0002\t0\t0\t0\t00000000\t0\t0\t0\n", route{}, errNoRoute},
		{"no default", procHeader +
			"eth0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n", route{}, errNoRoute},
		{"empty", procHeader, route{}, errNoRoute},
		{"garbage lines", procHeader + "x\n\neth0\tzz\t0101A8C0\t0003\t0\t0\t0\t00000000\n", route{}, errNoRoute},
	}
	for _, c := range cases {
		got, err := parseProcRoute(strings.NewReader(c.table))
		if !errors.Is(err, c.err) || got != c.want {
			t.Errorf("%s: got %+v, %v; want %+v, %v", c.name, got, err, c.want, c.err)
		}
	}
}

func TestDefaultRouteLive(t *testing.T) {
	// Only checks that reading the real table works (a CI box may have no
	// default route).
	r, err := defaultRoute()
	if err != nil && !errors.Is(err, errNoRoute) {
		t.Fatal(err)
	}
	t.Logf("default route: %+v, %v", r, err)
}

func TestParseExtendedErr(t *testing.T) {
	mk := func(origin, typ uint8, addr string) []byte {
		b := make([]byte, 32)
		b[4], b[5] = origin, typ
		b[16], b[17] = 2, 0 // AF_INET, native endian (little on test hosts)
		a := netip.MustParseAddr(addr).As4()
		copy(b[20:24], a[:])
		return b
	}
	if h, ok := parseExtendedErr(mk(2, 11, "10.99.1.1")); !ok || h != (hop{Addr: netip.MustParseAddr("10.99.1.1")}) {
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
