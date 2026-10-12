package tiles

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// ---- synthetic samples for the engine ----

// eSer is one tracked series as the probes would report it.
type eSer struct {
	key   model.SeriesKey // KindUDP for a STUN target
	every time.Duration
	phase time.Duration // the probe fires this long after its slot
	rtt   time.Duration
	tmo   time.Duration
}

// eArr is a sample and when it reaches the sinks.
type eArr struct {
	s      model.Sample
	arrive time.Time
}

// eEvent loses (or slows by mul) the checks of the matching targets fired
// in [from, to).
type eEvent struct {
	from, to time.Time
	targets  func(name string) bool
	mul      float64
}

func eNames(names ...string) func(string) bool {
	return func(n string) bool { return slices.Contains(names, n) }
}

func eAll(string) bool { return true }

// eSeries are an engine's tracked series with a probe phase, an RTT and a
// timeout each.
func eSeries(e *engine) []eSer {
	var out []eSer
	for i, s := range e.ss {
		x := eSer{key: s.key, every: s.every, rtt: 20 * time.Millisecond, tmo: 3 * time.Second}
		x.phase = time.Duration(i*317) * time.Millisecond % s.every
		switch s.rule {
		case rulePath:
			x.rtt, x.tmo = 2*time.Millisecond, time.Second
			if s.every > time.Second {
				x.rtt = 9 * time.Millisecond
			}
		case ruleUDP:
			x.rtt = 30 * time.Millisecond
		}
		out = append(out, x)
	}
	return out
}

// eGen makes the samples of ss for slots in [from, to), sorted by arrival.
// A STUN burst writes its three kinds, as probe.burstSamples does.
func eGen(ss []eSer, from, to time.Time, evs []eEvent, seed uint64) []eArr {
	r := rand.New(rand.NewPCG(seed, 11))
	var out []eArr
	for _, s := range ss {
		first := from.Truncate(s.every)
		if first.Before(from) {
			first = first.Add(s.every)
		}
		for slot := first; slot.Before(to); slot = slot.Add(s.every) {
			fire := slot.Add(s.phase)
			// web and path checks lose 1 in 500; STUN bursts none (one
			// lost burst is 2.8% of a call window: Choppy)
			lost := s.key.Kind != model.KindUDP && r.IntN(500) == 0
			rtt := s.rtt + time.Duration(r.IntN(int(s.rtt/4)+1))
			for _, ev := range evs {
				if !fire.Before(ev.from) && fire.Before(ev.to) && ev.targets(s.key.Target) {
					if ev.mul == 0 {
						lost = true
					} else {
						rtt = time.Duration(float64(rtt) * ev.mul)
					}
				}
			}
			arrive := fire.Add(rtt)
			if lost {
				arrive = fire.Add(s.tmo)
			}
			k := func(kind model.ProbeKind) model.SeriesKey { return model.SeriesKey{Target: s.key.Target, Kind: kind} }
			if s.key.Kind != model.KindUDP {
				x := model.Sample{Key: s.key, Slot: slot, RTT: rtt}
				if lost {
					x = model.Sample{Key: s.key, Slot: slot, Lost: true, Reason: model.ReasonTimeout}
				}
				out = append(out, eArr{x, arrive})
				continue
			}
			burst := []model.Sample{
				{Key: k(model.KindUDP), Slot: slot, RTT: rtt},
				{Key: k(model.KindUDPJitter), Slot: slot, RTT: time.Duration(1000+r.IntN(4000)) * time.Microsecond},
				{Key: k(model.KindUDPLoss), Slot: slot},
			}
			if r.IntN(400) == 0 {
				burst[2].RTT = 33 * time.Millisecond
			}
			if lost {
				burst[0] = model.Sample{Key: k(model.KindUDP), Slot: slot, Lost: true, Reason: model.ReasonTimeout}
				burst[1] = model.Sample{Key: k(model.KindUDPJitter), Slot: slot, Lost: true, Reason: model.ReasonGap}
				burst[2].RTT = 100 * time.Millisecond
			}
			for _, x := range burst {
				out = append(out, eArr{x, arrive})
			}
		}
	}
	// an untracked target, which every engine ignores
	for slot := from; slot.Before(to); slot = slot.Add(15 * time.Second) {
		out = append(out, eArr{model.Sample{Key: model.SeriesKey{Target: "Untracked", Kind: model.KindTCP}, Slot: slot, RTT: time.Millisecond}, slot.Add(time.Second)})
	}
	slices.SortStableFunc(out, func(a, b eArr) int { return a.arrive.Compare(b.arrive) })
	return out
}

