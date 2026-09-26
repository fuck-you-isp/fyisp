//go:build unix

package netinfo

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	"golang.org/x/net/icmp"
)

// packetTracer reads ICMP time-exceeded messages from an ICMP socket: a raw
// "ip4:icmp" socket (root or CAP_NET_RAW), or on macOS the unprivileged
// "udp4" ICMP socket.
type packetTracer struct {
	c     *icmp.PacketConn
	raw   bool
	id    uint16
	seq   uint16
	magic [8]byte
}

func openPacketTracer(network string) (*packetTracer, error) {
	c, err := icmp.ListenPacket(network, "0.0.0.0")
	if err != nil {
		return nil, err
	}
	return &packetTracer{c: c, raw: network == "ip4:icmp", id: uint16(os.Getpid()), magic: newMagic()}, nil
}

func (t *packetTracer) Close() error { return t.c.Close() }

func (t *packetTracer) Probe(ctx context.Context, dst netip.Addr, ttl int, timeout time.Duration) (hop, error) {
	if !dst.Is4() {
		return hop{}, fmt.Errorf("%s is not IPv4", dst)
	}
	if err := t.c.IPv4PacketConn().SetTTL(ttl); err != nil {
		return hop{}, fmt.Errorf("IP_TTL: %w", err)
	}
	t.seq++
	req := echoRequest(t.id, t.seq, t.magic)
	var to net.Addr = &net.UDPAddr{IP: dst.AsSlice()}
	if t.raw {
		to = &net.IPAddr{IP: dst.AsSlice()}
	}
	if _, err := t.c.WriteTo(req, to); err != nil {
		return hop{}, err
	}
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 1500)
	for {
		if ctx.Err() != nil {
			return hop{}, nil
		}
		// Short reads so a cancelled ctx is noticed.
		rd := time.Now().Add(200 * time.Millisecond)
		if rd.After(deadline) {
			rd = deadline
		}
		_ = t.c.SetReadDeadline(rd)
		n, from, err := t.c.ReadFrom(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				if time.Now().Before(deadline) {
					continue
				}
				return hop{}, nil
			}
			return hop{}, err
		}
		if h, ok := t.match(buf[:n], addrOf(from), dst); ok {
			return h, nil
		}
	}
}

// match checks that an ICMP message answers our current request.
func (t *packetTracer) match(b []byte, from, dst netip.Addr) (hop, bool) {
	b = stripIPv4(b)
	if len(b) < 8 || !from.IsValid() {
		return hop{}, false
	}
	switch b[0] {
	case 0: // echo reply
		if len(b) < 16 || [8]byte(b[8:16]) != t.magic || binary.BigEndian.Uint16(b[6:8]) != t.seq {
			return hop{}, false
		}
		return hop{Addr: from, Reached: true}, true
	case 3, 11:
		q := b[8:] // quoted IPv4 header + first 8 bytes of our request
		if len(q) < 20 || q[0]>>4 != 4 {
			return hop{}, false
		}
		ihl := int(q[0]&0x0f) * 4
		if ihl < 20 || len(q) < ihl+8 || q[9] != 1 || q[ihl] != 8 {
			return hop{}, false
		}
		if netip.AddrFrom4([4]byte(q[16:20])) != dst || binary.BigEndian.Uint16(q[ihl+6:ihl+8]) != t.seq {
			return hop{}, false
		}
		if t.raw && binary.BigEndian.Uint16(q[ihl+4:ihl+6]) != t.id {
			return hop{}, false
		}
		return hop{Addr: from, Unreach: b[0] == 3}, true
	}
	return hop{}, false
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
