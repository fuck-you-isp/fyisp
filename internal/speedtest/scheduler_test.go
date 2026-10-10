package speedtest

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// testCatalog: a metro of three providers with a nearby city, a far
// region whose servers include a dead one, a single-port server, and an
// opt-in region.
func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := LoadCatalog(fstest.MapFS{
		"p.yml": {Data: []byte(`provider: p
display: Prov
sources: [https://example.com]
servers:
  - {id: Near, host: near.example, ports: 5201-5210, city: Here, metro: here, country: GB, geo: eu, capacity_gbps: 1, verified: {date: 2026-10-10}}
  - {id: Far, host: far.example, ports: 5200-5209, city: Away, metro: away, country: JP, geo: as, verified: {date: 2026-10-10}}
`)},
		"q.yml": {Data: []byte(`provider: q
display: Other
sources: [https://example.com]
servers:
  - {id: Mid, host: mid.example, ports: 9204-9240, city: Here, metro: here, country: GB, geo: eu, verified: {date: 2026-10-10}}
  - {id: Single, host: single.example, ports: "5201", city: Next, metro: next, country: GB, geo: eu, verified: {date: 2026-10-10}}
  - {id: Dead, host: dead.example, ports: 5200-5201, city: Away, metro: away, country: JP, geo: as, verified: {date: 2026-10-10}}
`)},
		"r.yml": {Data: []byte(`provider: r
display: Third
sources: [https://example.com]
servers:
  - {id: Third, host: third.example, ports: 5201-5202, city: Here, metro: here, country: GB, geo: eu, verified: {date: 2026-10-10}}
`)},
		"regions.yml": {Data: []byte(`regions:
  - {id: home, title: Home, servers: [Near, Mid, Third], nearby: [Single]}
  - {id: away, title: Away, servers: [Dead, Far]}
  - {id: us-east, title: US East, servers: [Far], default: false}
`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// harness wires a Scheduler to stubs: RTTs per host, a scripted run.
type harness struct {
	s     *Scheduler
	st    *MemStore
	now   time.Time
	mu    sync.Mutex
	rtt   map[string]time.Duration
	calls []Config
	slept []time.Duration
	run   func(c Config, n int) (Result, error)
	step  time.Duration // each run advances the clock this much
}

func newHarness(t *testing.T, o Options) *harness {
	h := &harness{st: &MemStore{}, now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC), step: 6 * time.Second,
		rtt: map[string]time.Duration{"far.example": 90 * time.Millisecond, "near.example": 10 * time.Millisecond,
			"mid.example": 12 * time.Millisecond, "third.example": 40 * time.Millisecond, "single.example": 11 * time.Millisecond}}
	h.run = func(c Config, n int) (Result, error) {
		return Result{Direction: c.Direction, Bytes: 125_000_000, Elapsed: 9 * time.Second, Retransmits: 3, TotalBytes: 130_000_000}, nil
	}
	if o.Catalog == nil {
		o.Catalog = testCatalog(t)
	}
	o.Store = h.st
	o.Now = h.clock
	o.rand = rand.New(rand.NewPCG(1, 2))
	o.rtt = func(_ context.Context, host string, port int) (time.Duration, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if d, ok := h.rtt[host]; ok {
			return d, nil
		}
		return 0, errors.New("no route")
	}
	o.sleep = func(_ context.Context, d time.Duration) bool {
		h.mu.Lock()
		h.slept = append(h.slept, d)
		h.now = h.now.Add(d)
		h.mu.Unlock()
		return true
	}
	o.run = func(_ context.Context, c Config) (Result, error) {
		h.mu.Lock()
		h.calls = append(h.calls, c)
		n := len(h.calls)
		h.now = h.now.Add(h.step)
		run := h.run
		h.mu.Unlock()
		return run(c, n)
	}
	h.s = New(o)
	return h
}

func (h *harness) clock() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now }

func (h *harness) setRun(f func(c Config, n int) (Result, error)) {
	h.mu.Lock()
	h.run = f
	h.mu.Unlock()
}

// entry queues and runs one entry synchronously; it returns the tests it
// saved.
func (h *harness) entry(t *testing.T, e *Entry) []model.SpeedTest {
	t.Helper()
	n := len(h.st.tests)
	if _, _, err := h.s.q.add(e, h.clock()); err != nil {
		t.Fatal(err)
	}
	p := h.s.q.pop(h.clock())
	if p == nil {
		t.Fatal("nothing to run")
	}
	h.s.runEntry(context.Background(), p)
	return h.st.tests[n:]
}

// slot runs the scheduled slot of a region or server.
func (h *harness) slot(t *testing.T, tg target) model.SpeedTest {
	t.Helper()
	if tg.region == "" && tg.server == "" {
		tg.nearby = 0
	} else {
		tg.nearby = -1
	}
	ts := h.entry(t, &Entry{Kind: KindScheduled, Who: WhoScheduled, Target: tg.row(), slot: &tg})
	if len(ts) != 1 {
		t.Fatalf("%d tests", len(ts))
	}
	return ts[0]
}

func hostPort(c Config) (string, int) {
	h, p, _ := net.SplitHostPort(c.Addr)
	n, _ := strconv.Atoi(p)
	return h, n
}

// TestRotation: every hour of the day sees every provider over N days, for
// N 1..5, whatever the seed; one hour on consecutive days rotates even
// when the local day crosses UTC midnight.
func TestRotation(t *testing.T) {
	for n := 1; n <= 5; n++ {
		for _, seed := range []uint32{0, 7, 1 << 31} {
			for h := int64(0); h < 24; h++ {
				seen := map[int]bool{}
				for d := int64(100); d < 100+int64(n); d++ {
					seen[Rotation(d, h, seed, n)] = true
				}
				if len(seen) != n {
					t.Fatalf("n %d seed %d hour %d: %d providers in %d days", n, seed, h, len(seen), n)
				}
			}
		}
	}
	c := testCatalog(t)
	r := c.Regions[0]
	firsts := map[string]bool{}
	for d := range 3 {
		at := time.Date(2026, 10, 10+d, 23, 30, 0, 0, time.UTC) // 00:30 the next day in UTC+1
		firsts[r.Chain(c, at, 5)[0].Server.ID] = true
	}
	if len(firsts) != 3 {
		t.Errorf("one hour on 3 days: %v", firsts)
	}
}

// TestChain: the rotated provider first, the rest of the metro, then the
// nearby city, with fallback levels.
func TestChain(t *testing.T) {
	c := testCatalog(t)
	home := c.Regions[0]
	at := time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC)
	var ids, levels []string
	for _, s := range home.Chain(c, at, 0) {
		ids, levels = append(ids, s.Server.ID), append(levels, s.Level)
	}
	idx := Rotation(at.Unix()/86400, 20, 0, 3)
	want := []string{"Near", "Mid", "Third"}
	want = append(append(want[idx:], want[:idx]...), "Single")
	if strings.Join(ids, " ") != strings.Join(want, " ") || strings.Join(levels, " ") != "none same_metro same_metro nearby_city" {
		t.Errorf("%v %v", ids, levels)
	}
	if s, _ := c.Server("Single"); !s.SinglePort() {
		t.Error("single port")
	}
	if s, _ := c.Server("Near"); s.CapacityMbps != 1000 {
		t.Errorf("capacity %v", s.CapacityMbps)
	}
}

// TestSchedule: regions hourly, on the install's phase, spaced evenly;
// jitter only on cycle starts; opt-in regions join.
func TestSchedule(t *testing.T) {
	h := newHarness(t, Options{})
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = ModeRegions
	if iv := s.IntervalFor(ModeRegions); iv != time.Hour {
		t.Fatalf("interval %v", iv)
	}
	if iv := s.IntervalFor(ModeNearby); iv != 15*time.Minute {
		t.Fatalf("nearby interval %v", iv)
	}
	now := h.now
	s.restartLocked(now)
	if s.next.Before(now.Add(firstDelay)) || s.next.After(now.Add(time.Hour+firstDelay)) {
		t.Fatalf("first slot %v after start", s.next.Sub(now))
	}
	if off := s.cycleBase.Sub(s.cycleBase.Truncate(time.Hour)); off != s.phase(time.Hour) {
		t.Errorf("cycle base %v past the hour, phase %v", off, s.phase(time.Hour))
	}
	sp := s.spacingLocked(ModeRegions)
	if sp != 30*time.Minute { // 2 default regions
		t.Fatalf("spacing %v", sp)
	}
	n := len(s.planLocked(ModeRegions))
	for c := range 50 {
		for s.slot != n-1 {
			prev := s.next
			s.advanceLocked()
			if s.next.Sub(prev) != sp {
				t.Fatalf("cycle %d: spacing %v", c, s.next.Sub(prev))
			}
		}
		last := s.next
		base := s.cycleBase.Add(time.Hour)
		s.advanceLocked()
		if off := s.cycleStart.Sub(base); off < -Jitter || off > Jitter || s.next.Sub(last) < min(cycleGap, sp/2) {
			t.Fatalf("cycle %d: start %v from base", c, off)
		}
	}
	if load := s.planLoadLocked(ModeRegions); load.TestsPerHour != 2 || load.Share != 2*15.0/3600 {
		t.Errorf("load %+v", load)
	}
	s.extra = []string{"us-east"}
	if n := len(s.planLocked(ModeRegions)); n != 3 {
		t.Errorf("with the opt-in region: %d slots", n)
	}
	other := New(Options{Catalog: s.o.Catalog, rand: rand.New(rand.NewPCG(9, 9))})
	if other.seed == s.seed {
		t.Error("two installs, one seed")
	}
}

// TestRegionFallback: a busy first server gets 2 more ports, then the next
// server; unreachable moves on at once; a single-port server gets one try.
func TestRegionFallback(t *testing.T) {
	h := newHarness(t, Options{})
	c := h.s.o.Catalog
	first := c.Regions[0].Chain(c, h.clock(), h.s.seed)
	h.setRun(func(cf Config, n int) (Result, error) {
		if host, _ := hostPort(cf); host == first[0].Server.Host {
			return Result{}, ErrBusy
		}
		return Result{Bytes: 1e6, Elapsed: time.Second, TotalBytes: 2e6}, nil
	})
	r := h.slot(t, target{region: "home"})
	if r.Outcome != model.SpeedOK || r.Region != "home" || r.Server != first[1].Server.ID || r.FallbackLevel != FallbackSameMetro || !r.Fallback || r.Bytes != 4e6 {
		t.Fatalf("%+v", r)
	}
	if busyTries := r.Attempts - 2; busyTries != MaxAttempts && !(first[0].Server.SinglePort() && busyTries == 1) {
		t.Errorf("attempts %d", r.Attempts)
	}
	// Unreachable (a timeout): the next server at once.
	h.calls = nil
	h.setRun(func(cf Config, n int) (Result, error) {
		if host, _ := hostPort(cf); host == "dead.example" {
			return Result{}, fmt.Errorf("%w: i/o timeout", ErrUnreachable)
		}
		return Result{Bytes: 1e6, Elapsed: time.Second}, nil
	})
	ch := c.Regions[1].Chain(c, h.clock(), h.s.seed)
	r = h.slot(t, target{region: "away"})
	if ch[0].Server.ID == "Dead" && (r.Server != "Far" || len(h.calls) != 3) {
		t.Errorf("unreachable first: %+v, %d calls", r, len(h.calls))
	}
	// A busy single-port server: one try.
	h.calls = nil
	h.setRun(func(Config, int) (Result, error) { return Result{}, ErrBusy })
	if r = h.slot(t, target{server: "Single"}); len(h.calls) != 1 || r.Outcome != model.SpeedBusy {
		t.Errorf("single port: %d calls, %+v", len(h.calls), r)
	}
}

// TestRefused: a refused connection is retried on another port (a stock
// server re-listening), not "unreachable"; refused on every try is.
func TestRefused(t *testing.T) {
	h := newHarness(t, Options{})
	h.setRun(func(c Config, n int) (Result, error) {
		if n == 1 {
			return Result{}, fmt.Errorf("%w: connection refused", ErrRefused)
		}
		return Result{Bytes: 1e6, Elapsed: time.Second}, nil
	})
	if r := h.slot(t, target{server: "Mid"}); r.Outcome != model.SpeedOK || r.Attempts != 3 {
		t.Errorf("refused once: %+v", r)
	}
	h.calls, h.slept = nil, nil
	h.setRun(func(c Config, n int) (Result, error) {
		if c.Direction == Upload && n == 2 {
			return Result{}, fmt.Errorf("%w: connection refused", ErrRefused)
		}
		return Result{Bytes: 1e6, Elapsed: time.Second}, nil
	})
	if r := h.slot(t, target{server: "Mid"}); r.Outcome != model.SpeedOK || r.UpBps == nil || h.calls[1].Addr != h.calls[0].Addr || h.calls[2].Addr == h.calls[1].Addr || h.slept[0] != relisten {
		t.Errorf("upload refused once: %+v, calls %v, slept %v", r, h.calls, h.slept)
	}
	h.setRun(func(Config, int) (Result, error) { return Result{}, fmt.Errorf("%w: connection refused", ErrRefused) })
	if r := h.slot(t, target{server: "Mid"}); r.Outcome != model.SpeedUnreachable {
		t.Errorf("always refused: %+v", r)
	}
}

// TestSlotBudget: no new attempt or fallback once the slot has used its
// budget; a measured download is kept when the upload fails; every
// attempt is adaptive and byte-capped.
func TestSlotBudget(t *testing.T) {
	h := newHarness(t, Options{})
	h.step = 9 * time.Second
	h.setRun(func(Config, int) (Result, error) { return Result{}, ErrBusy })
	r := h.slot(t, target{region: "home"})
	if el := r.End.Sub(r.Start); el > SlotBudget+h.step || len(h.calls) > 3 {
		t.Errorf("slot took %v with %d calls", el, len(h.calls))
	}
	h.calls, h.step = nil, 6*time.Second
	h.setRun(func(c Config, n int) (Result, error) {
		if c.Direction == Upload {
			return Result{}, errors.New("protocol error")
		}
		return Result{Bytes: 1e6, Elapsed: time.Second, CappedByBytes: true, Ramp: 300 * time.Millisecond}, nil
	})
	r = h.slot(t, target{region: "home"})
	if r.Outcome != model.SpeedFailed || r.DownBps == nil || r.Fallback || len(h.calls) != 2 || !r.DownShort || r.DownSecs != 1 {
		t.Errorf("upload failed: %+v, %d calls", r, len(h.calls))
	}
	if x := ParseExtra(r.Extra); x.RampMs[0] != 300 {
		t.Errorf("extra %q", r.Extra)
	}
	if c := h.calls[0]; c.Duration != MaxMeasure || c.MinDuration != MinMeasure || c.Timeout != DirTimeout || c.MaxBytes != MaxBytes {
		t.Errorf("config %+v", c)
	}
}

// TestNearby: the nearest server, then the closest of other providers
// (the nearest's metro preferred), 3 providers in all; the far region by
// continent.
func TestNearby(t *testing.T) {
	h := newHarness(t, Options{})
	h.s.pick(context.Background())
	h.s.mu.Lock()
	picked, far := h.s.picked, h.s.far
	h.s.mode = ModeNearby
	h.s.mu.Unlock()
	// Near (10 ms) first. Other's nearest is Single (11 ms, another metro),
	// but Mid (12 ms) is in Near's metro: preferred. Then Third.
	if strings.Join(picked, " ") != "Near Mid Third" || far != "us-east" {
		t.Errorf("picked %v, far %s", picked, far)
	}
	st := h.s.State()
	if !st.Servers[0].Picked || st.Servers[2].RTTms == nil || len(st.Rows) != 4 || st.Rows[0].Why != "nearest Prov" || st.Rows[3].Why != "far: US East" {
		t.Errorf("state %+v %+v", st.Servers[:2], st.Rows)
	}
	if r := h.slot(t, target{}); r.Server != "Near" || r.Region != "" {
		t.Errorf("nearby slot %+v", r)
	}
	if r := h.slot(t, target{region: "far"}); r.Region != "us-east" || r.Server != "Far" {
		t.Errorf("far slot %+v", r)
	}
}

// TestScore: results relative to the server's baseline once it has 20
// first-choice scheduled tests; capped_by_server near the link capacity.
func TestScore(t *testing.T) {
	h := newHarness(t, Options{})
	f := func(v float64) *float64 { return &v }
	for i := range 25 {
		at := h.now.Add(-time.Duration(i+1) * time.Hour)
		h.st.tests = append(h.st.tests, model.SpeedTest{Start: at, Trigger: "schedule", Outcome: model.SpeedOK, Server: "Mid",
			FallbackLevel: FallbackNone, DownBps: f(float64(100+i) * 1e6), UpBps: f(20e6)})
	}
	if d, u, ok := Baseline(h.st.tests, "Mid", h.now); !ok || d < 120e6 || u != 20e6 {
		t.Fatalf("baseline %v %v %v", d, u, ok)
	}
	if _, _, ok := Baseline(h.st.tests[:10], "Mid", h.now); ok {
		t.Error("a baseline from 10 tests")
	}
	h.setRun(func(c Config, n int) (Result, error) {
		if c.Direction == Download {
			return Result{Bytes: 75e6 / 8 * 9, Elapsed: 9 * time.Second}, nil
		}
		return Result{Bytes: 20e6 / 8, Elapsed: time.Second}, nil
	})
	r := h.slot(t, target{server: "Mid"})
	if r.ScoreDown == nil || *r.ScoreDown < 0.5 || *r.ScoreDown > 0.7 || r.ScoreUp == nil || *r.ScoreUp != 1 {
		t.Errorf("score %v %v", r.ScoreDown, r.ScoreUp)
	}
	// Near's link is 1 Gbit/s: 950 Mbit/s measured the server.
	h.setRun(func(Config, int) (Result, error) { return Result{Bytes: 950e6 / 8, Elapsed: time.Second}, nil })
	if r := h.slot(t, target{server: "Near"}); !r.CappedByServer {
		t.Errorf("not capped: %+v", r)
	}
}

func TestBuiltinCatalog(t *testing.T) {
	c, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Servers) != 45 {
		t.Fatalf("%d servers", len(c.Servers))
	}
	var defaults []string
	for _, r := range c.Regions {
		if r.Default {
			defaults = append(defaults, r.ID)
		}
	}
	if strings.Join(defaults, " ") != "uk eu-west eu-central us-east us-central us-west asia-east asia-southeast oceania" {
		t.Errorf("default regions %v", defaults)
	}
	labels := map[string]string{}
	for _, s := range c.Servers {
		if prev, ok := labels[s.Label]; ok {
			t.Errorf("label %q of %s and %s", s.Label, prev, s.ID)
		}
		labels[s.Label] = s.ID
	}
	us := c.Regions[3]
	if us.ID != "us-east" || strings.Join(us.Servers, " ") != "Leaseweb-NewYork InterServer-Secaucus Clouvider-NewYork" || len(us.Providers(c)) != 3 {
		t.Errorf("%+v", us)
	}
	if s, _ := c.Server("InterServer-Secaucus"); s.Label != "InterServer Secaucus NJ" || s.Metro != "new-york" {
		t.Errorf("%+v", s)
	}
	if s, _ := c.Server("HE-Fremont"); !s.SinglePort() || s.Host != "iperf.he.net" {
		t.Errorf("%+v", s)
	}
	if s, _ := c.Server("OVH-Roubaix"); s.CapacityMbps != 20000 {
		t.Errorf("%+v", s)
	}
	if s, _ := c.Server("OVH-London"); s.Label != "OVH Erith" {
		t.Errorf("%+v", s)
	}
	cs, err := Custom("Leaseweb-Tokyo, 192.168.1.20:5201", c)
	if err != nil || len(cs) != 2 || cs[0].Host != "speedtest.tyo11.jp.leaseweb.net" || !cs[1].Custom || cs[1].ID != "192.168.1.20:5201" {
		t.Errorf("custom %+v %v", cs, err)
	}
	for _, bad := range []string{"host", "host:0", ":5201", "a b:1", "Nope", ""} {
		if _, err := Custom(bad, c); err == nil {
			t.Errorf("custom %q accepted", bad)
		}
	}
}

