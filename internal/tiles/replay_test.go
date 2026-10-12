package tiles

// A small engine built only from this package's rules, fed the same
// synthetic day live (results arriving with their lag, ticks at two
// phases) and in replay (slot order, per tick or an hour ahead): every
// minute, tile-minute, word run, drop and episode must be the same. It is
// the contract PR 3's engine follows (see tracker).

import (
	"fmt"
	"math/rand/v2"
	"os"
	"reflect"
	"runtime"
	"slices"
	"testing"
	"time"
	"unsafe"

	"github.com/fuck-you-isp/fyisp/internal/profile"
)

// ---- synthetic data ----

type hSpec struct {
	name  string
	every time.Duration
	off   time.Duration
	rule  runRule
	path  bool
	rttUs uint32
	tmo   time.Duration
}

type hCheck struct {
	s      int
	c      check
	arrive int64
}

func hSpecs10() []hSpec {
	var out []hSpec
	for i := range 2 {
		out = append(out, hSpec{name: fmt.Sprintf("path%d", i), every: time.Second, off: time.Duration(i) * 300 * time.Millisecond, rule: rulePath, path: true, rttUs: 2000, tmo: time.Second})
	}
	for i := range 4 {
		out = append(out, hSpec{name: fmt.Sprintf("web%d", i), every: 15 * time.Second, off: time.Duration(i)*3700*time.Millisecond + 100*time.Millisecond, rule: ruleWeb, rttUs: 20000, tmo: 3 * time.Second})
	}
	for i := range 4 {
		out = append(out, hSpec{name: fmt.Sprintf("udp%d", i), every: 5 * time.Second, off: time.Duration(i)*1100*time.Millisecond + 200*time.Millisecond, rule: ruleUDP, rttUs: 30000, tmo: 3 * time.Second})
	}
	return out
}

type hEvent struct {
	from, to time.Time
	which    func(spec hSpec, i int) bool
	slow     bool
}

// hGen makes dur of checks for specs; seed varies the events.
func hGen(specs []hSpec, start time.Time, dur time.Duration, seed uint64) []hCheck {
	r := rand.New(rand.NewPCG(seed, 7))
	var evs []hEvent
	all := func(hSpec, int) bool { return true }
	for t := start.Add(10 * time.Minute); t.Before(start.Add(dur)); t = t.Add(time.Duration(30+r.IntN(90)) * time.Minute) {
		switch r.IntN(6) {
		case 0: // global drop 10..150 s
			evs = append(evs, hEvent{t, t.Add(time.Duration(10+r.IntN(140)) * time.Second), all, false})
		case 1: // 3 of 4 web lost 1..6 min
			evs = append(evs, hEvent{t, t.Add(time.Duration(60+r.IntN(300)) * time.Second), func(s hSpec, i int) bool { return s.rule == ruleWeb && i%4 != 0 }, false})
		case 2: // 2..3 udp providers lost 8..40 s, staggered
			k := 2 + r.IntN(2)
			base := t
			evs = append(evs, hEvent{base, base.Add(time.Duration(8+r.IntN(32)) * time.Second), func(s hSpec, i int) bool { return s.rule == ruleUDP && (i-6) < k }, false})
		case 3: // latency x5 on the web for 4..15 min
			evs = append(evs, hEvent{t, t.Add(time.Duration(4+r.IntN(11)) * time.Minute), func(s hSpec, i int) bool { return s.rule == ruleWeb }, true})
		case 4: // flapping: each web endpoint loses 2 checks at its own time
			evs = append(evs, hEvent{t, t.Add(31 * time.Second), func(s hSpec, i int) bool { return s.rule == ruleWeb && i%2 == 1 }, false})
		case 5: // short global drop around 5..12 s (path only)
			evs = append(evs, hEvent{t, t.Add(time.Duration(5+r.IntN(8)) * time.Second), func(s hSpec, i int) bool { return s.path }, false})
		}
	}
	sleepFrom := start.Add(dur / 2)
	sleepTo := sleepFrom.Add(17 * time.Minute)
	var out []hCheck
	for i, sp := range specs {
		for t := start.Add(sp.off); t.Before(start.Add(dur)); t = t.Add(sp.every) {
			if !t.Before(sleepFrom) && t.Before(sleepTo) {
				continue
			}
			c := check{ms: t.UnixMilli(), rttUs: sp.rttUs + uint32(r.IntN(int(sp.rttUs/4)+1)), jitUs: noJitter}
			if sp.rule == ruleUDP {
				c.jitUs = uint32(1000 + r.IntN(4000))
				if r.IntN(50) == 0 {
					c.lossPct = 33
				}
			}
			lost := r.IntN(200) == 0
			for _, e := range evs {
				if !t.Before(e.from) && t.Before(e.to) && e.which(sp, i) {
					if e.slow {
						c.rttUs *= 5
					} else {
						lost = true
					}
				}
			}
			arrive := c.ms + int64(c.rttUs/1000)
			if lost {
				c = check{ms: c.ms, lost: true, lossPct: 100, jitUs: noJitter}
				arrive = c.ms + sp.tmo.Milliseconds()
			}
			out = append(out, hCheck{s: i, c: c, arrive: arrive})
		}
	}
	return out
}

