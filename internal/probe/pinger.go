package probe

import (
	"context"
	"net/netip"
	"time"
)

// Pinger sends ICMP echo requests on one socket (or handle), for bursts of
// pings such as latency under load during a speed test. Safe for
// concurrent use.
type Pinger struct{ p pinger }

// OpenPinger opens the best ICMP pinger this machine allows without
// elevation (see Caps.ICMP).
func OpenPinger() (*Pinger, error) {
	p, _, err := openPinger()
	if err != nil {
		return nil, err
	}
	return &Pinger{p: p}, nil
}

// Ping sends one echo request to dst and waits up to timeout for the reply.
// No reply in time is an error.
func (p *Pinger) Ping(ctx context.Context, dst netip.Addr, timeout time.Duration) (time.Duration, error) {
	rtt, _, err := p.p.Ping(ctx, dst, 0x7ffe, timeout) // idx: not a profile target (cf. PingOnce)
	return rtt, err
}

// Close releases the socket.
func (p *Pinger) Close() error { return p.p.Close() }
