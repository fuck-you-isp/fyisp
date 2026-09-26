//go:build windows

package hops

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows: IcmpSendEcho2Ex with IP_OPTION_INFORMATION.Ttl works for standard
// users. A router's time-exceeded answer comes back as a reply with status
// IP_TTL_EXPIRED_TRANSIT and the router's address.
var (
	iphlpapi            = windows.NewLazySystemDLL("iphlpapi.dll")
	procIcmpCreateFile  = iphlpapi.NewProc("IcmpCreateFile")
	procIcmpCloseHandle = iphlpapi.NewProc("IcmpCloseHandle")
	procIcmpSendEcho2Ex = iphlpapi.NewProc("IcmpSendEcho2Ex")
)

// IP_STATUS values (ipexport.h).
const (
	ipSuccess             = 0
	ipDestNetUnreachable  = 11002
	ipDestHostUnreachable = 11003
	ipDestProtUnreachable = 11004
	ipDestPortUnreachable = 11005
	ipReqTimedOut         = 11010
	ipTTLExpiredTransit   = 11013
)

// ipOptionInformation is IP_OPTION_INFORMATION (natural alignment matches
// the C layout on 32- and 64-bit Windows).
type ipOptionInformation struct {
	TTL         uint8
	Tos         uint8
	Flags       uint8
	OptionsSize uint8
	OptionsData uintptr
}

type winProber struct {
	h     uintptr
	magic [8]byte
}

func open() (Prober, error) {
	if err := procIcmpSendEcho2Ex.Find(); err != nil {
		return nil, err
	}
	h, _, err := procIcmpCreateFile.Call()
	if windows.Handle(h) == windows.InvalidHandle || h == 0 {
		return nil, fmt.Errorf("IcmpCreateFile: %w", err)
	}
	return &winProber{h: h, magic: newMagic()}, nil
}

func (t *winProber) Close() error {
	if t.h != 0 {
		procIcmpCloseHandle.Call(t.h)
		t.h = 0
	}
	return nil
}

func (t *winProber) Probe(ctx context.Context, dst netip.Addr, ttl int, timeout time.Duration) (Hop, error) {
	if !dst.Is4() {
		return Hop{}, fmt.Errorf("%s is not IPv4", dst)
	}
	req := make([]byte, 16)
	copy(req, t.magic[:])
	copy(req[8:], "fyisp-tr")
	opt := &ipOptionInformation{TTL: uint8(ttl)}
	// ICMP_ECHO_REPLY (40 bytes on 64-bit) + data + room for an ICMP error.
	reply := make([]byte, 256)
	a4 := dst.As4()
	sent := time.Now()
	_, _, callErr := procIcmpSendEcho2Ex.Call(t.h, 0, 0, 0,
		0, uintptr(binary.LittleEndian.Uint32(a4[:])),
		uintptr(unsafe.Pointer(&req[0])), uintptr(len(req)), uintptr(unsafe.Pointer(opt)),
		uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), uintptr(max(timeout.Milliseconds(), 1)))
	runtime.KeepAlive(req)
	runtime.KeepAlive(opt)
	runtime.KeepAlive(reply)
	rtt := time.Since(sent)
	if ctx.Err() != nil {
		return Hop{}, nil
	}
	// ICMP_ECHO_REPLY: Address uint32 (network order in memory), Status uint32.
	// The reply is filled in even when the call returns 0 for a
	// time-exceeded answer, so read the status from it first.
	from := netip.AddrFrom4([4]byte(reply[0:4]))
	status := binary.LittleEndian.Uint32(reply[4:8])
	if status == ipSuccess && from.IsUnspecified() {
		// Nothing written: take the status from the call's error.
		var e syscall.Errno
		if !errors.As(callErr, &e) {
			return Hop{}, nil
		}
		status = uint32(e)
	}
	switch status {
	case ipSuccess:
		return Hop{Addr: from, Reached: true, RTT: rtt}, nil
	case ipTTLExpiredTransit:
		return Hop{Addr: from, RTT: rtt}, nil
	case ipDestNetUnreachable, ipDestHostUnreachable, ipDestProtUnreachable, ipDestPortUnreachable:
		if from.IsUnspecified() {
			return Hop{}, nil
		}
		return Hop{Addr: from, Unreach: true, RTT: rtt}, nil
	case ipReqTimedOut:
		return Hop{}, nil
	case uint32(windows.ERROR_NETWORK_UNREACHABLE), uint32(windows.ERROR_HOST_UNREACHABLE):
		return Hop{}, fmt.Errorf("IcmpSendEcho2Ex: IP status %d", status)
	}
	return Hop{}, nil
}
