package tiles

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

func webMin(medMs float64, lost int) minute {
	return minute{ok: 4, lost: uint16(lost), medUs: uint32(medMs * 1000)}
}

// addMins adds n minutes from start, one a minute.
func addMins(u *usual, start time.Time, n int, medMs float64, lost int, s State) time.Time {
	for i := range n {
		u.add(start.Add(time.Duration(i)*time.Minute), webMin(medMs, lost), s)
	}
	return start.Add(time.Duration(n) * time.Minute)
}

func TestUsualGoodMinutesOnly(t *testing.T) {
	var u usual
	next := addMins(&u, t0, minGood-1, 10, 0, Good)
	if _, ok := u.value(next); ok {
		t.Error("59 good minutes: the usual should be unknown")
	}
	next = addMins(&u, next, 1, 10, 0, Good)
	next = addMins(&u, next, 100, 80, 0, Slow) // slow minutes are left out
	next = addMins(&u, next, 100, 1, 1, Good)  // lossy minutes are left out
	next = addMins(&u, next, 100, 1, 0, None)  // none
	u.add(next, minute{lost: 4}, Down)         // nothing answered
	if ms, ok := u.value(next.Add(time.Minute)); !ok || ms != 10 {
		t.Errorf("got %v %v, want 10", ms, ok)
	}
}

func TestUsualWindowSlides(t *testing.T) {
	var u usual
	addMins(&u, t0, 100, 10, 0, Good)
	addMins(&u, at(10*time.Hour), 100, 30, 0, Good)
	if ms, _ := u.value(at(12 * time.Hour)); ms != 20 {
		t.Errorf("both: got %v, want 20", ms)
	}
	// the first 100 minutes are more than 24 h old
	if ms, _ := u.value(at(25*time.Hour + 41*time.Minute)); ms != 30 {
		t.Errorf("slid: got %v, want 30", ms)
	}
	// nothing left: unknown again
	if _, ok := u.value(at(36 * time.Hour)); ok {
		t.Error("empty window: the usual should be unknown")
	}
}

// TestUsualRecompute: the usual changes only every usualEvery, and then
// equals the median of a full sort of the good minutes in the window; the
// incrementally sorted window always equals a sort of the good minutes.
func TestUsualRecompute(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	var u usual
	type gm struct {
		at time.Time
		ms float64
	}
	var all []gm
	last := 0.0
	var lastAt time.Time
	for i := range 30 * 60 {
		now := at(time.Duration(i) * time.Minute)
		m := webMin(10+r.Float64()*20, 0)
		s := Good
		if r.IntN(5) == 0 {
			s = Slow
		}
		u.add(now, m, s)
		if s == Good {
			all = append(all, gm{now, m.medMs()})
		}
		got, _ := u.value(now)
		if lastAt.IsZero() || now.Sub(lastAt) >= usualEvery {
			var v []float64
			for _, g := range all {
				if g.at.After(now.Add(-usualWindow)) {
					v = append(v, g.ms)
				}
			}
			want := 0.0
			if len(v) >= minGood {
				want = median(v)
			}
			if got != want {
				t.Fatalf("minute %d: got %v, want %v", i, got, want)
			}
			slices.Sort(v)
			if !slices.Equal(u.sorted, v) {
				t.Fatalf("minute %d: the sorted window differs from a sort", i)
			}
			last, lastAt = got, now
		} else if got != last {
			t.Fatalf("minute %d: changed between recomputes: %v -> %v", i, last, got)
		}
	}
}