// ---- the scenario ----

// eScene is a day and a bit of the default profile: learning ends after
// 24 h, then a Google-side outage, a 45 s connection drop the verdict
// blamed on the provider, a slow Microsoft 365 and a slow Netflix during a
// speed test.
type eScene struct {
	p                      *model.Profile
	start, end             time.Time
	google, drop, m365, nf window
	speed                  window
	incident               model.Incident
	evs                    []eEvent
}

func newScene(t *testing.T) *eScene {
	t.Helper()
	p, err := profile.Default()
	if err != nil {
		t.Fatal(err)
	}
	s := &eScene{p: p, start: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)}
	h := func(d time.Duration) time.Time { return s.start.Add(d) }
	s.end = h(26*time.Hour + 40*time.Minute)
	s.google = window{h(25 * time.Hour), h(25*time.Hour + 3*time.Minute)}
	s.drop = window{h(25*time.Hour + 30*time.Minute), h(25*time.Hour + 30*time.Minute + 45*time.Second)}
	s.m365 = window{h(26 * time.Hour), h(26*time.Hour + 10*time.Minute)}
	s.nf = window{h(26*time.Hour + 20*time.Minute), h(26*time.Hour + 28*time.Minute)}
	s.speed = window{h(26*time.Hour + 19*time.Minute), h(26*time.Hour + 27*time.Minute)}
	s.incident = model.Incident{ID: 1, Kind: model.VerdictISP, Start: s.drop.from.Add(10 * time.Second), End: s.drop.to.Add(time.Minute), PeakLoss: 1}
	s.evs = []eEvent{
		{s.google.from, s.google.to, eNames("Google-Meet", "Meet-STUN", "Google-Gmail", "Google-Docs", "Google-Drive", "YouTube"), 0},
		{s.drop.from, s.drop.to, eAll, 0},
		{s.m365.from, s.m365.to, eNames("M365-Outlook", "M365-Portal", "M365-Login"), 4},
		{s.nf.from, s.nf.to, eNames("Netflix"), 4},
	}
	return s
}

func (s *eScene) sources(st store.Reader) Sources {
	ok := true
	return Sources{
		Store: st,
		SpeedTests: func(_ context.Context, from, to time.Time) ([]model.SpeedTest, error) {
			return []model.SpeedTest{{ID: 1, Start: s.speed.from, End: s.speed.to, Outcome: model.SpeedOK}}, nil
		},
		Incidents: func(_ context.Context, from, to time.Time) ([]model.Incident, error) {
			return []model.Incident{s.incident}, nil
		},
		Verdict: func() model.Verdict { return model.Verdict{Kind: model.VerdictOK} },
		Layers: func() map[string]*bool {
			return map[string]*bool{model.LayerGateway: &ok, model.LayerEdge: &ok, model.LayerAnycast: &ok}
		},
	}
}

func (s *eScene) engine(st store.Reader) *engine {
	// the scene's 27 h, not 8 days of nothing
	return newEngine(s.p, profile.ResolveTiles(s.p), s.sources(st), Options{Replay: 28 * time.Hour})
}

