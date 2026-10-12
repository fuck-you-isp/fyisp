package tiles

import (
	"slices"
	"time"
)

// usual is one series' usual latency: the median of its good minute
// medians over the last usualWindow, recomputed every usualEvery. The good
// minutes are also kept sorted (a binary-search insert per minute, a
// delete per expired one), so a recompute costs O(expired) and not a sort:
// an 8-day replay of 45 series stays within its budget. Loss is never
// "usual". Until it has minGood good minutes (a new series, or after a
// reset) the series is judged on the fixed limits; once known, it lasts
// while the window holds any good minute.
type usual struct {
	good   []usualMin // good minutes of the last usualWindow, oldest first
	sorted []float64  // their medians, sorted
	recent []usualMin // every minute with data of the last relearnAfter, oldest first
	ms     float64    // the usual; 0 when unknown
	at     time.Time  // last recompute; zero: due
	buf    []float64  // shifted's scratch
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
	i := 0
	for i < len(u.recent) && u.recent[i].at.Before(at.Add(-relearnAfter+time.Minute)) {
		i++
	}
	u.recent = append(slices.Delete(u.recent, 0, i), x)
	if s == Good && !x.lossy {
		u.good = append(u.good, x)
		j, _ := slices.BinarySearch(u.sorted, x.ms)
		u.sorted = slices.Insert(u.sorted, j, x.ms)
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
		j, _ := slices.BinarySearch(u.sorted, u.good[i].ms)
		u.sorted = slices.Delete(u.sorted, j, j+1)
		i++
	}
	u.good = slices.Delete(u.good, 0, i)
	// minGood good minutes establish a usual; once known it follows
	// whatever good minutes the window still holds
	n := len(u.sorted)
	if n == 0 || (u.ms == 0 && n < minGood) {
		u.ms = 0
		return
	}
	if n%2 == 1 {
		u.ms = u.sorted[n/2]
	} else {
		u.ms = (u.sorted[n/2-1] + u.sorted[n/2]) / 2
	}
}

// shifted reports a route change: the last relearnAfter are a stable
// latency shift without loss, up (every minute with data latency-bad) or
// down (every one below usual/shiftDown), with data in at least shiftData
// of its minutes and p90/p10 under shiftStable.
func (u *usual) shifted(now time.Time) bool {
	if u.ms <= 0 {
		return false
	}
	v := u.buf[:0]
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
	u.buf = v
	if len(v) < shiftData || !(up || down) {
		return false
	}
	slices.Sort(v)
	p10, p90 := v[len(v)/10], v[len(v)*9/10]
	return p10 > 0 && p90/p10 < shiftStable
}

// rebase makes the last relearnAfter the usual. The minutes judged under
// the old usual are forgotten.
func (u *usual) rebase(now time.Time) {
	var good []usualMin
	for _, x := range u.recent {
		if !x.at.Before(now.Add(-relearnAfter)) {
			x.s = Good
			good = append(good, x)
		}
	}
	if len(good) == 0 {
		return
	}
	u.good, u.recent = good, nil
	u.sorted = u.sorted[:0]
	for _, x := range good {
		u.sorted = append(u.sorted, x.ms)
	}
	slices.Sort(u.sorted)
	u.ms = median(u.sorted)
	u.at = now
}

// reset forgets the usual (the gateway or ISP edge changed: a laptop that
// moved). The fixed limits apply until minGood good minutes.
func (u *usual) reset() { *u = usual{} }

// relearn rebases the series that shifted, unless half or more of all
// tracked series shifted at once: a shared shift is congestion on the
// access link, not a route change. (So with a single tracked series a
// rebase never happens.) Call it every usualEvery, after value. It returns
// how many were rebased.
func relearn(us []*usual, now time.Time) int {
	n := 0
	for _, u := range us {
		if u.shifted(now) {
			n++
		}
	}
	if n == 0 || 2*n >= len(us) {
		return 0
	}
	for _, u := range us {
		if u.shifted(now) {
			u.rebase(now)
		}
	}
	return n
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
