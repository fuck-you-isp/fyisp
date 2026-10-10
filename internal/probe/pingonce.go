package probe

import (
	"context"
	"errors"
	"net"
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