// live runs an engine fed as the probes would feed it, from the scene's
// start: every sample once it has arrived, a tick every 5 s. At each
// checkpoint it publishes a snapshot and calls at.
func (s *eScene) live(t *testing.T, arr []eArr, checks []time.Time, at func(e *engine, now time.Time)) *engine {
	t.Helper()
	ctx := context.Background()
	e := s.engine(store.NewFake())
	if err := e.replay(ctx, s.start); err != nil {
		t.Fatal(err)
	}
	e.goLive(ctx, s.start)
	i := 0
	last := checks[len(checks)-1]
	for tk := s.start.Add(tickEvery); !tk.After(last); tk = tk.Add(tickEvery) {
		for ; i < len(arr) && !arr[i].arrive.After(tk); i++ {
			e.Observe(arr[i].s)
		}
		if slices.Contains(checks, tk) {
			e.advance(ctx, tk)
			at(e, tk)
			continue
		}
		e.mu.Lock()
		e.tickTo(tk)
		e.mu.Unlock()
	}
	return e
}

// stored is a store holding the samples that arrived by now.
func stored(arr []eArr, now time.Time) *store.Fake {
	st := store.NewFake()
	for _, a := range arr {
		if !a.arrive.After(now) {
			st.Observe(a.s)
		}
	}
	return st
}

// replayed is an engine started at now over the samples stored by then.
func (s *eScene) replayed(t *testing.T, st store.Reader, now time.Time) *engine {
	t.Helper()
	ctx := context.Background()
	e := s.engine(st)
	if err := e.replay(ctx, now); err != nil {
		t.Fatal(err)
	}
	e.goLive(ctx, now)
	return e
}

// ---- comparing engines ----

func sameEngines(t *testing.T, name string, a, b *engine) {
	t.Helper()
	sa, sb := a.Snapshot(), b.Snapshot()
	if !reflect.DeepEqual(sa, sb) {
		t.Errorf("%s: snapshots differ", name)
		diffSnap(t, sa, sb)
	}
	from := sa.At.Add(-problemsFor)
	if pa, pb := a.Problems(from), b.Problems(from); !reflect.DeepEqual(pa, pb) {
		t.Errorf("%s: problems differ: %d vs %d", name, len(pa), len(pb))
	}
	for _, d := range profile.Tiles {
		ca, oka := a.Chart(d.ID)
		cb, okb := b.Chart(d.ID)
		if oka != okb {
			t.Errorf("%s: chart %s: %v vs %v", name, d.ID, oka, okb)
			continue
		}
		if !oka {
			continue
		}
		for i := range ca.Ms {
			if math.Float64bits(ca.Ms[i]) != math.Float64bits(cb.Ms[i]) {
				t.Errorf("%s: chart %s bucket %d: %v vs %v", name, d.ID, i, ca.Ms[i], cb.Ms[i])
				break
			}
		}
		ca.Ms, cb.Ms = [chartBuckets]float64{}, [chartBuckets]float64{}
		if !reflect.DeepEqual(ca, cb) {
			t.Errorf("%s: chart %s differs", name, d.ID)
		}
	}
	if la, lb := a.LateSamples(), b.LateSamples(); la != 0 || lb != 0 {
		t.Errorf("%s: late samples %d and %d, want none", name, la, lb)
	}
}

func diffSnap(t *testing.T, a, b *Snapshot) {
	t.Helper()
	if !reflect.DeepEqual(a.Conn, b.Conn) {
		t.Logf("  conn %+v\n    vs %+v", a.Conn, b.Conn)
	}
	for i := range a.Tiles {
		if x, y := a.Tiles[i], b.Tiles[i]; !reflect.DeepEqual(x, y) {
			t.Logf("  tile %s: %s since %v usual %v, measured %d vs %s since %v usual %v, measured %d", x.ID, x.State, x.Since, fp(x.UsualMs), x.Measured, y.State, y.Since, fp(y.UsualMs), y.Measured)
			for j := range x.Bars {
				if x.Bars[j] != y.Bars[j] {
					t.Logf("    bar %d: %+v vs %+v", j, x.Bars[j], y.Bars[j])
				}
			}
		}
	}
	if !reflect.DeepEqual(a.Layers, b.Layers) {
		t.Logf("  layers %+v vs %+v", a.Layers, b.Layers)
	}
	if !reflect.DeepEqual(a.eps, b.eps) {
		t.Logf("  episodes:\n%s  vs\n%s", epsText(a.eps), epsText(b.eps))
	}
	for _, f := range []string{"At", "LastMeasured", "FirstData", "Learning", "Monitored", "Day", "Rev"} {
		x, y := reflect.ValueOf(*a).FieldByName(f).Interface(), reflect.ValueOf(*b).FieldByName(f).Interface()
		if !reflect.DeepEqual(x, y) {
			t.Logf("  %s: %v vs %v", f, x, y)
		}
	}
}

