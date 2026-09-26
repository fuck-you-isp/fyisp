package main

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// fakeVerdict implements web.VerdictSource: it cycles through every verdict
// kind (or stays on one) and serves a few synthetic incidents matching the
// backfilled history, plus the current one while it is not OK.
type fakeVerdict struct {
	mu      sync.Mutex
	start   time.Time
	cycle   time.Duration
	fixed   model.VerdictKind
	noEdge  bool // -edge-undiscovered: no ISP-edge evidence
	past    []model.Incident
	started map[int]time.Time // cycle index -> since
}

type scripted struct {
	kind     model.VerdictKind
	summary  string
	targets  []string
	evidence map[string]float64
}

var script = []scripted{
	{model.VerdictService, "Your connection is fine. AWS-ap-east-1 and Discord are not responding; that is on their side, not yours.",
		[]string{"AWS-ap-east-1", "Discord"},
		map[string]float64{"gateway_loss": 0, "edge_loss": 0, "anycast_loss": 0, "services_loss": 0.02, "gateway_rtt_ms": 1.6, "edge_rtt_ms": 7.4, "anycast_rtt_ms": 13.1}},
	{model.VerdictOK, "Everything looks healthy: your router, your ISP and the internet are all responding normally.", nil,
		map[string]float64{"gateway_loss": 0, "edge_loss": 0, "anycast_loss": 0, "services_loss": 0.001}},
	{model.VerdictISP, "Your ISP is dropping 38% of packets. Your router and Wi-Fi are fine, so this is your ISP's fault.", nil,
		map[string]float64{"gateway_loss": 0, "edge_loss": 0.38, "anycast_loss": 0.41, "services_loss": 0.4}},
	{model.VerdictLAN, "Your Wi-Fi or router is dropping 22% of packets. Your ISP cannot be blamed for this one.", nil,
		map[string]float64{"gateway_loss": 0.22, "edge_loss": 0.25, "anycast_loss": 0.24, "services_loss": 0.25}},
	{model.VerdictUpstream, "Your ISP's first hop answers, but most of the internet does not: trouble beyond your ISP's edge or in its upstream links.", nil,
		map[string]float64{"gateway_loss": 0, "edge_loss": 0, "anycast_loss": 0.7, "services_loss": 0.72}},
	{model.VerdictDNS, "Name lookups are failing for most services while the network path is fine: a DNS problem.", nil,
		map[string]float64{"gateway_loss": 0, "edge_loss": 0, "anycast_loss": 0, "services_loss": 0.6}},
	{model.VerdictNoNetwork, "This computer has no network connection (no route to the internet).", nil,
		map[string]float64{"gateway_loss": 1, "edge_loss": 1, "anycast_loss": 1, "services_loss": 1}},
	{model.VerdictWarmingUp, "Collecting data: a verdict needs about two minutes of measurements.", nil, nil},
}

func newFakeVerdict(now time.Time, backfill, cycle time.Duration, fixed model.VerdictKind, noEdge bool) *fakeVerdict {
	f := &fakeVerdict{start: now, cycle: cycle, fixed: fixed, noEdge: noEdge, started: map[int]time.Time{}}
	if backfill > 0 {
		_, isp, lan := windows(backfill, now)
		at := func(frac float64) time.Time { return now.Add(-time.Duration(float64(backfill) * frac)) }
		f.past = []model.Incident{
			{ID: 1, Start: lan.from, End: lan.to, Kind: model.VerdictLAN, PeakLoss: 0.8,
				Summary: "Your Wi-Fi or router dropped most packets for a few minutes."},
			{ID: 2, Start: at(0.5), End: at(0.5).Add(4 * time.Minute), Kind: model.VerdictDNS, PeakLoss: 0.3,
				Summary: "Name lookups failed for most services; the network path was fine."},
			{ID: 3, Start: isp.from, End: isp.to, Kind: model.VerdictISP, PeakLoss: 0.6,
				Summary: "Your ISP dropped 60% of packets while your router was fine."},
			{ID: 4, Start: at(0.15), End: at(0.15).Add(6 * time.Minute), Kind: model.VerdictService, PeakLoss: 0.12,
				Summary: "AWS-ap-east-1 was unreachable; everything else was fine.", Targets: []string{"AWS-ap-east-1"}},
		}
	}
	return f
}

// state returns the scripted verdict now and its cycle index.
func (f *fakeVerdict) state(now time.Time) (scripted, int, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := 0
	if f.fixed != "" {
		for i, s := range script {
			if s.kind == f.fixed {
				idx = i
			}
		}
		return script[idx], -1, f.start.Add(-13 * time.Minute)
	}
	n := 0
	if f.cycle > 0 {
		n = int(now.Sub(f.start) / f.cycle)
	}
	idx = n % len(script)
	since, ok := f.started[n]
	if !ok {
		since = f.start.Add(time.Duration(n) * f.cycle)
		if n == 0 {
			since = since.Add(-13 * time.Minute) // make the first one look established
		}
		f.started[n] = since
	}
	return script[idx], n, since
}

func (f *fakeVerdict) Current() model.Verdict {
	s, _, since := f.state(time.Now())
	ev := map[string]float64{}
	for k, v := range s.evidence {
		if f.noEdge && strings.HasPrefix(k, "edge_") {
			continue
		}
		ev[k] = v
	}
	return model.Verdict{Kind: s.kind, Since: since, Summary: s.summary, Targets: s.targets, Evidence: ev}
}

func (f *fakeVerdict) Incidents(_ context.Context, from, to time.Time) ([]model.Incident, error) {
	all := append([]model.Incident(nil), f.past...)
	s, n, since := f.state(time.Now())
	if s.kind != model.VerdictOK && s.kind != model.VerdictWarmingUp {
		all = append(all, model.Incident{ID: int64(100 + n), Start: since, Kind: s.kind, Summary: s.summary,
			Targets: s.targets, PeakLoss: s.evidence["services_loss"]})
	}
	var out []model.Incident
	for _, in := range all {
		end := in.End
		if end.IsZero() {
			end = time.Now()
		}
		if in.Start.Before(to) && end.After(from) {
			out = append(out, in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.After(out[j].Start) })
	return out, nil
}