func TestUsualRebase(t *testing.T) {
	seed := func() (*usual, time.Time) {
		u := &usual{}
		next := addMins(u, t0, 24*60, 10, 0, Good)
		u.value(next)
		return u, next
	}
	// stable: 40..43 ms, all latency-bad without loss
	shift := func(u *usual, start time.Time, n int) time.Time {
		for i := range n {
			u.add(start.Add(time.Duration(i)*time.Minute), webMin(40+float64(i%4), 0), Slow)
		}
		return start.Add(time.Duration(n) * time.Minute)
	}

	u, next := seed()
	next = shift(u, next, 60)
	all := []*usual{u}
	for range 3 {
		o, _ := seed()
		all = append(all, o)
	}
	if n := relearn(all, next); n != 1 {
		t.Fatalf("rebased %d, want 1", n)
	}
	if ms, ok := u.value(next); !ok || ms != 41.5 {
		t.Errorf("rebased usual: got %v, want 41.5", ms)
	}
	// the minutes judged under the old usual are forgotten: no second
	// rebase from them, and the next minutes are judged afresh
	if len(u.recent) != 0 || u.shifted(next) {
		t.Errorf("after a rebase: recent %d", len(u.recent))
	}
	// the rebased usual lasts past later recomputes
	next = addMins(u, next, 30, 42, 0, Good)
	if ms, ok := u.value(next); !ok || ms < 41 || ms > 42 {
		t.Errorf("after a rebase and 30 minutes: got %v %v", ms, ok)
	}

	// a shift shorter than relearnAfter is not a route change
	u, next = seed()
	next = shift(u, next, 50)
	if u.shifted(next) {
		t.Error("50 minutes: shifted")
	}

	// downward: below usual/1.5 for 60 minutes
	u, next = seed()
	next = addMins(u, next, 60, 5, 0, Good)
	if !u.shifted(next) {
		t.Error("down: not shifted")
	}

	// loss is never rebased
	u, next = seed()
	next = shift(u, next, 59)
	u.add(next, webMin(41, 1), Slow)
	if u.shifted(next.Add(time.Minute)) {
		t.Error("lossy: shifted")
	}

	// unstable: p90/p10 ≥ 1.3
	u, next = seed()
	for i := range 60 {
		u.add(next.Add(time.Duration(i)*time.Minute), webMin([]float64{40, 60}[i%2], 0), Slow)
	}
	if u.shifted(next.Add(time.Hour)) {
		t.Error("unstable: shifted")
	}

	// some good minutes in between: not a shift
	u, next = seed()
	next = shift(u, next, 30)
	next = addMins(u, next, 1, 10, 0, Good)
	next = shift(u, next, 29)
	if u.shifted(next) {
		t.Error("interrupted: shifted")
	}

	// half or more of the series shifted together: congestion, no rebase
	var us []*usual
	var end time.Time
	for i := range 4 {
		u, next := seed()
		end = next.Add(relearnAfter)
		if i < 2 {
			shift(u, next, 60)
		}
		us = append(us, u)
	}
	if !us[0].shifted(end) {
		t.Fatal("shared shift: not shifted")
	}
	if n := relearn(us, end); n != 0 {
		t.Errorf("shared shift: rebased %d", n)
	}
	if ms, _ := us[0].value(end); ms != 10 {
		t.Errorf("shared shift: usual %v, want 10", ms)
	}

	// a rebase without minutes keeps the usual
	u, next = seed()
	u.recent = nil
	u.rebase(next)
	if ms, _ := u.value(next); ms != 10 {
		t.Errorf("empty rebase: got %v", ms)
	}
}

func TestUsualReset(t *testing.T) {
	var u usual
	next := addMins(&u, t0, 24*60, 10, 0, Good)
	if _, ok := u.value(next); !ok {
		t.Fatal("no usual")
	}
	// the gateway changed: fixed limits until minGood good minutes
	u.reset()
	ms, ok := u.value(next)
	if ok || limitsFor(ms, ok, false) != (limits{learnSlowMs, learnDownMs}) {
		t.Errorf("after reset: got %v %v", ms, ok)
	}
	next = addMins(&u, next, minGood, 50, 0, Good)
	if ms, ok := u.value(next); !ok || ms != 50 {
		t.Errorf("after %d good minutes: got %v %v", minGood, ms, ok)
	}
}

func TestLearningAndStale(t *testing.T) {
	if !learning(at(23*time.Hour+59*time.Minute), t0) || learning(at(24*time.Hour), t0) || !learning(t0, time.Time{}) {
		t.Error("learning")
	}
	if stale(at(119*time.Second), t0) || !stale(at(120*time.Second), t0) || !stale(t0, time.Time{}) {
		t.Error("stale")
	}
}

// BenchmarkUsualReplay8d is one series' usual over an 8-day replay: a
// minute added and the value read every minute, a relearn check every
// usualEvery. The replay budget (2 s for ~45 series) wants well under
// 10 ms per series.
func BenchmarkUsualReplay8d(b *testing.B) {
	r := rand.New(rand.NewPCG(1, 2))
	mins := make([]minute, 8*24*60)
	for i := range mins {
		mins[i] = webMin(10+r.Float64()*20, 0)
	}
	b.ReportAllocs()
	for range b.N {
		var u usual
		us := []*usual{&u}
		for i, m := range mins {
			now := at(time.Duration(i) * time.Minute)
			ms, ok := u.value(now)
			u.add(now, m, judgeWeb(m, limitsFor(ms, ok, false), false))
			if i%10 == 0 {
				relearn(us, now)
			}
		}
	}
}
