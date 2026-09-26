package trace

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// manualClock is a clock the simulation sets.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *manualClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

// sim runs the rounds of several targets in time order on a fake network
// and a manual clock, as the loops would (without their goroutines, so
// deterministically).
type sim struct {
	t       *testing.T
	tr      *tracer
	f       *fakeNet
	sink    *recSink
	clock   *manualClock
	targets []*target
}

var simStart = time.Unix(1_800_000_000, 0).UTC() // a multiple of 5s and 2.5s

const (
	gwIP   = "192.168.1.1"
	isp1IP = "62.1.1.1"
	isp2IP = "62.2.2.2"
)

// newSim sets up n traced targets 1.1.1.1..1.1.1.n (phases spread like Run),
// all behind f's routers unless f.paths says otherwise.
func newSim(t *testing.T, f *fakeNet, o Options, n int) *sim {
	t.Helper()
	clock := &manualClock{t: simStart}
	f.now = clock.Now
	f.jitter = true
	o.Log, o.Now = quiet(), clock.Now
	o.open = nil
	o.ReverseDNS = func(context.Context, netip.Addr) string { return "" }
	tr := newTracer(o)
	sink := &recSink{}
	tr.sink, tr.prober = sink, f
	tr.hops = newHopCache(tr.o, sink)
	tr.targets = map[string]*target{}
	s := &sim{t: t, tr: tr, f: f, sink: sink, clock: clock}
	for i := range n {
		host := fmt.Sprintf("1.1.1.%d", i+1)
		st := &target{t: model.Target{Name: fmt.Sprintf("t%d", i), Host: host, Trace: true}, host: host,
			idx: i, phase: float64(i) / float64(n), wake: make(chan struct{}, 1), sessions: map[uint64]time.Time{}}
		tr.targets[st.t.Name] = st
		s.targets = append(s.targets, st)
	}
	return s
}

// investigate switches st to the Investigate interval.
func (s *sim) investigate(st *target) {
	s.tr.mu.Lock()
	st.curIv = s.tr.o.Investigate
	s.tr.mu.Unlock()
}

func (s *sim) iv(st *target) time.Duration {
	s.tr.mu.Lock()
	defer s.tr.mu.Unlock()
	if st.curIv > 0 {
		return st.curIv
	}
	return s.tr.o.Interval
}

// run runs every round with a slot in [from, to), in firing order.
func (s *sim) run(from, to time.Time) {
	type ev struct {
		at, slot time.Time
		st       *target
	}
	var evs []ev
	for _, st := range s.targets {
		iv := s.iv(st)
		for slot := from; slot.Before(to); slot = slot.Add(iv) {
			evs = append(evs, ev{slot.Add(time.Duration(st.phase * float64(iv))), slot, st})
		}
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].at.Before(evs[j].at) })
	for _, e := range evs {
		s.clock.set(e.at)
		s.tr.round(context.Background(), e.st, e.slot)
	}
}

// probesAt returns the times of the probes router answered, from from on.
func (s *sim) probesAt(router string, from time.Time) []time.Time {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	var out []time.Time
	for _, r := range s.f.log {
		if r.addr == a(router) && !r.at.Before(from) {
			out = append(out, r.at)
		}
	}
	return out
}

// probesIn counts the probes in [from, to).
func (s *sim) probesIn(from, to time.Time) int {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	n := 0
	for _, r := range s.f.log {
		if !r.at.Before(from) && r.at.Before(to) {
			n++
		}
	}
	return n
}

// checkCap fails if router got more probes than the cap allows (rate 1/s,
// burst 2) in any window.
func checkCap(t *testing.T, router string, ts []time.Time) {
	t.Helper()
	for i := range ts {
		for j := i; j < len(ts); j++ {
			if w := ts[j].Sub(ts[i]); float64(j-i+1) > burst+w.Seconds()*DefaultRouterRate+1e-9 {
				t.Fatalf("%s: %d probes in %v (%v .. %v), over the cap", router, j-i+1, w, ts[i], ts[j])
			}
		}
	}
}

