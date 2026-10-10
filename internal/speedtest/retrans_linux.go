package speedtest

import (
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// tcpInfo is a TCP connection's retransmitted segments and bytes acked by
// the peer so far (TCP_INFO), -1 when unknown.
func tcpInfo(c net.Conn) (retrans, acked int64) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return -1, -1
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return -1, -1
	}
	retrans, acked = -1, -1
	_ = rc.Control(func(fd uintptr) {
		if ti, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO); err == nil {
			retrans, acked = int64(ti.Total_retrans), int64(ti.Bytes_acked)
		}
	})
	return retrans, acked
}

// tcpReceived is the bytes a TCP connection received so far, read or
// still in its receive buffer (TCP_INFO), -1 when unknown.
func tcpReceived(c net.Conn) int64 {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return -1
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return -1
	}
	n := int64(-1)
	_ = rc.Control(func(fd uintptr) {
		if ti, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO); err == nil {
			n = int64(ti.Bytes_received)
		}
	})
	return n
}

// tcpRTT is a TCP connection's smoothed RTT (TCP_INFO), 0 when unknown.
func tcpRTT(c net.Conn) time.Duration {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return 0
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return 0
	}
	var d time.Duration
	_ = rc.Control(func(fd uintptr) {
		if ti, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO); err == nil {
			d = time.Duration(ti.Rtt) * time.Microsecond
		}
	})
	return d
}
