package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store/blob"
	"github.com/fuck-you-isp/fyisp/internal/store/synth"
)

var ctx = context.Background()

// t0 is an hour-aligned start for synthetic data.
var t0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

type testSeries struct {
	key   model.SeriesKey
	iv    time.Duration
	phase time.Duration
	gen   *synth.Series
}

// mix is a small v0.1-like mix, plus a 7s series whose slots do not divide
// the hour.
func mix(seed uint64) []*testSeries { return mixN(seed, 1) }

// mixN is n copies of mix with distinct targets.
func mixN(seed uint64, n int) []*testSeries {
	ivs := []time.Duration{15 * time.Second, 5 * time.Second, 3 * time.Second, 2 * time.Second, 7 * time.Second, 15 * time.Second}
	var out []*testSeries
	for i := range n * len(ivs) {
		iv := ivs[i%len(ivs)]
		out = append(out, &testSeries{
			key:   model.SeriesKey{Target: fmt.Sprintf("target-%d", i/3), Kind: model.ProbeKind(1 + i%3)},
			iv:    iv,
			phase: time.Duration(i*1234) * time.Millisecond % iv,
			gen:   synth.New(seed, i),
		})
	}
	return out
}

func intervals(ss []*testSeries) func(model.SeriesKey) time.Duration {
	m := map[model.SeriesKey]time.Duration{}
	for _, s := range ss {
		m[s.key] = s.iv
	}
	return func(k model.SeriesKey) time.Duration { return m[k] }
}

func keysOf(ss []*testSeries) []model.SeriesKey {
	var out []model.SeriesKey
	for _, s := range ss {
		out = append(out, s.key)
	}
	return out
}

// sampleOf converts a synthetic slot to a sample (not-measured slots become
// Lost with ReasonGap, like the scheduler reports them).
func sampleOf(k model.SeriesKey, t time.Time, x blob.Slot) model.Sample {
	s := model.Sample{Key: k, Slot: t}
	if x.Valid {
		s.RTT = time.Duration(x.Value) * blob.Unit
	} else {
		s.Lost, s.Reason = true, model.Reason(x.Code)
	}
	return s
}

// samples generates every slot of every series in [from, to), dense from
// from (which must be hour-aligned), merged in time order.
func samples(ss []*testSeries, from, to time.Time) []model.Sample {
	var out []model.Sample
	for _, s := range ss {
		for t := from.Add(s.phase); t.Before(to); t = t.Add(s.iv) {
			out = append(out, sampleOf(s.key, t, s.gen.Next()))
		}
	}
	slices.SortStableFunc(out, func(a, b model.Sample) int { return a.Slot.Compare(b.Slot) })
	return out
}

