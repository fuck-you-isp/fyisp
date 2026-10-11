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

// rising is how far the window's last tick may read above its first for
// the rate to count as levelled off.
const rising = 0.05

// peak90 estimates a capped direction's steady rate. cum[i] is the bytes
// counted by the end of complete tick i+1 (the tick the cap fell in is
// left out); end is when the last byte was counted, endBytes the count
// then, both from the data phase's start.
//
//   - The ticks are smoothed by a 3-tick moving median (2 ticks averaged at
//     the edges), so one spike tick neither sets the peak nor starts the
//     window.
//   - The window runs from the first smoothed tick at 90% of the peak to
//     the last byte; its rate is its bytes over its length.
//   - It is a lower bound (still speeding up at the cap) when fewer than 2
//     smoothed ticks reach 90% of the peak, or when the window's last tick
//     reads more than 5% above its first.
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
	near := 0
	for _, v := range sm {
		if v >= 0.9*sm[pk] {
			near++
		}
	}
	if near < 2 || sm[n-1] > (1+rising)*sm[k] {
		e.lowerBound = true
	}
	return e
}

// How patterns use a capped direction (read side; nothing stored is
// rewritten). A lower bound (still speeding up at the cap, so the speed
// was at least this) is a new result flagged so, or a v0.7.0 capped
// direction capped in its ramp-up (RampMs 0: its rate included slow
// start). It never enters Baseline. Per server and direction over
// GuardSpan, the coverage guard (tooFast) decides how the rest go:
//   - covered: lower bounds count as at least normal (score max(score,
//     1)) in the patterns and the finding;
//   - too fast to measure from here: its scores are left out of the
//     patterns and the finding (its rows are still shown and exported).

// rateUse reports whether a direction (0 down, 1 up) is a lower bound.
func rateUse(short bool, x *Extra, i int) bool {
	if !short {
		return false
	}
	if x.RateMethod[i] == "" { // v0.7.0
		return x.RampMs[i] == 0
	}
	return x.LowerBound[i]
}

// LowerBounds reports which of t's directions are lower bounds.
func LowerBounds(t *model.SpeedTest) (down, up bool) {
	if !t.DownShort && !t.UpShort {
		return false, false
	}
	x := ParseExtra(t.Extra)
	return rateUse(t.DownShort, &x, 0), rateUse(t.UpShort, &x, 1)
}

// baselineRate reports whether a direction's rate may enter Baseline.
func baselineRate(short bool, x *Extra, i int) bool { return !rateUse(short, x, i) }

// The coverage guard: over GuardSpan, a server's direction is "too fast to
// measure from here" when it has lower bounds and its other (baseline)
// results cover fewer than guardHours distinct local hours, or more than
// guardShare of them fall within any guardWindow consecutive hours: they
// are then no sample of normal (a fast line's daytime reads as lower
// bounds, leaving the evening's slow results).
const (
	GuardSpan   = 14 * 24 * time.Hour
	guardHours  = 12
	guardWindow = 6
	guardShare  = 0.5
)

// covered applies the guard to the local hours of a server's baseline
// results.
func covered(hours []int) bool {
	var n [24]int
	distinct := 0
	for _, h := range hours {
		if n[h] == 0 {
			distinct++
		}
		n[h]++
	}
	if distinct < guardHours {
		return false
	}
	for h := range 24 {
		in := 0
		for i := range guardWindow {
			in += n[(h+i)%24]
		}
		if float64(in) > guardShare*float64(len(hours)) {
			return false
		}
	}
	return true
}

// patternRow is a result patterns may use: scheduled, on its region's own
// server, measured (a failed upload keeps its download).
func patternRow(t *model.SpeedTest, up bool) bool {
	if t.OnDemand() || t.Fallback || t.CappedByServer {
		return false
	}
	if up {
		return t.Outcome == model.SpeedOK && t.UpBps != nil
	}
	return t.DownBps != nil
}

// tooFast is, per direction (0 down, 1 up), the servers too fast to
// measure from here (the coverage guard) among ts, hours local to loc.
func tooFast(ts []model.SpeedTest, loc *time.Location) [2]map[string]bool {
	var newest time.Time
	for _, t := range ts {
		if t.Start.After(newest) {
			newest = t.Start
		}
	}
	var out [2]map[string]bool
	for d := range 2 {
		lb, hours := map[string]bool{}, map[string][]int{}
		for i := range ts {
			t := &ts[i]
			if !patternRow(t, d == 1) || newest.Sub(t.Start) > GuardSpan {
				continue
			}
			down, up := LowerBounds(t)
			if (d == 0 && down) || (d == 1 && up) {
				lb[t.Server] = true
			} else {
				hours[t.Server] = append(hours[t.Server], localHour(t.Start, loc))
			}
		}
		out[d] = map[string]bool{}
		for sv := range lb {
			if !covered(hours[sv]) {
				out[d][sv] = true
			}
		}
	}
	return out
}

// Censored is tests with each direction's score as patterns use it (see
// above), hours local to loc; ts is not changed (and is returned as is
// when nothing was capped).
func Censored(ts []model.SpeedTest, loc *time.Location) []model.SpeedTest {
	if !slices.ContainsFunc(ts, func(t model.SpeedTest) bool { return t.DownShort || t.UpShort }) {
		return ts
	}
	out := slices.Clone(ts)
	fast := tooFast(ts, loc)
	for i := range out {
		t := &out[i]
		if fast[0][t.Server] {
			t.ScoreDown = nil
		}
		if fast[1][t.Server] {
			t.ScoreUp = nil
		}
		down, up := LowerBounds(t)
		t.ScoreDown = atLeastNormal(t.ScoreDown, down)
		t.ScoreUp = atLeastNormal(t.ScoreUp, up)
	}
	return out
}

// atLeastNormal is a lower bound's score: never below 1 (normal).
func atLeastNormal(sc *float64, lb bool) *float64 {
	if sc == nil || !lb || *sc >= 1 {
		return sc
	}
	one := 1.0
	return &one
}
