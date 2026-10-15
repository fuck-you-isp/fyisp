//go:build windows

package main

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/user"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	tcpFailConnectOnICMPError = 18 // TCP_FAIL_CONNECT_ON_ICMP_ERROR (ws2ipdef.h)
	tcpICMPErrorInfo          = 19 // TCP_ICMP_ERROR_INFO
	ipRecvErr                 = 75 // IP_RECVERR
)

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// envTimeout lets MTRPROBE_TIMEOUT (a Go duration) override the per-probe timeout.
func envTimeout(def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv("MTRPROBE_TIMEOUT")); err == nil {
		return d
	}
	return def
}

func whoami() string {
	u, _ := user.Current()
	var tok windows.Token = windows.GetCurrentProcessToken()
	return fmt.Sprintf("%s elevated=%v", u.Username, tok.IsElevated())
}

func methods() []method {
	return []method{
		{"icmp-icmpsendecho", false, icmpEcho},
		{"tcp-failconnect", false, tcpFail},
		{"udp-recverr", true, udpRecvErr},
		{"udp-recverr-conn", true, func(d netip.AddrPort, ttl int, to time.Duration) (Hop, error) {
			return udpVariant(d, ttl, to, true, true, -1)
		}},
		{"udp-recverr-noreset", true, func(d netip.AddrPort, ttl int, to time.Duration) (Hop, error) {
			return udpVariant(d, ttl, to, false, true, 0)
		}},
		{"udp-conn-norecverr", true, func(d netip.AddrPort, ttl int, to time.Duration) (Hop, error) {
			return udpVariant(d, ttl, to, true, false, -1)
		}},
	}
}

var (
	iphlpapi            = windows.NewLazySystemDLL("iphlpapi.dll")
	procIcmpCreateFile  = iphlpapi.NewProc("IcmpCreateFile")
	procIcmpSendEcho2Ex = iphlpapi.NewProc("IcmpSendEcho2Ex")
)

type ipOptionInformation struct {
	TTL, Tos, Flags, OptionsSize uint8
	OptionsData                  uintptr
}

func icmpEcho(d netip.AddrPort, ttl int, to time.Duration) (Hop, error) {
	h, _, err := procIcmpCreateFile.Call()
	if h == 0 {
		return Hop{}, err
	}
	req := []byte("fyisp-mtrprobe!!")
	opt := &ipOptionInformation{TTL: uint8(ttl)}
	reply := make([]byte, 256)
	a4 := d.Addr().As4()
	sent := time.Now()
	procIcmpSendEcho2Ex.Call(h, 0, 0, 0, 0, uintptr(binary.LittleEndian.Uint32(a4[:])),
		uintptr(unsafe.Pointer(&req[0])), uintptr(len(req)), uintptr(unsafe.Pointer(opt)),
		uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), uintptr(to.Milliseconds()))
	runtime.KeepAlive(req)
	runtime.KeepAlive(opt)
	from := netip.AddrFrom4([4]byte(reply[0:4]))
	st := binary.LittleEndian.Uint32(reply[4:8])
	switch st {
	case 0:
		return Hop{Addr: from, Reached: true, RTT: time.Since(sent)}, nil
	case 11013:
		return Hop{Addr: from, ICMP: "11/0", RTT: time.Since(sent)}, nil
	}
	return Hop{Note: fmt.Sprintf("status %d", st)}, nil
}

