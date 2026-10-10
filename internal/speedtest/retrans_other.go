//go:build !linux

package speedtest

import "net"

// tcpInfo is unknown outside Linux (no portable TCP_INFO).
func tcpInfo(net.Conn) (retrans, acked int64) { return -1, -1 }

// tcpReceived is unknown outside Linux.
func tcpReceived(net.Conn) int64 { return -1 }
