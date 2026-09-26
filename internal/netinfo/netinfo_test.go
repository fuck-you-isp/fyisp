package netinfo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestIsPublic(t *testing.T) {
	for s, want := range map[string]bool{
		"1.1.1.1": true, "8.8.8.8": true, "203.0.113.7": true, "198.51.100.2": true, "81.2.69.1": true,
		"10.0.0.1": false, "172.16.5.4": false, "172.31.255.255": false, "192.168.1.1": false,
		"100.64.0.1": false, "100.127.255.254": false, "127.0.0.1": false, "169.254.1.1": false,
		"0.0.0.0": false, "192.0.0.2": false, "198.18.0.1": false, "224.0.0.1": false, "255.255.255.255": false,
		"100.63.255.255": true, "100.128.0.0": true, "172.32.0.1": true,
		"::ffff:8.8.8.8": true, "2606:4700::1111": false,
	} {
		if got := IsPublic(netip.MustParseAddr(s)); got != want {
			t.Errorf("IsPublic(%s) = %v, want %v", s, got, want)
		}
	}
	if IsPublic(netip.Addr{}) {
		t.Error("zero address is public")
	}
}

// fakeTracer answers from a hop table: hops[ttl-1] is the router at that
// TTL ("" = silent); a hop listed in flaky answers only on its third try.
type fakeTracer struct {
	mu     sync.Mutex
	hops   []string
	dst    string // answers with an echo reply at len(hops)+1
	flaky  map[int]int
	unreac map[int]bool
	err    error
	calls  []int
}

func (f *fakeTracer) Probe(ctx context.Context, dst netip.Addr, ttl int, _ time.Duration) (hop, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, ttl)
	if f.err != nil {
		return hop{}, f.err
	}
	if f.flaky[ttl] > 0 {
		f.flaky[ttl]--
		return hop{}, nil
	}
	if ttl > len(f.hops) {
		if f.dst != "" {
			return hop{Addr: netip.MustParseAddr(f.dst), Reached: true}, nil
		}
		return hop{}, nil
	}
	if f.hops[ttl-1] == "" {
		return hop{}, nil
	}
	return hop{Addr: netip.MustParseAddr(f.hops[ttl-1]), Unreach: f.unreac[ttl]}, nil
}

func (f *fakeTracer) Close() error { return nil }

func TestDiscoverEdge(t *testing.T) {
	dst := netip.MustParseAddr("1.1.1.1")
	cases := []struct {
		name    string
		tr      *fakeTracer
		edge    string
		hop     int
		err     bool
		ncalls  int
		errLike error
	}{
		{"home router then ISP", &fakeTracer{hops: []string{"192.168.1.1", "81.2.69.1", "1.2.3.4"}}, "81.2.69.1", 2, false, 2, nil},
		{"CGNAT is not the edge", &fakeTracer{hops: []string{"192.168.1.1", "100.64.0.1", "10.1.0.1", "62.1.1.1"}}, "62.1.1.1", 4, false, 4, nil},
		{"silent hop is skipped after retries", &fakeTracer{hops: []string{"192.168.1.1", "", "62.1.1.1"}}, "62.1.1.1", 3, false, 1 + 3 + 1, nil},
		{"flaky hop answers on retry", &fakeTracer{hops: []string{"192.168.1.1", "62.1.1.1"}, flaky: map[int]int{2: 2}}, "62.1.1.1", 2, false, 1 + 3, nil},
		{"public gateway", &fakeTracer{hops: []string{"62.1.1.1"}}, "62.1.1.1", 1, false, 1, nil},
		{"destination reached directly", &fakeTracer{hops: []string{"192.168.1.1"}, dst: "1.1.1.1"}, "1.1.1.1", 2, false, 2, nil},
		{"private destination reached", &fakeTracer{hops: []string{"192.168.1.1"}, dst: "10.9.9.9"}, "", 0, true, 2, errNoPublicHop},
		{"unreachable", &fakeTracer{hops: []string{"192.168.1.1", "10.0.0.1"}, unreac: map[int]bool{2: true}}, "", 0, true, 2, errNoPublicHop},
		{"nothing within 8 hops", &fakeTracer{hops: []string{"192.168.1.1", "10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5", "10.0.0.6", "10.0.0.7", "10.0.0.8"}}, "", 0, true, 8, errNoPublicHop},
		{"socket error", &fakeTracer{err: errors.New("network is unreachable")}, "", 0, true, 1, nil},
	}
	for _, c := range cases {
		edge, n, err := discoverEdge(context.Background(), c.tr, dst)
		if (err != nil) != c.err || (c.errLike != nil && !errors.Is(err, c.errLike)) {
			t.Errorf("%s: err %v", c.name, err)
		}
		want := netip.Addr{}
		if c.edge != "" {
			want = netip.MustParseAddr(c.edge)
		}
		if edge != want || n != c.hop {
			t.Errorf("%s: edge %v hop %d, want %v hop %d", c.name, edge, n, want, c.hop)
		}
		if len(c.tr.calls) != c.ncalls {
			t.Errorf("%s: %d probes %v, want %d", c.name, len(c.tr.calls), c.tr.calls, c.ncalls)
		}
	}
}

func TestDiscoverEdgeCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := discoverEdge(ctx, &fakeTracer{hops: []string{"192.168.1.1", "62.1.1.1"}}, DefaultProbe)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err %v", err)
	}
}

// fakeNet is a switchable network for watcher tests.
type fakeNet struct {
	mu     sync.Mutex
	rt     route
	rtErr  error
	hops   []string
	traces int
}

func (n *fakeNet) route() (route, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.rt, n.rtErr
}

func (n *fakeNet) tracer() (tracer, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.traces++
	return &fakeTracer{hops: append([]string(nil), n.hops...)}, nil
}

func (n *fakeNet) set(f func(n *fakeNet)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	f(n)
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitFor(t *testing.T, ch <-chan Path, what string, ok func(Path) bool) Path {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case p, open := <-ch:
			if !open {
				t.Fatalf("%s: channel closed", what)
			}
			if ok(p) {
				return p
			}
		case <-deadline:
			t.Fatalf("%s: timed out", what)
		}
	}
}

func TestWatcher(t *testing.T) {
	gw1, gw2 := netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("10.0.0.1")
	edge1, edge2 := netip.MustParseAddr("81.2.69.1"), netip.MustParseAddr("62.1.1.1")
	n := &fakeNet{rt: route{Gateway: gw1, Iface: "wlan0"}, hops: []string{"192.168.1.1", "81.2.69.1"}}
	w := newWatcher(Options{PollGateway: 10 * time.Millisecond, Recheck: time.Hour, Log: quietLog()}, n.route, n.tracer)
	if w.o.Probe != DefaultProbe {
		t.Errorf("default probe %v", w.o.Probe)
	}
	ch, unsub := w.Subscribe()
	defer unsub()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- w.Run(ctx) }()

	p := waitFor(t, ch, "initial edge", func(p Path) bool { return p.Edge.IsValid() })
	if p.Gateway != gw1 || p.Iface != "wlan0" || p.Edge != edge1 || p.EdgeHop != 2 || p.Err != "" || p.Updated.IsZero() {
		t.Errorf("initial path %+v", p)
	}
	if c := w.Current(); c.Edge != edge1 || c.Gateway != gw1 {
		t.Errorf("Current() = %+v", c)
	}

	// The route disappears: the gateway is unknown, NoRoute is reported, and
	// the last edge is kept.
	n.set(func(n *fakeNet) { n.rtErr = errNoRoute })
	p = waitFor(t, ch, "no route", func(p Path) bool { return !p.Gateway.IsValid() })
	if !p.NoRoute() || p.Edge != edge1 {
		t.Errorf("no-route path %+v", p)
	}

	// Another network: the edge is forgotten and rediscovered.
	n.set(func(n *fakeNet) {
		n.rtErr = nil
		n.rt = route{Gateway: gw2, Iface: "eth0"}
		n.hops = []string{"10.0.0.1", "100.64.0.1", "62.1.1.1"}
	})
	p = waitFor(t, ch, "new gateway", func(p Path) bool { return p.Gateway == gw2 })
	if p.Edge == edge1 {
		t.Errorf("stale edge kept after the gateway changed: %+v", p)
	}
	p = waitFor(t, ch, "new edge", func(p Path) bool { return p.Edge.IsValid() })
	if p.Edge != edge2 || p.EdgeHop != 3 || p.NoRoute() {
		t.Errorf("new path %+v", p)
	}

	// Edge discovery fails: the edge is kept, Err says why.
	n.set(func(n *fakeNet) { n.hops = []string{"10.0.0.1"} })
	w.trigger()
	deadline := time.Now().Add(5 * time.Second)
	for w.Current().Err == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c := w.Current(); c.Edge != edge2 || c.Err == "" || c.NoRoute() {
		t.Errorf("after failed discovery: %+v", c)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	unsub()
	unsub() // idempotent
}

func TestWatcherGatewayUnsupported(t *testing.T) {
	// Gateway discovery fails (not "no route"): the edge is still discovered.
	n := &fakeNet{rtErr: errors.New("unsupported"), hops: []string{"192.168.1.1", "81.2.69.1"}}
	w := newWatcher(Options{PollGateway: time.Hour, Recheck: time.Hour, Log: quietLog()}, n.route, n.tracer)
	ch, unsub := w.Subscribe()
	defer unsub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	p := waitFor(t, ch, "edge", func(p Path) bool { return p.Edge.IsValid() })
	if p.Gateway.IsValid() || p.NoRoute() || p.Err == "" {
		t.Errorf("path %+v", p)
	}
}

func TestWatcherRetriesUnknownEdge(t *testing.T) {
	n := &fakeNet{rt: route{Gateway: netip.MustParseAddr("192.168.1.1"), Iface: "eth0"}, hops: []string{"192.168.1.1"}}
	w := newWatcher(Options{PollGateway: time.Hour, Recheck: time.Hour, Log: quietLog()}, n.route, n.tracer)
	w.retry = 20 * time.Millisecond
	ch, unsub := w.Subscribe()
	defer unsub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	n.set(func(n *fakeNet) { n.hops = []string{"192.168.1.1", "81.2.69.1"} })
	p := waitFor(t, ch, "edge after retry", func(p Path) bool { return p.Edge.IsValid() })
	if p.Edge != netip.MustParseAddr("81.2.69.1") {
		t.Errorf("path %+v", p)
	}
}
