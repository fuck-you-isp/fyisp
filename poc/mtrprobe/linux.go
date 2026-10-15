//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func whoami() string { return fmt.Sprintf("uid=%d euid=%d", os.Getuid(), os.Geteuid()) }

func methods() []method {
	return []method{
		{"icmp-pingsock", false, func(d netip.AddrPort, ttl int, to time.Duration) (Hop, error) {
			return errq(unix.SOCK_DGRAM, unix.IPPROTO_ICMP, d, ttl, to)
		}},
		{"udp-recverr", true, func(d netip.AddrPort, ttl int, to time.Duration) (Hop, error) {
			return errq(unix.SOCK_DGRAM, 0, d, ttl, to)
		}},
		{"tcp-recverr", false, func(d netip.AddrPort, ttl int, to time.Duration) (Hop, error) {
			return errq(unix.SOCK_STREAM, 0, d, ttl, to)
		}},
	}
}

func errq(typ, proto int, d netip.AddrPort, ttl int, timeout time.Duration) (Hop, error) {
	fd, err := unix.Socket(unix.AF_INET, typ|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, proto)
	if err != nil {
		return Hop{}, err
	}
	defer unix.Close(fd)
	unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, ttl)
	unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVERR, 1)
	sa := &unix.SockaddrInet4{Addr: d.Addr().As4(), Port: int(d.Port())}
	sent := time.Now()
	switch {
	case typ == unix.SOCK_STREAM:
		if err := unix.Connect(fd, sa); err != nil && !errors.Is(err, unix.EINPROGRESS) {
			return Hop{}, err
		}
	case proto == unix.IPPROTO_ICMP:
		req := []byte{8, 0, 0, 0, 0, 0, 0, byte(ttl), 'f', 'y', 'i', 's', 'p'}
		sa.Port = 0
		if err := unix.Sendto(fd, req, 0, sa); err != nil {
			return Hop{}, err
		}
	default:
		if err := unix.Sendto(fd, make([]byte, 32), 0, sa); err != nil {
			return Hop{}, err
		}
	}
	buf, oob := make([]byte, 512), make([]byte, 512)
	for time.Since(sent) < timeout {
		pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN | unix.POLLOUT | unix.POLLERR}}
		unix.Poll(pfd, 50)
		if _, oobn, _, _, err := unix.Recvmsg(fd, buf, oob, unix.MSG_ERRQUEUE); err == nil {
			cms, _ := unix.ParseSocketControlMessage(oob[:oobn])
			for _, c := range cms {
				if c.Header.Level == unix.IPPROTO_IP && c.Header.Type == unix.IP_RECVERR && len(c.Data) >= 24 && c.Data[4] == unix.SO_EE_ORIGIN_ICMP {
					a := netip.AddrFrom4([4]byte(c.Data[20:24]))
					h := Hop{Addr: a, ICMP: fmt.Sprintf("%d/%d", c.Data[5], c.Data[6]), RTT: time.Since(sent)}
					h.Reached = c.Data[5] == 3 && a == d.Addr() // port unreachable from the destination (UDP)
					return h, nil
				}
			}
		}
		if typ == unix.SOCK_STREAM && pfd[0].Revents&unix.POLLOUT != 0 {
			e, _ := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
			if e == 0 || unix.Errno(e) == unix.ECONNREFUSED {
				return Hop{Addr: d.Addr(), Reached: true, RTT: time.Since(sent), Note: fmt.Sprintf("connect: %v", unix.Errno(e))}, nil
			}
		}
		if proto == unix.IPPROTO_ICMP {
			if n, _, err := unix.Recvfrom(fd, buf, 0); err == nil && n >= 8 && buf[0] == 0 {
				return Hop{Addr: d.Addr(), Reached: true, RTT: time.Since(sent)}, nil
			}
		}
	}
	_ = binary.NativeEndian
	return Hop{}, nil
}
