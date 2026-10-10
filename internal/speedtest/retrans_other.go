//go:build !linux

package speedtest

import "net"

// totalRetrans is unknown outside Linux (no portable TCP_INFO).
func totalRetrans(net.Conn) int64 { return -1 }
