package netinfo

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// Edge discovery parameters: hops 1..maxHops, each tried up to 1+hopRetries
// times with hopTimeout.
const (
	maxHops    = 8
	hopRetries = 2
	hopTimeout = time.Second
)

// hop is the answer to one TTL-limited echo request. A zero Addr means no
// answer before the timeout.
type hop struct {
	Addr    netip.Addr
	Reached bool // echo reply from the destination itself
	Unreach bool // Addr answered "destination unreachable": no hop beyond it
}

// tracer sends TTL-limited ICMP echo requests. Probe calls are sequential.
type tracer interface {
	// Probe sends one echo request to dst with the given TTL and waits up
	// to timeout. No answer is (hop{}, nil); err is a local failure.
	Probe(ctx context.Context, dst netip.Addr, ttl int, timeout time.Duration) (hop, error)
	Close() error
}

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
