package callq

import (
	"math"
	"slices"
	"testing"
)

// TestMOSReferencePoints: the G.107 reference points of the R → MOS map.
func TestMOSReferencePoints(t *testing.T) {
	for _, c := range []struct{ r, mos float64 }{
		{93.2, 4.41}, {80, 4.02}, {70, 3.60}, {60, 3.10}, {0, 1}, {100, 4.5},
	} {
		if got := MOS(c.r); math.Abs(got-c.mos) > 0.005 {
			t.Errorf("MOS(%v) = %.4f, want %.2f", c.r, got, c.mos)
		}
	}
	if MOS(-20) != 1 || MOS(140) != MOS(100) {
		t.Errorf("MOS does not clamp R: %v %v", MOS(-20), MOS(140))
	}
}

func TestR(t *testing.T) {
	for _, c := range []struct {
		name             string
		rtt, jitter, pct float64
		want             float64
	}{
		{"perfect", 0, 0, 0, 93.2 - 10.0/40},
		{"typical", 32, 3, 0, 93.2 - (16+6+10)/40.0},
		{"eff 160", 300, 0, 0, 93.2 - (160-120)/10.0}, // both branches give 89.2
		{"long delay", 500, 10, 0, 93.2 - (250+20+10-120)/10.0},
		{"loss 3%", 20, 0, 3, 93.2 - 20.0/40 - 6},
		{"loss 5%", 20, 0, 5, 93.2 - 20.0/40 - 10},
		{"loss 8%", 20, 0, 8, 93.2 - 20.0/40 - 10 - 7.5},
		{"all lost", 0, 0, 100, 0},
		{"clamped low", 2000, 100, 50, 0},
	} {
		got := R(c.rtt, c.jitter, c.pct)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: R(%v, %v, %v) = %v, want %v", c.name, c.rtt, c.jitter, c.pct, got, c.want)
		}
	}
	// The two delay branches meet at eff = 160.
	if a, b := R(299.999, 0, 0), R(300, 0, 0); math.Abs(a-b) > 1e-3 {
		t.Errorf("R is not continuous at eff 160: %v vs %v", a, b)
	}
}

func TestBandEdges(t *testing.T) {
	for _, c := range []struct {
		mos  float64
		want Band
	}{
		{4.41, Good}, {4.0, Good}, {3.9999, Fair}, {3.6, Fair}, {3.5999, Poor},
		{3.1, Poor}, {3.0999, Unusable}, {1, Unusable},
	} {
		if got := BandOf(c.mos); got != c.want {
			t.Errorf("BandOf(%v) = %s, want %s", c.mos, got, c.want)
		}
	}
	// Typical home connections and broken ones land where expected.
	for _, c := range []struct {
		rtt, jitter, pct float64
		want             Band
	}{
		{30, 3, 0, Good},
		{30, 3, 2, Good},
		{30, 3, 5, Good}, // Opus copes with a few percent
		{30, 3, 7, Fair},
		{150, 20, 1, Good},
		{30, 3, 10, Poor},
		{30, 3, 20, Unusable},
		{600, 40, 0, Poor},
		{1000, 50, 0, Unusable},
		{0, 0, 100, Unusable},
	} {
		if got := BandOf(Score(c.rtt, c.jitter, c.pct)); got != c.want {
			t.Errorf("Score(%v, %v, %v) = %.2f (%s), want %s", c.rtt, c.jitter, c.pct, Score(c.rtt, c.jitter, c.pct), got, c.want)
		}
	}
}

