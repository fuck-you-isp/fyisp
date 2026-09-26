package verdict

import (
	"context"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
)

// ---- fake store ----

type fakeStore struct {
	mu      sync.Mutex
	now     func() time.Time
	incs    map[int64]model.Incident
	updated map[int64]time.Time
	nextID  int64
	saves   int
	fail    bool
}

func newFakeStore(now func() time.Time) *fakeStore {
	return &fakeStore{now: now, incs: map[int64]model.Incident{}, updated: map[int64]time.Time{}}
}

func (f *fakeStore) SaveIncident(_ context.Context, in *model.Incident) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return context.DeadlineExceeded
	}
	f.saves++
	if in.ID == 0 {
		f.nextID++
		in.ID = f.nextID
	}
	c := *in
	c.Targets = slices.Clone(in.Targets)
	f.incs[in.ID] = c
	f.updated[in.ID] = f.now()
	return nil
}

func (f *fakeStore) Incidents(_ context.Context, from, to time.Time) ([]model.Incident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.Incident
	for _, in := range f.incs {
		if !in.Start.After(to) && (in.End.IsZero() || !in.End.Before(from)) {
			out = append(out, in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.After(out[j].Start) })
	return out, nil
}

func (f *fakeStore) OpenIncidents(_ context.Context, fn func(model.Incident, time.Time)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, in := range f.incs {
		if in.End.IsZero() {
			fn(in, f.updated[id])
		}
	}
	return nil
}

func (f *fakeStore) all() []model.Incident {
	out, _ := f.Incidents(ctx, time.Time{}, t0.Add(1000*time.Hour))
	slices.Reverse(out) // oldest first
	return out
}

// ---- profile and synthetic probes ----

func testProfile() *model.Profile {
	p := &model.Profile{
		Name: "test",
		Groups: []model.Group{
			{ID: "path", Title: "Network path"},
			{ID: "aws", Title: "Amazon Web Services"},
			{ID: "saas", Title: "Apps"},
			{ID: "cdn", Title: "CDNs"},
		},
	}
	icmp := []model.ProbeKind{model.KindICMP}
	add := func(name, host, group, layer string, kinds []model.ProbeKind) {
		p.Targets = append(p.Targets, model.Target{Name: name, Host: host, Group: group, Layer: layer, Kinds: kinds, Interval: 15 * time.Second})
	}
	add("Router", model.HostGateway, "path", model.LayerGateway, icmp)
	add("ISP edge", model.HostEdge, "path", model.LayerEdge, icmp)
	add("Cloudflare DNS", "1.1.1.1", "path", model.LayerAnycast, icmp)
	add("Google DNS", "8.8.8.8", "path", model.LayerAnycast, icmp)
	add("Quad9", "9.9.9.9", "path", model.LayerAnycast, icmp)
	for _, r := range []string{"us-east-1", "eu-west-1", "ap-south-1", "sa-east-1"} {
		add("AWS "+r, "ec2."+r+".amazonaws.com", "aws", "", nil)
	}
	add("GitHub", "github.com", "saas", "", nil)
	add("Slack", "slack.com", "saas", "", nil)
	add("Zoom", "zoom.us", "saas", "", nil)
	add("Google Meet", "meet.google.com", "saas", "", nil)
	add("Cloudflare", "www.cloudflare.com", "cdn", "", nil)
	add("Fastly", "www.fastly.com", "cdn", "", nil)
	p.Targets[len(p.Targets)-1].HostOverrides = map[model.ProbeKind]string{model.KindICMP: "151.101.1.57"}
	return p
}

// outcome is what a probe of (target, kind) at t returns.
type outcome struct {
	lost   bool
	reason model.Reason
	rtt    time.Duration
}

type behavior func(t model.Target, k model.ProbeKind, at time.Time) (outcome, bool)

func defaultRTT(t model.Target) time.Duration {
	switch t.Layer {
	case model.LayerGateway:
		return 2 * time.Millisecond
	case model.LayerEdge:
		return 8 * time.Millisecond
	case model.LayerAnycast:
		return 12 * time.Millisecond
	}
	return 30 * time.Millisecond
}

type harness struct {
	t    *testing.T
	p    *model.Profile
	now  time.Time
	st   *fakeStore
	e    *engine
	fx   []behavior // later ones win
	seen map[model.VerdictKind]bool
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, p: testProfile(), now: t0, seen: map[model.VerdictKind]bool{}}
	h.st = newFakeStore(func() time.Time { return h.now })
	h.e = newEngine(func() *model.Profile { return h.p }, h.st, Options{Now: func() time.Time { return h.now }})
	return h
}

