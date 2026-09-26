package trace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/asn"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo/hops"
)

var a = netip.MustParseAddr

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeNet answers TTL-limited probes from a hop table: routers[ttl-1] is the
// router at that TTL ("" = silent); the destination answers at
// len(routers)+1 and above. silent TTLs do not answer.
type fakeNet struct {
	mu       sync.Mutex
	routers  []string
	silent   map[int]bool
	err      error
	calls    map[netip.Addr][]int
	inflight map[netip.Addr]int
	maxIn    map[netip.Addr]int
	delay    time.Duration
}

func newFakeNet(routers ...string) *fakeNet {
	return &fakeNet{routers: routers, silent: map[int]bool{}, calls: map[netip.Addr][]int{},
		inflight: map[netip.Addr]int{}, maxIn: map[netip.Addr]int{}}
}

func (f *fakeNet) set(fn func(f *fakeNet)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeNet) Probe(ctx context.Context, dst netip.Addr, ttl int, _ time.Duration) (hops.Hop, error) {
	f.mu.Lock()
	f.calls[dst] = append(f.calls[dst], ttl)
	f.inflight[dst]++
	f.maxIn[dst] = max(f.maxIn[dst], f.inflight[dst])
	routers, silent, err, delay := f.routers, f.silent[ttl], f.err, f.delay
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.inflight[dst]--; f.mu.Unlock() }()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
	}
	if err != nil {
		return hops.Hop{}, err
	}
	if silent {
		return hops.Hop{}, nil
	}
	rtt := time.Duration(ttl) * time.Millisecond
	if ttl > len(routers) {
		return hops.Hop{Addr: dst, Reached: true, RTT: rtt}, nil
	}
	if routers[ttl-1] == "" {
		return hops.Hop{}, nil
	}
	return hops.Hop{Addr: a(routers[ttl-1]), RTT: rtt}, nil
}

func (f *fakeNet) Close() error { return nil }

func (f *fakeNet) callsTo(dst netip.Addr) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls[dst])
}

// recSink records everything.
type recSink struct {
	mu      sync.Mutex
	samples []model.Sample
	hops    []model.HopInfo
	routes  []model.Route
	changes []model.RouteChange
}

func (s *recSink) Observe(x model.Sample) {
	s.mu.Lock()
	s.samples = append(s.samples, x)
	s.mu.Unlock()
}
func (s *recSink) ObserveHop(x model.HopInfo) {
	s.mu.Lock()
	s.hops = append(s.hops, x)
	s.mu.Unlock()
}
func (s *recSink) ObserveRoute(x model.Route) {
	s.mu.Lock()
	s.routes = append(s.routes, x)
	s.mu.Unlock()
}
func (s *recSink) ObserveRouteChange(x model.RouteChange) {
	s.mu.Lock()
	s.changes = append(s.changes, x)
	s.mu.Unlock()
}

func (s *recSink) take() (smp []model.Sample, routes []model.Route, changes []model.RouteChange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	smp, routes, changes = s.samples, s.routes, s.changes
	s.samples, s.routes, s.changes = nil, nil, nil
	return
}

func (s *recSink) samplesOf(target string) []model.Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Sample
	for _, x := range s.samples {
		if x.Key.Target == target {
			out = append(out, x)
		}
	}
	return out
}

// roundTracer is a tracer set up for calling round directly.
func roundTracer(t *testing.T, f *fakeNet, o Options) (*tracer, *target, *recSink) {
	t.Helper()
	o.Log = quiet()
	o.open = func() (hops.Prober, error) { return f, nil }
	tr := newTracer(o)
	sink := &recSink{}
	tr.sink, tr.prober = sink, f
	tr.hops = newHopCache(tr.o, sink)
	st := &target{t: model.Target{Name: "x", Host: "1.1.1.1", Trace: true}, host: "1.1.1.1",
		wake: make(chan struct{}, 1), sessions: map[uint64]time.Time{}}
	return tr, st, sink
}

type hopWant struct {
	lost bool
	rtt  time.Duration
}