func fp(p *float64) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprintf("%.3f", *p)
}

func epsText(eps []Episode) string {
	var b strings.Builder
	for _, e := range eps {
		fmt.Fprintf(&b, "    %s %s..%s %s %s %v drop %v cause %+v\n", e.ID, e.Start.Format("15:04:05"), e.End.Format("15:04:05"), e.Sev, e.Kind, e.Tiles, e.DropDur, e.Cause)
	}
	return b.String()
}

// ---- tests ----

// TestEngineLiveVsReplay feeds the scene live and compares it, at two
// checkpoints, with engines that start then and replay the store: the
// snapshots, problems and charts must be identical.
func TestEngineLiveVsReplay(t *testing.T) {
	s := newScene(t)
	probe := s.engine(nil)
	arr := eGen(eSeries(probe), s.start, s.end, s.evs, 1)
	t1 := s.drop.to.Add(14 * time.Minute).Truncate(tickEvery)
	t2 := s.end
	s.live(t, arr, []time.Time{t1, t2}, func(a *engine, now time.Time) {
		b := s.replayed(t, stored(arr, now), now)
		sameEngines(t, now.Format("15:04"), a, b)
	})
}

// TestEngineScene checks what the scene shows: one Google-side problem,
// the connection drop with the provider's incident in it, Microsoft 365
// slow on its own side, Netflix slow during a speed test.
func TestEngineScene(t *testing.T) {
	s := newScene(t)
	arr := eGen(eSeries(s.engine(nil)), s.start, s.end, s.evs, 2)
	t1 := s.drop.to.Add(14 * time.Minute).Truncate(tickEvery)
	e := s.replayed(t, stored(arr, t1), t1)
	snap := e.Snapshot()
	if c := snap.Conn; c.Kind != ConnWorkingAgain && (c.Kind != ConnApp || c.Again == nil) {
		t.Errorf("14 min after the drop: card %+v, want working again", c)
	} else if a := c.Again; a.Cause.Who != WhoProvider || a.Variant != AgainDropped {
		t.Errorf("working again %+v, want the provider's drop", a)
	}
	if snap.Learning {
		t.Error("still learning after 25 h")
	}

	e = s.replayed(t, stored(arr, s.end), s.end)
	snap = e.Snapshot()
	eps := e.Problems(s.start)
	t.Logf("problems:\n%s", epsText(eps))
	find := func(w window, f func(Episode) bool) []Episode {
		var out []Episode
		for _, x := range eps {
			if x.window().overlaps(w, s.end) && f(x) {
				out = append(out, x)
			}
		}
		return out
	}
	near := func(a, b time.Time, d time.Duration) bool { return a.Sub(b).Abs() <= d }

	g := find(s.google, func(Episode) bool { return true })
	switch {
	case len(g) != 1:
		t.Errorf("Google outage: %d problems, want 1", len(g))
	case !slices.Equal(g[0].Tiles, []string{"meet", "gws", "youtube"}) || g[0].Connection:
		t.Errorf("Google outage: tiles %v (connection %v)", g[0].Tiles, g[0].Connection)
	case g[0].Cause != Cause{Who: WhoApp, Vendor: "google", Evidence: EvVendorOnly}:
		t.Errorf("Google outage: cause %+v", g[0].Cause)
	case !near(g[0].Start, s.google.from, time.Minute) || !near(g[0].End, s.google.to, time.Minute):
		t.Errorf("Google outage: %v..%v, want %v..%v ±1 min", g[0].Start, g[0].End, s.google.from, s.google.to)
	}

	d := find(s.drop, func(Episode) bool { return true })
	switch {
	case len(d) != 1:
		t.Errorf("connection drop: %d problems, want 1", len(d))
	case !d[0].Connection || d[0].Kind != EpDrop || d[0].Verdict != model.VerdictISP:
		t.Errorf("connection drop: %+v", d[0])
	case d[0].Cause.Who != WhoProvider || d[0].DropDur < 40*time.Second || d[0].DropDur > 50*time.Second:
		t.Errorf("connection drop: cause %+v, %v", d[0].Cause, d[0].DropDur)
	}

	m := find(s.m365, func(Episode) bool { return true })
	switch {
	case len(m) != 1:
		t.Errorf("Microsoft 365: %d problems, want 1", len(m))
	case !slices.Equal(m[0].Tiles, []string{"m365"}) || m[0].Sev != Slow || m[0].Cause != (Cause{Who: WhoApp, Vendor: "microsoft", Evidence: EvAppOnly}):
		t.Errorf("Microsoft 365: %+v", m[0])
	}

	n := find(s.nf, func(x Episode) bool { return slices.Contains(x.Tiles, "netflix") })
	switch {
	case len(n) != 1:
		t.Errorf("Netflix: %d problems, want 1", len(n))
	case n[0].Cause.Who != WhoSpeedtest || n[0].Sev != Slow:
		t.Errorf("Netflix: %+v, want slow during the speed test", n[0])
	}
	nf := snap.Tiles[slices.IndexFunc(profile.Tiles, func(d profile.TileDef) bool { return d.ID == "netflix" })]
	var during, labelled int
	for _, b := range nf.Bars {
		w := window{b.From, b.From.Add(barStep)}
		if w.overlaps(s.speed, s.end) {
			during++
			if b.Speedtest {
				labelled++
			}
		} else if b.Speedtest {
			t.Errorf("Netflix bar %v: labelled with the speed test, which ran %v..%v", b.From, s.speed.from, s.speed.to)
		}
	}
	if during == 0 || labelled != during {
		t.Errorf("Netflix: %d of %d bars during the speed test labelled", labelled, during)
	}
	for i, ts := range snap.Tiles {
		if ts.State != Good {
			t.Errorf("tile %s at the end: %s, want good", profile.Tiles[i].ID, ts.State)
		}
	}
	if c, ok := e.Chart("m365"); !ok || c.UsualMs == nil || len(c.Bands) == 0 {
		t.Errorf("m365 chart: %v, %+v", ok, c)
	} else if x := c.Ms[len(c.Ms)-40]; math.IsNaN(x) || x < 15 || x > 40 {
		t.Errorf("m365 chart before the slowdown: %v ms, want about 22", x)
	}
}