func TestJitterCaps(t *testing.T) {
	for _, c := range []struct {
		jitter float64
		want   Band
	}{
		{math.NaN(), Good}, {0, Good}, {29.9, Good}, {30, Fair}, {49.9, Fair},
		{50, Poor}, {99.9, Poor}, {100, Unusable}, {400, Unusable},
	} {
		if got := JitterCap(c.jitter); got != c.want {
			t.Errorf("JitterCap(%v) = %s, want %s", c.jitter, got, c.want)
		}
		// A short, clean path: the MOS alone says Good, so the cap decides.
		m := Minute{RTT: 20, Jitter: c.jitter, Loss: 0, Bursts: 12}
		if c.jitter <= JitterUnusableMs && BandOf(m.RawMOS()) != Good {
			t.Fatalf("jitter %v: MOS %.2f is not Good before the cap", c.jitter, m.RawMOS())
		}
		if got := m.Band(); got != c.want || got != Capped(BandOf(m.RawMOS()), c.jitter) || BandOf(m.MOS()) != got {
			t.Errorf("jitter %v: band %s (MOS %.2f), want %s", c.jitter, got, m.MOS(), c.want)
		}
		if want := min(m.RawMOS(), JitterCapMOS(c.jitter)); c.want != Good && m.MOS() != want {
			t.Errorf("jitter %v: MOS %.2f, want %.2f", c.jitter, m.MOS(), want)
		}
		if c.want != Good && m.Factor() != "jitter" {
			t.Errorf("jitter %v: factor %s, want jitter (the cap decided the band)", c.jitter, m.Factor())
		}
	}
	// A cap never improves a band.
	for _, b := range []Band{Good, Fair, Poor, Unusable} {
		for _, j := range []float64{math.NaN(), 0, 29.9, 30, 49.9, 50, 99.9, 100} {
			if got := Capped(b, j); Rank(got) > Rank(b) || Rank(got) > Rank(JitterCap(j)) {
				t.Errorf("Capped(%s, %v) = %s", b, j, got)
			}
		}
	}
	// Lossy and jittery: the MOS band is already worse than the cap, so
	// the loss decides.
	if m := (Minute{RTT: 20, Jitter: 35, Loss: 20, Bursts: 12}); m.Band() != Unusable || m.Factor() != "loss" {
		t.Errorf("20%% loss, 35 ms jitter: band %s factor %s, want unusable by loss", m.Band(), m.Factor())
	}
	// The worst minute is the lowest (capped) MOS: a minute capped to Poor
	// whose E-model MOS is Good beats a Fair one.
	s := Summarize([]Minute{
		{RTT: 30, Jitter: 3, Loss: 7, Bursts: 12},  // Fair by MOS
		{RTT: 20, Jitter: 60, Loss: 0, Bursts: 12}, // Good by MOS, Poor by jitter
		{RTT: 20, Jitter: 2, Loss: 0, Bursts: 12},
	})
	if s.Worst != 1 || s.Band != Poor || s.Factor != "jitter" || s.MOS != FairMOS-CapMargin {
		t.Errorf("worst minute %d band %s factor %s (MOS %.2f), want 1 poor by jitter", s.Worst, s.Band, s.Factor, s.MOS)
	}
}

func TestFactor(t *testing.T) {
	for _, c := range []struct {
		rtt, jitter, pct float64
		want             string
	}{
		{30, 2, 8, "loss"},
		{30, 60, 0, "jitter"},
		{400, 5, 0, "latency"},
	} {
		if got := Factor(c.rtt, c.jitter, c.pct); got != c.want {
			t.Errorf("Factor(%v, %v, %v) = %s, want %s", c.rtt, c.jitter, c.pct, got, c.want)
		}
	}
}

