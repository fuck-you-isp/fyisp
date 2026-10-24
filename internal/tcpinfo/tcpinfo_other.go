//go:build !linux

package tcpinfo

import "net"

// Read reports nothing outside Linux (no portable TCP_INFO).
func Read(net.Conn) (Info, bool) { return Info{}, false }