func checkRound(t *testing.T, name string, smp []model.Sample, slot time.Time, want []hopWant) {
	t.Helper()
	if len(smp) != len(want) {
		t.Fatalf("%s: %d samples, want %d: %+v", name, len(smp), len(want), smp)
	}
	for i, s := range smp {
		w := want[i]
		if s.Key != (model.SeriesKey{Target: "x", Kind: model.KindTrace, Hop: uint8(i + 1)}) || !s.Slot.Equal(slot) {
			t.Errorf("%s: sample %d key %+v slot %v", name, i, s.Key, s.Slot)
		}
		if s.Lost != w.lost || (!w.lost && s.RTT != w.rtt) || (s.Lost && s.Reason != model.ReasonTimeout) {
			t.Errorf("%s: hop %d = lost %v rtt %v reason %v, want lost %v rtt %v", name, i+1, s.Lost, s.RTT, s.Reason, w.lost, w.rtt)
		}
	}
}

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		if s != "" {
			out[i] = a(s)
		}
	}
	return out
}

func TestRoundRouteAndEarlyStop(t *testing.T) {
	f := newFakeNet("192.168.1.1", "62.1.1.1", "62.2.2.2")
	tr, st, sink := roundTracer(t, f, Options{})
	slot := time.Unix(1_800_000_000, 0).UTC()
	tr.round(context.Background(), st, slot)
	smp, routes, changes := sink.take()
	ms := time.Millisecond
	checkRound(t, "round", smp, slot, []hopWant{{rtt: ms}, {rtt: 2 * ms}, {rtt: 3 * ms}, {rtt: 4 * ms}})
	if len(routes) != 1 || !slices.Equal(routes[0].Hops, addrs("192.168.1.1", "62.1.1.1", "62.2.2.2", "1.1.1.1")) || routes[0].Target != "x" {
		t.Errorf("routes %+v", routes)
	}
	if len(changes) != 0 {
		t.Errorf("changes %+v", changes)
	}
	// TTLs past the destination are not sent once it answered: at most
	// Parallel-1 extra probes were in flight.
	calls := f.callsTo(a("1.1.1.1"))
	if slices.Max(calls) > 4+DefaultParallel-1 || len(calls) > 4+DefaultParallel-1 {
		t.Errorf("probed TTLs %v", calls)
	}
	if got := f.maxIn[a("1.1.1.1")]; got > DefaultParallel {
		t.Errorf("%d probes in flight, want <= %d", got, DefaultParallel)
	}
	if rs := tr.Routes(); len(rs) != 0 {
		t.Errorf("Routes without Run: %+v", rs) // targets are registered by Run
	}
}

func TestRoundLoss(t *testing.T) {
	f := newFakeNet("192.168.1.1", "62.1.1.1", "62.2.2.2")
	tr, st, sink := roundTracer(t, f, Options{MaxHops: 10})
	ctx := context.Background()
	slot := time.Unix(1_800_000_000, 0).UTC()
	ms := time.Millisecond
	tr.round(ctx, st, slot)
	sink.take()

	// A middle hop does not answer: loss on that hop only, route unchanged.
	f.set(func(f *fakeNet) { f.silent = map[int]bool{2: true} })
	tr.round(ctx, st, slot.Add(5*time.Second))
	smp, routes, changes := sink.take()
	checkRound(t, "hop 2 silent", smp, slot.Add(5*time.Second), []hopWant{{rtt: ms}, {lost: true}, {rtt: 3 * ms}, {rtt: 4 * ms}})
	if len(changes) != 0 || !slices.Equal(routes[0].Hops, addrs("192.168.1.1", "62.1.1.1", "62.2.2.2", "1.1.1.1")) {
		t.Errorf("hop 2 silent: route %+v changes %+v", routes, changes)
	}

	// Loss from hop 3 on, destination included: samples up to the known
	// destination TTL, none beyond (although TTLs up to MaxHops were sent).
	f.set(func(f *fakeNet) {
		f.silent = map[int]bool{3: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true, 10: true}
	})
	tr.round(ctx, st, slot.Add(10*time.Second))
	smp, _, changes = sink.take()
	checkRound(t, "loss from hop 3", smp, slot.Add(10*time.Second), []hopWant{{rtt: ms}, {rtt: 2 * ms}, {lost: true}, {lost: true}})
	if len(changes) != 0 {
		t.Errorf("loss from hop 3: changes %+v", changes)
	}
	if c := f.callsTo(a("1.1.1.1")); !slices.Contains(c, 10) {
		t.Errorf("destination silent: probed TTLs %v, want up to MaxHops", c)
	}

	// A local error (no route): every hop is lost with its reason.
	f.set(func(f *fakeNet) { f.silent = nil; f.err = errNetUnreach })
	tr.round(ctx, st, slot.Add(15*time.Second))
	smp, _, _ = sink.take()
	if len(smp) != 4 {
		t.Fatalf("local error: %d samples", len(smp))
	}
	for _, s := range smp {
		if !s.Lost || s.Reason != model.ReasonNoNetwork || s.Err == "" {
			t.Errorf("local error: %+v", s)
		}
	}
}

