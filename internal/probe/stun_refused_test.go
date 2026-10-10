//go:build linux || darwin

package probe

import (
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TestUDPRefused: a closed port answers with ICMP port unreachable, which a
// connected socket reports as ECONNREFUSED on Linux and macOS. That is not
// loss: all three siblings are lost as refused, and the socket is reopened.
// (Windows hides ICMP errors on UDP sockets: there it is a timeout.)
func TestUDPRefused(t *testing.T) {
	l, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := l.LocalAddr().(*net.UDPAddr).Port
	l.Close() // nothing listens there now
	var addrs sync.Map
	addrs.Store("closed.test", netip.MustParseAddr("127.0.0.1"))
	r := udpRunner(t, &addrs, nil, nil)
	tg := model.Target{Name: "C", Host: "closed.test", Port: port, Interval: time.Second, Kinds: []model.ProbeKind{model.KindUDP}}
	ss := collect(t, r, profileOf(tg), 10*time.Second, func(ss []model.Sample) bool { return len(ss) >= 6 })
	if len(ss) < 6 {
		t.Fatalf("%d samples", len(ss))
	}
	for _, s := range ss {
		if !s.Lost || s.Reason != model.ReasonRefused || s.Err != "udp: refused" {
			t.Errorf("%+v", s)
		}
	}
}
