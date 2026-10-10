package speedtest

import (
	"net"

	"golang.org/x/sys/unix"
)

// totalRetrans is a TCP connection's retransmitted segments so far
// (TCP_INFO tcpi_total_retrans), -1 when unknown.
func totalRetrans(c net.Conn) int64 {
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
			n = int64(ti.Total_retrans)
		}
	})
	return n
}
