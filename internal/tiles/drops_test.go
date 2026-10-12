package tiles

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"
	"unsafe"
)

func TestRoundDur(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		29 * time.Second: 30 * time.Second, 31 * time.Second: 30 * time.Second, 35 * time.Second: 30 * time.Second,
		45 * time.Second: 40 * time.Second, 46 * time.Second: 50 * time.Second, 5 * time.Second: 10 * time.Second,
		0: 0,
	} {
		if got := roundDur(in); got != want {
			t.Errorf("%v: got %v, want %v", in, got, want)
		}
	}
}

func TestRunEstimate(t *testing.T) {
	cases := []struct {
		name  string
		every time.Duration
		p     string
		est   time.Duration
		prior bool
	}{
		{"1 s: 29 lost", time.Second, "." + rep("x", 29) + ".", 29 * time.Second, true},
		{"1 s: 30 lost", time.Second, "." + rep("x", 30) + ".", 30 * time.Second, true},
		{"1 s: 31 lost", time.Second, "." + rep("x", 31) + ".", 31 * time.Second, true},
		{"5 s: 7 lost", 5 * time.Second, "." + rep("x", 7) + ".", 35 * time.Second, true},
		{"15 s: 2 lost", 15 * time.Second, ".xx.", 30 * time.Second, true},
		{"15 s: 3 lost", 15 * time.Second, ".xxx.", 45 * time.Second, true},
		{"a gap before: counted from the last success", 15 * time.Second, "._xx.", 45 * time.Second, true},
		{"after a sleep: no prior success", 15 * time.Second, ".___xx.", 30 * time.Second, false},
		{"no data before", 15 * time.Second, "xx.", 30 * time.Second, false},
		{"longer than any tail: 10 minutes", 15 * time.Second, "." + rep("x", 40) + ".", 10 * time.Minute, true},
	}
	for _, c := range cases {
		_, s := series(c.every, t0, c.p)
		if len(s.runs) != 1 {
			t.Errorf("%s: runs %+v", c.name, s.runs)
			continue
		}
		if r := s.runs[0]; r.est() != c.est || r.prior != c.prior || r.open() {
			t.Errorf("%s: got est %v prior %v open %v, want %v %v", c.name, r.est(), r.prior, r.open(), c.est, c.prior)
		}
	}
	// an open run is estimated as if its next check answered
	if _, s := series(time.Second, t0, "."+rep("x", 12)); len(s.runs) != 1 || !s.runs[0].open() || s.runs[0].est() != 12*time.Second {
		t.Errorf("open run: got %+v", s.runs)
	}
}

func TestTracker(t *testing.T) {
	const every = time.Second
	// ICMP times out after 3 s: slot 5's loss arrives after slots 6 and 7
	in := pattern(every, t0, "....."+rep("x", 35)+rep(".", 5), ms10)
	late := slices.Clone(in)
	late[4], late[5], late[6] = late[5], late[6], late[4]
	_, want := feed(every, in)
	_, got := feed(every, late)
	if !slices.Equal(got.runs, want.runs) || len(want.runs) != 1 || want.runs[0].lost != 35 {
		t.Errorf("late result: got %+v, want %+v", got.runs, want.runs)
	}
	// a result later than reorderFor is ignored, a duplicate too
	tr := &tracker{every: every}
	for _, c := range pattern(every, t0, rep(".", 10), ms10) {
		tr.add(c)
	}
	tr.add(check{ms: at(2 * time.Second).UnixMilli(), lost: true})
	tr.add(check{ms: at(9 * time.Second).UnixMilli(), lost: true})
	tr.flush(at(time.Minute))
	if _, open := tr.open(); open || tr.okN != 10 || tr.okAt != at(time.Second).UnixMilli() {
		t.Errorf("too late: got %+v", tr)
	}
	// gaps are not checks
	tr.add(check{ms: at(10 * time.Second).UnixMilli(), gap: true})
	if tr.last != at(9*time.Second).UnixMilli() {
		t.Errorf("gap: last %v", tr.last)
	}

	// a sleep inside a run ends it: STUN OK at 0, lost at 5 s and at 2 h,
	// OK at 2 h 5 s is two short runs, not a 2-hour drop
	const stun = 5 * time.Second
	cs := []check{
		{ms: t0.UnixMilli(), rttUs: 10000},
		{ms: at(stun).UnixMilli(), lost: true, lossPct: 100},
		{ms: at(2 * time.Hour).UnixMilli(), lost: true, lossPct: 100},
		{ms: at(2*time.Hour + stun).UnixMilli(), rttUs: 10000},
	}
	_, s := feed(stun, cs)
	if len(s.runs) != 2 || s.runs[0].est() != stun || !s.runs[0].prior || s.runs[1].prior || s.runs[1].est() != stun {
		t.Errorf("sleep inside a run: got %+v", s.runs)
	}
	if ds := tileDrops(nil, [][]runSet{{s}}, at(3*time.Hour)); ds != nil {
		t.Errorf("sleep inside a run: drops %+v", ds)
	}
}

