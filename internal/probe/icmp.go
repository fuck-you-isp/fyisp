package probe

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net/netip"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// pinger sends ICMP echo requests. Implementations are safe for concurrent
// use: one pinger (one socket or handle) serves every target.
type pinger interface {
	// Ping sends one echo request to dst and waits up to timeout for the
	// reply. idx identifies the target in the payload.
	Ping(ctx context.Context, dst netip.Addr, idx uint32, timeout time.Duration) (time.Duration, model.Reason, error)
	Close() error
}

// ICMP modes reported in Caps.ICMP.
const (
	ICMPUDP         = "udp"         // unprivileged datagram socket (Linux, macOS)
	ICMPRaw         = "raw"         // raw socket (root or CAP_NET_RAW)
	ICMPIphlpapi    = "iphlpapi"    // Windows IcmpSendEcho2Ex
	ICMPUnavailable = "unavailable" // TCP/HTTPS only
)

// payloadLen is the echo payload: 8-byte per-process magic, target index,
// counter. Replies are matched on it because Linux rewrites the echo ID of
// unprivileged sockets.
const payloadLen = 16

var errNoReply = errors.New("no echo reply before timeout")

func newMagic() (m [8]byte) {
	_, _ = rand.Read(m[:])
	return m
}

func putPayload(b []byte, magic [8]byte, idx, ctr uint32) {
	copy(b[:8], magic[:])
	binary.BigEndian.PutUint32(b[8:12], idx)
	binary.BigEndian.PutUint32(b[12:16], ctr)
}

// Detect reports which probe kinds work without elevated privileges. It
// opens (and closes) the ICMP socket or handle that Run would use.
func Detect(ctx context.Context) Caps {
	c := Caps{TCP: true, HTTPS: true}
	p, mode, err := openPinger()
	if err != nil {
		c.ICMP = ICMPUnavailable
		c.ICMPHint = icmpHint(err)
		return c
	}
	_ = p.Close()
	c.ICMP = mode
	return c
}
