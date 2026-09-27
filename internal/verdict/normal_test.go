package verdict

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// normals is a fake baseline.Source.Get: every series of the test profile
// has a long-term normal of its default RTT (p95 1.3×), unless overridden
// per target; a target mapped to a zero Baseline has none.
func normals(p *model.Profile, over map[string]model.Baseline) func(model.SeriesKey, time.Time) (model.Baseline, bool) {
	return func(k model.SeriesKey, _ time.Time) (model.Baseline, bool) {
		if b, ok := over[k.Target]; ok {
			b.Key = k
			return b, b.MedianMs > 0
		}
		for _, t := range p.Targets {
			if t.Name == k.Target {
				med := float64(defaultRTT(t)) / float64(time.Millisecond)
				return model.Baseline{Key: k, MedianMs: med, P95Ms: 1.3 * med, Samples: 10000}, true
			}
		}
		return model.Baseline{}, false
	}
}

func nb(med, p95 float64) model.Baseline {
	return model.Baseline{MedianMs: med, P95Ms: p95, Samples: 10000}
}

func TestLongTermLatency(t *testing.T) {
	aws := []string{"AWS ap-south-1", "AWS eu-west-1", "AWS sa-east-1", "AWS us-east-1"}
	cases := []struct {
		name     string
		over     map[string]model.Baseline
		fx       []behavior
		baseline time.Duration // healthy time first (default 2m: no 30-minute baseline yet)
		kind     model.VerdictKind
		summary  string
		targets  []string
		evidence map[string]float64
		absent   []string
	}{
		{
			name: "evening congestion: every service 3× its normal is upstream",
			fx:   []behavior{internet(rtt(90 * time.Millisecond))},
			kind: model.VerdictUpstream, summary: "Problems beyond your ISP: 10 of 10 monitored services are 3× slower than your normal (90 ms vs 30 ms).",
			evidence: map[string]float64{"services_vs_normal": 3, "gateway_vs_normal": 1, "edge_vs_normal": 1, "anycast_vs_normal": 1},
		},
		{
			name: "one group 3× its normal is that service",
			fx:   []behavior{targets(rtt(90*time.Millisecond), aws...)},
			kind: model.VerdictService, summary: "Amazon Web Services is 3× slower than your normal (90 ms vs 30 ms).",
			targets: aws,
		},
		{
			name: "part of a group",
			fx:   []behavior{targets(rtt(90*time.Millisecond), "AWS us-east-1", "AWS eu-west-1")},
			kind: model.VerdictService, summary: "Amazon Web Services is 3× slower than your normal (90 ms vs 30 ms) on 2 of its 4 targets.",
		},
		{
			name: "one target, a decimal ratio",
			over: map[string]model.Baseline{"Zoom": nb(42, 50)},
			fx:   []behavior{targets(rtt(120*time.Millisecond), "Zoom")},
			kind: model.VerdictService, summary: "Zoom is 2.9× slower than your normal (120 ms vs 42 ms).",
			targets: []string{"Zoom"},
		},
		{
			name: "several groups",
			fx:   []behavior{targets(rtt(100*time.Millisecond), "Zoom", "Fastly")},
			kind: model.VerdictService, summary: "2 services are 3.3× slower than your normal (100 ms vs 30 ms): Fastly and Zoom.",
		},
		{
			// 30 ms normal, p95 39: the threshold is max(75, 69) = 75 ms.
			name: "normal variance: every service 2.3× its normal is fine",
			fx:   []behavior{internet(rtt(70 * time.Millisecond))},
			kind: model.VerdictOK, summary: "Everything looks fine.",
			evidence: map[string]float64{"services_vs_normal": 2.333},
		},
		{
			// The 30-minute rule needs > 90 ms here, and a slow build-up
			// would have dragged its baseline up anyway.
			name:     "2.7× the normal is caught where the 30-minute rule is not",
			baseline: 10 * time.Minute,
			fx:       []behavior{targets(rtt(80*time.Millisecond), "Zoom")},
			kind:     model.VerdictService, summary: "Zoom is 2.7× slower than your normal (80 ms vs 30 ms).",
		},
		{
			// A jittery series: p95 150 ms puts the threshold at 180 ms
			// (the 30-minute rule would flag 100 ms).
			name:     "a jittery normal tolerates more",
			baseline: 10 * time.Minute,
			over:     map[string]model.Baseline{"Zoom": nb(30, 150)},
			fx:       []behavior{targets(rtt(100*time.Millisecond), "Zoom")},
			kind:     model.VerdictOK,
		},
		{
			name: "gateway slower than its normal",
			fx:   []behavior{layer(model.LayerGateway, rtt(200*time.Millisecond))},
			kind: model.VerdictLAN, summary: "Your Wi-Fi or router is 100× slower than your normal (200 ms vs 2 ms).",
			evidence: map[string]float64{"gateway_vs_normal": 100, "gateway_baseline_ms": 2, "gateway_rtt_ms": 200},
		},
		{
			// 2 ms normal: 30 ms is 15× but below p95 + 30 ms (32.6).
			name: "a small absolute rise on the gateway is fine",
			fx:   []behavior{layer(model.LayerGateway, rtt(30*time.Millisecond))},
			kind: model.VerdictOK,
		},
		{
			name: "edge slower than its normal",
			fx:   []behavior{layer(model.LayerEdge, rtt(90*time.Millisecond))},
			kind: model.VerdictISP, summary: "Your ISP's network is slow: the router is fine, but the first hop past it is 11× slower than your normal (90 ms vs 8 ms).",
		},
		{
			name: "anycast slower than its normal",
			fx:   []behavior{layer(model.LayerAnycast, rtt(60*time.Millisecond))},
			kind: model.VerdictUpstream, summary: "Problems beyond your ISP: 3 of 3 public DNS anycast servers are 5× slower than your normal (60 ms vs 12 ms).",
		},
		{
			name: "loss keeps the loss sentence",
			fx:   []behavior{targets(every(2, model.ReasonTimeout), "GitHub")},
			kind: model.VerdictService, summary: "Only GitHub looks affected: it loses 50% of probes.",
		},
		{
			name:     "a series without a normal falls back to the 30-minute baseline",
			baseline: 10 * time.Minute,
			over:     map[string]model.Baseline{"Zoom": {}},
			fx:       []behavior{targets(rtt(400*time.Millisecond), "Zoom")},
			kind:     model.VerdictService, summary: "Only Zoom looks affected: it answers in 400 ms instead of the usual 30 ms.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.e.o.Baseline = normals(h.p, c.over)
			base := c.baseline
			if base == 0 {
				base = 2 * time.Minute
			}
			h.run(base)
			if k := h.e.Current().Kind; k != model.VerdictOK {
				t.Fatalf("before the problem: %s (%s)", k, h.e.Current().Summary)
			}
			h.fx = c.fx
			h.run(2 * time.Minute)
			v := h.e.Current()
			if v.Kind != c.kind {
				t.Fatalf("kind %s (%q), want %s; evidence %v", v.Kind, v.Summary, c.kind, v.Evidence)
			}
			if c.summary != "" && v.Summary != c.summary {
				t.Errorf("summary\n got %q\nwant %q", v.Summary, c.summary)
			}
			if c.targets != nil && !slices.Equal(v.Targets, c.targets) {
				t.Errorf("targets %v, want %v", v.Targets, c.targets)
			}
			checkEvidence(t, v.Evidence)
			for k, want := range c.evidence {
				if got, ok := v.Evidence[k]; !ok || got != want {
					t.Errorf("evidence %s = %v (%v), want %v; all %v", k, got, ok, want, v.Evidence)
				}
			}
		})
	}
}

