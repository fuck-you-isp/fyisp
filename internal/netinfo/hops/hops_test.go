package hops

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// TestLoopback sends parallel TTL-limited probes to 127.0.0.1 with the best
// available socket: each gets its own answer.
func TestLoopback(t *testing.T) {
	p, err := Open()
	if err != nil {
		t.Skipf("TTL-limited ICMP unavailable here: %v", err)
	}
	defer p.Close()
	dst := netip.MustParseAddr("127.0.0.1")
	var wg sync.WaitGroup
	for ttl := 1; ttl <= 8; ttl++ {
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
}

func TestEchoRequestChecksum(t *testing.T) {
	b := echoRequest(1, 2, [8]byte{1, 2, 3, 4, 5, 6, 7, 8})
	if checksum(b) != 0 {
		t.Errorf("checksum of a checksummed message = %#x, want 0", checksum(b))
	}
}
