package tiles

import (
	"slices"
	"testing"
	"time"
)

const ms10 = 10 * time.Millisecond

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
	}
	for _, c := range cases {
		rs := runs(pattern(c.every, t0, c.p, ms10))
		if len(rs) != 1 {
			t.Errorf("%s: runs %+v", c.name, rs)
			continue
		}
		if r := rs[0]; r.est() != c.est || r.prior != c.prior || r.open() {
			t.Errorf("%s: got est %v prior %v open %v, want %v %v", c.name, r.est(), r.prior, r.open(), c.est, c.prior)
		}
	}
	// an open run is estimated as if its next check answered
	if r := runs(pattern(time.Second, t0, "."+rep("x", 12), ms10))[0]; !r.open() || r.est() != 12*time.Second {
		t.Errorf("open run: got %+v est %v", r, r.est())
	}
}

func TestTileDrops(t *testing.T) {
	const web, stun = 15 * time.Second, 5 * time.Second
	now := at(time.Hour)
	w := func(ps ...string) []tail {
		var out []tail
		for _, p := range ps {
			out = append(out, pattern(web, t0, p, ms10))
		}
		return out
	}
	u := func(ps ...string) [][]tail {
		var out [][]tail
		for _, p := range ps {
			out = append(out, []tail{pattern(stun, t0, p, ms10)})
		}
		return out
	}
	d := func(from, to, dur time.Duration) []Drop { return []Drop{{Start: at(from), End: at(to), Dur: dur}} }
	cases := []struct {
		name string
		web  []tail
		udp  [][]tail
		want []Drop
	}{
		{"web: 2 failed checks at 15 s are not a drop", w("...xx..."), nil, nil},
		{"web: 3 failed checks are (45 s, about 50 s)", w("...xxx..."), nil, d(45*time.Second, 90*time.Second, 50*time.Second)},
		{"single-endpoint tiles get drops", w("..xxxx.."), nil, d(30*time.Second, 90*time.Second, time.Minute)},
		{"a minority of endpoints is not a drop", w("...xxxx...", "..........", ".........."), nil, nil},
		{"a majority is", w("...xxxx...", "...xxxx...", ".........."), nil, d(45*time.Second, 105*time.Second, time.Minute)},
		{"together: only the overlap counts", w("..xxxx....", "....xxxx.."), nil, d(60*time.Second, 90*time.Second, 30*time.Second)},
		{"2 endpoints, 1 failing: a tie", w("..xxxx....", ".........."), nil, nil},
		{"STUN: a 25 s run is not a drop", nil, u(".." + rep("x", 5) + ".."), nil},
		{"STUN: a 35 s run is", nil, u(".." + rep("x", 7) + ".."), d(10*time.Second, 45*time.Second, 40*time.Second)},
		{"STUN: shared path, 1 of 2 providers", nil, u(".."+rep("x", 7)+"..", rep(".", 11)), nil},
		{"after a sleep: no drop", w("..______xxxx.."), nil, nil},
		{"after no data: no drop", w("xxxx...."), nil, nil},
		{"restart in the middle of an outage: no drop", w("...____xxx..."), nil, nil},
	}
	for _, c := range cases {
		got := tileDrops(c.web, c.udp, now)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}

	// a late timeout arrives after a later slot's success: same drop
	tl := pattern(web, t0, "...xxx...", ms10)
	late := slices.Clone(tl.checks)
	late[4], late[6] = late[6], late[4]
	got := tileDrops([]tail{{every: web, checks: late}}, nil, now)
	if want := tileDrops([]tail{tl}, nil, now); len(want) != 1 || !slices.Equal(got, want) {
		t.Errorf("late timeout: got %+v, want %+v", got, want)
	}
	// and the late timeout ends the run where it belongs, after the
	// minute was judged: a run still open is in progress
	if got := tileDrops(w("...xxx"), nil, at(90*time.Second)); len(got) != 1 || got[0].Ended() || got[0].Dur != 50*time.Second {
		t.Errorf("in progress: got %+v", got)
	}
}

