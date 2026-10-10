package probe

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"
)

// PingOnce resolves host (IPv4) and sends one ICMP echo with its own short
// lived socket: for occasional checks such as choosing the nearest speed
// test server, not for series.
func PingOnce(ctx context.Context, host string, timeout time.Duration) (time.Duration, error) {
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return 0, err
	}
	if len(ips) == 0 {
		return 0, errors.New("no IPv4 address")
	}
	p, _, err := openPinger()
	if err != nil {
		return 0, err
	}
	defer p.Close()
	rtt, _, err := p.Ping(ctx, ips[0].Unmap(), 0x7fff, timeout)
	return rtt, err
}

// Pinger sends ICMP echo requests on one long-lived socket (for the speed
// tests' latency-under-load probes).
type Pinger struct{ p pinger }

// OpenPinger opens an ICMP socket (unprivileged where the OS allows it).
func OpenPinger() (*Pinger, error) {
	p, _, err := openPinger()
	if err != nil {
		return nil, err
	}
	return &Pinger{p: p}, nil
}

// Ping sends one echo request to dst and waits up to timeout.
func (p *Pinger) Ping(ctx context.Context, dst netip.Addr, timeout time.Duration) (time.Duration, error) {
	rtt, _, err := p.p.Ping(ctx, dst.Unmap(), 0x7ffe, timeout)
	return rtt, err
}

// Close closes the socket.
func (p *Pinger) Close() error { return p.p.Close() }