func openT(t testing.TB, dir string, o Options) *SQLite {
	t.Helper()
	s, err := Open(dir, o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rawAll(t testing.TB, r Reader, keys []model.SeriesKey, from, to time.Time) []RawPoint {
	t.Helper()
	var out []RawPoint
	if err := r.Raw(ctx, keys, from, to, func(p RawPoint) error { out = append(out, p); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func equalRaw(t testing.TB, what string, got, want []RawPoint) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d points, want %d", what, len(got), len(want))
	}
	for i := range got {
		g, w := got[i], want[i]
		if g.Key != w.Key || !g.TS.Equal(w.TS) || g.RTTms != w.RTTms || g.Lost != w.Lost || g.Reason != w.Reason {
			t.Fatalf("%s: point %d = %+v, want %+v", what, i, g, w)
		}
	}
}

func near(a, b float32, rel float64) bool {
	if math.IsNaN(float64(a)) || math.IsNaN(float64(b)) {
		return math.IsNaN(float64(a)) && math.IsNaN(float64(b))
	}
	return math.Abs(float64(a)-float64(b)) <= rel*math.Max(1, math.Abs(float64(b)))
}

func equalPanel(t testing.TB, what string, got, want *PanelResult, rel float64) {
	t.Helper()
	if got.Tier != want.Tier || !got.Start.Equal(want.Start) || got.Step != want.Step || len(got.Series) != len(want.Series) {
		t.Fatalf("%s: header %s %v %v %d, want %s %v %v %d", what, got.Tier, got.Start, got.Step, len(got.Series),
			want.Tier, want.Start, want.Step, len(want.Series))
	}
	for i := range got.Series {
		g, w := got.Series[i], want.Series[i]
		if g.Key != w.Key || len(g.N) != len(w.N) {
			t.Fatalf("%s: series %d shape", what, i)
		}
		for b := range g.N {
			if g.N[b] != w.N[b] || g.Lost[b] != w.Lost[b] || !near(g.Mean[b], w.Mean[b], rel) ||
				!near(g.Min[b], w.Min[b], 0) || !near(g.Max[b], w.Max[b], 0) {
				t.Fatalf("%s: %v bucket %d: got n=%d lost=%d mean=%v min=%v max=%v, want n=%d lost=%d mean=%v min=%v max=%v",
					what, g.Key, b, g.N[b], g.Lost[b], g.Mean[b], g.Min[b], g.Max[b], w.N[b], w.Lost[b], w.Mean[b], w.Min[b], w.Max[b])
			}
		}
		for r := model.Reason(0); r <= model.MaxReason; r++ {
			gb, wb := g.LostBy[r], w.LostBy[r]
			for b := range g.N {
				var gv, wv uint32
				if gb != nil {
					gv = gb[b]
				}
				if wb != nil {
					wv = wb[b]
				}
				if gv != wv {
					t.Fatalf("%s: %v bucket %d reason %v: %d, want %d", what, g.Key, b, r, gv, wv)
				}
			}
		}
	}
}

func panel(t testing.TB, r Reader, q PanelQuery) *PanelResult {
	t.Helper()
	p, err := r.Panel(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// comparePanels checks random raw-tier ranges and hour-aligned hourly-tier
// ranges within [from, to] against the fake.
func comparePanels(t *testing.T, what string, s Reader, f *Fake, keys []model.SeriesKey, from, to time.Time, rnd *rand.Rand) {
	t.Helper()
	span := to.Sub(from)
	for i := range 12 {
		d := time.Duration(rnd.Int64N(int64(min(span, RawMaxRange))))
		a := from.Add(time.Duration(rnd.Int64N(int64(span - d + 1)))).Truncate(time.Millisecond)
		if i == 0 {
			a, d = from, min(span, RawMaxRange)
		}
		q := PanelQuery{Keys: keys, From: a, To: a.Add(d), MaxPoints: []int{1, 7, 100, 1000}[i%4]}
		equalPanel(t, fmt.Sprintf("%s raw %v+%v/%d", what, q.From, d, q.MaxPoints), panel(t, s, q), panel(t, f, q), 1e-6)
	}
	if span <= RawMaxRange+time.Hour {
		return
	}
	for i := range 6 {
		h0 := from.Truncate(time.Hour).Add(time.Duration(rnd.IntN(3)) * time.Hour)
		h1 := to.Truncate(time.Hour).Add(-time.Duration(rnd.IntN(2)) * time.Hour)
		if h1.Sub(h0) <= RawMaxRange {
			continue
		}
		q := PanelQuery{Keys: keys, From: h0, To: h1.Add(-time.Millisecond), MaxPoints: []int{10, 60, 1000}[i%3]}
		p := panel(t, s, q)
		if p.Tier != TierHourly {
			t.Fatalf("tier %s", p.Tier)
		}
		equalPanel(t, fmt.Sprintf("%s hourly %v..%v/%d", what, q.From, q.To, q.MaxPoints), p, panel(t, f, q), 1e-4)
	}
}

// TestEquivalence feeds the same samples to Fake and SQLite, flushing every
// 10 simulated minutes and reopening the store once mid-hour, and checks
// Panel (raw and hourly tiers) and Raw agree, including the in-memory
// current hour.
func TestEquivalence(t *testing.T) {
	ss := mix(1)
	keys := keysOf(ss)
	end := t0.Add(52*time.Hour + 17*time.Minute)
	in := samples(ss, t0, end)
	dir := t.TempDir()
	var now time.Time
	o := Options{Interval: intervals(ss), Now: func() time.Time { return now }, Version: "test"}
	s := openT(t, dir, o)
	defer func() { s.Close() }()
	f := NewFake()
	rnd := rand.New(rand.NewPCG(3, 4))
	nextFlush := t0.Add(10 * time.Minute)
	reopened := false
	for i, x := range in {
		now = x.Slot
		if !x.Slot.Before(nextFlush) {
			if err := s.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			nextFlush = nextFlush.Add(10 * time.Minute)
			if !reopened && x.Slot.After(t0.Add(26*time.Hour+30*time.Minute)) {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s = openT(t, dir, o)
				reopened = true
				comparePanels(t, "after reopen", s, f, keys, t0, now, rnd)
			}
		}
		s.Observe(x)
		f.Observe(x)
		if i == len(in)/3 {
			comparePanels(t, "mid-feed", s, f, keys, t0, x.Slot, rnd)
		}
	}
	comparePanels(t, "unflushed tail", s, f, keys, t0, end, rnd)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	comparePanels(t, "flushed", s, f, keys, t0, end, rnd)
	want := rawAll(t, f, keys, t0, end)
	if len(want) != len(in) {
		t.Fatalf("fake raw %d != input %d", len(want), len(in))
	}
	equalRaw(t, "raw", rawAll(t, s, keys, t0, end), want)
	sub0, sub1 := t0.Add(5*time.Hour+123*time.Millisecond), t0.Add(31*time.Hour+7*time.Second)
	equalRaw(t, "raw subrange", rawAll(t, s, keys[2:4], sub0, sub1), rawAll(t, f, keys[2:4], sub0, sub1))

	// Close every hour (the clock moved on), then compare again purely from
	// the database after a reopen.
	now = end.Add(2 * time.Hour)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openT(t, dir, o)
	if open := unsummarized(t, s, false); open != 0 || len(s.cur) != 0 {
		t.Fatalf("%d hours without summary, %d in memory", open, len(s.cur))
	}
	comparePanels(t, "reopened", s, f, keys, t0, end, rnd)
	equalRaw(t, "raw reopened", rawAll(t, s, keys, t0, end), want)

	si, err := s.Series(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := f.Series(ctx)
	if len(si) != len(fi) {
		t.Fatalf("series %d, want %d", len(si), len(fi))
	}
	for i := range si {
		if si[i].Key != fi[i].Key || !si[i].First.Equal(fi[i].First) || !si[i].Last.Equal(fi[i].Last) || si[i].Interval == 0 {
			t.Fatalf("series %d: %+v, want %+v", i, si[i], fi[i])
		}
	}
}

// TestRoundTripCodes stores every reason code, gaps and extreme values and
// reads them back exactly.
func TestRoundTripCodes(t *testing.T) {
	k := model.SeriesKey{Target: "x", Kind: model.KindTCP}
	s := openT(t, t.TempDir(), Options{Interval: func(model.SeriesKey) time.Duration { return time.Second }})
	defer s.Close()
	var in []model.Sample
	tt := t0
	for r := model.Reason(0); r <= model.MaxReason; r++ {
		in = append(in, model.Sample{Key: k, Slot: tt, Lost: true, Reason: r})
		tt = tt.Add(time.Second)
		in = append(in, model.Sample{Key: k, Slot: tt, RTT: time.Duration(r) * 12340 * time.Microsecond})
		tt = tt.Add(time.Second)
	}
	in = append(in, model.Sample{Key: k, Slot: tt, RTT: 0}, model.Sample{Key: k, Slot: tt.Add(time.Second), RTT: time.Duration(blob.MaxValue) * blob.Unit})
	f := NewFake()
	for _, x := range in {
		s.Observe(x)
		f.Observe(x)
	}
	check := func() {
		equalRaw(t, "codes", rawAll(t, s, []model.SeriesKey{k}, t0, tt.Add(time.Hour)), rawAll(t, f, []model.SeriesKey{k}, t0, tt.Add(time.Hour)))
		q := PanelQuery{Keys: []model.SeriesKey{k}, From: t0, To: t0.Add(time.Hour), MaxPoints: 3}
		equalPanel(t, "codes", panel(t, s, q), panel(t, f, q), 1e-6)
	}
	check()
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	check()
}

// TestClock: Sample.Slot is authoritative. A suspend (the clock jumps
// forward) leaves not-measured slots, never loss; a backward step drops
// samples until the clock catches up.
func TestClock(t *testing.T) {
	k := model.SeriesKey{Target: "x", Kind: model.KindHTTPS}
	s := openT(t, t.TempDir(), Options{Interval: func(model.SeriesKey) time.Duration { return 15 * time.Second }})
	defer s.Close()
	ok := func(at time.Time, ms int) model.Sample {
		return model.Sample{Key: k, Slot: at, RTT: time.Duration(ms) * time.Millisecond}
	}
	a := t0.Add(10 * time.Minute)
	s.Observe(ok(a, 10))
	s.Observe(ok(a.Add(15*time.Second), 11))
	// Suspend for 3h07m: the next slot lands in a later hour.
	b := a.Add(3*time.Hour + 7*time.Minute)
	s.Observe(ok(b, 12))
	// Clock steps back 1 minute: dropped until it passes b.
	s.Observe(ok(b.Add(-time.Minute), 99))
	s.Observe(ok(b, 99)) // duplicate slot
	s.Observe(ok(b.Add(7*time.Second), 99))
	s.Observe(ok(b.Add(15*time.Second), 13))
	for pass := range 2 {
		pts := rawAll(t, s, []model.SeriesKey{k}, t0, b.Add(time.Hour))
		var measured, gaps, lost int
		for _, p := range pts {
			switch {
			case !p.Lost:
				measured++
				if p.RTTms == 99 {
					t.Fatalf("pass %d: backward sample stored: %+v", pass, p)
				}
			case p.Reason == model.ReasonGap:
				gaps++
			default:
				lost++
			}
		}
		// Hour 0: 40 leading gaps + 2 values; hour 3: gaps from the
		// hour start to b, then 2 values.
		wantGaps := 40 + int(b.Sub(b.Truncate(time.Hour))/(15*time.Second))
		if measured != 4 || lost != 0 || gaps != wantGaps {
			t.Fatalf("pass %d: measured %d lost %d gaps %d (want 4, 0, %d)", pass, measured, lost, gaps, wantGaps)
		}
		p := panel(t, s, PanelQuery{Keys: []model.SeriesKey{k}, From: t0, To: b.Add(time.Hour), MaxPoints: 10})
		var n, l uint32
		for i := range p.Series[0].N {
			n += p.Series[0].N[i]
			l += p.Series[0].Lost[i]
		}
		if n != 4 || l != 0 || p.Series[0].LostBy != nil {
			t.Fatalf("pass %d: panel n=%d lost=%d", pass, n, l)
		}
		if err := s.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := s.Stats(ctx)
	if st.Rejected != 3 {
		t.Fatalf("rejected %d, want 3", st.Rejected)
	}
}

// TestHourCloseByClock: a series that stops receiving samples has its hour
// closed (final blob + summary) by Flush two minutes after the hour ends,
// and samples for a closed hour are rejected, not written over it.
func TestHourCloseByClock(t *testing.T) {
	k := model.SeriesKey{Target: "x", Kind: model.KindICMP}
	now := t0
	s := openT(t, t.TempDir(), Options{Now: func() time.Time { return now }})
	defer s.Close()
	for i := range 10 {
		s.Observe(model.Sample{Key: k, Slot: t0.Add(time.Duration(i) * 5 * time.Second), RTT: time.Millisecond})
	}
	now = t0.Add(time.Hour + time.Minute)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(summaries(t, s)) != 0 {
		t.Fatal("closed too early")
	}
	now = t0.Add(time.Hour + 2*time.Minute)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	sums := summaries(t, s)
	if u := sums[[2]int64{t0.Unix() / 3600, s.ids[k]}]; len(sums) != 1 || u.n != 10 || u.lost != 0 || u.min != 100 || u.sum != 1000 {
		t.Fatalf("summary %+v", sums)
	}
	s.Observe(model.Sample{Key: k, Slot: t0.Add(30 * time.Minute), RTT: time.Millisecond})
	if st, _ := s.Stats(ctx); st.Rejected != 1 {
		t.Fatalf("late sample for a closed hour: rejected=%d", st.Rejected)
	}
}

// TestReopenFinalizes: an older hour left without a summary (crash between
// writes, or an hour dropped from a full buffer) gets one at Open, and the
// newest open hour is continued, not overwritten.
func TestReopenFinalizes(t *testing.T) {
	dir := t.TempDir()
	k := model.SeriesKey{Target: "x", Kind: model.KindTCP}
	o := Options{Interval: func(model.SeriesKey) time.Duration { return 10 * time.Second }, Now: func() time.Time { return t0 }}
	s := openT(t, dir, o)
	for i := range 2 * 360 { // two full hours
		s.Observe(model.Sample{Key: k, Slot: t0.Add(time.Duration(i) * 10 * time.Second), RTT: time.Duration(i+1) * time.Millisecond})
	}
	s.Observe(model.Sample{Key: k, Slot: t0.Add(2 * time.Hour), Lost: true, Reason: model.ReasonTimeout})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Remove the summaries, as if the process died before writing them.
	db, err := sql.Open("sqlite", filepath.Join(dir, DBName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM summary_1h`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s = openT(t, dir, o)
	defer s.Close()
	sums := summaries(t, s)
	if u := sums[[2]int64{t0.Unix()/3600 + 1, s.ids[k]}]; len(sums) != 2 || u.n != 360 || u.min != 36100 || u.max != 72000 || u.p95 != 70200 {
		t.Fatalf("summaries after reopen: %+v", sums)
	}
	s.Observe(model.Sample{Key: k, Slot: t0.Add(2 * time.Hour), RTT: time.Millisecond}) // duplicate of a stored slot
	s.Observe(model.Sample{Key: k, Slot: t0.Add(2*time.Hour + 30*time.Second), RTT: 5 * time.Millisecond})
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	pts := rawAll(t, s, []model.SeriesKey{k}, t0.Add(2*time.Hour), t0.Add(3*time.Hour))
	if len(pts) != 4 || pts[0].Reason != model.ReasonTimeout || pts[1].Reason != model.ReasonGap || pts[3].RTTms != 5 {
		t.Fatalf("continued hour: %+v", pts)
	}
}

// TestDiskFull: with the database unable to grow, Flush fails, keeps the
// data in memory (visible to readers), reports the error, and a later Flush
// writes everything. The buffer of closed hours is bounded.
func TestDiskFull(t *testing.T) {
	ss := mix(5)
	keys := keysOf(ss)
	var now time.Time
	dir := t.TempDir()
	o := Options{Interval: intervals(ss), Now: func() time.Time { return now }}
	s := openT(t, dir, o)
	defer func() { s.Close() }()
	f := NewFake()
	in := samples(ss, t0, t0.Add(5*time.Hour))
	for _, x := range in[:len(in)/5] {
		s.Observe(x)
		f.Observe(x)
	}
	now = in[len(in)/5].Slot
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var pages int64
	s.db.w.QueryRow(`PRAGMA page_count`).Scan(&pages)
	if _, err := s.db.w.Exec(fmt.Sprintf(`PRAGMA max_page_count=%d`, pages)); err != nil {
		t.Fatal(err)
	}
	for _, x := range in[len(in)/5:] {
		s.Observe(x)
		f.Observe(x)
	}
	now = in[len(in)-1].Slot
	err := s.Flush(ctx)
	st, _ := s.Stats(ctx)
	t.Logf("flush with a full disk: %v; stats %+v", err, st)
	if err == nil || st.LastFlushErr == "" || st.BufferedBytes == 0 {
		t.Fatalf("flush on a full database: err=%v stats=%+v", err, st)
	}
	end := t0.Add(5 * time.Hour)
	equalRaw(t, "while full", rawAll(t, s, keys, t0, end), rawAll(t, f, keys, t0, end))
	q := PanelQuery{Keys: keys, From: t0, To: end, MaxPoints: 50}
	equalPanel(t, "while full", panel(t, s, q), panel(t, f, q), 1e-6)

	if _, err := s.db.w.Exec(`PRAGMA max_page_count=1073741823`); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Stats(ctx); st.LastFlushErr != "" || len(s.pending) != 0 {
		t.Fatalf("after recovery: %+v", st)
	}
	// Everything is on disk: compare from a fresh process's view.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openT(t, dir, o)
	equalRaw(t, "after recovery", rawAll(t, s, keys, t0, end), rawAll(t, f, keys, t0, end))
}

func TestBufferBound(t *testing.T) {
	k := model.SeriesKey{Target: "x", Kind: model.KindICMP}
	const maxBuf = 30 << 10
	now := t0
	s := openT(t, t.TempDir(), Options{Interval: func(model.SeriesKey) time.Duration { return time.Second }, MaxBuffer: maxBuf,
		Now: func() time.Time { return now }})
	defer s.Close()
	s.db.w.Exec(`PRAGMA max_page_count=1`)
	g := synth.New(1, 1)
	for h := range 20 {
		for i := range 3600 {
			now = t0.Add(time.Duration(h)*time.Hour + time.Duration(i)*time.Second)
			s.Observe(sampleOf(k, now, g.Next()))
		}
		s.Flush(ctx) // fails once the file is full; encodes and trims closed hours
		st, _ := s.Stats(ctx)
		if cur := int64(s.cur[k].memSize()); st.BufferedBytes > maxBuf+cur {
			t.Fatalf("hour %d: buffered %d > %d + current hour %d", h, st.BufferedBytes, maxBuf, cur)
		}
	}
	st, _ := s.Stats(ctx)
	t.Logf("20 hours with a full disk: %+v, %d closed hours buffered", st, len(s.pending))
	if st.Dropped == 0 || len(s.pending) == 0 {
		t.Fatalf("buffer: %+v", st)
	}
	s.db.w.Exec(`PRAGMA max_page_count=1073741823`)
}

// TestPrune: retention deletes old hours and the file shrinks.
func TestPrune(t *testing.T) {
	dir := t.TempDir()
	ss := mixN(9, 4)
	var now time.Time
	s := openT(t, dir, Options{Interval: intervals(ss), Now: func() time.Time { return now }})
	defer s.Close()
	hours := 24
	for _, x := range samples(ss, t0, t0.Add(time.Duration(hours)*time.Hour)) {
		s.Observe(x)
	}
	now = t0.Add(time.Duration(hours)*time.Hour + time.Hour)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	st1, _ := s.Stats(ctx)
	if err := s.Prune(ctx, t0.Add(time.Duration(hours/2)*time.Hour)); err != nil {
		t.Fatal(err)
	}
	st2, _ := s.Stats(ctx)
	var fl int64
	s.db.w.QueryRow(`PRAGMA freelist_count`).Scan(&fl)
	t.Logf("prune half: file+wal %d -> %d B, freelist %d, oldest %v -> %v", st1.FileBytes, st2.FileBytes, fl, st1.Oldest, st2.Oldest)
	if st2.FileBytes > st1.FileBytes*65/100 || fl != 0 || !st2.Oldest.Equal(t0.Add(time.Duration(hours/2)*time.Hour)) {
		t.Fatal("prune did not shrink the file")
	}
	pts := rawAll(t, s, keysOf(ss), t0, now)
	if len(pts) == 0 || pts[0].TS.Before(t0.Add(time.Duration(hours/2)*time.Hour)) {
		t.Fatal("pruned data still readable")
	}
}

// TestConcurrent runs readers against a writer flushing continuously (for
// the race detector and the snapshot consistency of memory vs database).
func TestConcurrent(t *testing.T) {
	ss := mix(11)
	keys := keysOf(ss)
	in := samples(ss, t0, t0.Add(6*time.Hour))
	var nowMs atomicInt
	s := openT(t, t.TempDir(), Options{Interval: intervals(ss), Now: func() time.Time { return time.UnixMilli(nowMs.get()) }})
	defer s.Close()
	done := make(chan struct{})
	errc := make(chan error, 8)
	for r := range 4 {
		go func() {
			rnd := rand.New(rand.NewPCG(uint64(r), 0))
			for {
				select {
				case <-done:
					errc <- nil
					return
				default:
				}
				now := time.UnixMilli(nowMs.get())
				from := t0.Add(time.Duration(rnd.Int64N(int64(6 * time.Hour))))
				var err error
				if r%2 == 0 {
					var p *PanelResult
					p, err = s.Panel(ctx, PanelQuery{Keys: keys[3:4], From: from, To: from.Add(2 * time.Hour), MaxPoints: 100})
					// Every slot up to now was observed: no bucket before
					// the last hour may be empty (a lost memory/DB snapshot).
					if err == nil {
						for i := range p.Series[0].N {
							bt := p.Start.Add(time.Duration(i) * p.Step)
							if !bt.Before(from) && bt.Add(p.Step).Before(now.Add(-time.Minute)) && !bt.Add(p.Step).After(from.Add(2*time.Hour)) && p.Series[0].N[i]+p.Series[0].Lost[i] == 0 {
								err = fmt.Errorf("empty bucket %v (now %v)", bt, now)
							}
						}
					}
				} else {
					var cnt int
					err = s.Raw(ctx, keys[:1], t0, now, func(RawPoint) error { cnt++; return nil })
					if want := int(now.Sub(t0)/ss[0].iv) - 1; err == nil && cnt < want {
						err = fmt.Errorf("raw returned %d points, want >= %d", cnt, want)
					}
				}
				if err != nil {
					errc <- err
					return
				}
			}
		}()
	}
	next := t0.Add(time.Minute)
	for _, x := range in {
		s.Observe(x)
		nowMs.set(x.Slot.UnixMilli())
		if x.Slot.After(next) {
			if err := s.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			next = next.Add(time.Minute)
		}
	}
	close(done)
	for range 4 {
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------- schema

func headerPageSize(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var h [100]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		t.Fatalf("header: %v", err)
	}
	ps := int(binary.BigEndian.Uint16(h[16:18]))
	if ps == 1 {
		ps = 65536
	}
	return ps
}

func pragmaInt(t *testing.T, db *sql.DB, p string) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRow("PRAGMA " + p).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", p, err)
	}
	return v
}

func pragmaStr(t *testing.T, db *sql.DB, p string) string {
	t.Helper()
	var v string
	if err := db.QueryRow("PRAGMA " + p).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", p, err)
	}
	return v
}

// TestOpenSettings: the spike S3 G1 gate, on the production Open.
func TestOpenSettings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s := openT(t, dir, Options{Version: "v9.9.9"})
	p := filepath.Join(dir, DBName)
	if ps, av, jm := headerPageSize(t, p), pragmaInt(t, s.db.w, "auto_vacuum"), pragmaStr(t, s.db.w, "journal_mode"); ps != PageSize || av != 2 || jm != "wal" {
		t.Fatalf("page_size %d auto_vacuum %d journal_mode %s", ps, av, jm)
	}
	if syn, uv := pragmaInt(t, s.db.w, "synchronous"), pragmaInt(t, s.db.w, "user_version"); syn != 1 || uv != int64(SchemaVersion()) {
		t.Fatalf("synchronous %d user_version %d", syn, uv)
	}
	if qo, bt := pragmaInt(t, s.db.r, "query_only"), pragmaInt(t, s.db.r, "busy_timeout"); qo != 1 || bt != 5000 {
		t.Fatalf("reader query_only %d busy_timeout %d", qo, bt)
	}
	if _, err := s.db.r.Exec(`INSERT INTO meta VALUES('x', 1)`); err == nil {
		t.Fatal("reader pool accepted a write")
	}
	var v string
	if err := s.db.r.QueryRow(`SELECT value FROM meta WHERE key='created_by_version'`).Scan(&v); err != nil || v != "v9.9.9" {
		t.Fatalf("meta created_by_version %q %v", v, err)
	}
	s.Close()
	s = openT(t, dir, Options{})
	if headerPageSize(t, p) != PageSize || pragmaInt(t, s.db.w, "auto_vacuum") != 2 {
		t.Fatal("settings lost on reopen")
	}
	s.Close()
}

type atomicInt struct {
	mu sync.Mutex
	v  int64
}

func (a *atomicInt) set(v int64) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicInt) get() int64  { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

// summaries decodes every stored summary, keyed by (hour, series id).
func summaries(t testing.TB, s *SQLite) map[[2]int64]summary {
	t.Helper()
	rows, err := s.db.r.Query(`SELECT day, series, data FROM summary_1h`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[[2]int64]summary{}
	for rows.Next() {
		var day, id int64
		var data []byte
		if err := rows.Scan(&day, &id, &data); err != nil {
			t.Fatal(err)
		}
		hs, err := decodeDay(nil, day, data)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hs {
			out[[2]int64{h.hour, id}] = h.s
		}
	}
	return out
}

// unsummarized counts stored hours without a summary (excluding each
// series' newest hour when newestOpen).
func unsummarized(t testing.TB, s *SQLite, newestOpen bool) int {
	t.Helper()
	sums := summaries(t, s)
	q := `SELECT hour, series FROM samples`
	if newestOpen {
		q += ` m WHERE hour < (SELECT max(hour) FROM samples x WHERE x.series = m.series)`
	}
	rows, err := s.db.r.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var hk [2]int64
		rows.Scan(&hk[0], &hk[1])
		if _, ok := sums[hk]; !ok {
			n++
		}
	}
	return n
}

func TestSummaryCodec(t *testing.T) {
	rnd := rand.New(rand.NewPCG(5, 6))
	for range 200 {
		day := int64(20000 + rnd.IntN(1000))
		var hs []hourSummary
		for h := range 24 {
			if rnd.IntN(3) == 0 {
				continue
			}
			var slots []blob.Slot
			g := synth.New(uint64(h), rnd.IntN(100))
			slots = g.Fill(slots, rnd.IntN(400))
			hs = append(hs, hourSummary{day*24 + int64(h), *summarize(slots)})
		}
		got, err := decodeDay(nil, day, appendDay(nil, day, hs))
		if err != nil || !slices.Equal(got, hs) {
			t.Fatalf("round trip: %v\n%+v\n%+v", err, got, hs)
		}
		add := hourSummary{day*24 + int64(rnd.IntN(24)), summary{n: 1, min: 5, max: 5, p95: 5, sum: 5}}
		m := mergeDay(slices.Clone(hs), add)
		if i := slices.IndexFunc(m, func(h hourSummary) bool { return h.hour == add.hour }); i < 0 || m[i] != add ||
			!slices.IsSortedFunc(m, func(a, b hourSummary) int { return int(a.hour - b.hour) }) {
			t.Fatal("merge")
		}
	}
}

func FuzzDecodeDay(f *testing.F) {
	f.Add(appendDay(nil, 1, []hourSummary{{24, summary{n: 3, lost: 1, min: 1, max: 9, p95: 8, sum: 15}}}))
	f.Add([]byte{1, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		hs, err := decodeDay(nil, 1, data)
		if err != nil {
			return
		}
		if again, err := decodeDay(nil, 1, appendDay(nil, 1, hs)); err != nil || !slices.Equal(again, hs) {
			t.Fatalf("re-encode: %v", err)
		}
	})
}