// tcpFail: a normal TCP socket with a low TTL and
// TCP_FAIL_CONNECT_ON_ICMP_ERROR; when connect fails, TCP_ICMP_ERROR_INFO
// names the router that sent the ICMP error.
func tcpFail(d netip.AddrPort, ttl int, to time.Duration) (Hop, error) {
	to = envTimeout(to)
	s, err := windows.Socket(windows.AF_INET, windows.SOCK_STREAM, windows.IPPROTO_TCP)
	if err != nil {
		return Hop{}, fmt.Errorf("socket: %w", err)
	}
	if err := windows.SetsockoptInt(s, windows.IPPROTO_IP, windows.IP_TTL, ttl); err != nil {
		windows.Closesocket(s)
		return Hop{}, fmt.Errorf("IP_TTL: %w", err)
	}
	if err := windows.SetsockoptInt(s, windows.IPPROTO_TCP, tcpFailConnectOnICMPError, 1); err != nil {
		windows.Closesocket(s)
		return Hop{}, fmt.Errorf("TCP_FAIL_CONNECT_ON_ICMP_ERROR: %w", err)
	}
	sa := &windows.SockaddrInet4{Addr: d.Addr().As4(), Port: int(d.Port())}
	sent := time.Now()
	done := make(chan error, 1)
	go func() { done <- windows.Connect(s, sa) }()
	var cerr error
	select {
	case cerr = <-done:
	case <-time.After(to):
		windows.Closesocket(s)
		<-done
		return Hop{Note: "timeout"}, nil
	}
	defer windows.Closesocket(s)
	rtt := time.Since(sent)
	if cerr == nil {
		return Hop{Addr: d.Addr(), Reached: true, RTT: rtt, Note: "connected"}, nil
	}
	if errors.Is(cerr, windows.WSAECONNREFUSED) {
		return Hop{Addr: d.Addr(), Reached: true, RTT: rtt, Note: "refused"}, nil
	}
	// ICMP_ERROR_INFO { SOCKADDR_INET srcaddress (28); IPPROTO protocol (4); UINT8 type, code }
	var info [64]byte
	n := int32(len(info))
	if err := windows.Getsockopt(s, windows.IPPROTO_TCP, tcpICMPErrorInfo, &info[0], &n); err != nil {
		return Hop{Note: fmt.Sprintf("connect: %v; TCP_ICMP_ERROR_INFO: %v", cerr, err)}, nil
	}
	fam := binary.LittleEndian.Uint16(info[0:2])
	if fam != windows.AF_INET {
		return Hop{Note: fmt.Sprintf("connect: %v; info family %d raw %s", cerr, fam, hex.EncodeToString(info[:n]))}, nil
	}
	a := netip.AddrFrom4([4]byte(info[4:8]))
	return Hop{Addr: a, ICMP: fmt.Sprintf("%d/%d", info[32], info[33]), RTT: rtt,
		Note: fmt.Sprintf("connect: %v (n=%d)", cerr, n), Reached: a == d.Addr()}, nil
}

// udpRecvErr: IP_RECVERR on a UDP socket, then WSARecvMsg; print what the
// stack hands back (control messages and the msg name).
func udpRecvErr(d netip.AddrPort, ttl int, to time.Duration) (Hop, error) {
	return udpVariant(d, ttl, to, false, true, -1)
}

const sioUDPConnReset = windows.IOC_IN | windows.IOC_VENDOR | 12