// fakeDuring is a store that, as the replay reads its last day, receives
// samples from the probes (as Observe would during replay).
type fakeDuring struct {
	*store.Fake
	last   time.Time
	during func()
}

func (f *fakeDuring) Raw(ctx context.Context, keys []model.SeriesKey, from, to time.Time, fn func(store.RawPoint) error) error {
	if f.during != nil && !to.Before(f.last.Add(-time.Hour)) {
		f.during()
		f.during = nil
	}
	return f.Fake.Raw(ctx, keys, from, to, fn)
}

// TestEngineObserveDuringReplay: samples that arrive while the replay
// runs, some of them already stored, are applied once, and the result is
// what the live engine has.
func TestEngineObserveDuringReplay(t *testing.T) {
	s := newScene(t)
	arr := eGen(eSeries(s.engine(nil)), s.start, s.end, s.evs, 3)
	t0 := s.drop.from.Add(10 * time.Second).Truncate(tickEvery) // mid-drop
	now := t0.Add(40 * time.Second)
	ctx := context.Background()
	st := &fakeDuring{Fake: stored(arr, t0), last: t0}
	b := s.engine(st)
	var early int
	st.during = func() {
		for _, a := range arr {
			// what arrives during the replay, and again what the store
			// already has of the last 30 s
			if a.arrive.After(t0) && !a.arrive.After(now) || !a.arrive.After(t0) && !a.s.Slot.Before(t0.Add(-bufferFrom)) {
				b.Observe(a.s)
			}
			if !a.arrive.After(t0) && a.s.Slot.Before(t0.Add(-bufferFrom)) && a.s.Slot.After(t0.Add(-2*bufferFrom)) {
				b.Observe(a.s) // older: the store has it, never buffered
				early++
			}
		}
		b.bufMu.Lock()
		n := len(b.buffer)
		b.bufMu.Unlock()
		if n == 0 || early == 0 {
			t.Errorf("buffered %d, older %d: the test feeds nothing", n, early)
		}
	}
	if err := b.replay(ctx, t0); err != nil {
		t.Fatal(err)
	}
	b.goLive(ctx, now)
	if st.during != nil {
		t.Fatal("no Observe during replay")
	}
	s.live(t, arr, []time.Time{now}, func(a *engine, now time.Time) {
		sameEngines(t, "observed during replay", a, b)
	})
}