// ---- a minimal engine, built only from the PR's APIs ----

type hSer struct {
	sp    hSpec
	tr    *tracker
	runs  []failRun
	byMin map[int64][]check
	mins  []minute
	u     usual
	buf   []failRun
}

type hTile struct {
	kind   profile.TileKind
	web    []int
	udp    []int
	w      word
	runs   []wordRun
	mins   []State
	sigs   []string
	mw, mu [][]*tracker
}

type hEng struct {
	ss       []*hSer
	tiles    []*hTile
	start    time.Time
	nextMin  time.Time
	trimRuns time.Duration // 0: keep every run
	usuals   []*usual
	lastRel  time.Time
}

func newHEng(specs []hSpec, start time.Time) *hEng {
	e := &hEng{start: start, nextMin: start}
	for _, sp := range specs {
		e.ss = append(e.ss, &hSer{sp: sp, tr: &tracker{every: sp.every}, byMin: map[int64][]check{}})
	}
	var web, udp []int
	for i, sp := range specs {
		switch sp.rule {
		case ruleWeb:
			web = append(web, i)
		case ruleUDP:
			udp = append(udp, i)
		}
	}
	if len(web) >= 2 {
		e.tiles = []*hTile{{kind: profile.TileApp, web: web}, {kind: profile.TileCall, web: web[:2], udp: udp}}
	}
	for _, s := range e.ss {
		if s.sp.rule == ruleWeb {
			e.usuals = append(e.usuals, &s.u)
		}
	}
	return e
}

func (e *hEng) add(hc hCheck) {
	s := e.ss[hc.s]
	k := hc.c.at().Truncate(time.Minute).UnixMilli()
	s.byMin[k] = append(s.byMin[k], hc.c)
	s.tr.add(hc.c)
}

// maxHold is the hold of the slowest series: a minute is summarized once
// every series has applied its checks up to the minute's closeAt.
var maxHold = holdFor(15 * time.Second)

func (s *hSer) runsNow() []failRun {
	rs := s.runs
	if r, ok := s.tr.open(); ok {
		s.buf = append(append(s.buf[:0], s.runs...), r)
		return s.buf
	}
	return rs
}

func (s *hSer) set() runSet {
	rs := runSet{runs: slices.Clone(s.runsNow()), every: s.sp.every}
	if s.tr.last != 0 {
		rs.last = time.UnixMilli(s.tr.last).UTC()
	}
	return rs
}

func members(e *hEng, ids []int) [][]*tracker {
	var out [][]*tracker
	for _, i := range ids {
		out = append(out, []*tracker{e.ss[i].tr})
	}
	return out
}

