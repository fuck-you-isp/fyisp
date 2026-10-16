//go:build unix

package hops

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// TestPacketLoopback exercises the shared-socket prober (raw socket; root
// or CAP_NET_RAW) with parallel probes: each gets its own answer.
func TestPacketLoopback(t *testing.T) {
	p, err := openPacket("ip4:icmp")
	if err != nil {
		t.Skipf("raw ICMP socket unavailable: %v", err)
	}
	defer p.Close()
	dst := netip.MustParseAddr("127.0.0.1")
	var wg sync.WaitGroup
	for ttl := 1; ttl <= 16; ttl++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := p.Probe(context.Background(), dst, ttl, 2*time.Second)
			if err != nil || h.Addr != dst || !h.Reached || h.RTT <= 0 || h.RTT > 2*time.Second {
				t.Errorf("ttl %d: %+v, %v", ttl, h, err)
			}
		}()
	}
	wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pending) != 0 {
		t.Errorf("%d probes left pending", len(p.pending))
	}
}

// icmpErr builds an ICMP error of type typ quoting a TCP or UDP packet.
func icmpErr(typ uint8, tp Transport, dst string, sport, dport uint16) []byte {
	b := make([]byte, 8+20+8)
	b[0] = typ
	q := b[8:]
	q[0], q[9] = 0x45, byte(tp)
	d := netip.MustParseAddr(dst).As4()
	copy(q[16:20], d[:])
	q[20], q[21] = byte(sport>>8), byte(sport)
	q[22], q[23] = byte(dport>>8), byte(dport)
	return b
}

// TestMatchTransport: ICMP errors quoting TCP and UDP probes go to the
// probe with the same protocol, source port and destination (the macOS
// path), and an unreachable from the destination is the destination.
func TestMatchTransport(t *testing.T) {
	dst := netip.MustParseAddr("192.0.2.9")
	router := netip.MustParseAddr("10.0.0.1")
	tcp := &pendingProbe{dst: dst, dport: 443}
	udp := &pendingProbe{dst: dst, dport: 33434}
	p := &packetProber{pending: map[uint16]*pendingProbe{}, tpend: map[l4Key]*pendingProbe{
		{TCP, 50001}: tcp, {UDP, 50001}: udp}}
	cases := []struct {
		msg  []byte
		from netip.Addr
		want *pendingProbe
		hop  Hop
	}{
		{icmpErr(11, TCP, "192.0.2.9", 50001, 443), router, tcp, Hop{Addr: router}},
		{icmpErr(11, UDP, "192.0.2.9", 50001, 33434), router, udp, Hop{Addr: router}},
		{icmpErr(3, UDP, "192.0.2.9", 50001, 33434), dst, udp, Hop{Addr: dst, Reached: true}},
		{icmpErr(3, TCP, "192.0.2.9", 50001, 443), router, tcp, Hop{Addr: router, Unreach: true}},
		{icmpErr(11, TCP, "192.0.2.9", 50002, 443), router, nil, Hop{}},    // another socket
		{icmpErr(11, TCP, "192.0.2.10", 50001, 443), router, nil, Hop{}},   // another destination
		{icmpErr(11, TCP, "192.0.2.9", 50001, 80), router, nil, Hop{}},     // another port
		{icmpErr(11, UDP, "192.0.2.9", 50001, 443), router, nil, Hop{}},    // UDP with TCP's port
		{icmpErr(0, TCP, "192.0.2.9", 50001, 443)[:8], router, nil, Hop{}}, // not an error
	}
	for i, c := range cases {
		got, h, ok := p.match(c.msg, c.from)
		if c.want == nil {
			if ok {
				t.Errorf("%d: matched %+v", i, h)
			}
			continue
		}
		if !ok || got != c.want || h != c.hop {
			t.Errorf("%d: %v %+v %v, want %+v", i, got == c.want, h, ok, c.hop)
		}
	}
	if q, ok := parseQuotedL4(icmpErr(11, UDP, "192.0.2.9", 1, 2)[8:]); !ok || q != (quotedL4{UDP, dst, 1, 2}) {
		t.Errorf("parseQuotedL4: %+v %v", q, ok)
	}
	if _, ok := parseQuotedL4(icmpErr(11, UDP, "192.0.2.9", 1, 2)[8:24]); ok {
		t.Error("short quote accepted")
	}
}