// TestEngineBufferBounded: the replay buffer keeps the newest samples.
func TestEngineBufferBounded(t *testing.T) {
	s := newScene(t)
	e := s.engine(nil)
	e.bufMax = 16
	e.bufFrom = s.start.UnixMilli()
	k := e.ss[0].key
	for i := range 100 {
		e.Observe(model.Sample{Key: k, Slot: s.start.Add(time.Duration(i) * time.Second), RTT: time.Millisecond})
	}
	if n := len(e.buffer); n > e.bufMax || n == 0 {
		t.Fatalf("buffer %d, want at most %d", n, e.bufMax)
	}
	if last := e.buffer[len(e.buffer)-1].ms; last != s.start.Add(99*time.Second).UnixMilli() {
		t.Errorf("newest buffered %v, want the last sample", msTime(last))
	}
	if !e.bufLost {
		t.Error("overflow not noted")
	}
}

// TestEnginePathChange: a new gateway resets every usual; the first one
// seen does not.
func TestEnginePathChange(t *testing.T) {
	s := newScene(t)
	arr := eGen(eSeries(s.engine(nil)), s.start, s.end, nil, 4)
	e := s.replayed(t, stored(arr, s.end), s.end)
	known := func() int {
		n := 0
		for _, x := range e.ss {
			if x.u.ms > 0 {
				n++
			}
		}
		return n
	}
	if known() == 0 {
		t.Fatal("no usual learned")
	}
	gw1, gw2 := netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("10.0.0.1")
	e.pathChanged(netinfo.Path{Gateway: gw1})
	e.pathChanged(netinfo.Path{Gateway: gw1, Edge: netip.MustParseAddr("198.51.100.1")})
	if known() == 0 {
		t.Fatal("the first path seen reset the usuals")
	}
	e.pathChanged(netinfo.Path{})                            // no route: unknown, not a change
	e.pathChanged(netinfo.Path{Gateway: gw1, Conn: "wired"}) // the uplink only
	if known() == 0 {
		t.Fatal("a lost route or a new uplink reset the usuals")
	}
	e.pathChanged(netinfo.Path{Gateway: gw2})
	if n := known(); n != 0 {
		t.Fatalf("after a new gateway %d usuals known, want 0", n)
	}
	e.advance(context.Background(), s.end.Add(tickEvery))
	for _, ts := range e.Snapshot().Tiles {
		if ts.UsualMs != nil {
			t.Errorf("tile %s: usual %v after the reset", ts.ID, *ts.UsualMs)
		}
	}
	// the path is watched through Sources.PathChanges while Run runs
	ch := make(chan netinfo.Path, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.watchPath(ctx, ch); close(done) }()
	ch <- netinfo.Path{Gateway: gw1}
	cancel()
	<-done
}

