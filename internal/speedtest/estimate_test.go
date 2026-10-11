package speedtest

import (
	"context"
	"math"
	"math/rand/v2"
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
	// 1 tick: a lower bound; 2 level ticks are enough.
	if e = run(10e6); !e.lowerBound {
		t.Errorf("1 tick: %+v", e)
	}
	if e = run(10e6, 10e6); e.lowerBound {
		t.Errorf("2 ticks: %+v", e)
	}
	// The 3-tick median at the edges, 4 ticks (net-isp): levelled off, or
	// still rising.
	if e = run(6e6, 9.5e6, 10e6, 9.8e6); e.lowerBound || e.method != MethodPeak90 {
		t.Errorf("[0.6 0.95 1.0 0.98]: %+v", e)
	}
	if e = run(4e6, 7e6, 9e6, 10e6); !e.lowerBound {
		t.Errorf("[0.4 0.7 0.9 1.0]: %+v", e)
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

// capFixture is days of hourly scheduled tests per region of c, all on a
// 1 Gbit/s line and capped: lb(h, r) marks the lower bounds (reading lbX
// of the true rate); evenings read eve of it and are never lower bounds.
// Scores are computed in time order against Baseline, as saving does.
func capFixture(c *Catalog, days int, lb func(h int, r *rand.Rand) bool, lbX, eve float64) []model.SpeedTest {
	r := rand.New(rand.NewPCG(3, 4))
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var ts []model.SpeedTest
	for d := range days {
		for h := range 24 {
			for i, reg := range c.Regions {
				at := start.Add(time.Duration(d*24+h)*time.Hour + time.Duration(i)*20*time.Minute)
				rate, isLB := 1e9, false
				switch {
				case evening(h):
					rate *= eve
				case lb(h, r):
					rate, isLB = rate*lbX, true
				}
				x := `{"rate_method":["peak90",""]}`
				if isLB {
					x = `{"rate_method":["peak90",""],"lower_bound":[true,false]}`
				}
				down, up := rate, 40e6
				ts = append(ts, model.SpeedTest{Start: at, Trigger: "schedule", Outcome: model.SpeedOK, Region: reg.ID, Server: reg.Chain(c, at, 0)[0].Server.ID,
					FallbackLevel: FallbackNone, DownBps: &down, UpBps: &up, DownShort: true, Extra: x, Conn: "wired"})
			}
		}
	}
	for k := range ts {
		if d, _, ok := Baseline(ts[:k], ts[k].Server, ts[k].Start); ok {
			sc := *ts[k].DownBps / d
			ts[k].ScoreDown = &sc
		}
	}
	return ts
}

// lbShare is the share of a fixture's downloads that are lower bounds.
func lbShare(ts []model.SpeedTest) float64 {
	n := 0
	for i := range ts {
		if d, _ := LowerBounds(&ts[i]); d {
			n++
		}
	}
	return float64(n) / float64(len(ts))
}

// TestFastLineGuard: net-isp's fixture. 14 days on a 1 Gbit/s line,
// daytime lower bounds at 0.85 of the true rate, evenings level at 0.6.
// With 0, 17 and 42% lower bounds the evening dip is found (isp, about
// -40%); with 83% (every daytime result) the baseline would be the evening
// itself, so every server is too fast to measure from here: too_fast,
// never nothing.
func TestFastLineGuard(t *testing.T) {
	c := findingCatalog(t)
	for _, tc := range []struct {
		name string
		lb   func(h int, r *rand.Rand) bool
		want string
	}{
		{"0%", func(int, *rand.Rand) bool { return false }, CaseISP},
		{"17%", func(h int, _ *rand.Rand) bool { return h >= 9 && h <= 12 }, CaseISP},
		{"42%", func(h int, _ *rand.Rand) bool { return h%2 == 0 }, CaseISP},
		{"83%", func(int, *rand.Rand) bool { return true }, CaseTooFast},
	} {
		ts := capFixture(c, 14, tc.lb, 0.85, 0.6)
		f := FindFinding(FindingInput{Tests: ts, Catalog: c})
		t.Logf("%s: %.0f%% lower bounds: %s, drop %.2f", tc.name, 100*lbShare(ts), f.Case, f.Drop)
		if f.Case != tc.want || (tc.want == CaseISP && math.Abs(f.Drop-0.4) > 0.05) {
			t.Errorf("%s (%.0f%% lower bounds): %s %.2f: %s", tc.name, 100*lbShare(ts), f.Case, f.Drop, f.Text)
		}
		if tc.want == CaseTooFast {
			if !strings.HasPrefix(f.Text, "Your line is too fast for these tests") || !strings.HasPrefix(f.TextPublic, "The line is too fast") {
				t.Errorf("too fast: %q / %q", f.Text, f.TextPublic)
			}
			if st := Summarize(ts, nil); !st.TooFast || st.Dip != nil || st.RampCappedShare < 0.8 {
				t.Errorf("too fast row: %v %v %.2f", st.TooFast, st.Dip, st.RampCappedShare)
			}
			if p := PooledPattern(ts, nil); p != nil {
				t.Errorf("a pattern from too-fast servers: %+v", p)
			}
		} else if st := Summarize(ts, nil); st.TooFast {
			t.Errorf("%s: row too fast", tc.name)
		}
	}
	// net-isp: near lower bounds spread at random over every hour but the
	// evenings, which are cut to 0.7 and plateau.
	share := func(p float64) func(h int, r *rand.Rand) bool {
		return func(_ int, r *rand.Rand) bool { return r.Float64() < p/(20.0/24) }
	}
	ts := capFixture(c, 14, share(0.5), 0.85, 0.7)
	f := FindFinding(FindingInput{Tests: ts, Catalog: c})
	t.Logf("50%% at random: %.0f%% lower bounds: %s, drop %.2f", 100*lbShare(ts), f.Case, f.Drop)
	if f.Case != CaseISP {
		t.Errorf("50%% at random (%.0f%%): %s: %s", 100*lbShare(ts), f.Case, f.Text)
	}
	// At 80% the few daytime results left can't cover the day: too fast.
	ts = capFixture(c, 14, share(0.8), 0.85, 0.7)
	f = FindFinding(FindingInput{Tests: ts, Catalog: c})
	t.Logf("80%% at random: %.0f%% lower bounds: %s", 100*lbShare(ts), f.Case)
	if f.Case != CaseTooFast {
		t.Errorf("80%% at random (%.0f%%): %s: %s", 100*lbShare(ts), f.Case, f.Text)
	}
	// Some servers out: judged over the others, and said so.
	ts = capFixture(c, 14, func(int, *rand.Rand) bool { return false }, 0.85, 0.6)
	for i := range ts {
		if ts[i].Region == "r3" && !evening(ts[i].Start.Hour()) {
			ts[i].Extra = `{"rate_method":["peak90",""],"lower_bound":[true,false]}`
		}
	}
	if f := FindFinding(FindingInput{Tests: ts, Catalog: c}); f.Case != CaseISP || !strings.Contains(f.Evidence, "2 of 8 servers were too fast to measure from here and are left out.") {
		t.Errorf("far out: %s: %s / %s", f.Case, f.Text, f.Evidence)
	}
}

// TestGuardBoundaries: fewer than 12 local hours, or more than half the
// results within 6 hours, is too fast.
func TestGuardBoundaries(t *testing.T) {
	hours := func(n int) []int {
		var h []int
		for i := range n {
			h = append(h, i)
		}
		return h
	}
	if covered(hours(11)) || !covered(hours(12)) {
		t.Error("11 vs 12 hours")
	}
	within := func(k int) []int { // k of 100 results in hours 0-5, the rest over 6-23
		var h []int
		for i := range k {
			h = append(h, i%6)
		}
		for i := range 100 - k {
			h = append(h, 6+i%18)
		}
		return h
	}
	if !covered(within(50)) || covered(within(51)) {
		t.Error("50% vs 51% within 6 hours")
	}
}

// TestOldCappedRows: v0.7.0's directions capped in the ramp-up (short,
// no rate method, ramp_ms 0) are lower bounds: out of the baseline, at
// least normal in the medians and the finding. The other old rows count
// as they are, capped after the ramp-up too.
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
		old[i].Extra = `{"ramp_ms":[700,500]}` // capped after the ramp-up: valid
	}
	if _, _, ok := Baseline(old, "A1", end); !ok {
		t.Error("old rows capped after the ramp-up left out")
	}
	if d, _ := LowerBounds(&old[0]); d {
		t.Error("an old row capped after the ramp-up is a lower bound")
	}
}
