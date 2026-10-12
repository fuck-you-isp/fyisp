package tiles

import (
	"slices"
	"time"
)

// usual is one series' usual latency: the median of its good minute
// medians over the last usualWindow, recomputed every usualEvery from a
// sorted copy. Loss is never "usual". Until it has minGood good minutes
// (a new series, or after a reset) the series is judged on the fixed
// limits; once known, it lasts while the window holds any good minute.
type usual struct {
	good   []usualMin // good minutes of the last usualWindow, oldest first
	recent []usualMin // every minute with data of the last relearnAfter, oldest first
	ms     float64    // the usual; 0 when unknown
	at     time.Time  // last recompute; zero: due
}

type usualMin struct {
	at    time.Time
	ms    float64
	lossy bool  // some check failed
	s     State // the minute's judgement under the usual at the time
}

// add records the closed minute at at with its judgement. A good minute
// (not lossy, not slow, not none) joins the usual.
func (u *usual) add(at time.Time, m minute, s State) {
	if m.ok == 0 || s == None {
		return
	}
	x := usualMin{at: at, ms: m.medMs(), lossy: m.lost > 0, s: s}
	u.recent = append(u.recent, x)
	i := 0
	for i < len(u.recent) && !u.recent[i].at.After(at.Add(-relearnAfter)) {
		i++
	}
	u.recent = u.recent[i:]
	if s == Good && !x.lossy {
		u.good = append(u.good, x)
	}
}

// value returns the usual at now, recomputing it when usualEvery has
// passed since the last time.
func (u *usual) value(now time.Time) (float64, bool) {
	if u.at.IsZero() || now.Sub(u.at) >= usualEvery {
		u.recompute(now)
	}
	return u.ms, u.ms > 0
}

func (u *usual) recompute(now time.Time) {
	u.at = now
	i := 0
	for i < len(u.good) && !u.good[i].at.After(now.Add(-usualWindow)) {
		i++
	}
	u.good = slices.Delete(u.good, 0, i)
	// minGood good minutes establish a usual; once known it follows
	// whatever good minutes the window still holds
	if len(u.good) == 0 || (u.ms == 0 && len(u.good) < minGood) {
		u.ms = 0
		return
	}
	u.ms = median(msOf(u.good))
}

// shifted reports a route change: the last relearnAfter are a stable
// latency shift without loss, up (every minute with data latency-bad) or
// down (every one below usual/shiftDown), with data in at least shiftData
// of its minutes and p90/p10 under shiftStable.
func (u *usual) shifted(now time.Time) bool {
	if u.ms <= 0 {
		return false
	}
	var v []float64
	up, down := true, true
	for _, x := range u.recent {
		if x.at.Before(now.Add(-relearnAfter)) {
			continue
		}
		if x.lossy {
			return false
		}
		up = up && x.s.Bad()
		down = down && x.ms < u.ms/shiftDown
		v = append(v, x.ms)
	}
	if len(v) < shiftData || !(up || down) {
		return false
	}
	slices.Sort(v)
	p10, p90 := v[len(v)/10], v[len(v)*9/10]
	return p10 > 0 && p90/p10 < shiftStable
}

// rebase makes the last relearnAfter the usual.
func (u *usual) rebase(now time.Time) {
	u.good = u.good[:0]
	for _, x := range u.recent {
		if !x.at.Before(now.Add(-relearnAfter)) {
			u.good = append(u.good, x)
		}
	}
	u.ms = median(msOf(u.good))
	u.at = now
}

func msOf(xs []usualMin) []float64 {
	v := make([]float64, len(xs))
	for i, x := range xs {
		v[i] = x.ms
	}
	return v
}

// reset forgets the usual (the gateway or ISP edge changed: a laptop that
// moved). The fixed limits apply until minGood good minutes.
func (u *usual) reset() { *u = usual{} }

// relearn rebases the series that shifted, unless half or more of all
// tracked series shifted at once: a shared shift is congestion on the
// access link, not a route change. It returns how many were rebased.
func relearn(us []*usual, now time.Time) int {
	var s []*usual
	for _, u := range us {
		if u.shifted(now) {
			s = append(s, u)
		}
	}
	if 2*len(s) >= len(us) {
		return 0
	}
	for _, u := range s {
		u.rebase(now)
	}
	return len(s)
}

// learning reports the first day after the first stored sample, when the
// Home says fyisp is still learning and the fixed limits apply.
func learning(now, firstData time.Time) bool {
	return firstData.IsZero() || now.Sub(firstData) < learnFor
}

// stale reports that nothing was measured for staleAfter.
func stale(now, lastMeasured time.Time) bool {
	return lastMeasured.IsZero() || now.Sub(lastMeasured) >= staleAfter
}
