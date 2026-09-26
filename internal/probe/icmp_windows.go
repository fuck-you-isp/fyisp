//go:build windows

package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Windows: IcmpSendEcho2Ex from iphlpapi.dll works for standard users, needs
// no cgo and no raw socket.
var (
	iphlpapi            = windows.NewLazySystemDLL("iphlpapi.dll")
	procIcmpCreateFile  = iphlpapi.NewProc("IcmpCreateFile")
	procIcmpCloseHandle = iphlpapi.NewProc("IcmpCloseHandle")
	procIcmpSendEcho2Ex = iphlpapi.NewProc("IcmpSendEcho2Ex")
)

// IP_STATUS values (ipexport.h).
const (
	ipSuccess              = 0
	ipDestNetUnreachable   = 11002
	ipDestHostUnreachable  = 11003
	ipDestProtUnreachable  = 11004
	ipDestPortUnreachable  = 11005
	ipReqTimedOut          = 11010
	ipBadRoute             = 11012
	ipTTLExpiredTransit    = 11013
	ipTTLExpiredReassembly = 11014
)

type winPinger struct {
	h     uintptr
	magic [8]byte
	ctr   atomic.Uint32
}

func openPinger() (pinger, string, error) {
	if err := procIcmpSendEcho2Ex.Find(); err != nil {
		return nil, ICMPUnavailable, err
	}
	h, _, err := procIcmpCreateFile.Call()
	if windows.Handle(h) == windows.InvalidHandle || h == 0 {
		return nil, ICMPUnavailable, fmt.Errorf("IcmpCreateFile: %w", err)
	}
	return &winPinger{h: h, magic: newMagic()}, ICMPIphlpapi, nil
}

func icmpHint(err error) string {
	return fmt.Sprintf("ICMP unavailable (%v): probing HTTPS and TCP only.", err)
}

func (p *winPinger) Close() error {
	if p.h != 0 {
		procIcmpCloseHandle.Call(p.h)
		p.h = 0
	}
	return nil
}

func statusReason(st uint32) (model.Reason, error) {
	switch st {
	case ipSuccess:
		return 0, nil
	case ipReqTimedOut:
		return model.ReasonTimeout, errNoReply
	case ipDestNetUnreachable, ipDestHostUnreachable, ipDestProtUnreachable, ipDestPortUnreachable,
		ipBadRoute, ipTTLExpiredTransit, ipTTLExpiredReassembly, uint32(windows.ERROR_HOST_UNREACHABLE):
		return model.ReasonUnreachable, fmt.Errorf("icmp: IP status %d", st)
	case uint32(windows.ERROR_NETWORK_UNREACHABLE):
		return model.ReasonNoNetwork, fmt.Errorf("icmp: network unreachable (%d)", st)
	}
	return model.ReasonOther, fmt.Errorf("icmp: IP status %d", st)
}

func (p *winPinger) Ping(ctx context.Context, dst netip.Addr, idx uint32, timeout time.Duration) (time.Duration, model.Reason, error) {
	if !dst.Is4() {
		return 0, model.ReasonOther, fmt.Errorf("icmp: %s is not IPv4", dst)
	}
	req := make([]byte, payloadLen)
	putPayload(req, p.magic, idx, p.ctr.Add(1))
	// ICMP_ECHO_REPLY (40 bytes on 64-bit) + data + 8 bytes for an ICMP
	// error + IO_STATUS_BLOCK, rounded up generously.
	reply := make([]byte, 256)
	a4 := dst.As4()
	ms := max(timeout.Milliseconds(), 1)
	t0 := time.Now()
	n, _, callErr := procIcmpSendEcho2Ex.Call(p.h, 0, 0, 0,
		0, uintptr(binary.LittleEndian.Uint32(a4[:])),
		uintptr(unsafe.Pointer(&req[0])), uintptr(len(req)), 0,
		uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), uintptr(ms))
	rtt := time.Since(t0)
	runtime.KeepAlive(req)
	runtime.KeepAlive(reply)
	if n == 0 {
		var e syscall.Errno
		if errors.As(callErr, &e) {
			r, err := statusReason(uint32(e))
			if err == nil {
				r, err = model.ReasonOther, callErr
			}
			return 0, r, err
		}
		return 0, model.ReasonOther, callErr
	}
	// ICMP_ECHO_REPLY: Address uint32, Status uint32, RoundTripTime uint32, ...
	if r, err := statusReason(binary.LittleEndian.Uint32(reply[4:8])); err != nil {
		return 0, r, err
	}
	if ctx.Err() != nil {
		return 0, model.ReasonOther, ctx.Err()
	}
	return rtt, 0, nil
}
