package probe

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
)

func TestPathAddr(t *testing.T) {
	gw, edge := netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("81.2.69.1")
	cases := []struct {
		name    string
		path    func() netinfo.Path
		special string
		ip      netip.Addr
		ok, err bool
	}{
		{"no Path func", nil, model.HostGateway, netip.Addr{}, false, false},
		{"gateway known", func() netinfo.Path { return netinfo.Path{Gateway: gw, Edge: edge} }, model.HostGateway, gw, true, false},
		{"edge known", func() netinfo.Path { return netinfo.Path{Gateway: gw, Edge: edge} }, model.HostEdge, edge, true, false},
		{"edge not yet discovered", func() netinfo.Path { return netinfo.Path{Gateway: gw} }, model.HostEdge, netip.Addr{}, false, false},
		{"gateway discovery failed", func() netinfo.Path { return netinfo.Path{Err: "unsupported"} }, model.HostGateway, netip.Addr{}, false, false},
		{"no default route", func() netinfo.Path { return netinfo.Path{Err: netinfo.ErrNoRoute} }, model.HostGateway, netip.Addr{}, true, true},
		{"no default route, edge unknown", func() netinfo.Path { return netinfo.Path{Err: netinfo.ErrNoRoute} }, model.HostEdge, netip.Addr{}, true, true},
		{"no default route, edge kept", func() netinfo.Path { return netinfo.Path{Edge: edge, Err: netinfo.ErrNoRoute} }, model.HostEdge, edge, true, false},
	}
	for _, c := range cases {
		r := &runner{o: Options{Path: c.path}}
		ip, ok, err := r.pathAddr(c.special)
		if ip != c.ip || ok != c.ok || (err != nil) != c.err {
			t.Errorf("%s: got %v %v %v", c.name, ip, ok, err)
		}
	}
}

// TestSpecialHosts probes @gateway (resolved through a stub Path to
// loopback) and @isp-edge (unknown): only ICMP series exist, the unknown
// edge produces no samples, and without a default route both are lost with
// ReasonNoNetwork.
func TestSpecialHosts(t *testing.T) {
	if !icmpAvailable(t) {
		t.Skip("ICMP unavailable")
	}
	var (
		mu      sync.Mutex
		path    = netinfo.Path{Gateway: netip.MustParseAddr("127.0.0.1"), Iface: "lo"}
		noRoute atomic.Bool
		calls   atomic.Int64
	)
	stub := func() netinfo.Path {
		calls.Add(1)
		mu.Lock()
		defer mu.Unlock()
		return path
	}
	r := New(Options{Log: quietLog(), Path: stub})
	iv := 600 * time.Millisecond
	p := profileOf(
		model.Target{Name: "Gateway", Host: model.HostGateway, Layer: model.LayerGateway, Kinds: []model.ProbeKind{model.KindICMP, model.KindTCP}, Interval: iv},
		model.Target{Name: "ISP edge", Host: model.HostEdge, Layer: model.LayerEdge, Kinds: []model.ProbeKind{model.KindICMP}, Interval: iv},
	)
	gwKey := model.SeriesKey{Target: "Gateway", Kind: model.KindICMP}
	edgeKey := model.SeriesKey{Target: "ISP edge", Kind: model.KindICMP}
	ss := collect(t, r, p, 20*time.Second, func(ss []model.Sample) bool {
		m := byKey(ss)
		if len(m[gwKey]) >= 3 && noRoute.CompareAndSwap(false, true) {
			mu.Lock()
			path = netinfo.Path{Err: netinfo.ErrNoRoute}
			mu.Unlock()
		}
		lost := 0
		for _, s := range m[edgeKey] {
			if s.Lost {
				lost++
			}
		}
		return lost >= 2
	})
	var okGW, lostGW, lostEdge int
	for _, s := range ss {
		switch {
		case s.Key.Kind != model.KindICMP:
			t.Errorf("non-ICMP sample for a special host: %+v", s)
		case !s.Lost && s.Key == gwKey:
			okGW++
		case s.Lost && s.Reason == model.ReasonNoNetwork:
			if s.Key == gwKey {
				lostGW++
			} else {
				lostEdge++
			}
		case s.Key == edgeKey:
			t.Errorf("edge sample while the edge was unknown: %+v", s)
		default:
			t.Errorf("unexpected sample %+v", s)
		}
	}
	if okGW < 3 || lostGW < 1 || lostEdge < 2 {
		t.Errorf("gateway ok=%d lost=%d, edge lost=%d", okGW, lostGW, lostEdge)
	}
	if calls.Load() == 0 {
		t.Error("Path was never called")
	}
}

// TestSpecialHostsWithoutPath: with no Path func, special hosts are never
// measured (and never resolved through DNS).
func TestSpecialHostsWithoutPath(t *testing.T) {
	if !icmpAvailable(t) {
		t.Skip("ICMP unavailable")
	}
	r := New(Options{Log: quietLog()})
	p := profileOf(
		model.Target{Name: "Gateway", Host: model.HostGateway, Kinds: []model.ProbeKind{model.KindICMP}, Interval: 300 * time.Millisecond},
		model.Target{Name: "local", Host: "127.0.0.1", Kinds: []model.ProbeKind{model.KindICMP}, Interval: 300 * time.Millisecond},
	)
	ss := collect(t, r, p, 10*time.Second, func(ss []model.Sample) bool { return len(ss) >= 5 })
	for _, s := range ss {
		if s.Key.Target != "local" {
			t.Errorf("sample for an unresolvable special host: %+v", s)
		}
	}
}