// udpVariant: connected or not; IP_RECVERR on/off; SIO_UDP_CONNRESET
// left default (-1) or set to 0/1.
func udpVariant(d netip.AddrPort, ttl int, to time.Duration, connected, recverr bool, connreset int) (Hop, error) {
	to = envTimeout(to)
	s, err := windows.Socket(windows.AF_INET, windows.SOCK_DGRAM, windows.IPPROTO_UDP)
	if err != nil {
		return Hop{}, err
	}
	defer windows.Closesocket(s)
	windows.SetsockoptInt(s, windows.IPPROTO_IP, windows.IP_TTL, ttl)
	if recverr {
		if err := windows.SetsockoptInt(s, windows.IPPROTO_IP, ipRecvErr, 1); err != nil {
			return Hop{}, fmt.Errorf("IP_RECVERR: %w", err)
		}
	}
	if connreset >= 0 {
		v := uint32(connreset)
		var ret uint32
		if err := windows.WSAIoctl(s, sioUDPConnReset, (*byte)(unsafe.Pointer(&v)), 4, nil, 0, &ret, nil, 0); err != nil {
			return Hop{}, fmt.Errorf("SIO_UDP_CONNRESET: %w", err)
		}
	}
	windows.SetsockoptInt(s, windows.SOL_SOCKET, windows.SO_RCVTIMEO, int(to.Milliseconds()))
	if err := windows.Bind(s, &windows.SockaddrInet4{}); err != nil {
		return Hop{}, fmt.Errorf("bind: %w", err)
	}
	dsa := &windows.SockaddrInet4{Addr: d.Addr().As4(), Port: int(d.Port())}
	if connected {
		if err := windows.Connect(s, dsa); err != nil {
			return Hop{}, fmt.Errorf("connect: %w", err)
		}
	}
	sent := time.Now()
	if connected {
		var n uint32
		b := make([]byte, 32)
		wb := windows.WSABuf{Len: 32, Buf: &b[0]}
		if err := windows.WSASend(s, &wb, 1, &n, 0, nil, nil); err != nil {
			return Hop{}, fmt.Errorf("send: %w", err)
		}
	} else if err := windows.Sendto(s, make([]byte, 32), 0, dsa); err != nil {
		return Hop{}, fmt.Errorf("sendto: %w", err)
	}
	for time.Since(sent) < to {
		buf := make([]byte, 512)
		ctl := make([]byte, 512)
		var name syscall.RawSockaddrAny
		wb := windows.WSABuf{Len: uint32(len(buf)), Buf: &buf[0]}
		msg := windows.WSAMsg{Name: &name, Namelen: int32(unsafe.Sizeof(name)), Buffers: &wb, BufferCount: 1,
			Control: windows.WSABuf{Len: uint32(len(ctl)), Buf: &ctl[0]}}
		var got uint32
		rerr := windows.WSARecvMsg(s, &msg, &got, nil, nil)
		rtt := time.Since(sent)
		if rerr != nil && errors.Is(rerr, windows.WSAETIMEDOUT) {
			return Hop{Note: "timeout"}, nil
		}
		nb := (*[16]byte)(unsafe.Pointer(&name))
		from := netip.Addr{}
		if binary.LittleEndian.Uint16(nb[0:2]) == windows.AF_INET {
			from = netip.AddrFrom4([4]byte(nb[4:8]))
		}
		ctlHex := "none"
		if rerr == nil || msg.Control.Len != uint32(len(ctl)) || !allZero(ctl) {
			ctlHex = hex.EncodeToString(ctl[:msg.Control.Len])
		}
		var en syscall.Errno
		errors.As(rerr, &en)
		note := fmt.Sprintf("recvmsg err=%d(%v) bytes=%d flags=%#x name=%s ctl=%s", uintptr(en), rerr, got, msg.Flags, from, ctlHex)
		if rerr != nil && os.Getenv("MTRPROBE_SECOND") != "" {
			// Is anything (e.g. an IP_RECVERR control message) queued behind the error?
			windows.SetsockoptInt(s, windows.SOL_SOCKET, windows.SO_RCVTIMEO, 300)
			ctl2 := make([]byte, 512)
			var name2 syscall.RawSockaddrAny
			msg2 := windows.WSAMsg{Name: &name2, Namelen: int32(unsafe.Sizeof(name2)), Buffers: &wb, BufferCount: 1,
				Control: windows.WSABuf{Len: uint32(len(ctl2)), Buf: &ctl2[0]}}
			var got2 uint32
			rerr2 := windows.WSARecvMsg(s, &msg2, &got2, nil, nil)
			note += fmt.Sprintf(" | 2nd recvmsg err=%v bytes=%d flags=%#x ctllen=%d ctlzero=%v", rerr2, got2, msg2.Flags, msg2.Control.Len, allZero(ctl2))
		}
		// Control messages: WSACMSGHDR {Len uintptr; Level, Type int32} then data
		// IN_RECVERR { IPPROTO protocol; ULONG info; UINT8 type; UINT8 code }.
		c := ctl[:msg.Control.Len]
		hdr := int(unsafe.Sizeof(windows.WSACMSGHDR{}))
		for len(c) >= hdr {
			ln := int(*(*uintptr)(unsafe.Pointer(&c[0])))
			lvl := int32(binary.LittleEndian.Uint32(c[8:12]))
			typ := int32(binary.LittleEndian.Uint32(c[12:16]))
			if ln < hdr || ln > len(c) {
				break
			}
			data := c[hdr:ln]
			if lvl == windows.IPPROTO_IP && typ == ipRecvErr && len(data) >= 10 {
				return Hop{Addr: from, ICMP: fmt.Sprintf("%d/%d", data[8], data[9]), RTT: rtt, Note: note,
					Reached: from == d.Addr()}, nil
			}
			c = c[(ln+7)&^7:]
		}
		if rerr == nil && got > 0 {
			return Hop{Addr: from, Reached: true, RTT: rtt, Note: note}, nil
		}
		// Without a control message Windows still reports the ICMP error
		// as the recv error, with the ICMP sender in msg.Name:
		// WSAENETRESET = TTL expired, WSAECONNRESET = port unreachable,
		// WSAEHOSTUNREACH/WSAENETUNREACH = other unreachables.
		if from.IsValid() {
			switch {
			case errors.Is(rerr, windows.WSAENETRESET):
				return Hop{Addr: from, ICMP: "11/?", RTT: rtt, Note: note, Reached: from == d.Addr()}, nil
			case errors.Is(rerr, windows.WSAECONNRESET):
				return Hop{Addr: from, ICMP: "3/3?", RTT: rtt, Note: note, Reached: true}, nil
			case errors.Is(rerr, windows.WSAEHOSTUNREACH), errors.Is(rerr, windows.WSAENETUNREACH):
				return Hop{Addr: from, ICMP: "3/?", RTT: rtt, Note: note, Reached: from == d.Addr()}, nil
			}
		}
		return Hop{Note: note}, nil
	}
	return Hop{}, nil
}
