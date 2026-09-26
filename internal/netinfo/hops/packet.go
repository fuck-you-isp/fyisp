//go:build unix

package hops

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"golang.org/x/net/icmp"
)

// packetProber reads ICMP answers from one ICMP socket: a raw "ip4:icmp"
// socket (root or CAP_NET_RAW), or on macOS the unprivileged "udp4" ICMP
// socket. A reader goroutine hands every answer to the pending probe with
// the same sequence number, so probes can run in parallel.
type packetProber struct {
	c     *icmp.PacketConn
	raw   bool
	id    uint16
	magic [8]byte

	wmu sync.Mutex // the TTL is a socket option: set it and send atomically

	mu      sync.Mutex
	seq     uint16
	pending map[uint16]*pendingProbe
	done    chan struct{}
}

type pendingProbe struct {
	dst  netip.Addr
	sent time.Time
	ch   chan Hop // buffered, 1
}

func openPacket(network string) (*packetProber, error) {
	c, err := icmp.ListenPacket(network, "0.0.0.0")
	if err != nil {
		return nil, err
	}
	t := &packetProber{c: c, raw: network == "ip4:icmp", id: uint16(os.Getpid()), magic: newMagic(),
		pending: map[uint16]*pendingProbe{}, done: make(chan struct{})}
	go t.read()
	return t, nil
}

func (t *packetProber) Close() error {
	err := t.c.Close()
	<-t.done
	return err
}

func (t *packetProber) Probe(ctx context.Context, dst netip.Addr, ttl int, timeout time.Duration) (Hop, error) {
	if !dst.Is4() {
		return Hop{}, fmt.Errorf("%s is not IPv4", dst)
	}
	p := &pendingProbe{dst: dst, ch: make(chan Hop, 1)}
	t.mu.Lock()
	for range 1 << 16 {
		t.seq++
		if t.pending[t.seq] == nil {
			break
		}
	}
	seq := t.seq
	t.pending[seq] = p
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.pending, seq)
		t.mu.Unlock()
	}()

	req := echoRequest(t.id, seq, t.magic)
	var to net.Addr = &net.UDPAddr{IP: dst.AsSlice()}
	if t.raw {
		to = &net.IPAddr{IP: dst.AsSlice()}
	}
	t.wmu.Lock()
	if err := t.c.IPv4PacketConn().SetTTL(ttl); err != nil {
		t.wmu.Unlock()
		return Hop{}, fmt.Errorf("IP_TTL: %w", err)
	}
	t.mu.Lock()
	p.sent = time.Now()
	t.mu.Unlock()
	_, err := t.c.WriteTo(req, to)
	t.wmu.Unlock()
	if err != nil {
		return Hop{}, err
	}
	tm := time.NewTimer(timeout)
	defer tm.Stop()
	select {
	case h := <-p.ch:
		return h, nil
	case <-tm.C:
	case <-ctx.Done():
	case <-t.done:
	}
	return Hop{}, nil
}

// read dispatches answers to pending probes until the socket is closed.
func (t *packetProber) read() {
	defer close(t.done)
	buf := make([]byte, 1500)
	for {
		n, from, err := t.c.ReadFrom(buf)
		now := time.Now()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		t.mu.Lock()
		if p, h, ok := t.match(buf[:n], addrOf(from)); ok {
			h.RTT = now.Sub(p.sent)
			select {
			case p.ch <- h:
			default:
			}
		}
		t.mu.Unlock()
	}
}

// match finds the pending probe an ICMP message answers. t.mu is held.
func (t *packetProber) match(b []byte, from netip.Addr) (*pendingProbe, Hop, bool) {
	b = stripIPv4(b)
	if len(b) < 8 || !from.IsValid() {
		return nil, Hop{}, false
	}
	switch b[0] {
	case 0: // echo reply
		if len(b) < 16 || [8]byte(b[8:16]) != t.magic {
			return nil, Hop{}, false
		}
		p := t.pending[binary.BigEndian.Uint16(b[6:8])]
		if p == nil || p.dst != from {
			return nil, Hop{}, false
		}
		return p, Hop{Addr: from, Reached: true}, true
	case 3, 11:
		q := b[8:] // quoted IPv4 header + first 8 bytes of our request
		if len(q) < 20 || q[0]>>4 != 4 {
			return nil, Hop{}, false
		}
		ihl := int(q[0]&0x0f) * 4
		if ihl < 20 || len(q) < ihl+8 || q[9] != 1 || q[ihl] != 8 {
			return nil, Hop{}, false
		}
		if t.raw && binary.BigEndian.Uint16(q[ihl+4:ihl+6]) != t.id {
			return nil, Hop{}, false
		}
		p := t.pending[binary.BigEndian.Uint16(q[ihl+6:ihl+8])]
		if p == nil || netip.AddrFrom4([4]byte(q[16:20])) != p.dst {
			return nil, Hop{}, false
		}
		return p, Hop{Addr: from, Unreach: b[0] == 3}, true
	}
	return nil, Hop{}, false
}

// stripIPv4 removes an IPv4 header if the platform delivered one.
func stripIPv4(b []byte) []byte {
	if len(b) >= 20 && b[0]>>4 == 4 {
		ihl := int(b[0]&0x0f) * 4
		if ihl >= 20 && len(b) >= ihl {
			return b[ihl:]
		}
	}
	return b
}

func addrOf(a net.Addr) netip.Addr {
	var ip net.IP
	switch v := a.(type) {
	case *net.UDPAddr:
		ip = v.IP
	case *net.IPAddr:
		ip = v.IP
	}
	x, _ := netip.AddrFromSlice(ip)
	return x.Unmap()
}
