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

func peak90(cum []int64, at []time.Duration, end time.Duration, endBytes int64, rtt time.Duration) estimate {
	return peak90From(0, cum, at, end, endBytes, rtt)
}

func TestPeak90(t *testing.T) {
	const tk = 100 * time.Millisecond
	nominal := func(n int) []time.Duration {
		at := make([]time.Duration, n)
		for i := range at {
			at[i] = time.Duration(i+1) * tk
		}
		return at
	}
	// end: the last byte half a tick after the last complete tick.
	run := func(d ...int64) estimate {
		cum := cumOf(d...)
		return peak90(cum, nominal(len(d)), time.Duration(len(d))*tk+tk/2, cum[len(cum)-1]+d[len(d)-1]/2, 0)
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
	// The first tick doesn't count: 1 or 2 ticks are lower bounds, 3 level
	// ones are enough.
	for _, d := range [][]int64{{10e6}, {10e6, 10e6}} {
		if e = run(d...); !e.lowerBound {
			t.Errorf("%d ticks: %+v", len(d), e)
		}
	}
	if e = run(10e6, 10e6, 10e6); e.lowerBound {
		t.Errorf("3 ticks: %+v", e)
	}
	// No tick with bytes, or none at all: the whole run.
	if e = peak90(cumOf(0, 0, 0), nominal(3), 350*time.Millisecond, 5, 0); e.method != MethodWhole {
		t.Errorf("no tick: %+v", e)
	}
	if e = peak90(nil, nil, 50*time.Millisecond, 5e6, 0); e.method != MethodWhole {
		t.Errorf("no ticks: %+v", e)
	}
	// Ticks read late: rates per actual tick length (net-isp). The near 2G
	// download's bytes at 0.100, 0.168 and 0.300 s: about 1.97 Gbit/s.
	g := func(x float64) int64 { return int64(x * 1e9 / 8 * tk.Seconds()) }
	cum := cumOf(g(2.19), g(1.46), g(2.26))
	at := []time.Duration{100 * time.Millisecond, 168 * time.Millisecond, 300 * time.Millisecond}
	if e = peak90(cum, at, at[2], cum[2], 0); e.lowerBound || math.Abs(rate(e)*8/1.97e9-1) > 0.02 {
		t.Errorf("jittered ticks: %+v, %.3g bit/s", e, rate(e)*8)
	}
}

// TestTickJitter: a steady rate read with ±30 ms of timer wake jitter
// stays valid and measures the rate (rates over the actual tick lengths).
func TestTickJitter(t *testing.T) {
	const rate = 125e6 // bytes/s (1 Gbit/s)
	for seed := range uint64(20) {
		r := rand.New(rand.NewPCG(seed, 9))
		var cum []int64
		var at []time.Duration
		for k := 1; k <= 8; k++ {
			a := time.Duration(k)*100*time.Millisecond + time.Duration((r.Float64()*2-1)*30e6)
			at = append(at, a)
			cum = append(cum, int64(rate*a.Seconds()))
		}
		end := 850 * time.Millisecond
		e := peak90(cum, at, end, int64(rate*end.Seconds()), 0)
		if got := float64(e.bytes) / e.win.Seconds(); e.lowerBound || math.Abs(got/rate-1) > 0.01 {
			t.Errorf("seed %d: %+v, %.4g B/s", seed, e, got)
		}
	}
}

// TestIdleRTT: the smoothing's RTT is the idle one (min_rtt, else the
// connect time), never a bloated line's loaded srtt: a level 2G run on a
// 10 ms path with 250 ms under load is valid.
func TestIdleRTT(t *testing.T) {
	flat := []float64{2.0, 2.0, 2.0, 2.0}
	if !stillRising(flat, 250*time.Millisecond) {
		t.Error("the loaded srtt would pass anyway: the test shows nothing")
	}
	for _, c := range []struct {
		dial time.Duration
		min  []time.Duration
		want time.Duration
	}{
		{10 * time.Millisecond, nil, 10 * time.Millisecond}, // no TCP_INFO
		{40 * time.Millisecond, []time.Duration{11 * time.Millisecond, 9 * time.Millisecond}, 9 * time.Millisecond},
		{10 * time.Millisecond, []time.Duration{0, 0}, 10 * time.Millisecond},
	} {
		if got := idleRTT(c.dial, c.min); got != c.want || stillRising(flat, got) {
			t.Errorf("dial %v min %v: %v", c.dial, c.min, got)
		}
	}
}

// TestFarPlateau: net-isp's far timed-dump series (150 ms) run on into a
// 3 s plateau is valid, measured at the plateau's rate.
func TestFarPlateau(t *testing.T) {
	far := []float64{0.01, 0.02, 0, 0.04, 0.07, 0, 0.15, 0.3, 0, 0.59, 1.19, 0}
	for range 30 { // 3 s at 1.25 Gbit/s, bursty per RTT
		far = append(far, []float64{1.4, 1.1, 1.25}...)
	}
	const tk = 100 * time.Millisecond
	var cum []int64
	var at []time.Duration
	var c int64
	for i, g := range far {
		c += int64(g * 1e9 / 8 * tk.Seconds())
		cum, at = append(cum, c), append(at, time.Duration(i+1)*tk)
	}
	e := peak90(cum, at, at[len(at)-1], cum[len(cum)-1], 150*time.Millisecond)
	if rate := float64(e.bytes) * 8 / e.win.Seconds(); e.lowerBound || e.method != MethodPeak90 || math.Abs(rate/1.25e9-1) > 0.05 {
		t.Errorf("%+v: %.3g bit/s", e, rate)
	}
}

// TestWrittenFirstTick: uploads counted by bytes written (no TCP_INFO)
// see the first tick fill the send buffers (about 32 MB for 8 streams):
// the window starts at tick 1 or later, so it doesn't overstate the rate.
func TestWrittenFirstTick(t *testing.T) {
	const tk = 100 * time.Millisecond
	const rate = 125e6 // bytes/s
	var cum []int64
	var at []time.Duration
	c := int64(32 << 20) // the send buffers, filled in the first tick
	for i := range 30 {
		c += int64(rate * tk.Seconds())
		cum, at = append(cum, c), append(at, time.Duration(i+1)*tk)
	}
	end := at[len(at)-1]
	got := func(e estimate) float64 { return float64(e.bytes) / e.win.Seconds() }
	if e := peak90From(1, cum, at, end, cum[len(cum)-1], 0); e.lowerBound || math.Abs(got(e)/rate-1) > 0.01 {
		t.Errorf("from tick 1: %+v, %.4g B/s", e, got(e))
	}
	if e := peak90(cum, at, end, cum[len(cum)-1], 0); got(e)/rate < 1.05 {
		t.Errorf("from tick 0 the burst should show (the test tests nothing): %.4g B/s", got(e))
	}
}

// TestStillRising: net-isp's vectors (rates per tick, as a share of the
// line rate) and cq-netns's real series from a shaped lab line (Gbit/s).
func TestStillRising(t *testing.T) {
	for _, c := range []struct {
		name string
		r    []float64
		rtt  time.Duration
		lb   bool
	}{
		{"[0.6 0.94 1 0.98]", []float64{0.6, 0.94, 1, 0.98}, 0, false},
		{"[0.5 0.91 1 1]", []float64{0.5, 0.91, 1, 1}, 0, false},
		{"[0.6 0.95 1 0.98]", []float64{0.6, 0.95, 1, 0.98}, 0, false},
		{"[0.3 1 1 1]", []float64{0.3, 1, 1, 1}, 0, false},
		{"[0.4 0.7 0.9 1.0]", []float64{0.4, 0.7, 0.9, 1.0}, 0, true},
		{"[0.1 0.3 0.6 0.9]", []float64{0.1, 0.3, 0.6, 0.9}, 0, true},
		{"near 2G down", []float64{2.19, 1.46, 2.26}, 0, false},
		{"near 2G up", []float64{2.24, 1.91, 1.93}, 0, false},
		{"cut down", []float64{1.63, 1.18, 1.42, 1.35, 1.31}, 0, false},
		{"1G down", []float64{1.25, 0.85, 0.96, 0.97, 0.99, 1.0, 0.97}, 0, false},
		{"1G up", []float64{1.27, 0.95, 0.87, 1.06, 0.94, 0.95}, 0, false},
		// htb bursts on the real 1G uploads r4, r5, r7 (cq-netns's timed
		// dump, Gbit/s per actual tick), and steady rises (net-isp's
		// amendment; a first tick prepended).
		{"1G up r4", []float64{1.2374, 0.7398, 0.954, 1.1395, 0.9672}, 0, false},
		{"1G up r5", []float64{1.2713, 0.7364, 0.8593, 1.1688, 1.0005}, 0, false},
		{"1G up r7", []float64{1.2651, 0.8564, 0.8385, 1.1756, 0.9378}, 0, false},
		{"rise 0.2-0.95", []float64{0.1, 0.2, 0.4, 0.6, 0.8, 0.95}, 0, true},
		{"rise 0.3-0.9", []float64{0.1, 0.3, 0.5, 0.7, 0.9}, 0, true},
		{"rise 0.5-1", []float64{0.1, 0.5, 0.7, 0.85, 1.0}, 0, true},
		{"rise 0.6-1", []float64{0.1, 0.6, 0.8, 0.9, 1.0}, 0, true},
		{"far down", []float64{0.01, 0.02, 0, 0.04, 0.07, 0, 0.15, 0.3, 0, 0.59, 1.19, 0}, 150 * time.Millisecond, true},
		{"far up", []float64{0, 0.01, 0.01, 0.01, 0.04, 0, 0.07, 0.15, 0, 0.3, 0.59, 0, 1.19, 1.27, 1.01}, 150 * time.Millisecond, true},
	} {
		if got := stillRising(c.r, c.rtt); got != c.lb {
			t.Errorf("%s: lower bound %v, want %v", c.name, got, c.lb)
		}
	}
}

// TestEstimateRun: one coarse timing check against the fake server. A
// download with a slow start is measured from the plateau (faster than
// the whole run's average); an upload is measured too (on loopback the
// send buffers take much of it in the first tick).
func TestEstimateRun(t *testing.T) {
	f := newFakeServer(t, "ramp") // full rate after 500 ms
	t0 := time.Now()
	r, err := Run(context.Background(), quick(f.addr(), Download))
	if err != nil {
		t.Fatal(err)
	}
	whole := float64(r.TotalBytes) * 8 / time.Since(t0).Seconds()
	if r.RateMethod != MethodPeak90 || r.BitsPerSecond() < 1.1*whole || r.Ramp < 200*time.Millisecond {
		t.Errorf("download: %+v: %.0f vs the whole run's %.0f", r, r.BitsPerSecond(), whole)
	}
	f2 := newFakeServer(t, "")
	r, err = Run(context.Background(), quick(f2.addr(), Upload))
	if err != nil {
		t.Fatal(err)
	}
	if r.RateMethod == "" || r.Bytes <= 0 {
		t.Errorf("upload: %+v", r)
	}
}

// capFixture is days of hourly scheduled tests per region of c, all on a
// 1 Gbit/s line and capped: lb(h, r) marks the lower bounds (reading lbX
// of the true rate); evenings read eve of it and are never lower bounds.
// Scores are computed in time order against Baseline, as saving does.
func capFixture(c *Catalog, days int, lb func(h int, r *rand.Rand) bool, lbX, eve float64) []model.SpeedTest {
	return capFixtureEve(c, days, lb, lbX, eve, 0)
}

// capFixtureEve is capFixture with a share eveLB of the evenings lower
// bounds too (reading lbX of the evening's rate).
func capFixtureEve(c *Catalog, days int, lb func(h int, r *rand.Rand) bool, lbX, eve, eveLB float64) []model.SpeedTest {
	r := rand.New(rand.NewPCG(3, 4))
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var ts []model.SpeedTest
	for d := range days {
		for h := range 24 {
			for i, reg := range c.Regions {
				at := start.Add(time.Duration(d*24+h)*time.Hour + time.Duration(i)*20*time.Minute)
				rate, isLB := 1e9, false
				switch {
				case evening(h) && eveLB > 0 && r.Float64() < eveLB:
					rate, isLB = rate*eve*lbX, true
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
					FallbackLevel: FallbackNone, DownBps: &down, UpBps: &up, Extra: x, Conn: "wired"})
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
			if !strings.HasPrefix(f.Text, "Your line is too fast for these tests") {
				t.Errorf("too fast: %q", f.Text)
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
	// Lower bounds in the evenings too (net-isp): below normal they are
	// left out, so the evening's real 0.6 still shows.
	for _, eveLB := range []float64{0.5, 0.7} {
		ts := capFixtureEve(c, 14, share(0.5), 0.85, 0.6, eveLB)
		f := FindFinding(FindingInput{Tests: ts, Catalog: c})
		t.Logf("evening lower bounds %.0f%%: %s, drop %.2f", 100*eveLB, f.Case, f.Drop)
		if f.Case != CaseISP || math.Abs(f.Drop-0.4) > 0.05 {
			t.Errorf("evening lower bounds %.0f%%: %s %.2f: %s", 100*eveLB, f.Case, f.Drop, f.Text)
		}
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