// TestEngineCheckingUntilReady: before the replay the card is checking
// and every tile has 24 empty bars.
func TestEngineCheckingUntilReady(t *testing.T) {
	s := newScene(t)
	e := s.engine(store.NewFake())
	if e.Ready() {
		t.Fatal("ready before Run")
	}
	snap := e.Snapshot()
	if snap == nil || snap.Conn.Kind != ConnChecking || snap.Monitored != nTiles {
		t.Fatalf("snapshot %+v", snap)
	}
	for _, ts := range snap.Tiles {
		if ts.State != None || ts.Bars[nBars-1].State != None || ts.Bars[0].From.IsZero() {
			t.Errorf("tile %s: %s, bars %v..%v", ts.ID, ts.State, ts.Bars[0], ts.Bars[nBars-1])
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Run returns once the (empty) replay is done
	if err := e.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestEngineOffTiles: the calls profile leaves the app and stream tiles
// off; they never count.
func TestEngineOffTiles(t *testing.T) {
	r, err := profile.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	p, err := r.Resolve([]string{"calls"}, profile.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(p, profile.ResolveTiles(p), Sources{}, Options{})
	e.goLive(context.Background(), time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC))
	snap := e.Snapshot()
	if snap.Monitored != 4 {
		t.Errorf("monitored %d, want 4", snap.Monitored)
	}
	for _, ts := range snap.Tiles[4:] {
		if ts.State != Off {
			t.Errorf("tile %s: %s, want off", ts.ID, ts.State)
		}
		if _, ok := e.Chart(ts.ID); ok {
			t.Errorf("chart of the off tile %s", ts.ID)
		}
	}
	if _, ok := e.Chart("nope"); ok {
		t.Error("chart of an unknown tile")
	}
}

// TestTileCount: Snapshot's tile array matches profile.Tiles.
func TestTileCount(t *testing.T) {
	if len(profile.Tiles) != nTiles {
		t.Fatalf("profile.Tiles has %d tiles, nTiles is %d", len(profile.Tiles), nTiles)
	}
}

// bigProfile is --profile all,calls (the live instance's): about 1,420
// targets, of which the tiles track a few dozen.
func bigProfile(t testing.TB) *model.Profile {
	t.Helper()
	r, err := profile.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	p, err := r.Resolve([]string{"all", "calls"}, profile.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// untracked are samples of every target the tiles do not track.
func untracked(e *engine, p *model.Profile, at time.Time) []model.Sample {
	var out []model.Sample
	for _, tg := range p.Targets {
		for _, k := range tg.ProbeKinds() {
			key := model.SeriesKey{Target: tg.Name, Kind: k}
			if _, ok := e.index[key]; !ok {
				out = append(out, model.Sample{Key: key, Slot: at, RTT: 12 * time.Millisecond})
			}
		}
	}
	return out
}

// TestObserveUntracked: samples of untracked targets cost no allocation
// and never take the engine's lock (the probes of ~1,420 targets fan out
// to it).
func TestObserveUntracked(t *testing.T) {
	p := bigProfile(t)
	e := newEngine(p, profile.ResolveTiles(p), Sources{}, Options{})
	e.goLive(context.Background(), time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC))
	ss := untracked(e, p, time.Date(2026, 10, 8, 0, 0, 1, 0, time.UTC))
	t.Logf("%d targets, %d tracked series, %d untracked samples", len(p.Targets), len(e.ss), len(ss))
	if len(ss) < 1000 || len(e.ss) < 30 || len(e.ss) > 60 {
		t.Fatalf("profile all,calls: %d untracked samples, %d tracked series", len(ss), len(e.ss))
	}
	e.mu.Lock() // a sample that took the lock would deadlock here
	for _, s := range ss {
		e.Observe(s)
	}
	e.mu.Unlock()
	if n := testing.AllocsPerRun(10, func() {
		for _, s := range ss {
			e.Observe(s)
		}
	}); n != 0 {
		t.Errorf("%v allocations per round of untracked samples", n)
	}
}
