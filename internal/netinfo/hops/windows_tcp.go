//go:build windows

package hops

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Windows' default firewall drops the routers' ICMP errors for TCP and UDP
// sockets of a standard user: only IcmpSendEcho2Ex sees them. A TCP trace
// therefore finds the routers with ICMP echo requests and probes the
// destination with a TTL-limited TCP connect; there is no UDP trace.
var supported = Support{TCP: true, TCPViaICMP: true}

// ProbeTransport (TCP only) sends an ICMP echo request and a TCP SYN with
// the same TTL at once. A router's answer to the echo request is the hop; a
// connect that completes or is refused within timeout is the destination
// answering TCP, with the connect's round trip. The connection is closed
// with SO_LINGER 0 (a RST).
func (t *winProber) ProbeTransport(ctx context.Context, tp Transport, dst netip.AddrPort, ttl int, timeout time.Duration) (Hop, error) {
	if tp != TCP {
		return Hop{}, errors.New("UDP traces are not available on Windows")
	}
	if !dst.Addr().Is4() {
		return Hop{}, fmt.Errorf("%s is not IPv4", dst.Addr())
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type res struct {
		ok  bool
		rtt time.Duration
	}
	tc := make(chan res, 1)
	go func() {
		ok, rtt := dialTTL(cctx, dst, ttl)
		tc <- res{ok, rtt}
	}()
	ih, err := t.Probe(ctx, dst.Addr(), ttl, timeout)
	if err == nil && routerAnswer(ih) {
		cancel()
		<-tc
		return ih, nil
	}
	r := <-tc
	return destHop(r.ok, dst.Addr(), r.rtt), nil
}

// routerAnswer reports an ICMP answer from a router (not the destination).
func routerAnswer(h Hop) bool { return h.Addr.IsValid() && !h.Reached }

// destHop is a TCP probe's hop when no router answered the echo request:
// the destination if it answered TCP, else nothing (an ICMP echo reply alone
// is not a TCP answer).
func destHop(connected bool, dst netip.Addr, rtt time.Duration) Hop {
	if connected {
		return Hop{Addr: dst, Reached: true, RTT: rtt}
	}
	return Hop{}
}

// dialTTL connects to dst with IP_TTL set; ok: connected or refused.
func dialTTL(ctx context.Context, dst netip.AddrPort, ttl int) (ok bool, rtt time.Duration) {
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, windows.IP_TTL, ttl)
		}); err != nil {
			return err
		}
		return serr
	}}
	start := time.Now()
	c, err := d.DialContext(ctx, "tcp4", dst.String())
	rtt = time.Since(start)
	if err != nil {
		return errors.Is(err, windows.WSAECONNREFUSED), rtt
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = c.Close()
	return true, rtt
}