func kindsOf(t model.Target) []model.ProbeKind {
	if len(t.Kinds) > 0 {
		return t.Kinds
	}
	return []model.ProbeKind{model.KindHTTPS, model.KindTCP, model.KindICMP}
}

// run advances the clock in 1s steps: ICMP every 5s, HTTPS/TCP every 15s,
// an evaluation every 5s.
func (h *harness) run(d time.Duration) {
	h.t.Helper()
	end := h.now.Add(d)
	for h.now.Before(end) {
		h.now = h.now.Add(time.Second)
		sec := h.now.Unix()
		for _, t := range h.p.Targets {
			for _, k := range kindsOf(t) {
				iv := int64(15)
				if k == model.KindICMP {
					iv = 5
				}
				if sec%iv != 0 {
					continue
				}
				o, send := outcome{rtt: defaultRTT(t)}, true
				for _, f := range h.fx {
					if x, ok := f(t, k, h.now); ok {
						o = x
					}
				}
				if o.reason == 255 {
					send = false
				}
				if send {
					h.e.Observe(model.Sample{Key: model.SeriesKey{Target: t.Name, Kind: k}, Slot: h.now, RTT: o.rtt, Lost: o.lost, Reason: o.reason})
				}
			}
		}
		if sec%5 == 0 {
			h.e.step(ctx, h.now)
			v := h.e.Current()
			h.seen[v.Kind] = true
			checkSummary(h.t, h.p, v.Summary)
		}
	}
}

func (h *harness) with(f behavior) { h.fx = append(h.fx, f) }

// target returns a behavior for the named targets.
func targets(o func(at time.Time) outcome, names ...string) behavior {
	return func(t model.Target, _ model.ProbeKind, at time.Time) (outcome, bool) {
		if slices.Contains(names, t.Name) {
			return o(at), true
		}
		return outcome{}, false
	}
}

func layer(l string, o func(at time.Time) outcome) behavior {
	return func(t model.Target, _ model.ProbeKind, at time.Time) (outcome, bool) {
		if t.Layer == l {
			return o(at), true
		}
		return outcome{}, false
	}
}

func internet(o func(at time.Time) outcome) behavior {
	return func(t model.Target, _ model.ProbeKind, at time.Time) (outcome, bool) {
		if t.Layer == "" {
			return o(at), true
		}
		return outcome{}, false
	}
}

func always(r model.Reason) func(time.Time) outcome {
	return func(time.Time) outcome { return outcome{lost: true, reason: r} }
}

// every loses one probe in n (by slot second).
func every(n int64, r model.Reason) func(time.Time) outcome {
	return func(at time.Time) outcome {
		if (at.Unix()/5)%n == 0 {
			return outcome{lost: true, reason: r}
		}
		return outcome{rtt: 30 * time.Millisecond}
	}
}

func rtt(d time.Duration) func(time.Time) outcome {
	return func(time.Time) outcome { return outcome{rtt: d} }
}

var ipPattern = regexp.MustCompile(`\d+\.\d+`)

func checkSummary(t *testing.T, p *model.Profile, s string) {
	t.Helper()
	if s == "" {
		t.Error("empty summary")
	}
	if ipPattern.MatchString(ratioTok.ReplaceAllString(s, "$1")) { // "2.3×" is a ratio, not an address
		t.Errorf("summary has an address-like number: %q", s)
	}
	for _, tg := range p.Targets {
		for _, h := range append([]string{tg.Host}, mapValues(tg.HostOverrides)...) {
			if h != "" && !strings.HasPrefix(h, "@") && strings.Contains(strings.ToLower(s), strings.ToLower(h)) {
				t.Errorf("summary has host %q: %q", h, s)
			}
		}
	}
}

// ---- rules ----