func TestConnDrops(t *testing.T) {
	sec := time.Second
	gw := func(p string) [nLayers][]tail {
		var l [nLayers][]tail
		l[layerGateway] = []tail{pattern(sec, t0, p, ms10)}
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
	}
	for _, c := range cases {
		got, cur := connDrops(gw(c.p), nil, 0, end(c.p))
		if !slices.Equal(got, c.want) || (cur == nil) != (c.cur == nil) || cur != nil && *cur != *c.cur {
			t.Errorf("%s: got %+v %+v, want %+v %+v", c.name, got, cur, c.want, c.cur)
		}
	}

	// the anycast layer loses everything only when all its series do
	var l [nLayers][]tail
	l[layerAnycast] = []tail{
		pattern(5*sec, t0, ".."+rep("x", 8)+rep(".", 4), ms10),
		pattern(5*sec, t0, rep(".", 14), ms10),
	}
	if got, _ := connDrops(l, nil, 0, at(70*sec)); got != nil {
		t.Errorf("one resolver: got %+v", got)
	}
	l[layerAnycast][1] = pattern(5*sec, t0, ".."+rep("x", 8)+rep(".", 4), ms10)
	if got, _ := connDrops(l, nil, 0, at(70*sec)); len(got) != 1 || got[0].Dur != 40*sec {
		t.Errorf("both resolvers: got %+v", got)
	}
	// an open run on the anycast layer is no drop in progress
	l[layerAnycast] = []tail{pattern(5*sec, t0, ".."+rep("x", 8), ms10)}
	if got, cur := connDrops(l, nil, 0, at(50*sec)); got != nil || cur != nil {
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
	if got, _ := connDrops([nLayers][]tail{}, tiles(6), 11, now); !slices.Equal(got, []Drop{tile}) {
		t.Errorf("6 of 11 tiles: got %+v", got)
	}
	if got, _ := connDrops([nLayers][]tail{}, tiles(5), 11, now); got != nil {
		t.Errorf("5 of 11 tiles: got %+v", got)
	}
	// the 1 s path gives the duration when it saw the drop too
	p := gw(rep(".", 62) + rep("x", 40) + rep(".", 20))
	if got, _ := connDrops(p, tiles(6), 11, now); !slices.Equal(got, []Drop{{at(62 * sec), at(102 * sec), 40 * sec}}) {
		t.Errorf("path and tiles: got %+v", got)
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

// TestDropRecovery: after a 40 s drop of a single-endpoint tile, the tile
// is Good again within one interval of the first answer and notes the
// drop; the drop's checks don't make its minutes bad.
func TestDropRecovery(t *testing.T) {
	const every = 15 * time.Second
	full := "...." + "xxx" + rep(".", 9) // lost at 1:00, 1:15, 1:30; answers from 1:45
	var w word
	steps(&w, at(-5*time.Minute), "ggggg")
	var notedAt time.Time
	for i := 4; i <= len(full); i++ {
		now := at(time.Duration(i-1) * every)
		tl := pattern(every, t0, full[:i], ms10)
		sig, since := immediate([][]tail{{tl}})
		w.outageSignal(sig, since, now)
		if s, _ := w.state(); s == Good && i > 7 && notedAt.IsZero() {
			notedAt = now
			if d := lastDrop(tileDrops([]tail{tl}, nil, now), Good, now); d == nil || d.Dur != 50*time.Second {
				t.Errorf("note: got %+v", d)
			}
		}
		if i == 7 {
			if s, since := w.state(); s != Down || !since.Equal(at(time.Minute)) {
				t.Errorf("during the drop: got %s %v", s, since)
			}
		}
	}
	if want := at(2 * time.Minute); !notedAt.Equal(want) {
		t.Errorf("good again at %v, want %v (one interval after the first answer)", notedAt, want)
	}
	tl := pattern(every, t0, full, ms10)
	for _, m := range []time.Time{t0, at(time.Minute), at(2 * time.Minute)} {
		if got := judgeWeb(summarize(tl, m), limitsFor(10, true, false), false); got.s != Good && got.s != None {
			t.Errorf("minute %v: got %s", m, got.s)
		}
	}

	// a tile-only failure under 30 s: a transient Down at most, no drop
	short := "...." + "xx" + rep(".", 6)
	if ds := tileDrops([]tail{pattern(every, t0, short, ms10)}, nil, at(5*time.Minute)); ds != nil {
		t.Errorf("short failure: got %+v", ds)
	}
}
