//go:build linux

package hops

import (
	"context"
	"net/netip"
	"time"
)

// ProbeTransport on Linux uses the socket's own error queue (errqTransport),
// whatever ICMP socket the prober reads.
func (t *packetProber) ProbeTransport(ctx context.Context, tp Transport, dst netip.AddrPort, ttl int, timeout time.Duration) (Hop, error) {
	return errqTransport(ctx, tp, dst, ttl, timeout)
}
