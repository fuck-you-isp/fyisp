package speedtest

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// memStore is an in-memory Store.
type memStore struct {
	mu       sync.Mutex
	tests    []model.SpeedTest
	settings map[string]string
}

func (m *memStore) SaveSpeedTest(_ context.Context, t *model.SpeedTest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t.ID = int64(len(m.tests) + 1)
	m.tests = append(m.tests, *t)
	return nil
}

func (m *memStore) SpeedTests(_ context.Context, from, to time.Time) ([]model.SpeedTest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.SpeedTest
	for _, t := range m.tests {
		if !t.Start.Before(from) && !t.Start.After(to) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (m *memStore) Setting(_ context.Context, k string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.settings[k]
	return v, ok, nil
}

func (m *memStore) SetSetting(_ context.Context, k, v string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.settings == nil {
		m.settings = map[string]string{}
	}
	m.settings[k] = v
	return nil
}

// testCatalog: two regions with fallback chains, and a far server.
func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := LoadCatalog(fstest.MapFS{
		"p.yml": {Data: []byte(`provider: p
display: Prov
sources: [https://example.com]
servers:
  - {id: Near, host: near.example, ports: 5201-5210, city: Here, country: GB, geo: eu, verified: {date: 2026-10-10}}
  - {id: Mid, host: mid.example, ports: 9204-9240, city: There, country: NL, geo: eu, verified: {date: 2026-10-10}}
  - {id: Far, host: far.example, ports: 5200-5209, city: Away, country: JP, geo: as, verified: {date: 2026-10-10}}
  - {id: Dead, host: dead.example, ports: 5200, city: Nowhere, country: US, geo: na, verified: {date: 2026-10-10}}
`)},
		"regions.yml": {Data: []byte(`regions:
  - {id: home, title: Home, chain: [Near, Mid]}
  - {id: away, title: Away, chain: [Dead, Far]}
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
	st    *memStore
	now   time.Time
	mu    sync.Mutex
	rtt   map[string]time.Duration
	calls []Config
	slept []time.Duration
	run   func(c Config, n int) (Result, error)
	step  time.Duration // each run advances the clock this much
}

func newHarness(t *testing.T, o Options) *harness {
	h := &harness{st: &memStore{}, now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC), step: 6 * time.Second,
		rtt: map[string]time.Duration{"far.example": 90 * time.Millisecond, "near.example": 10 * time.Millisecond, "mid.example": 30 * time.Millisecond}}
	h.run = func(c Config, n int) (Result, error) {
		return Result{Direction: c.Direction, Bytes: 125_000_000, Elapsed: 9 * time.Second, Retransmits: 3}, nil
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

// slot runs one target (as the schedule would) and returns its result.
func (h *harness) slot(t *testing.T, tg target) model.SpeedTest {
	t.Helper()
	n := len(h.st.tests)
	h.s.runTarget(context.Background(), tg, "schedule")
	if len(h.st.tests) != n+1 {
		t.Fatalf("%d tests saved", len(h.st.tests)-n)
	}
	return h.st.tests[n]
}

func (h *harness) region(t *testing.T, id string) model.SpeedTest {
	h.s.mu.Lock()
	var tg target
	for _, x := range h.s.planLocked(ModeRegions) {
		if x.region == id {
			tg = x
		}
	}
	h.s.mu.Unlock()
	return h.slot(t, tg)
}

func hostPort(c Config) (string, int) {
	h, p, _ := net.SplitHostPort(c.Addr)
	n, _ := strconv.Atoi(p)
	return h, n
}

func TestIntervalFloor(t *testing.T) {
	c := testCatalog(t)
	if iv := New(Options{Catalog: c, Interval: 5 * time.Minute}).Interval(); iv != MinInterval {
		t.Errorf("catalog servers: %v, want the 15m floor", iv)
	}
	own, _ := Custom("10.0.0.5:5201", c)
	if iv := New(Options{Catalog: c, Interval: 5 * time.Minute, Custom: own}).Interval(); iv != 5*time.Minute {
		t.Errorf("own server: %v, want no floor", iv)
	}
	mixed, _ := Custom("10.0.0.5:5201,Far", c)
	if iv := New(Options{Catalog: c, Interval: 5 * time.Minute, Custom: mixed}).Interval(); iv != MinInterval {
		t.Errorf("own and catalog servers: %v, want the floor", iv)
	}
}

// TestCycle: slots are spaced exactly interval/N within a cycle; each
// cycle starts at its base ± Jitter, never sooner than cycleGap after the
// previous cycle's last slot.
func TestCycle(t *testing.T) {
	h := newHarness(t, Options{Catalog: testCatalog(t)})
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = ModeRegions
	start := h.now
	s.restartLocked(start)
	if d := s.next.Sub(start); d < firstDelay || d > firstDelay+Jitter {
		t.Fatalf("first slot after %v", d)
	}
	sp := s.spacingLocked(ModeRegions)
	if sp != MinInterval/2 {
		t.Fatalf("spacing %v", sp)
	}
	base := s.cycleBase
	var offs []time.Duration
	prev := s.next
	for c := range 200 {
		s.advanceLocked() // slot 1
		if s.next.Sub(prev) != sp {
			t.Fatalf("cycle %d: slot spacing %v", c, s.next.Sub(prev))
		}
		last := s.next
		s.advanceLocked() // next cycle's slot 0
		base = base.Add(MinInterval)
		off := s.next.Sub(base)
		offs = append(offs, off)
		if off < -Jitter || off > Jitter || s.next.Sub(last) < cycleGap {
			t.Fatalf("cycle %d: start %v from base, %v after the last slot", c, off, s.next.Sub(last))
		}
		prev = s.next
	}
	lo, hi := time.Hour, -time.Hour
	for _, o := range offs {
		lo, hi = min(lo, o), max(hi, o)
	}
	if hi-lo < 80*time.Second {
		t.Errorf("cycle start jitter %v..%v", lo, hi)
	}
	if share := s.plannedShareLocked(ModeRegions); share != 2*15.0/900 {
		t.Errorf("planned share %v", share)
	}
}

// TestRegionFallback: a busy first server gets 2 more ports, then the next
// server in the chain; the result records the server used and fallback.
func TestRegionFallback(t *testing.T) {
	h := newHarness(t, Options{})
	h.run = func(c Config, n int) (Result, error) {
		if host, _ := hostPort(c); host == "near.example" {
			return Result{}, ErrBusy
		}
		return Result{Bytes: 1e6, Elapsed: time.Second}, nil
	}
	r := h.region(t, "home")
	if r.Outcome != model.SpeedOK || r.Region != "home" || r.Server != "Mid" || !r.Fallback || r.Attempts != 3+2 || r.UpBps == nil {
		t.Fatalf("%+v", r)
	}
	_, p1 := hostPort(h.calls[0])
	_, p2 := hostPort(h.calls[1])
	_, p3 := hostPort(h.calls[2])
	if p1 == p2 || p2 == p3 || p1 == p3 || h.slept[0] != AttemptGap {
		t.Errorf("busy ports %d %d %d, slept %v", p1, p2, p3, h.slept)
	}
	if host, p := hostPort(h.calls[3]); host != "mid.example" || p < 9204 || p > 9240 || p != r.Port {
		t.Errorf("fallback call %v, port %d", h.calls[3], r.Port)
	}

	// Unreachable (a timeout, DNS): the next server at once.
	h.calls = nil
	h.run = func(c Config, n int) (Result, error) {
		if host, _ := hostPort(c); host == "dead.example" {
			return Result{}, fmt.Errorf("%w: i/o timeout", ErrUnreachable)
		}
		return Result{Bytes: 1e6, Elapsed: time.Second}, nil
	}
	if r = h.region(t, "away"); r.Server != "Far" || !r.Fallback || r.Outcome != model.SpeedOK || h.calls[0].Addr != "dead.example:5200" || len(h.calls) != 3 {
		t.Errorf("unreachable first: %+v, calls %v", r, h.calls)
	}

	// The first server answers: no fallback.
	h.calls = nil
	h.run = func(Config, int) (Result, error) { return Result{Bytes: 1e6, Elapsed: time.Second}, nil }
	if r = h.region(t, "home"); r.Server != "Near" || r.Fallback {
		t.Errorf("first server fine: %+v", r)
	}

	// Every server busy: busy, the last server tried, never a speed.
	h.run = func(Config, int) (Result, error) { return Result{}, ErrBusy }
	if r = h.region(t, "home"); r.Outcome != model.SpeedBusy || r.Server != "Mid" || r.DownBps != nil {
		t.Errorf("all busy: %+v", r)
	}
}

// TestRefused: a refused connection is retried on another port (a stock
// server re-listening), not "unreachable"; refused on every try is.
func TestRefused(t *testing.T) {
	h := newHarness(t, Options{})
	h.run = func(c Config, n int) (Result, error) {
		if n == 1 {
			return Result{}, fmt.Errorf("%w: connection refused", ErrRefused)
		}
		return Result{Bytes: 1e6, Elapsed: time.Second}, nil
	}
	if r := h.region(t, "home"); r.Outcome != model.SpeedOK || r.Server != "Near" || r.Attempts != 3 {
		t.Errorf("refused once: %+v", r)
	}
	// The upload refused right after the download: another port, after the
	// 1 s re-listen pause.
	h.calls, h.slept = nil, nil
	h.run = func(c Config, n int) (Result, error) {
		if c.Direction == Upload && n == 2 {
			return Result{}, fmt.Errorf("%w: connection refused", ErrRefused)
		}
		return Result{Bytes: 1e6, Elapsed: time.Second}, nil
	}
	if r := h.region(t, "home"); r.Outcome != model.SpeedOK || r.UpBps == nil || h.calls[1].Addr != h.calls[0].Addr || h.calls[2].Addr == h.calls[1].Addr || h.slept[0] != relisten {
		t.Errorf("upload refused once: %+v, calls %v, slept %v", r, h.calls, h.slept)
	}
	h.calls = nil
	h.run = func(Config, int) (Result, error) { return Result{}, fmt.Errorf("%w: connection refused", ErrRefused) }
	if r := h.region(t, "home"); r.Outcome != model.SpeedUnreachable {
		t.Errorf("always refused: %+v", r)
	}
}

// TestSlotBudget: no new attempt or fallback once the slot has used its
// budget; a measured download is kept when the upload fails.
func TestSlotBudget(t *testing.T) {
	h := newHarness(t, Options{})
	h.step = 9 * time.Second
	h.run = func(Config, int) (Result, error) { return Result{}, ErrBusy }
	r := h.region(t, "home")
	if el := r.End.Sub(r.Start); el > SlotBudget+h.step || len(h.calls) > 3 {
		t.Errorf("slot took %v with %d calls", el, len(h.calls))
	}
	h.calls, h.step = nil, 6*time.Second
	h.run = func(c Config, n int) (Result, error) {
		if c.Direction == Upload {
			return Result{}, errors.New("protocol error")
		}
		return Result{Bytes: 1e6, Elapsed: time.Second}, nil
	}
	if r = h.region(t, "home"); r.Outcome != model.SpeedFailed || r.DownBps == nil || r.Fallback || len(h.calls) != 2 {
		t.Errorf("upload failed: %+v, %d calls", r, len(h.calls))
	}
	// Adaptive length on every attempt.
	if c := h.calls[0]; c.Duration != MaxMeasure || c.MinDuration != MinMeasure || c.Timeout != DirTimeout {
		t.Errorf("config %+v", c)
	}
}

// TestNearest: the two nearest catalog servers by RTT, alternating;
// re-picked after a day.
func TestNearest(t *testing.T) {
	h := newHarness(t, Options{})
	var got []string
	for range 4 {
		got = append(got, h.slot(t, target{}).Server)
	}
	if fmt.Sprint(got) != "[Near Mid Near Mid]" {
		t.Errorf("servers %v", got)
	}
	st := h.s.State()
	if len(st.Servers) != 4 || !st.Servers[0].Picked || !st.Servers[1].Picked || st.Servers[2].Picked || st.Servers[3].RTTms != nil || *st.Servers[0].RTTms != 10 {
		t.Errorf("state %+v", st.Servers)
	}
	h.mu.Lock()
	h.now = h.now.Add(RepickEvery)
	h.rtt["far.example"] = time.Millisecond
	h.mu.Unlock()
	h.slot(t, target{})
	if st := h.s.State(); !st.Servers[2].Picked || st.Servers[1].Picked {
		t.Errorf("re-pick %+v", st.Servers)
	}
}

func TestModesAndTestNow(t *testing.T) {
	h := newHarness(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { h.s.Run(ctx); close(done) }()
	if st := h.s.State(); st.Mode != ModeOff || !st.Next.IsZero() || len(st.Modes) != 3 {
		t.Fatalf("off by default: %+v", st)
	}
	if _, err := h.s.SetMode(ctx, ModeCustom); !errors.Is(err, ErrMode) {
		t.Errorf("custom without servers: %v", err)
	}
	if _, err := h.s.TestNow("Nope"); err == nil {
		t.Error("unknown id accepted")
	}
	release := make(chan struct{})
	h.mu.Lock()
	h.run = func(c Config, n int) (Result, error) {
		<-release
		return Result{Bytes: 1e6, Elapsed: time.Second}, nil
	}
	h.mu.Unlock()
	if _, err := h.s.TestNow("away"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.TestNow(""); !errors.Is(err, ErrRunning) {
		t.Errorf("second: %v", err)
	}
	for h.s.State().Running == nil || h.s.State().Running.Direction == "picking" {
		time.Sleep(5 * time.Millisecond)
	}
	if st := h.s.State(); st.Running.Server != "Dead" || st.Running.Region != "away" || st.Running.ExpectedEnd.Before(st.Running.Started) {
		t.Errorf("running %+v", st.Running)
	}
	close(release)
	for h.s.State().Running != nil {
		time.Sleep(5 * time.Millisecond)
	}
	h.st.mu.Lock()
	if len(h.st.tests) != 1 || h.st.tests[0].Trigger != "manual" || h.st.tests[0].Region != "away" {
		t.Fatalf("tests %+v", h.st.tests)
	}
	h.st.mu.Unlock()
	next, err := h.s.SetMode(ctx, ModeRegions)
	if err != nil || next.Sub(h.clock()) < firstDelay || next.Sub(h.clock()) > firstDelay+Jitter || h.st.settings[SettingKey] != "regions" {
		t.Errorf("regions: %v %v %v", next, err, h.st.settings)
	}
	if st := h.s.State(); st.Mode != ModeRegions || !st.Next.Equal(next) || st.NextRegion != "home" || st.NextServer != "Near" || st.Spacing != MinInterval/2 {
		t.Errorf("state %+v", st)
	}
	cancel()
	<-done
	// The saved mode is picked up at the next start; a flag wins over it.
	h2 := newHarness(t, Options{})
	h2.st.settings = map[string]string{SettingKey: "nearest"}
	ctx2, cancel2 := context.WithCancel(context.Background())
	go h2.s.Run(ctx2)
	for h2.s.State().Mode != ModeNearest {
		time.Sleep(5 * time.Millisecond)
	}
	cancel2()
	h3 := newHarness(t, Options{Mode: ModeRegions})
	h3.st.settings = map[string]string{SettingKey: "off"}
	ctx3, cancel3 := context.WithCancel(context.Background())
	go h3.s.Run(ctx3)
	for h3.s.State().Mode != ModeRegions {
		time.Sleep(5 * time.Millisecond)
	}
	cancel3()
}

// TestPlannedShareWarning: more slots than the line can bear warn.
func TestPlannedShareWarning(t *testing.T) {
	c := testCatalog(t)
	var many []string
	for i := range 20 {
		many = append(many, fmt.Sprintf("10.0.0.%d:5201", i+1))
	}
	own, _ := Custom(strings.Join(many, ","), c)
	s := New(Options{Catalog: c, Custom: own, Interval: 15 * time.Minute})
	s.SetMode(context.Background(), ModeCustom)
	st := s.State()
	if st.PlannedShare != 20*15.0/900 || !strings.Contains(st.Warning, "About 33% of the time") || st.Spacing != 45*time.Second {
		t.Errorf("%v %q %v", st.PlannedShare, st.Warning, st.Spacing)
	}
	s.SetMode(context.Background(), ModeRegions)
	if st := s.State(); st.Warning != "" {
		t.Errorf("regions: %q", st.Warning)
	}
}

// TestMask: exactly the probes that ran during a test (from Sample.At to
// their arrival) or within MaskTail after it are blanked.
func TestMask(t *testing.T) {
	w := &Windows{}
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	w.Add(Window{Start: t0, End: t0.Add(20 * time.Second)})
	now := t0
	var got []model.Sample
	m := Mask{Inner: model.SinkFunc(func(s model.Sample) { got = append(got, s) }), Windows: w, Now: func() time.Time { return now }}
	gw := model.SeriesKey{Target: "Gateway", Kind: model.KindICMP}
	for _, c := range []struct {
		at, arrive time.Duration // from t0
		masked     bool
	}{
		{-10 * time.Second, -9 * time.Second, false},       // before
		{-2 * time.Second, 100 * time.Millisecond, true},   // ran into the test
		{10 * time.Second, 11 * time.Second, true},         // during
		{21 * time.Second, 21500 * time.Millisecond, true}, // in the 2 s tail
		{22500 * time.Millisecond, 23 * time.Second, false},
	} {
		now = t0.Add(c.arrive)
		m.Observe(model.Sample{Key: gw, Slot: t0.Add(c.at).Truncate(time.Second), At: t0.Add(c.at), RTT: 30 * time.Millisecond})
		s := got[len(got)-1]
		if masked := s.Lost && s.Reason == model.ReasonGap && s.RTT == 0; masked != c.masked {
			t.Errorf("probe at %v: masked %v, want %v", c.at, masked, c.masked)
		}
	}
	// 20 s plus the 2 s tail of the last 5 minutes.
	if sh := w.Share(t0.Add(time.Minute), 5*time.Minute); sh < 21.9/300 || sh > 22.1/300 {
		t.Errorf("share %v", sh)
	}
	w.Begin(t0.Add(time.Hour))
	if !w.Overlaps(t0.Add(2*time.Hour), t0.Add(2*time.Hour)) || w.Overlaps(t0.Add(10*time.Minute), t0.Add(20*time.Minute)) {
		t.Error("running window")
	}
}

// fakePinger answers from reply (lost: an error at once).
type fakePinger struct {
	mu    sync.Mutex
	reply func(dst netip.Addr) (time.Duration, bool)
	n     map[netip.Addr]int
}

func (f *fakePinger) Ping(_ context.Context, dst netip.Addr, _ time.Duration) (time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == nil {
		f.n = map[netip.Addr]int{}
	}
	f.n[dst]++
	rtt, ok := f.reply(dst)
	if !ok {
		return 0, errors.New("timeout")
	}
	return rtt, nil
}

func (f *fakePinger) Close() error { return nil }

// Latency under load: idle from the tap (lost probes kept), loaded from the
// test's own probes of the gateway and the ISP edge per direction; lost
// probes count as loss and as LoadTimeout in the percentiles.
func TestLatencyUnderLoad(t *testing.T) {
	gwA, edgeA := netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("62.1.1.1")
	p := &model.Profile{Targets: []model.Target{
		{Name: "Gateway", Layer: model.LayerGateway, Kinds: []model.ProbeKind{model.KindICMP}},
		{Name: "ISP edge", Layer: model.LayerEdge, Kinds: []model.ProbeKind{model.KindICMP}},
	}}
	h := newHarness(t, Options{})
	tap := NewLoadTap(p, h.clock)
	fp := &fakePinger{}
	h.s.o.Load, h.s.o.loadEvery = tap, 2*time.Millisecond
	h.s.o.Path = func() (netip.Addr, netip.Addr) { return gwA, edgeA }
	h.s.o.OpenPinger = func() (LoadPinger, error) { return fp, nil }
	h.s.o.Conn = func() string { return model.ConnWired }
	gw := model.SeriesKey{Target: "Gateway", Kind: model.KindICMP}
	edge := model.SeriesKey{Target: "ISP edge", Kind: model.KindICMP}
	feed := func(k model.SeriesKey, ms float64, n int, lost bool) {
		for range n {
			s := model.Sample{Key: k, RTT: time.Duration(ms * float64(time.Millisecond))}
			if lost {
				s = model.Sample{Key: k, Lost: true, Reason: model.ReasonTimeout}
			}
			tap.Observe(s)
		}
	}
	h.mu.Lock()
	h.now = h.now.Add(-30 * time.Second)
	h.mu.Unlock()
	feed(gw, 2, 10, false)
	feed(edge, 8, 8, false)
	feed(edge, 0, 2, true)
	tap.Observe(model.Sample{Key: edge, Lost: true, Reason: model.ReasonGap}) // not measured: left out
	var dir Direction
	var upN int
	fp.reply = func(dst netip.Addr) (time.Duration, bool) {
		switch {
		case dst == gwA:
			return 3 * time.Millisecond, true
		case dir == Download:
			return 30 * time.Millisecond, true
		}
		upN++ // under fp.mu
		return 200 * time.Millisecond, upN%2 == 0
	}
	h.mu.Lock()
	h.now = h.now.Add(30 * time.Second)
	h.run = func(c Config, n int) (Result, error) {
		fp.mu.Lock()
		dir = c.Direction
		fp.mu.Unlock()
		time.Sleep(60 * time.Millisecond) // the direction runs; the prober pings
		return Result{Bytes: 1e6, Elapsed: time.Second}, nil
	}
	h.mu.Unlock()
	r := h.region(t, "home")
	l := r.Latency
	if l == nil || l.Edge.Idle == nil || l.Edge.Down == nil || l.Edge.Up == nil || l.Gateway.Idle == nil || l.Gateway.Down == nil || l.Gateway.Up == nil {
		t.Fatalf("latency %+v", l)
	}
	if e := l.Edge.Idle; e.N != 10 || e.Lost != 2 || e.Loss != 0.2 || e.P50Ms != 8 || e.P95Ms != 1000 {
		t.Errorf("edge idle %+v, want 10 probes, 2 lost, p50 8, p95 1000 (lost = timeout)", *e)
	}
	if e := l.Edge.Down; e.Lost != 0 || e.P50Ms != 30 || e.N < 5 {
		t.Errorf("edge download %+v, want p50 30, no loss", *e)
	}
	if e := l.Edge.Up; e.Loss < 0.3 || e.Loss > 0.7 || e.P95Ms != 1000 || e.N < 5 {
		t.Errorf("edge upload %+v, want about half lost, p95 1000", *e)
	}
	if g := l.Gateway; g.Idle.P50Ms != 2 || g.Down.P50Ms != 3 || g.Up.P50Ms != 3 || g.Up.Lost != 0 {
		t.Errorf("gateway %+v %+v %+v", *g.Idle, *g.Down, *g.Up)
	}
	if r.IdleGatewayMs == nil || *r.IdleGatewayMs != 2 || r.LoadedGatewayMs == nil || *r.LoadedGatewayMs != 3 || r.Conn != model.ConnWired {
		t.Errorf("idle/loaded gateway %v %v conn %q", r.IdleGatewayMs, r.LoadedGatewayMs, r.Conn)
	}
}

// Without the network path or ICMP there is no loaded side; the idle side
// still comes from the tap, and fewer than 3 probes are no stats.
func TestLatencyIdleOnly(t *testing.T) {
	p := &model.Profile{Targets: []model.Target{{Name: "Gateway", Layer: model.LayerGateway, Kinds: []model.ProbeKind{model.KindICMP}}}}
	h := newHarness(t, Options{})
	tap := NewLoadTap(p, h.clock)
	h.s.o.Load = tap
	h.s.o.Path = func() (netip.Addr, netip.Addr) { return netip.Addr{}, netip.Addr{} }
	h.s.o.OpenPinger = func() (LoadPinger, error) { t.Error("pinger opened without addresses"); return nil, errors.New("no") }
	tap.Observe(model.Sample{Key: model.SeriesKey{Target: "Gateway", Kind: model.KindICMP}, RTT: time.Millisecond})
	tap.Observe(model.Sample{Key: model.SeriesKey{Target: "Gateway", Kind: model.KindICMP}, RTT: time.Millisecond})
	if r := h.region(t, "home"); r.Latency != nil || r.Conn != model.ConnUnknown {
		t.Errorf("latency %+v conn %q, want none (2 probes) and unknown", r.Latency, r.Conn)
	}
	if statsOf([]loadSample{{ms: 1}, {lost: true}, {ms: 3}}) == nil {
		t.Error("3 probes, one lost: want stats")
	}
}

// TestBuiltinCatalog: the shipped catalog's servers and regions.
func TestBuiltinCatalog(t *testing.T) {
	c, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Servers) != 41 || len(c.Regions) != 9 {
		t.Fatalf("%d servers, %d regions", len(c.Servers), len(c.Regions))
	}
	labels := map[string]string{}
	for _, s := range c.Servers {
		if prev, ok := labels[s.Label]; ok {
			t.Errorf("label %q of %s and %s", s.Label, prev, s.ID)
		}
		labels[s.Label] = s.ID
		if s.Label == "" || s.Provider == "" || s.ports() < 1 {
			t.Errorf("%+v", s)
		}
	}
	want := map[string]string{"uk": "Clouvider-London Leaseweb-London OVH-London", "oceania": "Leaseweb-Sydney OVH-Sydney"}
	for _, r := range c.Regions {
		if w, ok := want[r.ID]; ok && strings.Join(r.Chain, " ") != w {
			t.Errorf("%s: %v", r.ID, r.Chain)
		}
	}
	if s, _ := c.Server("Leaseweb-NewYork"); s.Label != "Leaseweb New York" || s.Host != "speedtest.nyc1.us.leaseweb.net" || s.PortMin != 5201 {
		t.Errorf("%+v", s)
	}
	if s, _ := c.Server("Eranium-Amsterdam"); s.Provider != "Eranium" || s.Label != "Eranium Amsterdam" {
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
	good := "provider: p\ndisplay: P\nsources: [x]\nservers:\n  - {id: A, host: a.example, ports: 5201-5210, city: C, country: GB, geo: eu, verified: {date: 2026-10-10}}\n"
	for want, files := range map[string]fstest.MapFS{
		"want first-last": {"p.yml": {Data: []byte(strings.Replace(good, "5201-5210", "9-3", 1))}},
		"private address": {"p.yml": {Data: []byte(strings.Replace(good, "a.example", "10.0.0.1", 1))}},
		"duplicate id":    {"p.yml": {Data: []byte(good)}, "q.yml": {Data: []byte(good)}},
		"sources":         {"p.yml": {Data: []byte(strings.Replace(good, "sources: [x]\n", "", 1))}},
		"verified.date":   {"p.yml": {Data: []byte(strings.Replace(good, ", verified: {date: 2026-10-10}", "", 1))}},
		"geo \"xx\"":      {"p.yml": {Data: []byte(strings.Replace(good, "geo: eu", "geo: xx", 1))}},
		"no server \"B\"": {"p.yml": {Data: []byte(good)}, "regions.yml": {Data: []byte("regions: [{id: r, title: R, chain: [A, B]}]")}},
		"field bogus":     {"p.yml": {Data: []byte(good + "bogus: 1\n")}},
	} {
		if _, err := LoadCatalog(files); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", want, err)
		}
	}
}

// The idle side: samples are timed by the probe's start (Sample.At), and a
// test earlier in the idle window (back-to-back runs) is not idle.
func TestLoadTapIdle(t *testing.T) {
	p := &model.Profile{Targets: []model.Target{{Name: "ISP edge", Layer: model.LayerEdge, Kinds: []model.ProbeKind{model.KindICMP}}}}
	t0 := time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC)
	now := t0
	tap := NewLoadTap(p, func() time.Time { return now })
	edge := model.SeriesKey{Target: "ISP edge", Kind: model.KindICMP}
	obs := func(at time.Time, ms float64) {
		tap.Observe(model.Sample{Key: edge, At: at, RTT: time.Duration(ms * float64(time.Millisecond))})
	}
	w := &Windows{}
	// An earlier test 60-50 s before: its loaded probes must not count.
	w.Add(Window{Start: t0.Add(-60 * time.Second), End: t0.Add(-50 * time.Second)})
	for i := range 10 {
		now = t0.Add(-90*time.Second + time.Duration(i)*time.Second)
		obs(now, 8) // idle
		now = t0.Add(-59*time.Second + time.Duration(i)*time.Second)
		obs(now, 900) // during the earlier test
	}
	// A probe sent just before this test (t0) that arrives during it is idle
	// by its start.
	now = t0.Add(time.Second)
	obs(t0.Add(-500*time.Millisecond), 9)
	st := tap.Stats(LayerEdge, t0.Add(-idleWindow), t0, w)
	if st == nil || st.N != 11 || st.P95Ms > 9 {
		t.Errorf("idle %+v, want the 11 idle probes only", st)
	}
	if all := tap.Stats(LayerEdge, t0.Add(-idleWindow), t0, nil); all == nil || all.N != 21 || all.P95Ms != 900 {
		t.Errorf("without windows %+v, want 21 probes", all)
	}
}