func TestCombineAndSummarize(t *testing.T) {
	nan := math.NaN()
	// Three servers: one slow outlier; the median is robust to it.
	c := Combine([]Minute{
		{RTT: 20, Jitter: 2, Loss: 0, Bursts: 12},
		{RTT: 24, Jitter: 3, Loss: 0, Bursts: 12},
		{RTT: 300, Jitter: 40, Loss: 50, Bursts: 12, Unknown: 1},
		{RTT: nan, Jitter: nan, Loss: nan, Unknown: 4}, // only unknown failures: not loss
	})
	if c.RTT != 24 || c.Jitter != 3 || c.Loss != 0 || c.Bursts != 36 || c.Unknown != 5 {
		t.Errorf("Combine = %+v", c)
	}

	ms := []Minute{
		{RTT: 30, Jitter: 2, Loss: 0, Bursts: 12},
		{RTT: 32, Jitter: 3, Loss: 0, Bursts: 12},
		{RTT: 31, Jitter: 2, Loss: 12, Bursts: 12},    // a lossy minute: the worst
		{RTT: nan, Jitter: nan, Loss: 100, Bursts: 2}, // too few bursts: not scored
		Empty, // no data
		{RTT: 35, Jitter: 4, Loss: 0, Bursts: 12, Unknown: 3},
	}
	s := Summarize(ms)
	if s.Worst != 2 || s.Band != Poor || s.Factor != "loss" {
		t.Errorf("worst minute %d band %s factor %s (MOS %.2f)", s.Worst, s.Band, s.Factor, s.MOS)
	}
	if s.Scored != 4 || s.Bursts != 50 || s.Unknown != 3 {
		t.Errorf("counts: %+v", s)
	}
	if math.Abs(s.Loss-3) > 1e-9 || math.Abs(s.RTT50-31.5) > 1e-9 || math.Abs(s.Jitter-2.75) > 1e-9 {
		t.Errorf("means: loss %v rtt50 %v jitter %v", s.Loss, s.RTT50, s.Jitter)
	}
	if s.RTT95 <= s.RTT50 || s.RTT95 > 35 {
		t.Errorf("p95 %v", s.RTT95)
	}

	// Every burst timed out: unusable, scored on loss alone.
	s = Summarize([]Minute{{RTT: nan, Jitter: nan, Loss: 100, Bursts: 12}})
	if s.Band != Unusable || math.Abs(s.MOS-1) > 1e-9 {
		t.Errorf("all lost: %+v", s)
	}
	// Nothing scored.
	s = Summarize([]Minute{Empty, {RTT: 20, Loss: 0, Bursts: 1}})
	if s.Worst != -1 || s.Band != "" || !math.IsNaN(s.MOS) || s.Bursts != 1 {
		t.Errorf("unscored: %+v", s)
	}
}

func TestQuantile(t *testing.T) {
	v := []float64{5, 1, 3, 2, 4}
	if Median(v) != 3 || Quantile(v, 0) != 1 || Quantile(v, 1) != 5 || Quantile(v, 0.95) != 4.8 {
		t.Errorf("quantiles of %v: %v %v %v %v", v, Median(v), Quantile(v, 0), Quantile(v, 1), Quantile(v, 0.95))
	}
	if v[0] != 5 {
		t.Error("Quantile sorted its input")
	}
	if !math.IsNaN(Median(nil)) {
		t.Error("median of nothing")
	}
}

func TestGroup(t *testing.T) {
	nan := math.NaN()
	good := Minute{RTT: 10, Jitter: 1, Loss: 0, Bursts: 12, Answered: 12}
	far := Minute{RTT: 30, Jitter: 1, Loss: 0, Bursts: 12, Answered: 12}
	partial := Minute{RTT: 30, Jitter: 1, Loss: 0, Bursts: 3, Answered: 3}
	lost := Minute{RTT: nan, Jitter: nan, Loss: 100, Bursts: 12}
	refused := Minute{RTT: nan, Jitter: nan, Loss: nan, Unknown: 12}
	targets := [][]Minute{
		{good, good, Empty},
		{good, good, Empty},
		{far, far, partial}, // the minute in progress: only this one has probed
		{lost, lost, lost},  // never answers while the others do
		{refused, refused, refused},
	}
	dead := NotAnswering(targets)
	if !slices.Equal(dead, []bool{false, false, false, true, true}) {
		t.Fatalf("not answering: %v", dead)
	}
	ms, ok := Group(targets, dead, 3)
	if !ok || ms[0].RTT != 10 || ms[0].Loss != 0 || ms[0].Bursts != 36 {
		t.Errorf("minute 0: %+v ok %v", ms[0], ok)
	}
	if ms[2].Scored() || ms[2].Bursts != 3 {
		t.Errorf("a minute one of three targets measured is scored: %+v", ms[2])
	}
	// Alone, a dead target is not "not answering": nothing answered at all.
	if d := NotAnswering([][]Minute{{lost, lost}}); d[0] {
		t.Error("a lone silent target is not answering")
	}
	if _, ok := Group([][]Minute{{lost}}, []bool{true}, 1); ok {
		t.Error("every target dead: ok")
	}
	if ms, ok := Group(nil, nil, 2); !ok || len(ms) != 2 || ms[0].Scored() {
		t.Errorf("no targets: %+v %v", ms, ok)
	}
}
