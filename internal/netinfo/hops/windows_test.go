//go:build windows

package hops

import (
	"net/netip"
	"testing"
	"time"
)

// TestTCPViaICMP: a router's echo answer is the hop; otherwise only a TCP
// answer reaches the destination.
func TestTCPViaICMP(t *testing.T) {
	dst := netip.MustParseAddr("192.0.2.9")
	r := netip.MustParseAddr("10.0.0.1")
	if !routerAnswer(Hop{Addr: r}) || !routerAnswer(Hop{Addr: r, Unreach: true}) {
		t.Error("router answers not taken")
	}
	if routerAnswer(Hop{Addr: dst, Reached: true}) || routerAnswer(Hop{}) {
		t.Error("echo reply or silence taken as a router")
	}
	if h := destHop(true, dst, 7*time.Millisecond); h != (Hop{Addr: dst, Reached: true, RTT: 7 * time.Millisecond}) {
		t.Errorf("connected: %+v", h)
	}
	if h := destHop(false, dst, time.Second); h != (Hop{}) {
		t.Errorf("no TCP answer: %+v", h)
	}
	if !Supported().TCP || Supported().UDP || !Supported().TCPViaICMP {
		t.Errorf("Supported() = %+v", Supported())
	}
}
