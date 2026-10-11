package speedtest

import (
	"math"
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

// peak90 estimates a capped direction's steady rate. cum[i] is the bytes
// counted by the end of complete tick i+1 (the tick the cap fell in is
// left out) and at[i] when that tick was read; end is when the last byte
// was counted, endBytes the count then, all from the data phase's start.
// rtt is the path's idle RTT (see test.pathRTT).
//
//   - Each tick's rate is its bytes over its actual length (ticks are read
//     late on a busy machine).
//   - The rates are smoothed by a 3-tick moving median (2 ticks averaged
//     at the edges), so one spike tick neither sets the peak nor starts
//     the window.
//   - The window runs from the first smoothed tick at 90% of the peak to
//     the last byte; its rate is its bytes over its length.
//   - Whether it was still speeding up at the cap (a lower bound) is
//     stillRising's.
//   - With no ticks, or none with bytes, it is MethodWhole.
func peak90(cum []int64, at []time.Duration, end time.Duration, endBytes int64, rtt time.Duration) estimate {
	n := len(cum)
	if n == 0 || end <= 0 || len(at) != n {
		return estimate{method: MethodWhole}
	}
	r := make([]float64, n)
	var prev int64
	var prevAt time.Duration
	for i, c := range cum {
		if dt := at[i] - prevAt; dt > 0 {
			r[i] = float64(c-prev) / dt.Seconds()
		}
		prev, prevAt = c, at[i]
	}
	sm := median3(r)
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
	var start time.Duration
	if k > 0 {
		base, start = cum[k-1], at[k-1]
	}
	e := estimate{method: MethodPeak90, start: start, bytes: endBytes - base, win: end - start}
	if e.win <= 0 || e.bytes <= 0 {
		return estimate{method: MethodWhole}
	}
	e.lowerBound = stillRising(r, rtt)
	return e
}

// median3 is a 3-tick moving median (the 2 ticks averaged at the edges).
func median3(d []float64) []float64 {
	n := len(d)
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
	return sm
}

// stillRising reports a lower bound: no plateau seen in the per-tick
// rates r (net-isp's rule, checked against real shaped lines).
//   - The first tick (connection start, a shaper's burst) is left out;
//     fewer than 2 ticks left is a lower bound.
//   - Up to 50 ms of RTT the rates are smoothed by the 3-tick moving
//     median; beyond, by a moving mean over w = ⌈RTT / 100 ms⌉ ticks (a
//     long path's bytes arrive in bursts per RTT).
//   - Levelled off when two smoothed values within 5% of the peak are at
//     least w ticks apart, or when the peak is at least w ticks before the
//     end and at least 2 values are within 10% of it.
//   - With w = 1 the median is taken twice, the edge ticks averaged and the
//     edge tick repeated: a lower bound only when both see it rising (a
//     shaper's burst next to the last tick can't fake a lone peak).
func stillRising(r []float64, rtt time.Duration) bool {
	if len(r) < 3 {
		return true
	}
	d := r[1:]
	w := 1
	if rtt > tick/2 {
		w = int(math.Ceil(float64(rtt) / float64(tick)))
	}
	if w == 1 {
		return rising(median3(d), 1) && rising(median3Repeat(d), 1)
	}
	if len(d) < w+1 {
		return true
	}
	var sm []float64
	for i := w - 1; i < len(d); i++ {
		var x float64
		for _, v := range d[i-w+1 : i+1] {
			x += v
		}
		sm = append(sm, x/float64(w))
	}
	return rising(sm, w)
}

// median3Repeat is a 3-tick moving median with the edge ticks repeated
// (2 ticks: their mean).
func median3Repeat(d []float64) []float64 {
	n := len(d)
	if n == 2 {
		m := (d[0] + d[1]) / 2
		return []float64{m, m}
	}
	p := append(append([]float64{d[0]}, d...), d[n-1])
	sm := make([]float64, n)
	for i := range sm {
		w := slices.Clone(p[i : i+3])
		slices.Sort(w)
		sm[i] = w[1]
	}
	return sm
}

// rising is the plateau test on smoothed rates sm, w ticks per RTT.
func rising(sm []float64, w int) bool {
	p := slices.Max(sm)
	if p <= 0 {
		return true
	}
	first, lastHi := -1, -1
	near := 0
	for i, v := range sm {
		if v >= 0.95*p {
			if first < 0 {
				first = i
			}
			lastHi = i
		}
		if v >= 0.9*p {
			near++
		}
	}
	if lastHi-first >= w {
		return false
	}
	ip := slices.Index(sm, p)
	return !(len(sm)-1-ip >= w && near >= 2)
}

// How patterns use a capped direction (read side; nothing stored is
// rewritten). A lower bound (still speeding up at the cap, so the speed
// was at least this) is a new result flagged so, or a v0.7.0 capped
// direction capped in its ramp-up (RampMs 0: its rate included slow
// start). It never enters Baseline. Per server and direction over
// GuardSpan, the coverage guard (tooFast) decides how the rest go:
//   - covered: a lower bound that already reaches normal counts as normal
//     (score 1) in the patterns and the finding; one below normal is left
//     out (it says nothing about how slow the line was);
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

// atLeastNormal is a lower bound's score: 1 (normal) when it reaches
// normal, else none (left out).
func atLeastNormal(sc *float64, lb bool) *float64 {
	if sc == nil || !lb {
		return sc
	}
	if *sc < 1 {
		return nil
	}
	one := 1.0
	return &one
}

// LowerBoundCounted reports whether a lower bound counts in the patterns
// (as normal: it already reached it) rather than being left out.
func LowerBoundCounted(sc *float64, lb bool) bool { return lb && sc != nil && *sc >= 1 }
