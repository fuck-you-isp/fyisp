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
//
// TransportProber sends TTL-limited TCP SYNs and UDP datagrams instead (mtr
// -T and -u), also without elevation; see Supported for what each OS can do.
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

// Transport is the protocol of a TCP or UDP probe (its IP protocol number).
type Transport uint8

const (
	TCP Transport = 6
	UDP Transport = 17
)

func (tp Transport) String() string {
	if tp == TCP {
		return "tcp"
	}
	return "udp"
}

// TransportProber sends TTL-limited TCP SYNs or UDP datagrams. The Probers
// Open returns implement it for the transports Supported reports.
type TransportProber interface {
	// ProbeTransport sends one probe to dst with the given TTL and waits up
	// to timeout, like Prober.Probe. Reached: the destination answered (TCP:
	// SYN-ACK or RST; UDP: an ICMP unreachable from dst itself). A TCP
	// probe never leaves a connection open: it is closed with a RST.
	ProbeTransport(ctx context.Context, tp Transport, dst netip.AddrPort, ttl int, timeout time.Duration) (Hop, error)
}

// Support says which transports this OS traces without elevation.
type Support struct {
	TCP, UDP bool
	// TCPViaICMP: a TCP trace finds the routers with ICMP echo requests and
	// probes only the destination with TCP connects (Windows: its default
	// firewall drops the routers' answers to TCP probes).
	TCPViaICMP bool
}

// Supported reports what ProbeTransport can do on this OS.
func Supported() Support { return supported }

// forTransport adapts the Hop of an ICMP error for a TCP or UDP probe to
// dst: an unreachable from dst itself is the destination answering (for
// UDP, port unreachable is how a destination answers).
func forTransport(dst netip.Addr, h Hop) Hop {
	if h.Unreach && h.Addr == dst {
		h.Unreach, h.Reached = false, true
	}
	return h
}
