// Package hops sends TTL-limited ICMP echo requests without elevated
// privileges and reports which router answered: the building block of
// netinfo's edge discovery and of package trace.
//
//   - Linux: an unprivileged ping socket ("udp4" ICMP, allowed by
//     net.ipv4.ping_group_range) with IP_TTL and IP_RECVERR; routers'
//     time-exceeded messages arrive on the socket's error queue. Falls back
//     to a raw socket (root or CAP_NET_RAW).
//   - macOS and other BSDs: the "udp4" ICMP socket where the OS delivers ICMP
//     errors to it (macOS), else a raw socket. Best effort.
//   - Windows: IcmpSendEcho2Ex with IP_OPTION_INFORMATION.Ttl.
package hops

import (
	"context"
	"net/netip"
	"time"
)

// Hop is the answer to one TTL-limited echo request. A zero Addr means no
// answer before the timeout.
type Hop struct {
	Addr    netip.Addr
	RTT     time.Duration // request sent -> answer received; valid when Addr is
	Reached bool          // echo reply from the destination itself
	Unreach bool          // Addr answered "destination unreachable": no hop beyond it
}

// Prober sends TTL-limited ICMP echo requests. Probe is safe for concurrent
// use: every call gets its own answer.
type Prober interface {
	// Probe sends one echo request to dst with the given TTL and waits up
	// to timeout. No answer is (Hop{}, nil); err is a local failure (e.g.
	// no route to dst).
	Probe(ctx context.Context, dst netip.Addr, ttl int, timeout time.Duration) (Hop, error)
	Close() error
}

// Open returns the best Prober this machine allows without elevation.
func Open() (Prober, error) { return open() }