func TestCatalogErrors(t *testing.T) {
	good := "provider: p\ndisplay: P\nsources: [x]\nservers:\n  - {id: A, host: a.example, ports: 5201-5210, city: C, metro: c, country: GB, geo: eu, verified: {date: 2026-10-10}}\n  - {id: B, host: b.example, ports: \"5201\", city: D, metro: d, country: GB, geo: eu, verified: {date: 2026-10-10}}\n"
	for want, files := range map[string]fstest.MapFS{
		"want first-last": {"p.yml": {Data: []byte(strings.Replace(good, "5201-5210", "9-3", 1))}},
		"private address": {"p.yml": {Data: []byte(strings.Replace(good, "a.example", "10.0.0.1", 1))}},
		"duplicate id":    {"p.yml": {Data: []byte(good)}, "q.yml": {Data: []byte(good)}},
		"sources":         {"p.yml": {Data: []byte(strings.Replace(good, "sources: [x]\n", "", 1))}},
		"verified.date":   {"p.yml": {Data: []byte(strings.Replace(good, ", verified: {date: 2026-10-10}", "", 1))}},
		"metro":           {"p.yml": {Data: []byte(strings.Replace(good, "metro: c, ", "", 1))}},
		"geo \"xx\"":      {"p.yml": {Data: []byte(strings.Replace(good, "geo: eu", "geo: xx", 1))}},
		"no server \"Z\"": {"p.yml": {Data: []byte(good)}, "regions.yml": {Data: []byte("regions: [{id: r, title: R, servers: [A, Z]}]")}},
		"use nearby":      {"p.yml": {Data: []byte(good)}, "regions.yml": {Data: []byte("regions: [{id: r, title: R, servers: [A, B]}]")}},
		"field bogus":     {"p.yml": {Data: []byte(good + "bogus: 1\n")}},
	} {
		if _, err := LoadCatalog(files); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", want, err)
		}
	}
}