func (e *hEng) tick(wall time.Time) {
	now := wall.Truncate(tickEvery) // the Unix-aligned grid, whatever the timer's phase
	for _, s := range e.ss {
		s.runs = append(s.runs, s.tr.flush(now)...)
	}
	for _, t := range e.tiles {
		var sigs []signal
		var ats []time.Time
		if t.mw == nil {
			t.mw, t.mu = members(e, t.web), members(e, t.udp)
		}
		for _, g := range [][][]*tracker{t.mw, t.mu} {
			if len(g) == 0 {
				continue
			}
			sg, a := immediate(g)
			sigs, ats = append(sigs, sg), append(ats, a)
		}
		sg, a := tileSignal(sigs, ats)
		if r := t.w.outageSignal(sg, a); r != nil {
			t.runs = append(t.runs, *r)
		}
	}
	for !e.nextMin.Add(80*time.Second + maxHold).After(now) {
		from := e.nextMin
		closeAt := from.Add(80 * time.Second)
		states := make([]State, len(e.ss))
		for i, s := range e.ss {
			cs := s.byMin[from.UnixMilli()]
			if e.trimRuns > 0 { // only the runs that can touch this minute
				s.runs = pruneRuns(s.runs, from.Add(-e.trimRuns), s.sp.rule)
			}
			rs := s.runsNow()
			m := summarize(cs, rs, s.sp.rule, from, closeAt)
			s.mins = append(s.mins, m)
			delete(s.byMin, from.UnixMilli())
			switch s.sp.rule {
			case ruleWeb:
				us, ok := s.u.value(closeAt)
				st := judgeWeb(m, limitsFor(us, ok, false), false)
				s.u.add(from, m, st)
				states[i] = st
			case ruleUDP:
				ms := s.mins[max(0, len(s.mins)-3):]
				st, _ := judgeCall(ms, s.sp.every)
				states[i] = st
			}
		}
		if closeAt.Sub(e.lastRel) >= usualEvery {
			relearn(e.usuals, closeAt)
			e.lastRel = closeAt
		}
		for _, t := range e.tiles {
			var web, udp []State
			for _, i := range t.web {
				web = append(web, states[i])
			}
			for _, i := range t.udp {
				udp = append(udp, states[i])
			}
			st := tileMinute(t.kind, web, udp)
			t.mins = append(t.mins, st)
			if r := t.w.step(from, st); r != nil {
				t.runs = append(t.runs, *r)
			}
		}
		e.nextMin = e.nextMin.Add(time.Minute)
	}
}

type hOut struct {
	mins     [][]minute
	tileMins [][]State
	runs     [][]wordRun
	open     []wordRun
	drops    [][]Drop
	conn     []Drop
	cur      *Drop
	eps      []Episode
}

func (e *hEng) result(now time.Time) hOut {
	var o hOut
	for _, s := range e.ss {
		o.mins = append(o.mins, s.mins)
	}
	var tileDs [][]Drop
	var tileEps []Episode
	for i, t := range e.tiles {
		o.tileMins = append(o.tileMins, t.mins)
		o.runs = append(o.runs, t.runs)
		r, _ := t.w.open()
		o.open = append(o.open, r)
		var web []runSet
		for _, j := range t.web {
			web = append(web, e.ss[j].set())
		}
		var udp [][]runSet
		for _, j := range t.udp {
			udp = append(udp, []runSet{e.ss[j].set()})
		}
		ds := tileDrops(web, udp, now)
		o.drops = append(o.drops, ds)
		tileDs = append(tileDs, ds)
		id := fmt.Sprint("tile", i)
		for _, r := range append(slices.Clone(t.runs), func() []wordRun {
			if r, ok := t.w.open(); ok {
				return []wordRun{r}
			}
			return nil
		}()...) {
			tileEps = append(tileEps, runEpisode(id, t.kind, r))
		}
		for _, d := range ds {
			tileEps = append(tileEps, dropEpisode(d, id))
		}
	}
	var layers [nLayers][]runSet
	if len(e.ss) >= 2 {
		layers[layerGateway] = []runSet{e.ss[0].set()}
		layers[layerEdge] = []runSet{e.ss[1].set()}
	}
	o.conn, o.cur = connDrops(layers, tileDs, len(e.tiles), now)
	var conn []Episode
	for _, d := range o.conn {
		conn = append(conn, dropEpisode(d, ""))
	}
	o.eps = groupEpisodes(conn, tileEps, func(Episode) Cause { return Cause{Who: WhoUnknown} }, now)
	return o
}

// ---- feeding modes ----

const tickEvery = 5 * time.Second

func hRunLive(specs []hSpec, cs []hCheck, start, end time.Time, phase time.Duration) hOut {
	cs = slices.Clone(cs)
	slices.SortStableFunc(cs, func(a, b hCheck) int { return int(a.arrive - b.arrive) })
	e := newHEng(specs, start)
	i := 0
	for t := start.Add(phase); !t.After(end); t = t.Add(tickEvery) {
		for i < len(cs) && cs[i].arrive <= t.UnixMilli() {
			e.add(cs[i])
			i++
		}
		e.tick(t)
	}
	return e.result(end)
}

