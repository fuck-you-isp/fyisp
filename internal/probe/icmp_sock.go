//go:build unix

package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// sockPinger shares one ICMP socket between all targets: an unprivileged
// "udp4" socket when the OS allows it (Linux ping_group_range, macOS), else
// a raw "ip4:icmp" socket.
type sockPinger struct {
	c     *icmp.PacketConn
	raw   bool
	id    int
	magic [8]byte
	done  chan struct{}

	mu   sync.Mutex
	seq  uint16
	ctr  uint32
	pend map[uint16]*pending
}

type pending struct {
	idx, ctr uint32
	dst      netip.Addr
	ch       chan echoResult
}

type echoResult struct {
	at     time.Time
	reason model.Reason
	err    error
}

func openPinger() (pinger, string, error) {
	c, errUDP := icmp.ListenPacket("udp4", "0.0.0.0")
	mode := ICMPUDP
	if errUDP != nil {
		var errRaw error
		c, errRaw = icmp.ListenPacket("ip4:icmp", "0.0.0.0")
		if errRaw != nil {
			return nil, ICMPUnavailable, fmt.Errorf("udp4: %w; raw: %w", errUDP, errRaw)
		}
		mode = ICMPRaw
	}
	p := &sockPinger{
		c: c, raw: mode == ICMPRaw, id: os.Getpid() & 0xffff, magic: newMagic(),
		done: make(chan struct{}), pend: map[uint16]*pending{},
	}
	go p.read()
	return p, mode, nil
}

func icmpHint(err error) string {
	h := fmt.Sprintf("ICMP unavailable (%v): probing HTTPS and TCP only.", err)
	if runtime.GOOS == "linux" {
		cur, _ := os.ReadFile("/proc/sys/net/ipv4/ping_group_range")
		h += fmt.Sprintf(" Unprivileged ping is limited by net.ipv4.ping_group_range (now %q, your gid %d);"+
			` allow it with: sudo sysctl -w net.ipv4.ping_group_range="0 2147483647"`,
			strings.Join(strings.Fields(string(cur)), " "), os.Getgid())
	}
	return h
}

func (p *sockPinger) Close() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	close(p.done)
	return p.c.Close()
}

func (p *sockPinger) Ping(ctx context.Context, dst netip.Addr, idx uint32, timeout time.Duration) (time.Duration, model.Reason, error) {
	if !dst.Is4() {
		return 0, model.ReasonOther, fmt.Errorf("icmp: %s is not IPv4", dst)
	}
	pe := &pending{idx: idx, dst: dst, ch: make(chan echoResult, 1)}
	p.mu.Lock()
	p.seq++
	p.ctr++
	seq := p.seq
	pe.ctr = p.ctr
	p.pend[seq] = pe
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.pend[seq] == pe {
			delete(p.pend, seq)
		}
		p.mu.Unlock()
	}()

	body := make([]byte, payloadLen)
	putPayload(body, p.magic, idx, pe.ctr)
	msg := icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: p.id, Seq: int(seq), Data: body}}
	b, err := msg.Marshal(nil)
	if err != nil {
		return 0, model.ReasonOther, err
	}
	var addr net.Addr = &net.UDPAddr{IP: dst.AsSlice()}
	if p.raw {
		addr = &net.IPAddr{IP: dst.AsSlice()}
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	t0 := time.Now()
	if _, err := p.c.WriteTo(b, addr); err != nil {
		return 0, classify(err), err
	}
	select {
	case r := <-pe.ch:
		if r.err != nil {
			return 0, r.reason, r.err
		}
		return r.at.Sub(t0), 0, nil
	case <-t.C:
		return 0, model.ReasonTimeout, errNoReply
	case <-ctx.Done():
		return 0, model.ReasonOther, ctx.Err()
	}
}

func (p *sockPinger) read() {
	buf := make([]byte, 1500)
	for {
		n, _, err := p.c.ReadFrom(buf)
		at := time.Now()
		if err != nil {
			select {
			case <-p.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		p.handle(buf[:n], at)
	}
}

// handle matches one received ICMP message to a pending echo request.
func (p *sockPinger) handle(b []byte, at time.Time) {
	// The IPv4 header is stripped on Linux and (via IP_STRIPHDR) on macOS;
	// strip it here too in case a platform leaves it on.
	if len(b) >= 20 && b[0]>>4 == 4 {
		ihl := int(b[0]&0x0f) * 4
		if ihl < 20 || len(b) < ihl {
			return
		}
		b = b[ihl:]
	}
	if len(b) < 8 {
		return
	}
	switch b[0] {
	case 0: // echo reply
		data := b[8:]
		if len(data) < payloadLen || [8]byte(data[:8]) != p.magic {
			return
		}
		seq := binary.BigEndian.Uint16(b[6:8])
		idx := binary.BigEndian.Uint32(data[8:12])
		ctr := binary.BigEndian.Uint32(data[12:16])
		p.deliver(seq, func(pe *pending) bool { return pe.idx == idx && pe.ctr == ctr },
			echoResult{at: at})
	case 3, 11: // destination unreachable, time exceeded (raw sockets only)
		q := b[8:] // quoted IPv4 header + first 8 bytes of our echo request
		if len(q) < 20 || q[0]>>4 != 4 {
			return
		}
		ihl := int(q[0]&0x0f) * 4
		if len(q) < ihl+8 || q[9] != 1 || q[ihl] != 8 {
			return
		}
		dst := netip.AddrFrom4([4]byte(q[16:20]))
		id := int(binary.BigEndian.Uint16(q[ihl+4 : ihl+6]))
		seq := binary.BigEndian.Uint16(q[ihl+6 : ihl+8])
		if p.raw && id != p.id {
			return
		}
		what := "destination unreachable"
		if b[0] == 11 {
			what = "time exceeded"
		}
		p.deliver(seq, func(pe *pending) bool { return pe.dst == dst },
			echoResult{reason: model.ReasonUnreachable, err: fmt.Errorf("icmp %s (code %d)", what, b[1])})
	}
}

func (p *sockPinger) deliver(seq uint16, match func(*pending) bool, r echoResult) {
	p.mu.Lock()
	pe := p.pend[seq]
	if pe == nil || !match(pe) {
		p.mu.Unlock()
		return
	}
	delete(p.pend, seq)
	p.mu.Unlock()
	select {
	case pe.ch <- r:
	default:
	}
}
