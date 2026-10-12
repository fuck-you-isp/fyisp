package tiles

import (
	"math/rand/v2"
	"testing"
	"time"
)

func webMin(medMs float64, lost int) minute {
	return minute{ok: 4, lost: uint16(lost), medUs: uint32(medMs * 1000)}
}

// feed adds n minutes from start, one a minute.
func feed(u *usual, start time.Time, n int, medMs float64, lost int, s State) time.Time {
	for i := range n {
		u.add(start.Add(time.Duration(i)*time.Minute), webMin(medMs, lost), s)
	}
	return start.Add(time.Duration(n) * time.Minute)
}

func TestUsualGoodMinutesOnly(t *testing.T) {
	var u usual
	next := feed(&u, t0, minGood-1, 10, 0, Good)
	if _, ok := u.value(next); ok {
		t.Error("59 good minutes: the usual should be unknown")
	}
	next = feed(&u, next, 1, 10, 0, Good)
	next = feed(&u, next, 100, 80, 0, Slow) // slow minutes are left out
	next = feed(&u, next, 100, 1, 1, Good)  // lossy minutes are left out
	next = feed(&u, next, 100, 1, 0, None)  // none
	u.add(next, minute{lost: 4}, Down)      // nothing answered
	if ms, ok := u.value(next.Add(time.Minute)); !ok || ms != 10 {
		t.Errorf("got %v %v, want 10", ms, ok)
	}
}

func TestUsualWindowSlides(t *testing.T) {
	var u usual
	feed(&u, t0, 100, 10, 0, Good)
	feed(&u, at(10*time.Hour), 100, 30, 0, Good)
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
// equals the median of a full sort of the good minutes in the window.
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
		ms := 10 + r.Float64()*20
		s := Good
		if r.IntN(5) == 0 {
			s = Slow
		}
		u.add(now, webMin(ms, 0), s)
		if s == Good {
			all = append(all, gm{now, float64(webMin(ms, 0).medUs) / 1000})
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
			last, lastAt = got, now
		} else if got != last {
			t.Fatalf("minute %d: changed between recomputes: %v -> %v", i, last, got)
		}
	}
}

func TestUsualRebase(t *testing.T) {
	seed := func() (*usual, time.Time) {
		u := &usual{}
		next := feed(u, t0, 24*60, 10, 0, Good)
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
	others := []*usual{u}
	for range 3 {
		o, _ := seed()
		others = append(others, o)
	}
	if n := relearn(others, next); n != 1 {
		t.Fatalf("rebased %d, want 1", n)
	}
	if ms, ok := u.value(next); !ok || ms != 41.5 {
		t.Errorf("rebased usual: got %v, want 41.5", ms)
	}

	// a shift shorter than relearnAfter is not a route change
	u, next = seed()
	next = shift(u, next, 50)
	if u.shifted(next) {
		t.Error("50 minutes: shifted")
	}

	// downward: below usual/1.5 for 60 minutes
	u, next = seed()
	next = feed(u, next, 60, 5, 0, Good)
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
	next = feed(u, next, 1, 10, 0, Good)
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
}

func TestUsualReset(t *testing.T) {
	var u usual
	next := feed(&u, t0, 24*60, 10, 0, Good)
	if _, ok := u.value(next); !ok {
		t.Fatal("no usual")
	}
	// the gateway changed: fixed limits until minGood good minutes
	u.reset()
	ms, ok := u.value(next)
	if ok || limitsFor(ms, ok, false) != (limits{learnSlowMs, learnDownMs}) {
		t.Errorf("after reset: got %v %v", ms, ok)
	}
	next = feed(&u, next, minGood, 50, 0, Good)
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
