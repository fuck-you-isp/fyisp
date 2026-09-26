package trace

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo/hops"
)

// TestLiveLoopback traces 127.0.0.1 for real (ping socket or raw socket):
// one hop, the destination itself.
func TestLiveLoopback(t *testing.T) {
	if p, err := hops.Open(); err != nil {
		t.Skipf("TTL-limited ICMP unavailable here: %v", err)
	} else {
		_ = p.Close()
	}
	tr := New(Options{Interval: 200 * time.Millisecond, Log: quiet(),
		ReverseDNS: func(context.Context, netip.Addr) string { return "localhost" }})
	sink := &recSink{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tr.Run(ctx, &model.Profile{Name: "t", Groups: []model.Group{{ID: "g"}},
			Targets: []model.Target{{Name: "lo", Host: "127.0.0.1", Group: "g", Trace: true}}}, sink)
	}()
	waitFor(t, "3 rounds", func() bool { return len(sink.samplesOf("lo")) >= 3 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, s := range sink.samplesOf("lo") {
		if s.Key.Hop != 1 || s.Lost || s.RTT <= 0 || s.RTT > time.Second {
			t.Errorf("sample %+v, want hop 1 answered", s)
		}
	}
	rs := tr.Routes()
	if len(rs) != 1 || !slices.Equal(rs[0].Hops, addrs("127.0.0.1")) {
		t.Errorf("routes %+v", rs)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.changes) != 0 {
		t.Errorf("route changes %+v", sink.changes)
	}
}