func TestRouteChangeNeedsTwoRounds(t *testing.T) {
	f := newFakeNet("192.168.1.1", "62.1.1.1", "62.2.2.2")
	tr, st, sink := roundTracer(t, f, Options{})
	ctx := context.Background()
	slot := time.Unix(1_800_000_000, 0).UTC()
	next := func() { slot = slot.Add(5 * time.Second); tr.round(ctx, st, slot) }
	next()
	since := st.route.Since

	// A one-round blip is not a change.
	f.set(func(f *fakeNet) { f.routers = []string{"192.168.1.1", "62.1.1.1", "77.7.7.7"} })
	next()
	f.set(func(f *fakeNet) { f.routers = []string{"192.168.1.1", "62.1.1.1", "62.2.2.2"} })
	next()
	if _, _, ch := sink.take(); len(ch) != 0 {
		t.Fatalf("blip: changes %+v", ch)
	}

	// A longer path via a new router, held for 2 rounds.
	f.set(func(f *fakeNet) { f.routers = []string{"192.168.1.1", "62.1.1.1", "77.7.7.7", "77.7.7.8"} })
	next()
	if _, _, ch := sink.take(); len(ch) != 0 {
		t.Fatalf("after 1 round: changes %+v", ch)
	}
	next()
	_, routes, ch := sink.take()
	if len(ch) != 1 {
		t.Fatalf("after 2 rounds: changes %+v", ch)
	}
	c := ch[0]
	if c.Target != "x" || c.FirstDiff != 3 ||
		!slices.Equal(c.From, addrs("192.168.1.1", "62.1.1.1", "62.2.2.2", "1.1.1.1")) ||
		!slices.Equal(c.To, addrs("192.168.1.1", "62.1.1.1", "77.7.7.7", "77.7.7.8", "1.1.1.1")) {
		t.Errorf("change %+v", c)
	}
	if r := routes[len(routes)-1]; !slices.Equal(r.Hops, c.To) || !r.Since.After(since) {
		t.Errorf("route after change %+v", r)
	}
	// Stable again: no further change.
	next()
	next()
	if _, _, ch := sink.take(); len(ch) != 0 {
		t.Errorf("stable: changes %+v", ch)
	}
	// Same length, one responding address differs: FirstDiff is that hop.
	f.set(func(f *fakeNet) { f.routers = []string{"192.168.1.1", "62.9.9.9", "77.7.7.7", "77.7.7.8"} })
	next()
	next()
	if _, _, ch := sink.take(); len(ch) != 1 || ch[0].FirstDiff != 2 {
		t.Errorf("hop 2 changed: %+v", ch)
	}
}

func TestSilentHopsAreNotRouteChanges(t *testing.T) {
	f := newFakeNet("192.168.1.1", "62.1.1.1", "62.2.2.2", "62.3.3.3")
	tr, st, sink := roundTracer(t, f, Options{})
	ctx := context.Background()
	slot := time.Unix(1_800_000_000, 0).UTC()
	patterns := []map[int]bool{nil, {2: true}, {2: true}, {3: true}, {2: true, 4: true}, {2: true, 4: true}, nil, {3: true}}
	for i, p := range patterns {
		f.set(func(f *fakeNet) { f.silent = p })
		tr.round(ctx, st, slot.Add(time.Duration(i)*5*time.Second))
	}
	_, routes, ch := sink.take()
	if len(ch) != 0 {
		t.Errorf("silent hops flapping: changes %+v", ch)
	}
	if r := routes[len(routes)-1]; !slices.Equal(r.Hops, addrs("192.168.1.1", "62.1.1.1", "62.2.2.2", "62.3.3.3", "1.1.1.1")) {
		t.Errorf("route %+v", r)
	}
}

