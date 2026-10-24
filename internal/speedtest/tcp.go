package speedtest

import (
	"net"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/tcpinfo"
)

// tcpInfo is a TCP connection's retransmitted segments and bytes acked by
// the peer so far (TCP_INFO), -1 when unknown.
func tcpInfo(c net.Conn) (retrans, acked int64) {
	if in, ok := tcpinfo.Read(c); ok {
		return in.Retrans, in.BytesAcked
	}
	return -1, -1
}

// tcpReceived is the bytes a TCP connection received so far, read or
// still in its receive buffer (TCP_INFO), -1 when unknown.
func tcpReceived(c net.Conn) int64 {
	if in, ok := tcpinfo.Read(c); ok {
		return in.BytesReceived
	}
	return -1
}

// tcpMinRTT is the smallest RTT a TCP connection has seen (TCP_INFO
// min_rtt: its idle RTT), 0 when unknown.
func tcpMinRTT(c net.Conn) time.Duration {
	in, _ := tcpinfo.Read(c)
	return in.MinRTT
}