// hRunSlot feeds in slot order (store.Raw); before each tick only checks
// whose slot is at most the tick, or with chunk > 0, a whole chunk ahead.
func hRunSlot(specs []hSpec, cs []hCheck, start, end time.Time, phase, chunk time.Duration) hOut {
	cs = slices.Clone(cs)
	slices.SortStableFunc(cs, func(a, b hCheck) int { return int(a.c.ms - b.c.ms) })
	e := newHEng(specs, start)
	i := 0
	for t := start.Add(phase); !t.After(end); t = t.Add(tickEvery) {
		lim := t
		if chunk > 0 {
			lim = start.Add((t.Sub(start)/chunk + 1) * chunk)
		}
		for i < len(cs) && cs[i].c.ms <= lim.UnixMilli() {
			e.add(cs[i])
			i++
		}
		e.tick(t)
	}
	return e.result(end)
}

func hDiff(t *testing.T, name string, a, b hOut) {
	t.Helper()
	nd := 0
	for i := range a.mins {
		for k := range min(len(a.mins[i]), len(b.mins[i])) {
			if a.mins[i][k] != b.mins[i][k] {
				if nd < 6 {
					t.Errorf("%s: series %d minute %d: %+v vs %+v", name, i, k, a.mins[i][k], b.mins[i][k])
				}
				nd++
			}
		}
		if len(a.mins[i]) != len(b.mins[i]) {
			t.Errorf("%s: series %d: %d vs %d minutes", name, i, len(a.mins[i]), len(b.mins[i]))
		}
	}
	if nd > 0 {
		t.Errorf("%s: %d minute differences", name, nd)
	}
	for i := range a.tileMins {
		n := 0
		for k := range min(len(a.tileMins[i]), len(b.tileMins[i])) {
			if a.tileMins[i][k] != b.tileMins[i][k] {
				n++
			}
		}
		if n > 0 {
			t.Errorf("%s: tile %d: %d tile-minute differences", name, i, n)
		}
		if !reflect.DeepEqual(a.runs[i], b.runs[i]) || a.open[i] != b.open[i] {
			t.Errorf("%s: tile %d word runs differ: %d vs %d", name, i, len(a.runs[i]), len(b.runs[i]))
			showRunDiff(t, a.runs[i], b.runs[i])
		}
		if !reflect.DeepEqual(a.drops[i], b.drops[i]) {
			t.Errorf("%s: tile %d drops differ: %d vs %d", name, i, len(a.drops[i]), len(b.drops[i]))
		}
	}
	if !reflect.DeepEqual(a.conn, b.conn) || !reflect.DeepEqual(a.cur, b.cur) {
		t.Errorf("%s: conn drops differ: %d vs %d", name, len(a.conn), len(b.conn))
	}
	if !reflect.DeepEqual(a.eps, b.eps) {
		t.Errorf("%s: episodes differ: %d vs %d", name, len(a.eps), len(b.eps))
	}
}

func showRunDiff(t *testing.T, a, b []wordRun) {
	t.Helper()
	n := 0
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y wordRun
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			t.Logf("   run %d: %v..%v %s  vs  %v..%v %s", i, x.Start.Format("15:04:05"), x.End.Format("15:04:05"), x.Sev, y.Start.Format("15:04:05"), y.End.Format("15:04:05"), y.Sev)
			if n++; n > 4 {
				return
			}
		}
	}
}

func TestHarnessLiveVsReplay(t *testing.T) {
	specs := hSpecs10()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	for _, seed := range []uint64{1, 2, 3} {
		cs := hGen(specs, start, 24*time.Hour, seed)
		live := hRunLive(specs, cs, start, end, 0)
		nr := 0
		for _, r := range live.runs {
			nr += len(r)
		}
		t.Logf("seed %d: %d checks, word runs %d, tile drops %d/%d, conn drops %d, episodes %d", seed, len(cs), nr, len(live.drops[0]), len(live.drops[1]), len(live.conn), len(live.eps))
		hDiff(t, fmt.Sprint(seed, " live phase 0 vs live phase 2.5s"), live, hRunLive(specs, cs, start, end, 2500*time.Millisecond))
		hDiff(t, fmt.Sprint(seed, " live vs slot-order phase 0"), live, hRunSlot(specs, cs, start, end, 0, 0))
		hDiff(t, fmt.Sprint(seed, " live vs slot-order phase 1.3s"), live, hRunSlot(specs, cs, start, end, 1300*time.Millisecond, 0))
		hDiff(t, fmt.Sprint(seed, " live vs slot-order 1h chunks"), live, hRunSlot(specs, cs, start, end, 0, time.Hour))
	}
}