func TestFirstDiff(t *testing.T) {
	for _, c := range []struct {
		a, b []string
		want int
	}{
		{[]string{"1.0.0.1", "1.0.0.2"}, []string{"1.0.0.1", "1.0.0.2"}, 0},
		{[]string{"1.0.0.1", ""}, []string{"", "1.0.0.2"}, 0},
		{[]string{"1.0.0.1", "1.0.0.2"}, []string{"1.0.0.1", "1.0.0.3"}, 2},
		{[]string{"1.0.0.1", "1.0.0.2"}, []string{"1.0.0.1", "1.0.0.2", "1.0.0.3"}, 3},
		{[]string{"1.0.0.1", "1.0.0.2", "9.9.9.9"}, []string{"1.0.0.1", "1.0.0.2", "", "9.9.9.9"}, 3},
		{[]string{"1.0.0.1", "", "9.9.9.9"}, []string{"1.0.0.1", "", "5.5.5.5", "9.9.9.9"}, 3},
	} {
		if got := firstDiff(addrs(c.a...), addrs(c.b...)); got != c.want {
			t.Errorf("firstDiff(%v, %v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestStickyDNS(t *testing.T) {
	f := newFakeNet("192.168.1.1")
	var n atomic.Int32
	answers := [][]netip.Addr{addrs("5.5.5.1", "5.5.5.2"), addrs("5.5.5.2", "5.5.5.1"), addrs("5.5.5.3")}
	tr, st, sink := roundTracer(t, f, Options{ResolveEvery: time.Nanosecond,
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			i := min(int(n.Add(1))-1, len(answers)-1)
			if i == 1 {
				return nil, errors.New("SERVFAIL")
			}
			return answers[i], nil
		}})
	st.host, st.t.Host = "svc.example", "svc.example"
	ctx := context.Background()
	slot := time.Unix(1_800_000_000, 0).UTC()
	for i := range 6 {
		tr.round(ctx, st, slot.Add(time.Duration(i)*5*time.Second))
		// Wait for the background re-resolution before the next round.
		for st.resolving.Load() {
			time.Sleep(time.Millisecond)
		}
		if i == 0 && st.dst != a("5.5.5.1") {
			t.Fatalf("first address %v", st.dst)
		}
	}
	if st.dst != a("5.5.5.3") {
		t.Errorf("dst %v, want the new address once the old one is gone", st.dst)
	}
	if _, _, ch := sink.take(); len(ch) != 0 {
		t.Errorf("a new address is not a route change: %+v", ch)
	}
}

// runTracer runs a tracer on a fake network until the test ends.
func runTracer(t *testing.T, f *fakeNet, o Options, p *model.Profile) (*tracer, *recSink) {
	t.Helper()
	o.Log = quiet()
	o.open = func() (hops.Prober, error) { return f, nil }
	if o.ReverseDNS == nil {
		o.ReverseDNS = func(context.Context, netip.Addr) string { return "" }
	}
	tr := newTracer(o)
	sink := &recSink{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tr.Run(ctx, p, sink) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	waitFor(t, "running", func() bool { tr.mu.Lock(); defer tr.mu.Unlock(); return tr.running })
	return tr, sink
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func labProfile() *model.Profile {
	return &model.Profile{Name: "t", Groups: []model.Group{{ID: "g"}}, Targets: []model.Target{
		{Name: "traced", Host: "1.1.1.1", Group: "g", Trace: true},
		{Name: "plain", Host: "2.2.2.2", Group: "g"},
		{Name: "traced2", Host: "3.3.3.3", Group: "g", Trace: true},
	}}
}

func TestRunTracesMarkedTargets(t *testing.T) {
	f := newFakeNet("192.168.1.1", "62.1.1.1")
	f.delay = 5 * time.Millisecond
	iv := 60 * time.Millisecond
	tr, sink := runTracer(t, f, Options{Interval: iv}, labProfile())
	waitFor(t, "3 rounds of both traced targets", func() bool {
		return len(sink.samplesOf("traced")) >= 9 && len(sink.samplesOf("traced2")) >= 9
	})
	if n := len(sink.samplesOf("plain")); n != 0 {
		t.Errorf("untraced target: %d samples", n)
	}
	for _, s := range sink.samplesOf("traced") {
		if s.Slot.UnixNano()%int64(iv) != 0 || s.Slot.Location() != time.UTC {
			t.Errorf("slot %v not aligned to %v", s.Slot, iv)
		}
	}
	// One round at a time per target: never more than Parallel probes.
	for _, d := range []string{"1.1.1.1", "3.3.3.3"} {
		f.mu.Lock()
		m := f.maxIn[a(d)]
		f.mu.Unlock()
		if m > DefaultParallel {
			t.Errorf("%s: %d probes in flight", d, m)
		}
	}
	// Slots are unique per series.
	seen := map[model.SeriesKey]map[time.Time]bool{}
	for _, s := range sink.samplesOf("traced") {
		if seen[s.Key] == nil {
			seen[s.Key] = map[time.Time]bool{}
		}
		if seen[s.Key][s.Slot] {
			t.Errorf("slot %v of %+v reported twice", s.Slot, s.Key)
		}
		seen[s.Key][s.Slot] = true
	}
	rs := tr.Routes()
	if len(rs) != 2 || rs[0].Target != "traced" || rs[1].Target != "traced2" ||
		!slices.Equal(rs[0].Hops, addrs("192.168.1.1", "62.1.1.1", "1.1.1.1")) {
		t.Errorf("Routes %+v", rs)
	}
	if got := tr.Interval("traced"); got != iv {
		t.Errorf("Interval %v", got)
	}
}

func TestHopInfo(t *testing.T) {
	f := newFakeNet("192.168.1.1", "62.1.1.1")
	block := make(chan struct{})
	var rdnsCalls atomic.Int32
	_, sink := runTracer(t, f, Options{Interval: 20 * time.Millisecond,
		ReverseDNS: func(ctx context.Context, ip netip.Addr) string {
			rdnsCalls.Add(1)
			if ip == a("62.1.1.1") {
				<-block // a slow resolver must not hold up rounds
				return "edge.isp.example"
			}
			return ""
		},
		Lookup: func(ip netip.Addr) asn.Info {
			if ip == a("62.1.1.1") {
				return asn.Info{ASN: 64500, Owner: "EXAMPLE-ISP"}
			}
			return asn.Info{}
		}}, &model.Profile{Name: "t", Groups: []model.Group{{ID: "g"}},
		Targets: []model.Target{{Name: "x", Host: "1.1.1.1", Group: "g", Trace: true}}})
	waitFor(t, "rounds while rDNS blocks", func() bool { return len(sink.samplesOf("x")) >= 3*6 })
	close(block)
	waitFor(t, "hop infos", func() bool { sink.mu.Lock(); defer sink.mu.Unlock(); return len(sink.hops) >= 3 })
	time.Sleep(100 * time.Millisecond) // more rounds: no more reports
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.hops) != 3 {
		t.Fatalf("hop infos %+v, want one per address", sink.hops)
	}
	by := map[netip.Addr]model.HopInfo{}
	for _, h := range sink.hops {
		by[h.IP] = h
	}
	e := by[a("62.1.1.1")]
	if e.RDNS != "edge.isp.example" || e.ASN != 64500 || e.Owner != "EXAMPLE-ISP" || e.FirstSeen.IsZero() || e.LastSeen.Before(e.FirstSeen) {
		t.Errorf("edge hop info %+v", e)
	}
	if _, ok := by[a("1.1.1.1")]; !ok {
		t.Errorf("destination hop info missing: %+v", sink.hops)
	}
	if n := rdnsCalls.Load(); n != 3 {
		t.Errorf("%d rDNS lookups, want 3 (cached)", n)
	}
}

func TestInvestigate(t *testing.T) {
	f := newFakeNet("192.168.1.1")
	iv, inv := 400*time.Millisecond, 20*time.Millisecond
	now := struct {
		sync.Mutex
		off time.Duration
	}{}
	clock := func() time.Time { now.Lock(); defer now.Unlock(); return time.Now().Add(now.off) }
	if _, err := New(Options{}).Investigate("plain", time.Minute); err == nil {
		t.Error("Investigate before Run: no error")
	}
	tr, sink := runTracer(t, f, Options{Interval: iv, Investigate: inv, Now: clock}, labProfile())

	if _, err := tr.Investigate("nope", time.Minute); err == nil {
		t.Error("unknown target: no error")
	}
	if _, err := tr.Investigate("plain", 0); err == nil {
		t.Error("ttl 0: no error")
	}

	// Two callers share one fast trace of an untraced target.
	stop1, err := tr.Investigate("plain", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	stop2, err := tr.Investigate("plain", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got := tr.Interval("plain"); got != inv {
		waitFor(t, "investigate interval", func() bool { return tr.Interval("plain") == inv })
	}
	waitFor(t, "fast rounds", func() bool { return len(sink.samplesOf("plain")) >= 2*5 })
	tr.mu.Lock()
	loops := 0
	if tr.targets["plain"].looping {
		loops++
	}
	tr.mu.Unlock()
	if loops != 1 {
		t.Fatal("no loop for the investigated target")
	}
	for _, s := range sink.samplesOf("plain") {
		if s.Slot.UnixNano()%int64(inv) != 0 {
			t.Errorf("investigate slot %v not aligned to %v", s.Slot, inv)
		}
	}
	stop1()
	stop1() // idempotent
	n := len(sink.samplesOf("plain"))
	waitFor(t, "rounds continue for the second caller", func() bool { return len(sink.samplesOf("plain")) >= n+4 })
	stop2()
	waitFor(t, "loop ends", func() bool { tr.mu.Lock(); defer tr.mu.Unlock(); return !tr.targets["plain"].looping })
	n = len(sink.samplesOf("plain"))
	time.Sleep(5 * inv)
	if m := len(sink.samplesOf("plain")); m != n {
		t.Errorf("%d samples after the last caller stopped", m-n)
	}
	if got := tr.Interval("plain"); got != iv {
		t.Errorf("Interval after investigate %v, want the default %v", got, iv)
	}

	// A traced target speeds up and slows down again; ttl expiry ends it.
	before := len(sink.samplesOf("traced"))
	if _, err := tr.Investigate("traced", time.Minute); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fast rounds of the traced target", func() bool { return len(sink.samplesOf("traced")) >= before+2*6 })
	now.Lock()
	now.off = 2 * time.Minute // past the ttl
	now.Unlock()
	waitFor(t, "back to the normal interval", func() bool { return tr.Interval("traced") == iv })
	tr.mu.Lock()
	looping := tr.targets["traced"].looping
	tr.mu.Unlock()
	if !looping {
		t.Error("always-on trace stopped after investigate")
	}
	// Slots stay unique across the interval switches.
	seen := map[model.SeriesKey]map[time.Time]bool{}
	for _, s := range append(sink.samplesOf("traced"), sink.samplesOf("plain")...) {
		if seen[s.Key] == nil {
			seen[s.Key] = map[time.Time]bool{}
		}
		if seen[s.Key][s.Slot] {
			t.Errorf("slot %v of %+v reported twice", s.Slot, s.Key)
		}
		seen[s.Key][s.Slot] = true
	}
}

func TestRunErrors(t *testing.T) {
	tr := newTracer(Options{Log: quiet(), open: func() (hops.Prober, error) { return nil, errors.New("no ICMP") }})
	if err := tr.Run(context.Background(), labProfile(), &recSink{}); err == nil {
		t.Error("Run without a prober: no error")
	}
}

var errNetUnreach = fmt.Errorf("sendto: %w", syscall.ENETUNREACH)
