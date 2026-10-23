package tcpinfo

import (
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// Read is c's counters; false when c is not a TCP connection or the kernel
// refused.
func Read(c net.Conn) (Info, bool) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return Info{}, false
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return Info{}, false
	}
	var in Info
	got := false
	_ = rc.Control(func(fd uintptr) {
		ti, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if err != nil {
			return
		}
		got = true
		in = Info{RTT: time.Duration(ti.Rtt) * time.Microsecond, MinRTT: time.Duration(ti.Min_rtt) * time.Microsecond,
			Retrans: int64(ti.Total_retrans), MSS: int(ti.Snd_mss), BytesAcked: int64(ti.Bytes_acked), BytesReceived: int64(ti.Bytes_received)}
	})
	return in, got
}