// ---- 45-series, 8-day replay cost ----

func hSpecs45() []hSpec {
	var out []hSpec
	for i := range 2 {
		out = append(out, hSpec{name: "path", every: time.Second, off: time.Duration(i) * 300 * time.Millisecond, rule: rulePath, path: true, rttUs: 2000, tmo: time.Second})
	}
	for i := range 4 {
		out = append(out, hSpec{name: "path5", every: 5 * time.Second, off: time.Duration(i) * 700 * time.Millisecond, rule: rulePath, path: true, rttUs: 9000, tmo: 3 * time.Second})
	}
	for i := range 6 {
		out = append(out, hSpec{name: "udp", every: 5 * time.Second, off: time.Duration(i)*800*time.Millisecond + 100*time.Millisecond, rule: ruleUDP, rttUs: 30000, tmo: 3 * time.Second})
	}
	for i := range 33 {
		out = append(out, hSpec{name: "web", every: 15 * time.Second, off: time.Duration(i) * 450 * time.Millisecond, rule: ruleWeb, rttUs: 20000, tmo: 3 * time.Second})
	}
	return out
}

func newHEng45(specs []hSpec, start time.Time) *hEng {
	e := newHEng(specs, start)
	// 11 tiles: 9 web-only of 3 endpoints, 2 call tiles of 3 web + 6 udp
	var web, udp []int
	for i, sp := range specs {
		switch sp.rule {
		case ruleWeb:
			web = append(web, i)
		case ruleUDP:
			udp = append(udp, i)
		}
	}
	e.tiles = nil
	for k := range 11 {
		w := web[k*3 : k*3+3]
		t := &hTile{kind: profile.TileApp, web: w}
		if k >= 9 {
			t.kind, t.udp = profile.TileCall, udp
		}
		e.tiles = append(e.tiles, t)
	}
	e.trimRuns = 2 * time.Minute
	return e
}

// TestHarnessReplayCost (FYISP_PERF=1, without -race) times an 8-day
// replay of 45 series and 11 tiles through these rules and reports the
// memory it keeps.
func TestHarnessReplayCost(t *testing.T) {
	if os.Getenv("FYISP_PERF") != "1" {
		t.Skip("set FYISP_PERF=1")
	}
	specs := hSpecs45()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	dur := 8 * 24 * time.Hour
	end := start.Add(dur)
	cs := hGen(specs, start, dur, 9)
	slices.SortStableFunc(cs, func(a, b hCheck) int { return int(a.c.ms - b.c.ms) })
	t.Logf("checks: %d", len(cs))

	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	m0.HeapAlloc -= uint64(cap(cs)) * uint64(unsafe.Sizeof(hCheck{}))
	t0w := time.Now()
	e := newHEng45(specs, start)
	i := 0
	var tTick, tAdd time.Duration
	for tt := start; !tt.After(end); tt = tt.Add(tickEvery) {
		a := time.Now()
		for i < len(cs) && cs[i].c.ms <= tt.UnixMilli() {
			e.add(cs[i])
			i++
		}
		b := time.Now()
		e.tick(tt)
		tAdd += b.Sub(a)
		tTick += time.Since(b)
	}
	walk := time.Since(t0w)
	a := time.Now()
	o := e.result(end)
	tRes := time.Since(a)
	a = time.Now()
	for k, tl := range e.tiles {
		from := end.Add(-time.Duration(len(tl.mins)) * time.Minute)
		_ = bars(end, from, tl.mins, o.drops[k], o.eps, nil)
	}
	tBars := time.Since(a)
	runtime.ReadMemStats(&m1)
	total := time.Since(t0w)
	t.Logf("replay walk %v (add %v, tick %v), drops+episodes %v, bars %v, TOTAL %v", walk, tAdd, tTick, tRes, tBars, total)
	t.Logf("allocs: %d objects, %.1f MB", m1.Mallocs-m0.Mallocs, float64(m1.TotalAlloc-m0.TotalAlloc)/1e6)
	nr := 0
	for _, s := range e.ss {
		nr += len(s.runs)
	}
	t.Logf("runs kept: %d; episodes %d; conn drops %d", nr, len(o.eps), len(o.conn))
	cs = nil
	runtime.GC()
	runtime.ReadMemStats(&m1)
	t.Logf("heap held by the engine after replay (8 d of minutes, all runs, words, usual): %.1f MB (before: %.1f MB)", float64(m1.HeapAlloc)/1e6, float64(m0.HeapAlloc)/1e6)
	t.Logf("sizes: check %d, minute %d, failRun %d, wordRun %d, Episode %d, Drop %d, usualMin %d", unsafe.Sizeof(check{}), unsafe.Sizeof(minute{}), unsafe.Sizeof(failRun{}), unsafe.Sizeof(wordRun{}), unsafe.Sizeof(Episode{}), unsafe.Sizeof(Drop{}), unsafe.Sizeof(usualMin{}))
	nm, ng := 0, 0
	for _, s := range e.ss {
		nm += len(s.mins)
		ng += cap(s.u.good) + cap(s.u.recent)
	}
	t.Logf("minutes %d (%.1f MB), usual mins %d (%.1f MB), runs %d (%.1f MB)", nm, float64(nm)*float64(unsafe.Sizeof(minute{}))/1e6, ng, float64(ng)*float64(unsafe.Sizeof(usualMin{}))/1e6, nr, float64(nr)*float64(unsafe.Sizeof(failRun{}))/1e6)
	runtime.KeepAlive(e)
}