// TestNilBaselineUnchanged: with Options.Baseline nil, or one that knows
// nothing, verdicts and evidence are exactly the 30-minute rule's.
func TestNilBaselineUnchanged(t *testing.T) {
	scenario := func(b func(model.SeriesKey, time.Time) (model.Baseline, bool)) (model.Verdict, []SlowTarget) {
		h := newHarness(t)
		h.e.o.Baseline = b
		h.run(10 * time.Minute)
		h.fx = []behavior{targets(rtt(400*time.Millisecond), "Zoom"), layer(model.LayerEdge, rtt(20*time.Millisecond))}
		h.run(2 * time.Minute)
		return h.e.Current(), h.e.Slow()
	}
	v0, s0 := scenario(nil)
	v1, s1 := scenario(func(model.SeriesKey, time.Time) (model.Baseline, bool) { return model.Baseline{}, false })
	if v0.Kind != model.VerdictService || v0.Summary != "Only Zoom looks affected: it answers in 400 ms instead of the usual 30 ms." {
		t.Fatalf("nil: %+v", v0)
	}
	if v1.Kind != v0.Kind || v1.Summary != v0.Summary || !maps.Equal(v1.Evidence, v0.Evidence) {
		t.Errorf("unknown normals changed the verdict:\n%+v\n%+v", v0, v1)
	}
	for k := range v0.Evidence {
		if strings.HasSuffix(k, "_vs_normal") {
			t.Errorf("evidence %s without normals", k)
		}
	}
	if len(s0) != 0 || len(s1) != 0 {
		t.Errorf("Slow without normals: %v %v", s0, s1)
	}
}

