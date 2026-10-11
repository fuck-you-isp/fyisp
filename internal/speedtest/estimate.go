package speedtest

import (
	"slices"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// A direction that hits the byte cap is measured from where its rate
// levels off, the same way whether or not the cap came before the end of
// the ramp-up (v0.7.0 measured a cap in the ramp-up from the connections'
// start, slow start included, which made fast lines' results bimodal).
//
// Rate methods (Extra.RateMethod; "" for a direction that wasn't capped).
const (
	// MethodPeak90: from the first tick whose smoothed rate reaches 90% of
	// the peak to the last byte.
	MethodPeak90 = "peak90"
	// MethodWhole: no tick qualified (or no per-tick counts: uploads
	// outside Linux), so the v0.7.0 window.
	MethodWhole = "whole"
)

// estimate is a capped direction's steady rate.
type estimate struct {
	bytes      int64         // in the window
	win        time.Duration // the window's length
	start      time.Duration // the window's start, from the data phase's
	method     string
	lowerBound bool // still speeding up at the cap: the speed was at least this
}

// plateau is how close an earlier smoothed tick must come to a peak in the
// last 2 ticks for the rate to count as levelled off.
const plateau = 0.95

// peak90 estimates a capped direction's steady rate. cum[i] is the bytes
// counted by the end of complete tick i+1 (the tick the cap fell in is
// left out); end is when the last byte was counted, endBytes the count
// then, both from the data phase's start.
//
//   - The ticks are smoothed by a 3-tick moving median, so one spike tick
//     neither sets the peak nor starts the window.
//   - The window runs from the first smoothed tick at 90% of the peak to
//     the last byte; its rate is its bytes over its length.
//   - It is a lower bound with fewer than 3 complete ticks in the window,
//     or when the peak is in the last 2 ticks with nothing earlier within
//     5% of it (still rising: no plateau seen).
//   - With no ticks, or none with bytes, it is MethodWhole.
func peak90(cum []int64, tick, end time.Duration, endBytes int64) estimate {
	n := len(cum)
	if n == 0 || end <= 0 {
		return estimate{method: MethodWhole}
	}
	d := make([]float64, n)
	var prev int64
	for i, c := range cum {
		d[i], prev = float64(c-prev), c
	}
	sm := make([]float64, n)
	for i := range d {
		w := slices.Clone(d[max(i-1, 0):min(i+2, n)])
		slices.Sort(w)
		if len(w) == 3 {
			sm[i] = w[1]
		} else {
			sm[i] = (w[0] + w[len(w)-1]) / 2
		}
	}
	pk := 0
	for i := range sm {
		if sm[i] > sm[pk] {
			pk = i
		}
	}
	if sm[pk] <= 0 {
		return estimate{method: MethodWhole}
	}
	k := 0
	for sm[k] < 0.9*sm[pk] {
		k++
	}
	var base int64
	if k > 0 {
		base = cum[k-1]
	}
	e := estimate{method: MethodPeak90, start: time.Duration(k) * tick, bytes: endBytes - base}
	e.win = end - e.start
	if e.win <= 0 || e.bytes <= 0 {
		return estimate{method: MethodWhole}
	}
	if n-k < 3 {
		e.lowerBound = true
	}
	if pk >= n-2 && (n < 3 || slices.Max(sm[:n-2]) < plateau*sm[pk]) {
		e.lowerBound = true
	}
	return e
}

// How patterns use a capped direction (read side; nothing stored is
// rewritten):
//   - a v0.7.0 capped direction (short, no RateMethod) is left out of
//     Baseline, the scores' medians and the finding: its rate included slow
//     start. It is still shown and exported.
//   - a lower bound never enters Baseline. In the scores it counts as 1.0
//     (full speed) when it already reaches the baseline, and is left out
//     otherwise (censored: never read as slow).

// rateUse is a direction's use: unscored (a v0.7.0 capped one), or a lower
// bound.
func rateUse(short bool, x *Extra, i int) (unscored, lowerBound bool) {
	if !short {
		return false, false
	}
	if x.RateMethod[i] == "" {
		return true, false
	}
	return false, x.LowerBound[i]
}

// LowerBounds reports which of t's directions are lower bounds.
func LowerBounds(t *model.SpeedTest) (down, up bool) {
	if !t.DownShort && !t.UpShort {
		return false, false
	}
	x := ParseExtra(t.Extra)
	_, down = rateUse(t.DownShort, &x, 0)
	_, up = rateUse(t.UpShort, &x, 1)
	return down, up
}

// censor is a direction's score as patterns use it (nil: left out).
func censor(sc *float64, short bool, x *Extra, i int) *float64 {
	unscored, lb := rateUse(short, x, i)
	switch {
	case sc == nil || unscored:
		return nil
	case !lb:
		return sc
	case *sc >= 1:
		one := 1.0
		return &one
	}
	return nil
}

// Censored is tests with each direction's score as patterns use it; ts is
// not changed (and is returned as is when nothing was capped).
func Censored(ts []model.SpeedTest) []model.SpeedTest {
	if !slices.ContainsFunc(ts, func(t model.SpeedTest) bool { return t.DownShort || t.UpShort }) {
		return ts
	}
	out := make([]model.SpeedTest, len(ts))
	for i, t := range ts {
		if t.DownShort || t.UpShort {
			x := ParseExtra(t.Extra)
			t.ScoreDown, t.ScoreUp = censor(t.ScoreDown, t.DownShort, &x, 0), censor(t.ScoreUp, t.UpShort, &x, 1)
		}
		out[i] = t
	}
	return out
}

// baselineRate reports whether a direction's rate may enter Baseline:
// neither a v0.7.0 capped one nor a lower bound.
func baselineRate(short bool, x *Extra, i int) bool {
	unscored, lb := rateUse(short, x, i)
	return !unscored && !lb
}