// Targeted: a short majority-failing window between ticks.
func hUDPCase() ([]hSpec, []hCheck, time.Time, time.Time) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var specs []hSpec
	for i := range 4 {
		specs = append(specs, hSpec{every: 5 * time.Second, off: time.Duration(i)*1100*time.Millisecond + 200*time.Millisecond, rule: ruleUDP, rttUs: 30000, tmo: 3 * time.Second})
	}
	lostAt := map[int][]int{0: {12, 13}, 2: {12, 13}, 3: {12, 13}} // slot indexes
	var cs []hCheck
	for i, sp := range specs {
		for k := 0; k < 60; k++ {
			t := start.Add(sp.off + time.Duration(k)*sp.every)
			c := check{ms: t.UnixMilli(), rttUs: 30000, jitUs: 2000}
			arrive := c.ms + 30
			if slices.Contains(lostAt[i], k) {
				c = check{ms: c.ms, lost: true, lossPct: 100, jitUs: noJitter}
				arrive = c.ms + 3000
			}
			cs = append(cs, hCheck{s: i, c: c, arrive: arrive})
		}
	}
	return specs, cs, start, start.Add(5 * time.Minute)
}

func hUDPEng(specs []hSpec, start time.Time) *hEng {
	e := newHEng(specs, start)
	e.tiles = []*hTile{{kind: profile.TileCall, udp: []int{0, 1, 2, 3}}}
	return e
}

func TestHarnessPhaseUDP(t *testing.T) {
	specs, cs, start, end := hUDPCase()
	var first []wordRun
	for i, ph := range []time.Duration{0, 1000 * time.Millisecond, 2500 * time.Millisecond, 4000 * time.Millisecond} {
		sorted := slices.Clone(cs)
		slices.SortStableFunc(sorted, func(a, b hCheck) int { return int(a.arrive - b.arrive) })
		e := hUDPEng(specs, start)
		j := 0
		for tt := start.Add(ph); !tt.After(end); tt = tt.Add(tickEvery) {
			for j < len(sorted) && sorted[j].arrive <= tt.UnixMilli() {
				e.add(sorted[j])
				j++
			}
			e.tick(tt)
		}
		runs := e.tiles[0].runs
		if i == 0 {
			first = runs
			if len(runs) != 1 {
				t.Errorf("phase 0: runs %+v, want the short majority outage", runs)
			}
		} else if !reflect.DeepEqual(runs, first) {
			t.Errorf("phase %v: runs %+v, want %+v", ph, runs, first)
		}
	}
}
