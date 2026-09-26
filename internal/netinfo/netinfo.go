// Package netinfo discovers the local network path: the default gateway and
// the first public hop (the ISP edge), without elevated privileges.
package netinfo

import (
	"context"
	"log/slog"
	"net/netip"
	"time"
)

// Path is the current view of the local path. Zero addresses are unknown.
type Path struct {
	Gateway netip.Addr // default IPv4 gateway
	Iface   string     // interface of the default route
	Edge    netip.Addr // first non-private hop towards the internet
	EdgeHop int        // TTL of Edge (1 = gateway)
	Updated time.Time
	Err     string // last discovery problem, for local logs only
}

// Options configures a Watcher.
type Options struct {
	// Recheck re-runs edge discovery (default 15m). The gateway is polled
	// every PollGateway (default 10s); a change triggers edge discovery.
	Recheck     time.Duration
	PollGateway time.Duration
	// Probe is the traceroute destination used to find the edge
	// (default 1.1.1.1).
	Probe netip.Addr
	Log   *slog.Logger
}

// Watcher keeps Path current while Run runs.
type Watcher interface {
	Run(ctx context.Context) error
	Current() Path
	// Subscribe delivers every change of Gateway or Edge.
	Subscribe() (<-chan Path, func())
}
