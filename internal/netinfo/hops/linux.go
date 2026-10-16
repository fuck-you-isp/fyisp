//go:build linux

package hops

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"golang.org/x/sys/unix"
)

// open prefers an unprivileged ping socket ("udp4" ICMP, allowed by
// net.ipv4.ping_group_range) with IP_RECVERR: ICMP time-exceeded messages
// for it are queued on the socket's error queue with the offending router's
// address. Without ping sockets it falls back to a raw socket.
func open() (Prober, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMP)
	if err == nil {
		unix.Close(fd)
		return errqProber{magic: newMagic()}, nil
	}
	t, errRaw := openPacket("ip4:icmp")
	if errRaw != nil {
		return nil, fmt.Errorf("ping socket: %w; raw socket: %w", err, errRaw)
	}
	return t, nil
}

// errqProber uses a fresh ping socket per probe, so every error-queue entry
// and reply it reads belongs to that probe (and probes can run in parallel).
type errqProber struct{ magic [8]byte }

func (errqProber) Close() error { return nil }

func (t errqProber) Probe(ctx context.Context, dst netip.Addr, ttl int, timeout time.Duration) (Hop, error) {
	if !dst.Is4() {
		return Hop{}, fmt.Errorf("%s is not IPv4", dst)
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.IPPROTO_ICMP)
	if err != nil {
		return Hop{}, fmt.Errorf("ping socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, ttl); err != nil {
		return Hop{}, fmt.Errorf("IP_TTL: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVERR, 1); err != nil {
		return Hop{}, fmt.Errorf("IP_RECVERR: %w", err)
	}
	// The kernel fills in the echo ID and checksum of ping sockets.
	req := echoRequest(0, uint16(ttl), t.magic)
	if err := unix.Sendto(fd, req, 0, &unix.SockaddrInet4{Addr: dst.As4()}); err != nil {
		return Hop{}, err
	}
	sent := time.Now()
	deadline := sent.Add(timeout)
	buf, oob := make([]byte, 512), make([]byte, 512)
	for {
		left := time.Until(deadline)
		if left <= 0 || ctx.Err() != nil {
			return Hop{}, nil
		}
		pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(pfd, int(min(left, 100*time.Millisecond)/time.Millisecond)+1); err != nil && !errors.Is(err, unix.EINTR) {
			return Hop{}, err
		}
		if _, oobn, _, _, err := unix.Recvmsg(fd, buf, oob, unix.MSG_ERRQUEUE); err == nil {
			if h, ok := parseErrQueue(oob[:oobn]); ok {
				h.RTT = time.Since(sent)
				return h, nil
			}
			continue
		}
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err == nil && n >= 8+len(t.magic) && buf[0] == 0 && [8]byte(buf[8:16]) == t.magic {
			return Hop{Addr: dst, Reached: true, RTT: time.Since(sent)}, nil
		}
	}
}

// parseErrQueue extracts the offender of an ICMP error from IP_RECVERR
// control messages.
func parseErrQueue(oob []byte) (Hop, bool) {
	cms, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return Hop{}, false
	}
	for _, c := range cms {
		if c.Header.Level == unix.IPPROTO_IP && c.Header.Type == unix.IP_RECVERR {
			if h, ok := parseExtendedErr(c.Data); ok {
				return h, true
			}
		}
	}
	return Hop{}, false
}

// parseExtendedErr decodes struct sock_extended_err followed by the
// offender's sockaddr_in (SO_EE_OFFENDER).
//
//	u32 ee_errno; u8 ee_origin, ee_type, ee_code, ee_pad; u32 ee_info, ee_data;
//	struct sockaddr_in { u16 family; u16 port; u8 addr[4]; ... }
func parseExtendedErr(b []byte) (Hop, bool) {
	const sizeofEE = 16
	if len(b) < sizeofEE+8 || b[4] != unix.SO_EE_ORIGIN_ICMP {
		return Hop{}, false
	}
	if fam := binary.NativeEndian.Uint16(b[sizeofEE:]); fam != unix.AF_INET {
		return Hop{}, false
	}
	a := netip.AddrFrom4([4]byte(b[sizeofEE+4 : sizeofEE+8]))
	switch b[5] {
	case 11: // time exceeded
		return Hop{Addr: a}, true
	case 3: // destination unreachable
		return Hop{Addr: a, Unreach: true}, true
	}
	return Hop{}, false
}

var supported = Support{TCP: true, UDP: true}

func (errqProber) ProbeTransport(ctx context.Context, tp Transport, dst netip.AddrPort, ttl int, timeout time.Duration) (Hop, error) {
	return errqTransport(ctx, tp, dst, ttl, timeout)
}

// errqTransport sends one TCP SYN (a non-blocking connect) or UDP datagram
// from an ordinary socket with IP_TTL and IP_RECVERR: the ICMP error a
// router sends back is queued on the socket's error queue with the router's
// address. It needs neither ping sockets nor privileges. A connect that
// completes or is refused is the destination answering. The socket is
// closed with SO_LINGER 0 (a RST: no half-open connection on the server)
// within 900 ms, before the first SYN retransmit (1 s).
func errqTransport(ctx context.Context, tp Transport, dst netip.AddrPort, ttl int, timeout time.Duration) (Hop, error) {
	if !dst.Addr().Is4() {
		return Hop{}, fmt.Errorf("%s is not IPv4", dst.Addr())
	}
	typ := unix.SOCK_DGRAM
	if tp == TCP {
		typ = unix.SOCK_STREAM
	}
	fd, err := unix.Socket(unix.AF_INET, typ|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return Hop{}, fmt.Errorf("%s socket: %w", tp, err)
	}
	defer unix.Close(fd)
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, ttl); err != nil {
		return Hop{}, fmt.Errorf("IP_TTL: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVERR, 1); err != nil {
		return Hop{}, fmt.Errorf("IP_RECVERR: %w", err)
	}
	sa := &unix.SockaddrInet4{Addr: dst.Addr().As4(), Port: int(dst.Port())}
	sent := time.Now()
	reached := func() (Hop, error) { return Hop{Addr: dst.Addr(), Reached: true, RTT: time.Since(sent)}, nil }
	ev := int16(unix.POLLIN)
	if tp == TCP {
		ev = unix.POLLOUT
		if err := unix.SetsockoptLinger(fd, unix.SOL_SOCKET, unix.SO_LINGER, &unix.Linger{Onoff: 1, Linger: 0}); err != nil {
			return Hop{}, fmt.Errorf("SO_LINGER: %w", err)
		}
		switch err := unix.Connect(fd, sa); {
		case err == nil, errors.Is(err, unix.ECONNREFUSED):
			return reached()
		case !errors.Is(err, unix.EINPROGRESS):
			return Hop{}, err
		}
	} else if err := unix.Sendto(fd, []byte("fyisp-tr"), 0, sa); err != nil {
		return Hop{}, err
	}
	if tp == TCP {
		timeout = min(timeout, 900*time.Millisecond) // before the SYN retransmit (1 s)
	}
	deadline := sent.Add(timeout)
	buf, oob := make([]byte, 512), make([]byte, 512)
	for {
		left := time.Until(deadline)
		if left <= 0 || ctx.Err() != nil {
			return Hop{}, nil
		}
		pfd := []unix.PollFd{{Fd: int32(fd), Events: ev}}
		if _, err := unix.Poll(pfd, int(min(left, 100*time.Millisecond)/time.Millisecond)+1); err != nil && !errors.Is(err, unix.EINTR) {
			return Hop{}, err
		}
		if _, oobn, _, _, err := unix.Recvmsg(fd, buf, oob, unix.MSG_ERRQUEUE); err == nil {
			if h, ok := parseErrQueue(oob[:oobn]); ok {
				h = forTransport(dst.Addr(), h)
				h.RTT = time.Since(sent)
				return h, nil
			}
			continue
		}
		re := pfd[0].Revents
		switch {
		case tp == TCP && re != 0:
			// An ICMP error that aborted the connect was read above.
			e, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
			if err == nil && (e == 0 || unix.Errno(e) == unix.ECONNREFUSED) {
				return reached()
			}
			return Hop{}, nil
		case tp == UDP && re&unix.POLLIN != 0:
			if _, from, err := unix.Recvfrom(fd, buf, 0); err == nil {
				if a, ok := from.(*unix.SockaddrInet4); ok && netip.AddrFrom4(a.Addr) == dst.Addr() {
					return reached() // the destination answered in UDP
				}
			}
		}
	}
}