func TestTileDrops(t *testing.T) {
	const web, stun = 15 * time.Second, 5 * time.Second
	now := at(time.Hour)
	w := func(ps ...string) []runSet {
		var out []runSet
		for _, p := range ps {
			_, s := series(web, t0, p)
			out = append(out, s)
		}
		return out
	}
	u := func(ps ...string) [][]runSet {
		var out [][]runSet
		for _, p := range ps {
			_, s := series(stun, t0, p)
			out = append(out, []runSet{s})
		}
		return out
	}
	d := func(from, to, dur time.Duration) []Drop { return []Drop{{Start: at(from), End: at(to), Dur: dur}} }
	cases := []struct {
		name string
		web  []runSet
		udp  [][]runSet
		want []Drop
	}{
		{"web: 2 failed checks at 15 s are not a drop", w("...xx..."), nil, nil},
		{"web: 3 failed checks are (45 s, about 40 s)", w("...xxx..."), nil, d(45*time.Second, 90*time.Second, 40*time.Second)},
		{"single-endpoint tiles get drops", w("..xxxx.."), nil, d(30*time.Second, 90*time.Second, time.Minute)},
		{"longer than any tail", w(".." + rep("x", 40) + ".."), nil, d(30*time.Second, 630*time.Second, 10*time.Minute)},
		{"a minority of endpoints is not a drop", w("...xxxx...", "..........", ".........."), nil, nil},
		{"a majority is", w("...xxxx...", "...xxxx...", ".........."), nil, d(45*time.Second, 105*time.Second, time.Minute)},
		{"together: only the overlap counts", w("..xxxx....", "....xxxx.."), nil, d(60*time.Second, 90*time.Second, 30*time.Second)},
		{"2 endpoints, 1 failing: a tie", w("..xxxx....", ".........."), nil, nil},
		{"endpoints without checks don't count", w("..xxxx....", ""), nil, d(30*time.Second, 90*time.Second, time.Minute)},
		{"STUN: a 25 s run is not a drop", nil, u(".." + rep("x", 5) + ".."), nil},
		{"STUN: a 35 s run is (about 30 s)", nil, u(".." + rep("x", 7) + ".."), d(10*time.Second, 45*time.Second, 30*time.Second)},
		{"STUN: no success before: no drop", nil, u(rep("x", 9) + ".."), nil},
		{"STUN: after a sleep: no drop", nil, u(".." + rep("_", 6) + rep("x", 9) + ".."), nil},
		{"STUN: shared path, 1 of 2 providers", nil, u(".."+rep("x", 7)+"..", rep(".", 11)), nil},
		{"after a sleep: no drop", w("..______xxxx.."), nil, nil},
		{"after no data: no drop", w("xxxx...."), nil, nil},
		{"restart in the middle of an outage: no drop", w("...____xxx..."), nil, nil},
	}
	for _, c := range cases {
		if got := tileDrops(c.web, c.udp, now); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
	// in progress
	if got := tileDrops(w("...xxx"), nil, at(90*time.Second)); len(got) != 1 || got[0].Ended() || got[0].Dur != 40*time.Second {
		t.Errorf("in progress: got %+v", got)
	}
}

func TestConnDrops(t *testing.T) {
	sec := time.Second
	gw := func(p string) [nLayers][]runSet {
		var l [nLayers][]runSet
		_, s := series(sec, t0, p)
		l[layerGateway] = []runSet{s}
		return l
	}
	end := func(p string) time.Time { return at(time.Duration(len(p)) * sec) }
	cases := []struct {
		name string
		p    string
		want []Drop
		cur  *Drop
	}{
		{"29 s on 1 s probes: not a drop", ".." + rep("x", 29) + rep(".", 15), nil, nil},
		{"31 s: a drop of about 30 s", ".." + rep("x", 31) + rep(".", 15), []Drop{{at(2 * sec), at(33 * sec), 30 * sec}}, nil},
		{"in progress after 5 s", ".." + rep("x", 5), nil, &Drop{Start: at(2 * sec), Dur: 10 * sec}},
		{"not after 4 s", ".." + rep("x", 4), nil, nil},
		{"not ended until OK for 10 s", ".." + rep("x", 40) + rep(".", 9), nil, &Drop{Start: at(2 * sec), Dur: 40 * sec}},
		{"ended after 10 s OK", ".." + rep("x", 40) + rep(".", 10), []Drop{{at(2 * sec), at(42 * sec), 40 * sec}}, nil},
		{"an answer in the middle doesn't split it", ".." + rep("x", 20) + "..." + rep("x", 15) + rep(".", 12),
			[]Drop{{at(2 * sec), at(40 * sec), 40 * sec}}, nil},
		{"scattered loss is no drop", rep(rep(".", 8)+"x", 20) + rep(".", 12), nil, nil},
		{"10 minutes, still in progress", ".." + rep("x", 600), nil, &Drop{Start: at(2 * sec), Dur: 10 * time.Minute}},
		{"10 minutes, ended", ".." + rep("x", 600) + rep(".", 12), []Drop{{at(2 * sec), at(602 * sec), 10 * time.Minute}}, nil},
	}
	for _, c := range cases {
		got, cur := connDrops(gw(c.p), nil, 0, end(c.p))
		if !slices.Equal(got, c.want) || (cur == nil) != (c.cur == nil) || cur != nil && *cur != *c.cur {
			t.Errorf("%s: got %+v %+v, want %+v %+v", c.name, got, cur, c.want, c.cur)
		}
	}

	// the anycast layer loses everything only when all its series do
	ac := func(ps ...string) [nLayers][]runSet {
		var l [nLayers][]runSet
		for _, p := range ps {
			_, s := series(5*sec, t0, p)
			l[layerAnycast] = append(l[layerAnycast], s)
		}
		return l
	}
	drop := ".." + rep("x", 8) + rep(".", 4)
	if got, _ := connDrops(ac(drop, rep(".", 14)), nil, 0, at(70*sec)); got != nil {
		t.Errorf("one resolver: got %+v", got)
	}
	if got, _ := connDrops(ac(drop, drop), nil, 0, at(70*sec)); len(got) != 1 || got[0].Dur != 40*sec {
		t.Errorf("both resolvers: got %+v", got)
	}
	// an open run on the anycast layer is no drop in progress
	if got, cur := connDrops(ac(".."+rep("x", 8)), nil, 0, at(50*sec)); got != nil || cur != nil {
		t.Errorf("anycast in progress: got %+v %+v", got, cur)
	}

	// more than half of the covered tiles in a drop together
	tile := Drop{Start: at(time.Minute), End: at(2 * time.Minute), Dur: time.Minute}
	tiles := func(n int) [][]Drop {
		out := make([][]Drop, 11)
		for i := range n {
			out[i] = []Drop{tile}
		}
		return out
	}
	now := at(time.Hour)
	if got, _ := connDrops([nLayers][]runSet{}, tiles(6), 11, now); !slices.Equal(got, []Drop{tile}) {
		t.Errorf("6 of 11 tiles: got %+v", got)
	}
	if got, _ := connDrops([nLayers][]runSet{}, tiles(5), 11, now); got != nil {
		t.Errorf("5 of 11 tiles: got %+v", got)
	}
	// the 1 s path gives the duration when it saw the drop too
	p := gw(rep(".", 62) + rep("x", 40) + rep(".", 20))
	if got, _ := connDrops(p, tiles(6), 11, now); !slices.Equal(got, []Drop{{at(62 * sec), at(102 * sec), 40 * sec}}) {
		t.Errorf("path and tiles: got %+v", got)
	}
}

func TestCover(t *testing.T) {
	now := at(time.Minute)
	// a window that ends exactly now has ended; an open one has not
	got := cover([][]window{{{t0, now}}, {{at(10 * time.Second), time.Time{}}}}, 1, now)
	if len(got) != 1 || !got[0].to.IsZero() {
		t.Errorf("union with an open window: got %+v", got)
	}
	got = cover([][]window{{{t0, now}}}, 1, now)
	if len(got) != 1 || !got[0].to.Equal(now) {
		t.Errorf("ended now: got %+v", got)
	}
}

func TestLastDrop(t *testing.T) {
	ds := []Drop{
		{Start: at(0), End: at(40 * time.Second), Dur: 40 * time.Second},
		{Start: at(10 * time.Minute), End: at(11 * time.Minute), Dur: time.Minute},
		{Start: at(20 * time.Minute)}, // in progress
	}
	if d := lastDrop(ds, Good, at(20*time.Minute)); d == nil || d.Dur != time.Minute {
		t.Errorf("newest only: got %+v", d)
	}
	if d := lastDrop(ds, Good, at(40*time.Minute)); d == nil || d.Dur != time.Minute {
		t.Errorf("29 min after: got %+v", d)
	}
	if d := lastDrop(ds, Good, at(41*time.Minute)); d != nil {
		t.Errorf("30 min after: got %+v", d)
	}
	if d := lastDrop(ds, Slow, at(20*time.Minute)); d != nil {
		t.Errorf("only while Good: got %+v", d)
	}
}

// TestDropRecovery: after a 45 s drop of a single-endpoint tile, fed one
// check at a time with a tick 5 s after each, the tile is Good again at
// the second answer (one interval after the first) and notes the drop;
// the drop's checks don't make its minutes bad.
func TestDropRecovery(t *testing.T) {
	const every = 15 * time.Second
	cs := pattern(every, t0, "...."+"xxx"+rep(".", 9), ms10) // lost at 1:00, 1:15, 1:30
	tr := &tracker{every: every}
	var w word
	steps(&w, at(-5*time.Minute), "ggggg")
	var runs []failRun
	var goodAt time.Time
	for _, c := range cs {
		now := c.at().Add(5 * time.Second)
		runs = append(runs, tr.add(c)...)
		runs = append(runs, tr.flush(now)...)
		sig, a := immediate([][]*tracker{{tr}})
		w.outageSignal(tileSignal([]signal{sig}, []time.Time{a}))
		s, since := w.state()
		if c.ms == at(90*time.Second).UnixMilli() && (s != Down || !since.Equal(at(time.Minute))) {
			t.Errorf("during the drop: got %s %v", s, since)
		}
		if s == Good && goodAt.IsZero() && c.ms > at(90*time.Second).UnixMilli() {
			goodAt = c.at()
			rs := runSet{runs: runs, last: c.at(), every: every}
			if d := lastDrop(tileDrops([]runSet{rs}, nil, now), Good, now); d == nil || d.Dur != 40*time.Second {
				t.Errorf("note: got %+v", d)
			}
		}
	}
	if want := at(2 * time.Minute); !goodAt.Equal(want) {
		t.Errorf("good again at %v, want %v (one interval after the first answer)", goodAt, want)
	}
	for _, m := range []time.Time{t0, at(time.Minute), at(2 * time.Minute)} {
		if got := judgeWeb(summarize(cs, runs, ruleWeb, m, m.Add(80*time.Second)), limitsFor(10, true, false), false); got != Good && got != None {
			t.Errorf("minute %v: got %s", m, got)
		}
	}

	// a tile-only failure under 30 s: a transient Down at most, no drop
	if _, s := series(every, t0, "...."+"xx"+rep(".", 6)); tileDrops([]runSet{s}, nil, at(5*time.Minute)) != nil {
		t.Error("short failure: a drop")
	}
}

// TestCheckSize: a check stays small; the engine keeps a few minutes of
// them per tracked series.
func TestCheckSize(t *testing.T) {
	if n := unsafe.Sizeof(check{}); n > 24 {
		t.Errorf("check is %d bytes", n)
	}
}

// BenchmarkTrackerReplay8d is a 1 s path series (the busiest) over an
// 8-day replay through its tracker, with 1% loss in short runs and a
// 40 s drop an hour.
func BenchmarkTrackerReplay8d(b *testing.B) {
	r := rand.New(rand.NewPCG(1, 2))
	cs := make([]check, 8*24*3600)
	for i := range cs {
		cs[i] = check{ms: at(time.Duration(i) * time.Second).UnixMilli(), rttUs: 2000}
		if r.IntN(100) == 0 || i%3600 < 40 {
			cs[i] = check{ms: cs[i].ms, lost: true, lossPct: 100}
		}
	}
	b.ReportAllocs()
	for range b.N {
		tr := &tracker{every: time.Second}
		n := 0
		for _, c := range cs {
			n += len(tr.add(c))
		}
		if n == 0 {
			b.Fatal("no runs")
		}
	}
}