func TestRules(t *testing.T) {
	cases := []struct {
		name     string
		baseline time.Duration // healthy time before the problem (default 2m)
		fx       []behavior
		stop     bool // no samples at all during the problem
		kind     model.VerdictKind
		summary  string
		targets  []string
		evidence map[string]float64
	}{
		{name: "ok", kind: model.VerdictOK, summary: "Everything looks fine."},
		{
			name: "lan",
			fx: []behavior{
				layer(model.LayerGateway, every(2, model.ReasonTimeout)),
				layer(model.LayerEdge, every(2, model.ReasonTimeout)),
				internet(every(2, model.ReasonTimeout)),
			},
			kind: model.VerdictLAN, summary: "Your Wi-Fi or router is dropping 50% of packets.",
			evidence: map[string]float64{"gateway_loss": 0.5},
		},
		{
			name: "isp",
			// This harness pings the layers every 5s (12 samples a minute):
			// the edge must lose 6 of 12 to be significantly worse than
			// the gateway.
			fx: []behavior{
				layer(model.LayerEdge, every(2, model.ReasonTimeout)),
				internet(every(2, model.ReasonTimeout)),
			},
			kind:    model.VerdictISP,
			summary: "Your ISP's network is dropping packets: the router is fine, the first hop past it loses 50%.",
		},
		{
			name: "upstream via anycast",
			fx:   []behavior{layer(model.LayerAnycast, always(model.ReasonTimeout))},
			kind: model.VerdictUpstream, summary: "Problems beyond your ISP: 3 of 3 public DNS anycast servers are unreachable.",
			evidence: map[string]float64{"anycast_unhealthy": 3},
		},
		{
			name: "upstream via anycast majority",
			fx:   []behavior{targets(always(model.ReasonTimeout), "Quad9", "Google DNS")},
			kind: model.VerdictUpstream, summary: "Problems beyond your ISP: 2 of 3 public DNS anycast servers are unreachable.",
		},
		{
			name: "one anycast down is a minority",
			fx:   []behavior{targets(always(model.ReasonTimeout), "Quad9")},
			kind: model.VerdictOK,
		},
		{
			name: "upstream via targets",
			fx: []behavior{targets(always(model.ReasonTimeout),
				"AWS us-east-1", "AWS eu-west-1", "AWS ap-south-1", "GitHub", "Slack")},
			kind: model.VerdictUpstream, summary: "Problems beyond your ISP: 5 of 10 monitored services are having trouble.",
		},
		{
			name: "dns",
			fx: []behavior{targets(always(model.ReasonDNS),
				"GitHub", "Slack", "Zoom", "Google Meet", "Cloudflare", "Fastly")},
			kind: model.VerdictDNS, summary: "Name lookups are failing: 6 of 10 services can't be found, but the network itself looks fine.",
			targets: []string{"Cloudflare", "Fastly", "GitHub", "Google Meet", "Slack", "Zoom"},
		},
		{
			name: "dns on a few targets is a service problem",
			fx:   []behavior{targets(always(model.ReasonDNS), "GitHub", "Slack")},
			kind: model.VerdictService, summary: "Only Apps looks affected (2 of 4 targets).",
			targets: []string{"GitHub", "Slack"},
		},
		{
			name: "service single target",
			fx:   []behavior{targets(every(2, model.ReasonTimeout), "GitHub")},
			kind: model.VerdictService, summary: "Only GitHub looks affected: it loses 50% of probes.",
			targets: []string{"GitHub"},
		},
		{
			name: "service whole group",
			fx: []behavior{targets(always(model.ReasonTimeout),
				"AWS us-east-1", "AWS eu-west-1", "AWS ap-south-1", "AWS sa-east-1")},
			kind: model.VerdictService, summary: "Only Amazon Web Services looks affected (all 4 of its targets).",
			targets: []string{"AWS ap-south-1", "AWS eu-west-1", "AWS sa-east-1", "AWS us-east-1"},
		},
		{
			name: "service part of a group",
			fx:   []behavior{targets(always(model.ReasonRefused), "AWS us-east-1", "AWS eu-west-1")},
			kind: model.VerdictService, summary: "Only Amazon Web Services looks affected (2 of 4 targets).",
		},
		{
			name: "service several groups",
			fx:   []behavior{targets(always(model.ReasonTLS), "Zoom", "Fastly", "AWS sa-east-1", "Slack")},
			kind: model.VerdictService, summary: "4 services look affected: AWS sa-east-1, Fastly, Slack and 1 more.",
		},
		{
			name:     "latency spike on the gateway",
			baseline: 10 * time.Minute,
			fx:       []behavior{layer(model.LayerGateway, rtt(200*time.Millisecond))},
			kind:     model.VerdictLAN, summary: "Your Wi-Fi or router is slow: it answers in 200 ms instead of the usual 2 ms.",
			evidence: map[string]float64{"gateway_loss": 0, "gateway_rtt_ms": 200, "gateway_baseline_ms": 2},
		},
		{
			name:     "latency spike on the edge",
			baseline: 10 * time.Minute,
			fx:       []behavior{layer(model.LayerEdge, rtt(90*time.Millisecond))},
			kind:     model.VerdictISP, summary: "Your ISP's network is slow: the router is fine, but the first hop past it answers in 90 ms instead of the usual 8 ms.",
		},
		{
			name:     "latency spike on one service",
			baseline: 10 * time.Minute,
			fx:       []behavior{targets(rtt(400*time.Millisecond), "Zoom")},
			kind:     model.VerdictService, summary: "Only Zoom looks affected: it answers in 400 ms instead of the usual 30 ms.",
			targets: []string{"Zoom"},
		},
		{
			name:     "small latency rise is fine",
			baseline: 10 * time.Minute,
			fx:       []behavior{targets(rtt(75*time.Millisecond), "Zoom")}, // < 30ms + 50ms
			kind:     model.VerdictOK,
		},
		{
			name: "latency without a baseline is not judged",
			fx:   []behavior{targets(rtt(400*time.Millisecond), "Zoom")},
			kind: model.VerdictOK,
		},
		{
			name: "no network",
			fx: []behavior{
				layer(model.LayerGateway, always(model.ReasonNoNetwork)),
				layer(model.LayerEdge, always(model.ReasonNoNetwork)),
				layer(model.LayerAnycast, always(model.ReasonNoNetwork)),
				internet(always(model.ReasonNoNetwork)),
			},
			kind: model.VerdictNoNetwork, summary: "This device has no working network connection.",
		},
		{
			name: "no network: layers unreachable, internet times out",
			fx: []behavior{
				layer(model.LayerGateway, always(model.ReasonUnreachable)),
				layer(model.LayerEdge, always(model.ReasonUnreachable)),
				layer(model.LayerAnycast, always(model.ReasonUnreachable)),
				internet(always(model.ReasonTimeout)),
			},
			kind: model.VerdictNoNetwork,
		},
		{
			name: "no samples at all",
			stop: true,
			kind: model.VerdictNoNetwork,
		},
		{
			name: "edge unknown: gateway fine, internet broken is upstream beyond the router",
			fx: []behavior{
				layer(model.LayerEdge, func(time.Time) outcome { return outcome{reason: 255} }),
				internet(always(model.ReasonTimeout)),
			},
			kind: model.VerdictUpstream, summary: "Problems beyond your router: 10 of 10 monitored services are having trouble.",
		},
		{
			name: "a kind that never worked is ignored",
			fx: []behavior{func(t model.Target, k model.ProbeKind, _ time.Time) (outcome, bool) {
				if t.Name == "Zoom" && k == model.KindICMP {
					return outcome{lost: true, reason: model.ReasonTimeout}, true
				}
				return outcome{}, false
			}},
			kind: model.VerdictOK,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			// A kind that never worked must be never-working from the start.
			if c.name == "a kind that never worked is ignored" {
				h.fx = c.fx
			}
			base := c.baseline
			if base == 0 {
				base = 2 * time.Minute
			}
			h.run(base)
			if k := h.e.Current().Kind; k != model.VerdictOK {
				t.Fatalf("before the problem: %s (%s)", k, h.e.Current().Summary)
			}
			h.fx = c.fx
			if c.stop {
				h.fx = []behavior{func(model.Target, model.ProbeKind, time.Time) (outcome, bool) { return outcome{reason: 255}, true }}
			}
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

func TestWarmingUp(t *testing.T) {
	h := newHarness(t)
	h.run(55 * time.Second)
	if v := h.e.Current(); v.Kind != model.VerdictWarmingUp || !v.Since.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("%+v", v)
	}
	// Even a broken network is not judged while warming up.
	h2 := newHarness(t)
	h2.with(layer(model.LayerGateway, always(model.ReasonTimeout)))
	h2.run(55 * time.Second)
	if k := h2.e.Current().Kind; k != model.VerdictWarmingUp {
		t.Fatalf("%s", k)
	}
	if len(h2.st.all()) != 0 {
		t.Fatal("incident while warming up")
	}
	h.run(10 * time.Second)
	if v := h.e.Current(); v.Kind != model.VerdictOK {
		t.Fatalf("after warm-up: %+v", v)
	}
	// Right after warm-up the judgement is taken at once (no hysteresis).
	h2.run(10 * time.Second)
	if k := h2.e.Current().Kind; k != model.VerdictLAN {
		t.Fatalf("after warm-up: %s", k)
	}
}

func TestLayersAndMetricsSource(t *testing.T) {
	h := newHarness(t)
	h.with(layer(model.LayerEdge, func(time.Time) outcome { return outcome{reason: 255} })) // edge not discovered
	h.with(layer(model.LayerAnycast, always(model.ReasonTimeout)))
	h.run(2 * time.Minute)
	v, l := MetricsSource(h.e)()
	if v.Kind != model.VerdictUpstream {
		t.Fatalf("%s", v.Kind)
	}
	ev := h.e.Current().Evidence
	if _, ok := ev["edge_loss"]; ok {
		t.Errorf("edge_loss for an unknown edge: %v", ev)
	}
	if _, ok := ev["edge_rtt_ms"]; ok {
		t.Errorf("edge_rtt_ms for an unknown edge: %v", ev)
	}
	for _, k := range []string{"gateway_loss", "gateway_rtt_ms", "anycast_loss", "services_loss"} {
		if _, ok := ev[k]; !ok {
			t.Errorf("no %s: %v", k, ev)
		}
	}
	if ev["anycast_loss"] != 1 || ev["gateway_rtt_ms"] != 2 || ev["services_loss"] != 0 {
		t.Errorf("evidence %v", ev)
	}
	if l[model.LayerGateway] == nil || !*l[model.LayerGateway] || l[model.LayerEdge] != nil || l[model.LayerAnycast] == nil || *l[model.LayerAnycast] {
		t.Fatalf("layers %v %v %v", l[model.LayerGateway], l[model.LayerEdge], l[model.LayerAnycast])
	}
}

// Target names that look like hosts are never shown.
func TestSummaryHidesHostLikeNames(t *testing.T) {
	h := newHarness(t)
	h.p.Targets = append(h.p.Targets,
		model.Target{Name: "example.com", Host: "example.com", Group: "misc"},
		model.Target{Name: "my box", Host: "10.1.2.3", Group: "misc"},
	)
	h.p.Groups = append(h.p.Groups, model.Group{ID: "misc", Title: "status.example.org"})
	h.run(2 * time.Minute)
	h.with(targets(always(model.ReasonTimeout), "example.com"))
	h.run(2 * time.Minute)
	v := h.e.Current()
	if v.Kind != model.VerdictService || v.Summary != "Only one monitored service looks affected: it loses 100% of probes." {
		t.Fatalf("%s %q", v.Kind, v.Summary)
	}
	h.fx = nil
	h.run(2 * time.Minute)
	h.with(targets(always(model.ReasonTimeout), "example.com", "my box"))
	h.run(2 * time.Minute)
	if v := h.e.Current(); v.Summary != "Only one group of services looks affected (all 2 of its targets)." {
		t.Fatalf("%q", v.Summary)
	}
	nm := newNamer(h.p)
	for _, s := range []string{"see 10.1.2.3", "fe80::1 down", "ping github.com", "my EXAMPLE.COM"} {
		if !nm.unsafe(s) {
			t.Errorf("%q considered safe", s)
		}
	}
	for k, s := range generic {
		checkSummary(t, h.p, s)
		if nm.clean(k, "bad 1.1.1.1") != s {
			t.Errorf("clean %s", k)
		}
	}
}

// ---- hysteresis and incidents (synthetic judgements) ----

type seq struct {
	t  *testing.T
	e  *engine
	st *fakeStore
	at time.Time
}

func newSeq(t *testing.T) *seq {
	s := &seq{t: t, at: t0}
	s.st = newFakeStore(func() time.Time { return s.at })
	s.e = newEngine(func() *model.Profile { return nil }, s.st, Options{Now: func() time.Time { return s.at }})
	return s
}

func jd(k model.VerdictKind, loss float64, targets ...string) judgement {
	return judgement{v: model.Verdict{Kind: k, Summary: string(k) + " summary", Targets: targets}, loss: loss}
}

// feed applies j at every 5s evaluation for d.
func (s *seq) feed(d time.Duration, j judgement) {
	end := s.at.Add(d)
	for s.at.Before(end) {
		s.at = s.at.Add(5 * time.Second)
		s.e.apply(ctx, s.at, j)
	}
}

func (s *seq) warm() {
	s.feed(65*time.Second, jd(model.VerdictOK, 0))
	if k := s.e.Current().Kind; k != model.VerdictOK {
		s.t.Fatalf("after warm-up %s", k)
	}
}

func TestHysteresisOneBadEvaluation(t *testing.T) {
	s := newSeq(t)
	s.warm()
	s.feed(5*time.Second, jd(model.VerdictLAN, 0.5))
	if k := s.e.Current().Kind; k != model.VerdictOK {
		t.Fatalf("one bad evaluation flipped to %s", k)
	}
	s.feed(5*time.Second, jd(model.VerdictOK, 0))
	s.feed(5*time.Second, jd(model.VerdictISP, 0.5))
	s.feed(5*time.Second, jd(model.VerdictLAN, 0.5))
	if k := s.e.Current().Kind; k != model.VerdictOK {
		t.Fatalf("two different bad evaluations flipped to %s", k)
	}
	if n := len(s.st.all()); n != 0 {
		t.Fatalf("%d incidents", n)
	}
	s.feed(5*time.Second, jd(model.VerdictLAN, 0.5))
	v := s.e.Current()
	if v.Kind != model.VerdictLAN || !v.Since.Equal(s.at.Add(-5*time.Second)) {
		t.Fatalf("%+v", v)
	}
}

func TestHysteresisRecovery(t *testing.T) {
	s := newSeq(t)
	s.warm()
	s.feed(30*time.Second, jd(model.VerdictISP, 0.3))
	// ok evaluations at +0s ... +55s: not yet 60s of ok.
	s.feed(60*time.Second, jd(model.VerdictOK, 0))
	if k := s.e.Current().Kind; k != model.VerdictISP {
		t.Fatalf("recovered after 55s: %s", k)
	}
	// A bad evaluation restarts the 60s.
	s.feed(5*time.Second, jd(model.VerdictISP, 0.3))
	okFrom := s.at.Add(5 * time.Second)
	s.feed(60*time.Second, jd(model.VerdictOK, 0))
	if k := s.e.Current().Kind; k != model.VerdictISP {
		t.Fatalf("recovered: %s", k)
	}
	s.feed(5*time.Second, jd(model.VerdictOK, 0))
	v := s.e.Current()
	if v.Kind != model.VerdictOK || !v.Since.Equal(okFrom) {
		t.Fatalf("%+v, want ok since %v", v, okFrom)
	}
	inc := s.st.all()
	if len(inc) != 1 || !inc[0].End.Equal(okFrom) || inc[0].Kind != model.VerdictISP {
		t.Fatalf("%+v", inc)
	}
}

func TestIncidentLifecycle(t *testing.T) {
	s := newSeq(t)
	s.warm()
	start := s.at.Add(5 * time.Second)
	s.feed(10*time.Second, jd(model.VerdictService, 0.3, "GitHub"))
	inc := s.st.all()
	if len(inc) != 1 || !inc[0].Start.Equal(start) || !inc[0].End.IsZero() || inc[0].Kind != model.VerdictService || inc[0].PeakLoss != 0.3 {
		t.Fatalf("opened: %+v", inc)
	}
	saves := s.st.saves
	// Updates are saved at most every 30s.
	s.feed(20*time.Second, judgement{v: model.Verdict{Kind: model.VerdictService, Summary: "worse", Targets: []string{"Slack"}}, loss: 0.8})
	if s.st.saves != saves {
		t.Fatalf("saved within 30s: %d", s.st.saves-saves)
	}
	s.feed(10*time.Second, jd(model.VerdictService, 0.4, "GitHub"))
	inc = s.st.all()
	if s.st.saves != saves+1 || inc[0].PeakLoss != 0.8 || inc[0].Summary != "worse" || !slices.Equal(inc[0].Targets, []string{"GitHub", "Slack"}) {
		t.Fatalf("update: saves %d %+v", s.st.saves-saves, inc)
	}
	// Heartbeat while unchanged.
	s.feed(30*time.Second, jd(model.VerdictService, 0.1, "GitHub"))
	if s.st.saves != saves+2 {
		t.Fatalf("heartbeat: saves %d", s.st.saves-saves)
	}
	// Kind change: service -> upstream closes one and opens another.
	change := s.at.Add(5 * time.Second)
	s.feed(10*time.Second, jd(model.VerdictUpstream, 0.6))
	inc = s.st.all()
	if len(inc) != 2 || !inc[0].End.Equal(change) || !inc[1].Start.Equal(change) || !inc[1].End.IsZero() || inc[1].Kind != model.VerdictUpstream {
		t.Fatalf("change: %+v", inc)
	}
	if v := s.e.Current(); v.Kind != model.VerdictUpstream || !v.Since.Equal(change) {
		t.Fatalf("%+v", v)
	}
	// Recovery closes it at the start of the ok streak.
	okFrom := s.at.Add(5 * time.Second)
	s.feed(65*time.Second, jd(model.VerdictOK, 0))
	inc = s.st.all()
	if !inc[1].End.Equal(okFrom) {
		t.Fatalf("close: %+v", inc[1])
	}
	got, _ := s.e.Incidents(ctx, t0, s.at)
	if len(got) != 2 || got[0].Kind != model.VerdictUpstream {
		t.Fatalf("Incidents: %+v", got)
	}
}

func TestIncidentSaveRetry(t *testing.T) {
	s := newSeq(t)
	s.warm()
	s.st.fail = true
	s.feed(10*time.Second, jd(model.VerdictLAN, 0.5))
	s.feed(65*time.Second, jd(model.VerdictOK, 0)) // closes; the save fails
	if len(s.st.all()) != 0 {
		t.Fatal("saved while failing")
	}
	s.st.fail = false
	s.feed(5*time.Second, jd(model.VerdictOK, 0))
	inc := s.st.all()
	if len(inc) != 1 || inc[0].End.IsZero() {
		t.Fatalf("retry: %+v", inc)
	}
}

func TestClockJumpClosesIncident(t *testing.T) {
	s := newSeq(t)
	s.warm()
	s.feed(30*time.Second, jd(model.VerdictLAN, 0.5))
	last := s.at
	s.at = s.at.Add(10 * time.Minute) // slept
	s.feed(5*time.Second, jd(model.VerdictLAN, 0.5))
	if k := s.e.Current().Kind; k != model.VerdictWarmingUp {
		t.Fatalf("after sleep %s", k)
	}
	inc := s.st.all()
	if len(inc) != 1 || !inc[0].End.Equal(last) {
		t.Fatalf("%+v", inc)
	}
}

func TestCrashRecovery(t *testing.T) {
	for _, c := range []struct {
		name      string
		age       time.Duration // since the open incident's last save
		kind      model.VerdictKind
		wantEnd   bool
		continue_ bool
	}{
		{"stale is closed at its last save", 10 * time.Minute, model.VerdictLAN, true, false},
		{"fresh and same kind continues", 30 * time.Second, model.VerdictLAN, false, true},
		{"fresh but now ok is closed at its last save", 30 * time.Second, model.VerdictOK, true, false},
		{"fresh but another kind is closed", 30 * time.Second, model.VerdictISP, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSeq(t)
			s.at = t0.Add(-time.Hour)
			prev := &model.Incident{Start: t0.Add(-time.Hour), Kind: model.VerdictLAN, Summary: "old", PeakLoss: 0.9, Targets: []string{"a"}}
			s.st.SaveIncident(ctx, prev)
			s.at = t0.Add(-c.age)
			s.st.SaveIncident(ctx, prev)
			lastSave := s.at
			s.at = t0
			s.e.recoverIncidents(ctx)
			if c.age > staleAfter {
				if in := s.st.all()[0]; !in.End.Equal(lastSave) {
					t.Fatalf("stale not closed at start: %+v", in)
				}
			}
			s.feed(65*time.Second, jd(c.kind, 0.4, "b"))
			inc := s.st.all()
			switch {
			case c.continue_:
				if len(inc) != 1 || !inc[0].End.IsZero() || inc[0].PeakLoss != 0.9 || !slices.Equal(inc[0].Targets, []string{"a", "b"}) {
					t.Fatalf("not continued: %+v", inc)
				}
				if v := s.e.Current(); v.Kind != model.VerdictLAN || !v.Since.Equal(prev.Start) {
					t.Fatalf("%+v", v)
				}
			case c.wantEnd:
				if !inc[0].End.Equal(lastSave) {
					t.Fatalf("not closed at last save: %+v", inc[0])
				}
				want := 1
				if c.kind != model.VerdictOK {
					want = 2
				}
				if len(inc) != want {
					t.Fatalf("%d incidents: %+v", len(inc), inc)
				}
			}
		})
	}
}

// Run evaluates on its ticker and saves the open incident on exit.
func TestRun(t *testing.T) {
	st := newFakeStore(time.Now)
	e := New(func() *model.Profile { return testProfile() }, st, Options{Interval: 10 * time.Millisecond})
	c, cancel := context.WithCancel(ctx)
	done := make(chan error)
	go func() { done <- e.Run(c) }()
	for i := 0; i < 20; i++ {
		e.Observe(model.Sample{Key: model.SeriesKey{Target: "GitHub", Kind: model.KindTCP}, Slot: time.Now(), RTT: time.Millisecond})
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if k := e.Current().Kind; k != model.VerdictWarmingUp {
		t.Fatalf("%s", k)
	}
}

// Memory stays bounded: raw samples older than keepRaw are dropped and
// series without samples for the baseline span are forgotten.
func TestBoundedMemory(t *testing.T) {
	e := newEngine(func() *model.Profile { return nil }, nil, Options{})
	k := model.SeriesKey{Target: "x", Kind: model.KindICMP}
	for i := 0; i < 5000; i++ {
		e.Observe(model.Sample{Key: k, Slot: t0.Add(time.Duration(i) * 100 * time.Millisecond), RTT: time.Millisecond})
	}
	if n := len(e.series[k].raw); n != maxRaw {
		t.Fatalf("raw %d", n)
	}
	e.collect(t0.Add(500*time.Second), nil)
	if n := len(e.series[k].raw); n > int(keepRaw/(100*time.Millisecond))+1 {
		t.Fatalf("raw after trim %d", n)
	}
	e.collect(t0.Add(2*time.Hour), nil)
	if len(e.series) != 0 {
		t.Fatal("stale series kept")
	}
	// Gaps are ignored; duplicates and late samples are handled.
	e.Observe(model.Sample{Key: k, Slot: t0, Lost: true, Reason: model.ReasonGap})
	if len(e.series) != 0 {
		t.Fatal("gap recorded")
	}
	e.Observe(model.Sample{Key: k, Slot: t0.Add(time.Second), RTT: time.Millisecond})
	e.Observe(model.Sample{Key: k, Slot: t0, RTT: time.Millisecond})
	e.Observe(model.Sample{Key: k, Slot: t0, RTT: time.Millisecond})
	if r := e.series[k].raw; len(r) != 2 || r[0].ms > r[1].ms {
		t.Fatalf("%+v", r)
	}
}

var evidenceKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// checkEvidence enforces the web contract: identifier keys, fractions for
// *_loss, and the layer keys whenever the test profile's layers report.
func checkEvidence(t *testing.T, ev map[string]float64) {
	t.Helper()
	for k, v := range ev {
		if !evidenceKey.MatchString(k) {
			t.Errorf("evidence key %q", k)
		}
		if strings.HasSuffix(k, "_loss") && (v < 0 || v > 1) {
			t.Errorf("evidence %s = %v", k, v)
		}
	}
}
