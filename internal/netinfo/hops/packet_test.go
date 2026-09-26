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