type slotHop struct {
	slot time.Time
	hop  uint8
}

// samplesBy returns target's samples by (slot, hop), failing on duplicates
// and on any loss (the fake network loses nothing: loss would be the cap
// reported as loss).
func (s *sim) samplesBy(target string) map[slotHop]model.Sample {
	s.t.Helper()
	out := map[slotHop]model.Sample{}
	for _, x := range s.sink.samplesOf(target) {
		k := slotHop{x.Slot, x.Key.Hop}
		if _, dup := out[k]; dup {
			s.t.Errorf("%s: slot %v hop %d reported twice", target, x.Slot, x.Key.Hop)
		}
		if x.Lost {
			s.t.Errorf("%s: slot %v hop %d lost (%v): nothing was lost", target, x.Slot, x.Key.Hop, x.Reason)
		}
		out[k] = x
	}
	return out
}

func (s *sim) route(st *target) []netip.Addr {
	st.rmu.Lock()
	defer st.rmu.Unlock()
	return slices.Clone(st.route.Hops)
}

// Seven traces share three hops: each shared hop is probed exactly once per
// round (and within the per-router cap), every trace gets a sample of it per
// round, the same measurement for all.
func TestSharedPrefix(t *testing.T) {
	f := newFakeNet(gwIP, isp1IP, isp2IP)
	s := newSim(t, f, Options{Revalidate: 1000}, 7)
	const rounds = 12
	iv := DefaultInterval
	s.run(simStart, simStart.Add(rounds*iv))

	warm := simStart.Add(2 * iv) // first rounds: learning the routes
	for _, r := range []string{gwIP, isp1IP, isp2IP} {
		ts := s.probesAt(r, simStart)
		checkCap(t, r, ts)
		for w := warm; w.Before(simStart.Add(rounds * iv)); w = w.Add(iv) {
			n := 0
			for _, x := range ts {
				if !x.Before(w) && x.Before(w.Add(iv)) {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%s probed %d times in the round at %v, want exactly 1", r, n, w.Sub(simStart))
			}
		}
	}
	by := map[string]map[slotHop]model.Sample{}
	for _, st := range s.targets {
		by[st.t.Name] = s.samplesBy(st.t.Name)
		want := addrs(gwIP, isp1IP, isp2IP, st.host)
		if r := s.route(st); !slices.Equal(r, want) {
			t.Errorf("%s: route %v, want %v", st.t.Name, r, want)
		}
	}
	for slot := warm; slot.Before(simStart.Add(rounds * iv)); slot = slot.Add(iv) {
		for hop := uint8(1); hop <= 4; hop++ {
			var rtts []time.Duration
			for _, st := range s.targets {
				x, ok := by[st.t.Name][slotHop{slot, hop}]
				if !ok {
					t.Errorf("%s: no sample for hop %d at %v", st.t.Name, hop, slot.Sub(simStart))
					continue
				}
				rtts = append(rtts, x.RTT)
			}
			slices.Sort(rtts)
			distinct := len(slices.Compact(rtts))
			if hop <= 3 && distinct != 1 {
				t.Errorf("hop %d at %v: RTTs %v, want one shared measurement", hop, slot.Sub(simStart), rtts)
			}
			if hop == 4 && distinct != len(s.targets) {
				t.Errorf("destinations at %v: RTTs %v, want one measurement per trace", slot.Sub(simStart), rtts)
			}
		}
	}
	if _, _, ch := s.sink.take(); len(ch) != 0 {
		t.Errorf("route changes %+v", ch)
	}
}

// Probe load of 7 traces sharing 3 hops, before (every trace probes its
// whole path, no cap) and after (shared hops, cap, re-validation).
func TestProbeLoad(t *testing.T) {
	iv := DefaultInterval
	const rounds = 40
	load := func(o Options) (gwPerSec float64, perRound float64) {
		f := newFakeNet(gwIP, isp1IP, isp2IP)
		s := newSim(t, f, o, 7)
		s.run(simStart, simStart.Add(rounds*iv))
		from, to := simStart.Add(4*iv), simStart.Add(rounds*iv)
		gw := len(s.probesAt(gwIP, from))
		return float64(gw) / to.Sub(from).Seconds(), float64(s.probesIn(from, to)) / float64(rounds-4)
	}
	gwBefore, before := load(Options{Revalidate: 1, RouterRate: -1})
	gwAfter, after := load(Options{})
	t.Logf("7 traces, 3 shared hops, destination at hop 4, every %v:", iv)
	t.Logf("  before: gateway %.2f probes/s, %.1f probes per round", gwBefore, before)
	t.Logf("  after:  gateway %.2f probes/s, %.1f probes per round", gwAfter, after)
	if gwBefore < 1.3 || gwAfter > 0.5 || after >= before*0.75 {
		t.Errorf("load before %.2f/s %.1f/round, after %.2f/s %.1f/round", gwBefore, before, gwAfter, after)
	}
}

// One trace leaves the shared path at hop 2: it still shares the gateway,
// probes its own hops 2 and 3, and the others still share theirs.
func TestSharedPrefixDivergence(t *testing.T) {
	f := newFakeNet(gwIP, isp1IP, isp2IP)
	f.paths = map[netip.Addr][]string{a("1.1.1.2"): {gwIP, "62.9.9.1", "62.9.9.2"}}
	s := newSim(t, f, Options{Revalidate: 1000}, 7)
	const rounds = 10
	iv := DefaultInterval
	s.run(simStart, simStart.Add(rounds*iv))
	warm := simStart.Add(2 * iv)
	for _, r := range []string{gwIP, isp1IP, isp2IP, "62.9.9.1", "62.9.9.2"} {
		ts := s.probesAt(r, warm)
		checkCap(t, r, ts)
		if len(ts) != rounds-2 {
			t.Errorf("%s probed %d times in %d rounds, want once per round", r, len(ts), rounds-2)
		}
	}
	for _, st := range s.targets {
		want := addrs(gwIP, isp1IP, isp2IP, st.host)
		if st.host == "1.1.1.2" {
			want = addrs(gwIP, "62.9.9.1", "62.9.9.2", st.host)
		}
		if r := s.route(st); !slices.Equal(r, want) {
			t.Errorf("%s: route %v, want %v", st.t.Name, r, want)
		}
	}
	div, other := s.samplesBy("t1"), s.samplesBy("t0")
	for slot := warm; slot.Before(simStart.Add(rounds * iv)); slot = slot.Add(iv) {
		for hop := uint8(1); hop <= 4; hop++ {
			d, ok1 := div[slotHop{slot, hop}]
			o, ok2 := other[slotHop{slot, hop}]
			if !ok1 || !ok2 {
				t.Fatalf("hop %d at %v: samples missing", hop, slot.Sub(simStart))
			}
			if shared := d.RTT == o.RTT; shared != (hop == 1) {
				t.Errorf("hop %d at %v: diverging trace RTT %v, other %v: want shared only at hop 1", hop, slot.Sub(simStart), d.RTT, o.RTT)
			}
		}
	}
	if _, _, ch := s.sink.take(); len(ch) != 0 {
		t.Errorf("route changes %+v", ch)
	}
}

// A shared hop changes for every trace: each trace reports the change once,
// from its own probes, and the cap holds throughout.
func TestSharedHopRouteChange(t *testing.T) {
	f := newFakeNet(gwIP, isp1IP, isp2IP)
	s := newSim(t, f, Options{}, 7)
	iv := DefaultInterval
	s.run(simStart, simStart.Add(4*iv))
	s.sink.take()
	f.set(func(f *fakeNet) { f.routers = []string{gwIP, "62.5.5.5", isp2IP} })
	changed := simStart.Add(4 * iv)
	s.run(changed, changed.Add(16*iv))
	_, _, ch := s.sink.take()
	per := map[string]int{}
	for _, c := range ch {
		per[c.Target]++
		want := addrs(gwIP, "62.5.5.5", isp2IP, s.tr.targets[c.Target].host)
		if c.FirstDiff != 2 || !slices.Equal(c.To, want) || !slices.Equal(c.From, addrs(gwIP, isp1IP, isp2IP, s.tr.targets[c.Target].host)) {
			t.Errorf("change %+v", c)
		}
		if d := c.At.Sub(changed); d > 8*iv {
			t.Errorf("%s: change reported %v after it happened", c.Target, d)
		}
	}
	for _, st := range s.targets {
		if per[st.t.Name] != 1 {
			t.Errorf("%s: %d route changes, want 1", st.t.Name, per[st.t.Name])
		}
		s.samplesBy(st.t.Name)
	}
	for _, r := range []string{gwIP, isp1IP, "62.5.5.5", isp2IP} {
		checkCap(t, r, s.probesAt(r, simStart))
	}
	// Settled: the new hop is shared again, once per round.
	last := changed.Add(16 * iv)
	s.run(last, last.Add(4*iv))
	if n := len(s.probesAt("62.5.5.5", last)); n != 4 {
		t.Errorf("new shared hop probed %d times in 4 rounds, want 4", n)
	}
	if _, _, ch := s.sink.take(); len(ch) != 0 {
		t.Errorf("changes after settling: %+v", ch)
	}
}

// Investigate (every 2.5s) on top of 7 always-on traces: the gateway stays
// within the cap, with or without shared hops; hops over the cap are not
// measured (no sample), never reported lost.
func TestInvestigateCapped(t *testing.T) {
	for _, c := range []struct {
		name string
		o    Options
	}{
		{"shared", Options{}},
		{"no sharing", Options{Revalidate: 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeNet(gwIP, isp1IP, isp2IP)
			s := newSim(t, f, c.o, 10)
			for _, st := range s.targets[7:] {
				s.investigate(st)
			}
			d := 60 * time.Second
			s.run(simStart, simStart.Add(d))
			ts := s.probesAt(gwIP, simStart)
			checkCap(t, gwIP, ts)
			t.Logf("gateway: %d probes in %v (demand without cap/sharing: %.0f)", len(ts), d,
				d.Seconds()*(7/DefaultInterval.Seconds()+3/DefaultInvestigate.Seconds()))
			for _, st := range s.targets {
				by := s.samplesBy(st.t.Name)
				n := 0
				for k := range by {
					if k.hop == 1 {
						n++
					}
				}
				slots := int(d / s.iv(st))
				t.Logf("%s every %v: hop 1 measured in %d of %d slots", st.t.Name, s.iv(st), n, slots)
				if n < slots/3 {
					t.Errorf("%s: hop 1 measured in %d of %d slots", st.t.Name, n, slots)
				}
			}
		})
	}
}

// Every Revalidate-th round each trace probes its whole path itself, and
// finds a path that left the shared prefix for its destination only.
func TestRevalidation(t *testing.T) {
	f := newFakeNet(gwIP, isp1IP, isp2IP)
	s := newSim(t, f, Options{}, 7)
	iv := DefaultInterval
	s.run(simStart, simStart.Add(3*iv))
	s.sink.take()
	// t3's path now differs at hop 2; the traces sharing hop 2 with it do
	// not see that.
	f.set(func(f *fakeNet) { f.paths = map[netip.Addr][]string{a("1.1.1.4"): {gwIP, "62.7.7.7", isp2IP}} })
	from := simStart.Add(3 * iv)
	s.run(from, from.Add(time.Duration(DefaultRevalidate+3)*iv))
	_, _, ch := s.sink.take()
	if len(ch) != 1 || ch[0].Target != "t3" || ch[0].FirstDiff != 2 {
		t.Fatalf("changes %+v, want t3's at hop 2", ch)
	}
	for _, st := range s.targets {
		f.mu.Lock()
		n := 0
		for _, r := range f.log {
			if r.dst == a(st.host) && r.ttl == 2 && !r.at.Before(from) {
				n++
			}
		}
		f.mu.Unlock()
		if n == 0 {
			t.Errorf("%s never probed hop 2 itself in %d rounds", st.t.Name, DefaultRevalidate+3)
		}
	}
}
