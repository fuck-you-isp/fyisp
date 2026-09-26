package redact

import (
	"net/netip"
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

func TestIsPrivate(t *testing.T) {
	for s, want := range map[string]bool{
		"10.1.2.3": true, "192.168.1.1": true, "172.16.0.1": true, "100.64.1.1": true,
		"169.254.1.1": true, "127.0.0.1": true, "fe80::1": true, "fd00::1": true, "0.1.2.3": true,
		"::ffff:192.168.0.1": true,
		"8.8.8.8":            false, "100.128.0.1": false, "2001:4860::8888": false,
	} {
		if got := IsPrivate(netip.MustParseAddr(s)); got != want {
			t.Errorf("IsPrivate(%s) = %v", s, got)
		}
	}
}

func TestMaskEdge(t *testing.T) {
	if got := MaskEdge(netip.MustParseAddr("203.0.113.77")); got != "203.0.113.x" {
		t.Error(got)
	}
	if got := MaskEdge(netip.MustParseAddr("2001:db8:1234:5678::1")); got != "2001:db8:1234::x" {
		t.Error(got)
	}
}

func TestClassifierHop(t *testing.T) {
	a := netip.MustParseAddr
	route := []netip.Addr{a("192.168.1.1"), a("100.64.0.1"), a("203.0.113.9"), {}, a("198.51.100.1"), a("198.51.100.2"), a("8.8.8.8")}
	c := NewClassifier()
	c.Route(route)
	info := model.HopInfo{RDNS: "host.example", ASN: 64500, Owner: "EXAMPLE"}
	cases := []struct {
		ip   netip.Addr
		want Hop
	}{
		{route[0], Hop{Private: true}},
		{route[1], Hop{Private: true}},
		{route[2], Hop{IP: "203.0.113.x", Masked: true, ASN: 64500, Owner: "EXAMPLE"}},
		{route[3], Hop{NoReply: true}},
		{route[4], Hop{IP: "198.51.100.1", ASN: 64500, Owner: "EXAMPLE"}},
		{route[5], Hop{IP: "198.51.100.2", ASN: 64500, Owner: "EXAMPLE"}},
		{route[6], Hop{IP: "8.8.8.8", RDNS: "host.example", ASN: 64500, Owner: "EXAMPLE"}},
		{a("9.9.9.9"), Hop{IP: "9.9.9.x", Masked: true, ASN: 64500, Owner: "EXAMPLE"}}, // unclassified: strict
	}
	for _, tc := range cases {
		if got := c.Hop(tc.ip, info, true); got != tc.want {
			t.Errorf("Hop(%v) = %+v, want %+v", tc.ip, got, tc.want)
		}
	}
	if got := c.Hop(route[0], info, false); got.IP != "192.168.1.1" || got.RDNS != "host.example" {
		t.Errorf("local view %+v", got)
	}
	// The strictest level wins across paths.
	c.Route([]netip.Addr{a("8.8.8.8")})
	if got := c.Hop(a("8.8.8.8"), info, true); !got.Masked {
		t.Errorf("8.8.8.8 after being a first public hop: %+v", got)
	}
}

func TestText(t *testing.T) {
	for in, want := range map[string]string{
		"router 192.168.1.1 rebooted":     "router [private address] rebooted",
		"gw 10.0.0.1. then 8.8.8.8":       "gw [private address]. then 8.8.8.8",
		"at 18:02:33 edge 100.64.3.4":     "at 18:02:33 edge [private address]",
		"v6 fe80::1%eth0 and 2001:db8::1": "v6 [private address]%eth0 and 2001:db8::1",
		"x110.0.0.1":                      "x110.0.0.1",
		"no addresses here":               "no addresses here",
	} {
		if got := Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
}
