package main

import (
	"slices"
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/netinfo/hops"
	"github.com/fuck-you-isp/fyisp/internal/web"
)

// TestTraceModes: what /api/trace's "modes" says per OS.
func TestTraceModes(t *testing.T) {
	win := traceModes(hops.Support{TCP: true, TCPViaICMP: true}, "windows")
	want := []web.TraceModeInfo{{Mode: "icmp", Available: true}, {Mode: "tcp", Port: 443, Available: true, HopsVia: "icmp"},
		{Mode: "udp", Port: 33434, Reason: "windows_firewall"}}
	if !slices.Equal(win, want) {
		t.Errorf("windows: %+v", win)
	}
	lin := traceModes(hops.Support{TCP: true, UDP: true}, "linux")
	want = []web.TraceModeInfo{{Mode: "icmp", Available: true}, {Mode: "tcp", Port: 443, Available: true},
		{Mode: "udp", Port: 33434, Available: true}}
	if !slices.Equal(lin, want) {
		t.Errorf("linux: %+v", lin)
	}
	if other := traceModes(hops.Support{}, "plan9"); other[1].Reason != "os" || other[2].Available {
		t.Errorf("no transports: %+v", other)
	}
}
