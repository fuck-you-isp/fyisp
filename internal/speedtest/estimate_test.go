package speedtest

import (
	"context"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// cumOf turns per-tick bytes into the counts at each tick's end.
func cumOf(d ...int64) []int64 {
	out := make([]int64, len(d))
	var c int64
	for i, x := range d {
		c += x
		out[i] = c
	}
	return out
}

func TestPeak90(t *testing.T) {
	const tk = 100 * time.Millisecond
	// end: the last byte half a tick after the last complete tick.
	run := func(d ...int64) estimate {
		cum := cumOf(d...)
		return peak90(cum, tk, time.Duration(len(d))*tk+tk/2, cum[len(cum)-1]+d[len(d)-1]/2)
	}
	rate := func(e estimate) float64 { return float64(e.bytes) / e.win.Seconds() }
	plateau := 10e6 / tk.Seconds()

	// Slow start, then a plateau: the plateau's rate.
	e := run(1e6, 2e6, 4e6, 8e6, 10e6, 10e6, 10e6, 10e6, 10e6, 10e6)
	if e.method != MethodPeak90 || e.lowerBound || math.Abs(rate(e)/plateau-1) > 0.03 || e.start != 4*tk {
		t.Errorf("slow start: %+v, %.0f", e, rate(e))
	}
	// One spike tick neither sets the peak nor starts the window there.
	e = run(2e6, 6e6, 10e6, 10e6, 30e6, 10e6, 10e6, 10e6, 10e6, 10e6)
	if e.method != MethodPeak90 || e.lowerBound || e.start != 2*tk {
		t.Errorf("spike: %+v", e)
	}
	// Still rising at the cap: a lower bound.
	if e = run(1e6, 2e6, 3e6, 4e6, 5e6, 6e6, 7e6, 8e6); !e.lowerBound || e.method != MethodPeak90 {
		t.Errorf("rising: %+v", e)
	}
	// A plateau that ends in the last 2 ticks is no lower bound.
	if e = run(2e6, 8e6, 10e6, 10e6, 10e6, 10.2e6, 10.3e6); e.lowerBound {
		t.Errorf("plateau at the end: %+v", e)
	}
	// 1 and 2 ticks: lower bounds.
	if e = run(10e6); !e.lowerBound {
		t.Errorf("1 tick: %+v", e)
	}
	if e = run(10e6, 10e6); !e.lowerBound {
		t.Errorf("2 ticks: %+v", e)
	}
	// No tick with bytes, or none at all: the whole run.
	if e = peak90(cumOf(0, 0, 0), tk, 350*time.Millisecond, 5); e.method != MethodWhole {
		t.Errorf("no tick: %+v", e)
	}
	if e = peak90(nil, tk, 50*time.Millisecond, 5e6); e.method != MethodWhole {
		t.Errorf("no ticks: %+v", e)
	}
}

// TestEstimateRun: one coarse timing check against the fake server. A
// download with a slow start capped after it levels off is measured from
// the plateau (faster than the whole run's average); a capped upload is
// measured from what the server acknowledged, never past the cap (on
// loopback the send buffers take it all in the first tick).
func TestEstimateRun(t *testing.T) {
	f := newFakeServer(t, "ramp") // full rate after 500 ms
	cfg := quick(f.addr(), Download)
	cfg.Duration, cfg.MaxBytes = 5*time.Second, 24<<20
	t0 := time.Now()
	r, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	whole := float64(r.TotalBytes) * 8 / time.Since(t0).Seconds()
	if !r.CappedByBytes || r.RateMethod != MethodPeak90 || r.BitsPerSecond() < 1.1*whole || r.Ramp < 200*time.Millisecond {
		t.Errorf("download: %+v: %.0f vs the whole run's %.0f", r, r.BitsPerSecond(), whole)
	}
	f2 := newFakeServer(t, "")
	cfg = quick(f2.addr(), Upload)
	cfg.Duration, cfg.MaxBytes = 5*time.Second, 16<<20
	r, err = Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !r.CappedByBytes || r.RateMethod == "" || r.Bytes > cfg.MaxBytes {
		t.Errorf("upload: %+v", r)
	}
}

// fast marks every test's download capped by the 100 MB limit, levelled
// off, except where lower says it was still speeding up.
func fast(ts []model.SpeedTest, lower func(t model.SpeedTest) bool) []model.SpeedTest {
	for i := range ts {
		ts[i].DownShort = true
		ts[i].Extra = `{"rate_method":["peak90",""]}`
		if lower(ts[i]) {
			ts[i].Extra = `{"rate_method":["peak90",""],"lower_bound":[true,false]}`
		}
	}
	return ts
}

// TestFastLine: a 2 Gbit/s line with a 30% evening cut, every test capped,
// the far region's mostly lower bounds that read low.
func TestFastLine(t *testing.T) {
	c := findingCatalog(t)
	far := func(sv string) bool { return strings.HasSuffix(sv, "3") }
	score := func(sv string, h int) float64 {
		switch {
		case far(sv) && h%3 != 0:
			return 0.6 // a lower bound: only "at least" this
		case far(sv):
			return 1.05
		case evening(h):
			return 0.7
		}
		return 1
	}
	base := func(sv string) float64 {
		if far(sv) {
			return 400e6
		}
		return 2e9
	}
	ts := fast(synthDown(c, 9, score, base, 0), func(t model.SpeedTest) bool { return far(t.Server) })
	end := ts[len(ts)-1].Start.Add(time.Hour)

	f := FindFinding(FindingInput{Tests: ts, Catalog: c})
	if f.Case != CaseISP || !slices.Contains(f.Hours, 20) {
		t.Errorf("fast line: %s %v: %s", f.Case, f.Hours, f.Text)
	}
	// The baseline comes from the levelled-off tests (daytime's p90); far
	// lower bounds never enter it.
	if d, _, ok := Baseline(ts, "A1", end); !ok || math.Abs(d/2e9-1) > 0.01 {
		t.Errorf("baseline A1: %.0f %v", d, ok)
	}
	if _, _, ok := Baseline(ts, "A3", end); ok {
		t.Error("a baseline from lower bounds")
	}
	// The far region: lower bounds below normal are left out, never slow.
	var farTests []model.SpeedTest
	for _, x := range ts {
		if far(x.Server) {
			farTests = append(farTests, x)
		}
	}
	st := Summarize(farTests, nil)
	if !st.TooFast || st.Dip != nil || st.DownMedian != nil || st.Score == nil || *st.Score != 1 {
		t.Errorf("far row: too fast %v, dip %v, median %v, score %v", st.TooFast, st.Dip, st.DownMedian, st.Score)
	}
	for h, m := range HourMedians(farTests, nil, false) {
		if !math.IsNaN(m) && m < 1 {
			t.Errorf("far hour %d reads %.2f", h, m)
		}
	}
	// 1 Gbit/s: the near regions dip, the far ones are lower bounds.
	ts = fast(synthDown(c, 9, score, func(sv string) float64 { return base(sv) / 2 }, 0), func(t model.SpeedTest) bool { return far(t.Server) })
	if f := FindFinding(FindingInput{Tests: ts, Catalog: c}); f.Case != CaseISP {
		t.Errorf("1 Gbit: %s: %s", f.Case, f.Text)
	}
}

// TestOldCappedRows: v0.7.0's capped directions (short, no rate method)
// are left out of the baseline, the medians and the finding; the other
// old rows still count.
func TestOldCappedRows(t *testing.T) {
	c := findingCatalog(t)
	ts := synth(c, 9, func(string, int) float64 { return 1 }, "wired") // new, uncapped, flat
	old := synth(c, 9, func(_ string, h int) float64 {
		if evening(h) {
			return 0.5 // slow start in the window, not the line
		}
		return 1
	}, "wired")
	for i := range old {
		old[i].Start = old[i].Start.Add(-10 * time.Minute)
		old[i].DownShort = true
		old[i].Extra = `{"ramp_ms":[0,0]}`
	}
	mixed := append(append([]model.SpeedTest{}, old...), ts...)
	if f := FindFinding(FindingInput{Tests: mixed, Catalog: c}); f.Case != CaseNothing {
		t.Errorf("old capped rows count: %s: %s", f.Case, f.Text)
	}
	for h, m := range HourMedians(mixed, nil, false) {
		if m != 1 {
			t.Errorf("hour %d: %.2f", h, m)
		}
	}
	end := ts[len(ts)-1].Start.Add(time.Hour)
	if _, _, ok := Baseline(old, "A1", end); ok {
		t.Error("a baseline from v0.7.0 capped rows")
	}
	for i := range old {
		old[i].DownShort = false // old, uncapped: still counts
	}
	if _, _, ok := Baseline(old, "A1", end); !ok {
		t.Error("old uncapped rows left out")
	}
}
