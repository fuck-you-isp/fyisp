package netinfo

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/netinfo/hops"
)

// Edge discovery parameters: hops 1..maxHops, each tried up to 1+hopRetries
// times with hopTimeout.
const (
	maxHops    = 8
	hopRetries = 2
	hopTimeout = time.Second
)

// hop is the answer to one TTL-limited echo request (see package hops).
type hop = hops.Hop

// tracer sends TTL-limited ICMP echo requests (see package hops).
type tracer = hops.Prober

func openTracer() (tracer, error) { return hops.Open() }

var errNoPublicHop = errors.New("no public hop found")

// discoverEdge returns the first hop towards dst whose address is public
// (see IsPublic), and its TTL. Hops that do not answer are skipped.
func discoverEdge(ctx context.Context, tr tracer, dst netip.Addr) (netip.Addr, int, error) {
	for ttl := 1; ttl <= maxHops; ttl++ {
		var h hop
		for try := 0; try <= hopRetries; try++ {
			var err error
			h, err = tr.Probe(ctx, dst, ttl, hopTimeout)
			if ctx.Err() != nil {
				return netip.Addr{}, 0, ctx.Err()
			}
			if err != nil {
				return netip.Addr{}, 0, fmt.Errorf("hop %d: %w", ttl, err)
			}
			if h.Addr.IsValid() {
				break
			}
		}
		switch {
		case !h.Addr.IsValid():
			continue // silent hop
		case IsPublic(h.Addr):
			return h.Addr.Unmap(), ttl, nil
		case h.Reached:
			return netip.Addr{}, 0, fmt.Errorf("%w: reached %s at hop %d", errNoPublicHop, dst, ttl)
		case h.Unreach:
			return netip.Addr{}, 0, fmt.Errorf("%w: hop %d reports %s unreachable", errNoPublicHop, ttl, dst)
		}
	}
	return netip.Addr{}, 0, fmt.Errorf("%w within %d hops", errNoPublicHop, maxHops)
}
