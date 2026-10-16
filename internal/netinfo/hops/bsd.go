//go:build unix && !linux

package hops

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"golang.org/x/sys/unix"
)

// open uses the unprivileged "udp4" ICMP socket where the OS delivers ICMP
// errors to it (macOS), else a raw socket.
func open() (Prober, error) {
	t, err := openPacket("udp4")
	if err == nil {
		return t, nil
	}
	t, errRaw := openPacket("ip4:icmp")
	if errRaw != nil {
		return nil, fmt.Errorf("udp4 ICMP: %w; raw: %w", err, errRaw)
	}
	return t, nil
}

var supported = Support{TCP: true, UDP: true}

// ProbeTransport sends the probe from an ordinary TCP or UDP socket with
// IP_TTL. The routers' ICMP errors (and a destination's port unreachable)
// are delivered to the prober's ICMP socket too, where the reader matches
// them to this probe by the quoted header: protocol, source port,
// destination address and port. A TCP connect that completes or is
// refused is the destination answering; the socket is closed with
// SO_LINGER 0 (a RST: no half-open connection on the server).
func (t *packetProber) ProbeTransport(ctx context.Context, tp Transport, dst netip.AddrPort, ttl int, timeout time.Duration) (Hop, error) {
	if !dst.Addr().Is4() {
		return Hop{}, fmt.Errorf("%s is not IPv4", dst.Addr())
	}
	typ := unix.SOCK_DGRAM
	if tp == TCP {
		typ = unix.SOCK_STREAM
	}
	fd, err := unix.Socket(unix.AF_INET, typ, 0)
	if err != nil {
		return Hop{}, fmt.Errorf("%s socket: %w", tp, err)
	}
	defer unix.Close(fd)
	if err := unix.SetNonblock(fd, true); err != nil {
		return Hop{}, err
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, ttl); err != nil {
		return Hop{}, fmt.Errorf("IP_TTL: %w", err)
	}
	if tp == TCP {
		if err := unix.SetsockoptLinger(fd, unix.SOL_SOCKET, unix.SO_LINGER, &unix.Linger{Onoff: 1, Linger: 0}); err != nil {
			return Hop{}, fmt.Errorf("SO_LINGER: %w", err)
		}
	}
	// Bind first: the source port names the probe before anything is sent.
	if err := unix.Bind(fd, &unix.SockaddrInet4{}); err != nil {
		return Hop{}, err
	}
	la, err := unix.Getsockname(fd)
	if err != nil {
		return Hop{}, err
	}
	l4, ok := la.(*unix.SockaddrInet4)
	if !ok {
		return Hop{}, errors.New("not an IPv4 socket")
	}
	key := l4Key{tp, uint16(l4.Port)}
	p := &pendingProbe{dst: dst.Addr(), dport: dst.Port(), ch: make(chan Hop, 1)}
	t.mu.Lock()
	t.tpend[key] = p
	p.sent = time.Now()
	sent := p.sent
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.tpend, key)
		t.mu.Unlock()
	}()
	reached := func() (Hop, error) { return Hop{Addr: dst.Addr(), Reached: true, RTT: time.Since(sent)}, nil }
	sa := &unix.SockaddrInet4{Addr: dst.Addr().As4(), Port: int(dst.Port())}
	if tp == TCP {
		switch err := unix.Connect(fd, sa); {
		case err == nil, errors.Is(err, unix.ECONNREFUSED):
			return reached()
		case !errors.Is(err, unix.EINPROGRESS):
			return Hop{}, err
		}
	} else if err := unix.Sendto(fd, []byte("fyisp-tr"), 0, sa); err != nil {
		return Hop{}, err
	}
	tm := time.NewTimer(timeout)
	defer tm.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case h := <-p.ch:
			return h, nil
		case <-tm.C:
			return Hop{}, nil
		case <-ctx.Done():
			return Hop{}, nil
		case <-t.done:
			return Hop{}, nil
		case <-tick.C:
		}
		if tp != TCP {
			continue
		}
		pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		if n, _ := unix.Poll(pfd, 0); n > 0 {
			e, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
			if err == nil && (e == 0 || unix.Errno(e) == unix.ECONNREFUSED) {
				return reached()
			}
		}
	}
}