func TestSlow(t *testing.T) {
	h := newHarness(t)
	var asked []time.Time
	nf := normals(h.p, map[string]model.Baseline{"Zoom": nb(40, 50)})
	h.e.o.Baseline = func(k model.SeriesKey, at time.Time) (model.Baseline, bool) {
		asked = append(asked, at)
		return nf(k, at)
	}
	e := Engine(h.e)
	slow, each := SlowSource(e), VsNormalSource(e)
	h.run(2 * time.Minute)
	if s := slow(); len(s) != 0 {
		t.Fatalf("slow while fine: %v", s)
	}
	if !slices.Contains(asked, h.now) {
		t.Error("Baseline not asked about the evaluation time")
	}

	// 2.3× on the AWS targets and Slack, 2.4× on Zoom: no verdict, but the
	// UI line lists them; 5 ms on the 2 ms gateway is not news.
	h.fx = []behavior{
		targets(rtt(70*time.Millisecond), "AWS us-east-1", "AWS eu-west-1", "AWS ap-south-1", "AWS sa-east-1", "Slack"),
		targets(rtt(95*time.Millisecond), "Zoom"),
		layer(model.LayerGateway, rtt(5*time.Millisecond)),
		targets(rtt(50*time.Millisecond), "GitHub"), // 1.7×
	}
	h.run(2 * time.Minute)
	if v := h.e.Current(); v.Kind != model.VerdictOK {
		t.Fatalf("verdict %s (%s)", v.Kind, v.Summary)
	}
	s := slow()
	var names []string
	for _, x := range s {
		names = append(names, x.Target)
	}
	want := []string{"Zoom", "AWS ap-south-1", "AWS eu-west-1", "AWS sa-east-1", "AWS us-east-1", "Slack"}
	if !slices.Equal(names, want) {
		t.Fatalf("Slow targets %v, want %v", names, want)
	}
	if z := s[0]; z.Ratio != 2.375 || z.NowMs != 95 || z.NormalMs != 40 {
		t.Errorf("Zoom: %+v", z)
	}
	if a := s[1]; a.Ratio != 2.333 || a.NowMs != 70 || a.NormalMs != 30 {
		t.Errorf("AWS: %+v", a)
	}

	// Every (target, kind) with a normal goes to the metrics.
	got := map[string]float64{}
	each(func(name string, k model.ProbeKind, r float64) { got[name+"/"+k.String()] = r })
	if got["Zoom/tcp"] != 2.375 || got["GitHub/tcp"] != round3(50.0/30) || got["Router/icmp"] != 2.5 {
		t.Errorf("EachVsNormal: %v", got)
	}
	for _, k := range []string{"Router/tcp", "Zoom/https", "Zoom/icmp"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s: a kind the target does not probe", k)
		}
	}

	// Other Engines: no signal.
	if SlowSource(fakeEngine{})() != nil {
		t.Error("SlowSource of another Engine")
	}
	VsNormalSource(fakeEngine{})(func(string, model.ProbeKind, float64) { t.Error("VsNormalSource of another Engine") })
}

type fakeEngine struct{ Engine }

func TestNormalSummariesIPFree(t *testing.T) {
	h := newHarness(t)
	h.p.Groups[1].Title = "aws.amazon.com"
	h.p.Targets[slices.IndexFunc(h.p.Targets, func(t model.Target) bool { return t.Name == "Zoom" })].Name = "10.1.2.3"
	h.e.o.Baseline = normals(h.p, nil)
	h.run(2 * time.Minute)
	h.fx = []behavior{targets(rtt(90*time.Millisecond), "AWS us-east-1", "AWS eu-west-1", "AWS ap-south-1", "AWS sa-east-1")}
	h.run(2 * time.Minute) // run checks every summary (checkSummary)
	v := h.e.Current()
	if v.Summary != "one group of services is 3× slower than your normal (90 ms vs 30 ms)." {
		t.Errorf("group: %q", v.Summary)
	}
	h.fx = []behavior{targets(rtt(120*time.Millisecond), "10.1.2.3")}
	h.run(3 * time.Minute)
	if v := h.e.Current(); v.Summary != "one monitored service is 4× slower than your normal (120 ms vs 30 ms)." {
		t.Errorf("target: %q", v.Summary)
	}

	nm := newNamer(h.p)
	for s, safe := range map[string]bool{
		"Zoom is 2.9× slower than your normal (120 ms vs 42 ms).": true,
		"Zoom is 14× slower than your normal (120 ms vs 42 ms).":  true,
		"10.0.0.1 is 2.9× slower than your normal.":               false,
		"Router 1.2.3.4× is slow.":                                false,
		"Router 192.168.1× is slow.":                              false,
		"Router 2.35× is slow.":                                   false,
		"fe80::1 is 2.9× slower.":                                 false,
		"github.com is 2.9× slower.":                              false,
	} {
		if got := !nm.unsafe(s); got != safe {
			t.Errorf("unsafe(%q) = %v", s, !got)
		}
	}
}
