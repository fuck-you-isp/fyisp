//go:build darwin

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/sys/unix"
)

func whoami() string { return fmt.Sprintf("uid=%d euid=%d", os.Getuid(), os.Geteuid()) }

func methods() []method {
	return []method{
		{"icmp-dgram", false, func(d netip.AddrPort, ttl int, to time.Duration) (Hop, error) { return viaICMP("icmp", d, ttl, to) }},
		{"udp-via-icmpsock", true, func(d netip.AddrPort, ttl int, to time.Duration) (Hop, error) { return viaICMP("udp", d, ttl, to) }},
		{"tcp-via-icmpsock", false, func(d netip.AddrPort, ttl int, to time.Duration) (Hop, error) { return viaICMP("tcp", d, ttl, to) }},
	}
}

// viaICMP sends the probe from an ordinary socket and listens for the
// router's ICMP error on the unprivileged SOCK_DGRAM ICMP socket, matching
// it by the quoted header.
func viaICMP(kind string, d netip.AddrPort, ttl int, to time.Duration) (Hop, error) {
	l, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		return Hop{}, fmt.Errorf("icmp dgram listen: %w", err)
	}
	defer l.Close()
	var proto byte
	var fd int = -1
	sent := time.Now()
	sa := &unix.SockaddrInet4{Addr: d.Addr().As4(), Port: int(d.Port())}
	switch kind {
	case "icmp":
		proto = 1
		l.IPv4PacketConn().SetTTL(ttl)
		req := []byte{8, 0, 0, 0, 0x12, 0x34, 0, byte(ttl), 'f', 'y', 'i', 's', 'p', 0, 0, 0}
		cs := checksum(req)
		req[2], req[3] = byte(cs>>8), byte(cs)
		if _, err := l.WriteTo(req, &net.UDPAddr{IP: d.Addr().AsSlice()}); err != nil {
			return Hop{}, err
		}
	case "udp", "tcp":
		typ := unix.SOCK_DGRAM
		proto = 17
		if kind == "tcp" {
			typ, proto = unix.SOCK_STREAM, 6
		}
		fd, err = unix.Socket(unix.AF_INET, typ, 0)
		if err != nil {
			return Hop{}, err
		}
		defer unix.Close(fd)
		unix.SetNonblock(fd, true)
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, ttl); err != nil {
			return Hop{}, fmt.Errorf("IP_TTL: %w", err)
		}
		if kind == "udp" {
			err = unix.Sendto(fd, make([]byte, 32), 0, sa)
		} else if err = unix.Connect(fd, sa); errors.Is(err, unix.EINPROGRESS) {
			err = nil
		}
		if err != nil {
			return Hop{}, err
		}
	}
	buf := make([]byte, 1500)
	for time.Since(sent) < to {
		if kind == "tcp" {
			pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
			if n, _ := unix.Poll(pfd, 0); n > 0 {
				e, _ := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
				if e == 0 || unix.Errno(e) == unix.ECONNREFUSED {
					return Hop{Addr: d.Addr(), Reached: true, RTT: time.Since(sent), Note: fmt.Sprintf("connect: %v", unix.Errno(e))}, nil
				}
			}
		}
		l.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, from, err := l.ReadFrom(buf)
		if err != nil {
			continue
		}
		b := buf[:n]
		if len(b) >= 20 && b[0]>>4 == 4 { // macOS delivers the IP header
			b = b[int(b[0]&0x0f)*4:]
		}
		if len(b) < 8 {
			continue
		}
		fa, _ := netip.AddrFromSlice(from.(*net.UDPAddr).IP)
		fa = fa.Unmap()
		if b[0] == 0 && kind == "icmp" {
			return Hop{Addr: fa, Reached: true, RTT: time.Since(sent)}, nil
		}
		if b[0] != 11 && b[0] != 3 {
			continue
		}
		q := b[8:]
		if len(q) < 28 || q[0]>>4 != 4 || q[9] != proto || netip.AddrFrom4([4]byte(q[16:20])) != d.Addr() {
			continue
		}
		ihl := int(q[0]&0x0f) * 4
		if kind != "icmp" && binary.BigEndian.Uint16(q[ihl+2:ihl+4]) != d.Port() {
			continue
		}
		return Hop{Addr: fa, ICMP: fmt.Sprintf("%d/%d", b[0], b[1]), RTT: time.Since(sent), Reached: b[0] == 3 && fa == d.Addr()}, nil
	}
	return Hop{}, nil
}

func checksum(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(b[i])<<8 | uint32(b[i+1])
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}
