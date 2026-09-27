package report

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"sort"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/redact"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// Deps is what a report reads. Only Profile and Store are required.
type Deps struct {
	Profile func() *model.Profile
	Store   store.Reader
	// Trace reads per-hop statistics and route changes (nil: no trace
	// evidence section).
	Trace store.TraceReader
	// Incidents returns the outage log overlapping [from, to] (nil: none).
	Incidents func(ctx context.Context, from, to time.Time) ([]model.Incident, error)
	// Annotations (nil: no notes section).
	Annotations store.AnnotationStore
	// Baselines returns a series' normal as of a time (nil: no "vs normal").
	Baselines func(model.SeriesKey, time.Time) (model.Baseline, bool)
	// ISP returns the ISP edge hop's owner (nil or 0: derived from the
	// traced routes when possible).
	ISP func() (asn uint32, owner string)
	// Version is shown in reports that are not redacted.
	Version string
}

// Limits.
const (
	// ChartPoints is the most buckets per chart series (a 7-day report
	// uses the hourly tier: 168 points).
	ChartPoints = 600
	// MaxChartSeries is the most targets drawn in one chart (one colour
	// each); the rest are in the group's table.
	MaxChartSeries = 8
	// MaxIncidentRows caps the outage log table (totals include all).
	MaxIncidentRows = 200
	// MaxTraceTargets caps the trace evidence section.
	MaxTraceTargets = 4
	// MaxRouteChanges caps the route changes listed per target.
	MaxRouteChanges = 20
	// DefaultRange is used when Options.From is zero.
	DefaultRange = 7 * 24 * time.Hour
)

// New returns a Builder over d.
func New(d Deps) Builder { return &builder{d: d} }

type builder struct{ d Deps }

func (b *builder) Build(ctx context.Context, o Options) ([]byte, error) {
	if b.d.Store == nil {
		return nil, errors.New("report: no store")
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	gen := now().UTC()
	from, to := o.From.UTC(), o.To.UTC()
	if to.IsZero() {
		to = gen
	}
	if from.IsZero() {
		from = to.Add(-DefaultRange)
	}
	if !to.After(from) {
		return nil, errors.New("report: the range ends before it starts")
	}
	var prof model.Profile
	if b.d.Profile != nil {
		if p := b.d.Profile(); p != nil {
			prof = *p
		}
	}
	r := &run{b: b, o: o, from: from, to: to, now: gen, prof: &prof}
	if err := r.collect(ctx); err != nil {
		return nil, err
	}
	return r.render()
}

// run holds one report's data while it is built.
type run struct {
	b        *builder
	o        Options
	from, to time.Time
	now      time.Time
	prof     *model.Profile

	incidents []incident
	groups    []*group
	measured  float64 // share of the range with any measurement, 0..1 (-1: unknown)
	hasData   bool
	traces    []*traceView
	notes     []model.Annotation
	ispASN    uint32
	ispOwner  string
	baselines bool // any baseline known
}

type incident struct {
	model.Incident
	start, end time.Time // clipped to the range; end: now for ongoing ones
	ongoing    bool
}

func (i incident) dur() time.Duration { return i.end.Sub(i.start) }

// group is one profile group's statistics over the range.
type group struct {
	g       model.Group
	series  []*seriesStat // profile order
	panel   *store.PanelResult
	charted bool
	// affected: some target of the group is named by an incident.
	affected bool
	n, lost  uint64
	// typical is the median of the targets' medians; ratio the median of
	// their ratios to normal (0: unknown).
	typical, ratio float64
	// worst hour-of-day window (UTC hour of its start) vs normal.
	winHour  int
	winRatio float64
}

type seriesStat struct {
	target  model.Target
	key     model.SeriesKey
	cols    *store.SeriesCols
	n, lost uint64
	lostBy  map[model.Reason]uint64
	mean    float64 // NaN: no successful samples
	median  float64
	p95     float64
	base    *model.Baseline
	ratio   float64 // median / baseline median, 0 unknown
	chart   bool
	color   string
}

func (s *seriesStat) loss() float64 {
	if t := s.n + s.lost; t > 0 {
		return float64(s.lost) / float64(t)
	}
	return 0
}

func (g *group) loss() float64 {
	if t := g.n + g.lost; t > 0 {
		return float64(g.lost) / float64(t)
	}
	return 0
}

// primaryKind is the kind charted for a target: ICMP on the network path,
// else TCP, HTTPS or ICMP, whichever it has first.
func primaryKind(t model.Target) model.ProbeKind {
	ks := t.ProbeKinds()
	if t.Group == profile.PathGroup && slices.Contains(ks, model.KindICMP) {
		return model.KindICMP
	}
	for _, k := range []model.ProbeKind{model.KindTCP, model.KindHTTPS, model.KindICMP} {
		if slices.Contains(ks, k) {
			return k
		}
	}
	return ks[0]
}

func (r *run) collect(ctx context.Context) error {
	d := r.b.d
	if d.Incidents != nil {
		list, err := d.Incidents(ctx, r.from, r.to)
		if err != nil {
			return fmt.Errorf("report: incidents: %w", err)
		}
		for _, in := range list {
			if in.Kind == model.VerdictOK || in.Kind == model.VerdictWarmingUp {
				continue
			}
			x := incident{Incident: in, start: in.Start.UTC(), end: in.End.UTC()}
			if in.End.IsZero() {
				x.ongoing, x.end = true, r.now
			}
			x.start = maxTime(x.start, r.from)
			x.end = minTime(x.end, r.to)
			if x.end.Before(x.start) {
				continue
			}
			if r.o.Redact {
				x.Summary = redact.Text(x.Summary)
			}
			r.incidents = append(r.incidents, x)
		}
		sort.SliceStable(r.incidents, func(i, j int) bool { return r.incidents[i].Start.Before(r.incidents[j].Start) })
	}
	affected := map[string]bool{}
	for _, in := range r.incidents {
		for _, t := range in.Targets {
			affected[t] = true
		}
	}

	if err := r.collectGroups(ctx, affected); err != nil {
		return err
	}
	if d.Annotations != nil {
		notes, err := d.Annotations.Annotations(ctx, r.from, r.to, r.o.Redact)
		if err != nil {
			return fmt.Errorf("report: annotations: %w", err)
		}
		for _, a := range notes {
			if r.o.Redact && !a.Public {
				continue // belt and braces
			}
			if r.o.Redact {
				a.Text = redact.Text(a.Text)
			}
			r.notes = append(r.notes, a)
		}
	}
	if d.ISP != nil {
		r.ispASN, r.ispOwner = d.ISP()
	}
	if d.Trace != nil {
		if err := r.collectTraces(ctx, affected); err != nil {
			return err
		}
	}
	return nil
}

func (r *run) collectGroups(ctx context.Context, affected map[string]bool) error {
	d := r.b.d
	groups := slices.Clone(r.prof.Groups)
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Order < groups[j].Order })
	var cover []bool
	var coverStart time.Time
	var coverStep time.Duration
	for _, mg := range groups {
		g := &group{g: mg}
		var keys []model.SeriesKey
		for _, t := range r.prof.Targets {
			if t.Group != mg.ID {
				continue
			}
			k := model.SeriesKey{Target: t.Name, Kind: primaryKind(t)}
			g.series = append(g.series, &seriesStat{target: t, key: k, mean: math.NaN()})
			keys = append(keys, k)
			if affected[t.Name] {
				g.affected = true
			}
		}
		if len(keys) == 0 {
			continue
		}
		res, err := d.Store.Panel(ctx, store.PanelQuery{Keys: keys, From: r.from, To: r.to, MaxPoints: ChartPoints})
		if err != nil {
			return fmt.Errorf("report: panel %s: %w", mg.ID, err)
		}
		g.panel = res
		byKey := map[model.SeriesKey]*store.SeriesCols{}
		for i := range res.Series {
			byKey[res.Series[i].Key] = &res.Series[i]
		}
		if cover == nil {
			coverStart, coverStep = res.Start, res.Step
			cover = make([]bool, int(r.to.Sub(res.Start)/res.Step)+1)
		}
		for _, s := range g.series {
			s.cols = byKey[s.key]
			if s.cols == nil {
				continue
			}
			r.seriesStats(s, res)
			g.n += s.n
			g.lost += s.lost
			if res.Start.Equal(coverStart) && res.Step == coverStep {
				for i := range s.cols.N {
					if i < len(cover) && s.cols.N[i]+s.cols.Lost[i] > 0 {
						cover[i] = true
					}
				}
			}
			if d.Baselines != nil {
				if bl, ok := d.Baselines(s.key, r.from); ok && bl.MedianMs > 0 {
					s.base = &bl
					r.baselines = true
					if !math.IsNaN(s.median) {
						s.ratio = s.median / bl.MedianMs
					}
				}
			}
		}
		if g.n+g.lost > 0 {
			r.hasData = true
		}
		var meds, ratios []float64
		for _, s := range g.series {
			if !math.IsNaN(s.median) {
				meds = append(meds, s.median)
			}
			if s.ratio > 0 {
				ratios = append(ratios, s.ratio)
			}
		}
		g.typical, g.ratio = median(meds), median(ratios)
		r.hourWindow(g)
		worstTarget := 0.0
		for _, s := range g.series {
			worstTarget = max(worstTarget, s.loss())
		}
		g.charted = g.n+g.lost > 0 && (mg.ID == profile.PathGroup || g.affected || g.loss() >= 0.01 || worstTarget >= 0.02)
		if g.charted {
			pickChartSeries(g, affected)
		}
		r.groups = append(r.groups, g)
	}
	// Share of the range with any measurement.
	r.measured = -1
	if cover != nil {
		total, got := 0, 0
		for i := range cover {
			bs := coverStart.Add(time.Duration(i) * coverStep)
			if bs.Add(coverStep).Before(r.from) || bs.After(r.to) {
				continue
			}
			total++
			if cover[i] {
				got++
			}
		}
		if total > 0 {
			r.measured = float64(got) / float64(total)
		}
	}
	return nil
}

// inRange reports whether bucket i of res overlaps the report range.
func (r *run) inRange(res *store.PanelResult, i int) bool {
	bs := res.Start.Add(time.Duration(i) * res.Step)
	return !bs.Add(res.Step).Before(r.from) && !bs.After(r.to)
}

type wv struct {
	v float64
	w float64
}

// seriesStats fills counts, mean, median and p95 (of bucket means, weighted
// by samples) from a series' buckets.
func (r *run) seriesStats(s *seriesStat, res *store.PanelResult) {
	c := s.cols
	var sum float64
	var vals []wv
	for i := range c.N {
		if !r.inRange(res, i) {
			continue
		}
		s.n += uint64(c.N[i])
		s.lost += uint64(c.Lost[i])
		if c.N[i] > 0 && !isNaN32(c.Mean[i]) {
			sum += float64(c.Mean[i]) * float64(c.N[i])
			vals = append(vals, wv{float64(c.Mean[i]), float64(c.N[i])})
		}
	}
	for reason, cnt := range c.LostBy {
		for i, v := range cnt {
			if v > 0 && r.inRange(res, i) {
				if s.lostBy == nil {
					s.lostBy = map[model.Reason]uint64{}
				}
				s.lostBy[reason] += uint64(v)
			}
		}
	}
	s.median, s.p95 = math.NaN(), math.NaN()
	if len(vals) == 0 {
		return
	}
	s.mean = sum / float64(sumW(vals))
	sort.Slice(vals, func(i, j int) bool { return vals[i].v < vals[j].v })
	s.median = wQuantile(vals, 0.5)
	s.p95 = wQuantile(vals, 0.95)
}

func sumW(vs []wv) float64 {
	t := 0.0
	for _, v := range vs {
		t += v.w
	}
	return t
}

// wQuantile returns the weighted q-quantile of sorted vs.
func wQuantile(vs []wv, q float64) float64 {
	total := sumW(vs)
	acc := 0.0
	for _, v := range vs {
		acc += v.w
		if acc >= q*total {
			return v.v
		}
	}
	return vs[len(vs)-1].v
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	xs = slices.Clone(xs)
	slices.Sort(xs)
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}

// hourWindow finds the 3-hour window of the UTC day with the highest
// latency relative to normal (ranges of a day or more with baselines).
func (r *run) hourWindow(g *group) {
	if r.to.Sub(r.from) < 24*time.Hour || g.panel == nil || g.panel.Step > time.Hour {
		return
	}
	best, bestH := 0.0, 0
	for h := 0; h < 24; h++ {
		var ratios []float64
		for _, s := range g.series {
			if s.base == nil || s.cols == nil {
				continue
			}
			sum, n := 0.0, 0.0
			for i := range s.cols.N {
				if s.cols.N[i] == 0 || isNaN32(s.cols.Mean[i]) || !r.inRange(g.panel, i) {
					continue
				}
				bh := g.panel.Start.Add(time.Duration(i) * g.panel.Step).UTC().Hour()
				if (bh-h+24)%24 < 3 {
					sum += float64(s.cols.Mean[i]) * float64(s.cols.N[i])
					n += float64(s.cols.N[i])
				}
			}
			if n > 0 {
				ratios = append(ratios, sum/n/s.base.MedianMs)
			}
		}
		if m := median(ratios); m > best {
			best, bestH = m, h
		}
	}
	g.winRatio, g.winHour = best, bestH
}

// pickChartSeries marks at most MaxChartSeries targets to draw: those named
// by incidents first, then the lossiest, then the slowest vs normal; the
// colours follow profile order.
func pickChartSeries(g *group, affected map[string]bool) {
	var cand []*seriesStat
	for _, s := range g.series {
		if s.n+s.lost > 0 {
			cand = append(cand, s)
		}
	}
	sort.SliceStable(cand, func(i, j int) bool {
		a, b := cand[i], cand[j]
		if affected[a.target.Name] != affected[b.target.Name] {
			return affected[a.target.Name]
		}
		if a.loss() != b.loss() {
			return a.loss() > b.loss()
		}
		return a.ratio > b.ratio
	})
	for i, s := range cand {
		if i < MaxChartSeries {
			s.chart = true
		}
	}
	c := 0
	for _, s := range g.series {
		if s.chart {
			s.color = seriesColors[c%len(seriesColors)]
			c++
		}
	}
}

// ---------- traces ----------

type traceView struct {
	Target  string
	Why     string
	Hops    []hopRow
	Changes []changeRow
	More    int // changes not listed
}

type hopRow struct {
	Hop       int
	Addr      string
	Note      string // "private", "masked", "no reply"
	RDNS      string
	Owner     string
	Loss      string
	Mean, P95 string
	Verdict   string
	Class     string // row class: real, ratelimit, ""
}

type changeRow struct {
	At        string
	FirstDiff int
	From, To  string
}

var anycastHosts = map[string]bool{"1.1.1.1": true, "8.8.8.8": true, "9.9.9.9": true, "1.0.0.1": true, "8.8.4.4": true, "149.112.112.112": true}

func (r *run) collectTraces(ctx context.Context, affected map[string]bool) error {
	tr := r.b.d.Trace
	var traced []model.Target
	for _, t := range r.prof.Targets {
		if t.Trace {
			traced = append(traced, t)
		}
	}
	if len(traced) == 0 {
		return nil
	}
	// The path's anycast target: a traced anycast layer target, else one
	// probing an anycast resolver, else the first traced target.
	anchor := -1
	for i, t := range traced {
		if t.Layer == model.LayerAnycast {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		for i, t := range traced {
			if anycastHosts[t.Host] {
				anchor = i
				break
			}
		}
	}
	if anchor < 0 {
		anchor = 0
	}
	type pick struct {
		t   model.Target
		why string
	}
	picks := []pick{{traced[anchor], "Reference path to a nearby anycast service"}}
	for i, t := range traced {
		if i != anchor && affected[t.Name] && len(picks) < MaxTraceTargets {
			picks = append(picks, pick{t, "Named by an incident in this range"})
		}
	}
	for i, t := range picks {
		if affected[t.t.Name] && i == 0 {
			picks[0].why = "Reference path to a nearby anycast service; also named by an incident"
		}
	}

	allChanges, err := tr.RouteChanges(ctx, r.from, r.to)
	if err != nil {
		return fmt.Errorf("report: route changes: %w", err)
	}
	for _, p := range picks {
		hops, err := tr.Hops(ctx, p.t.Name, r.from, r.to)
		if err != nil {
			return fmt.Errorf("report: hops %s: %w", p.t.Name, err)
		}
		var changes []model.RouteChange
		for _, c := range allChanges {
			if c.Target == p.t.Name {
				changes = append(changes, c)
			}
		}
		sort.SliceStable(changes, func(i, j int) bool { return changes[i].At.After(changes[j].At) })
		if len(hops) == 0 && len(changes) == 0 {
			continue
		}
		r.traces = append(r.traces, r.traceView(ctx, p.t.Name, p.why, hops, changes))
	}
	return nil
}

func hopAddr(h store.HopStat) netip.Addr {
	if h.IP.IsValid() {
		return h.IP.Unmap()
	}
	return h.MostCommonIP.Unmap()
}

func (r *run) traceView(ctx context.Context, target, why string, hops []store.HopStat, changes []model.RouteChange) *traceView {
	tr := r.b.d.Trace
	cl := redact.NewClassifier()
	info := map[netip.Addr]model.HopInfo{}
	sort.SliceStable(hops, func(i, j int) bool { return hops[i].Hop < hops[j].Hop })
	maxHop := 0
	for _, h := range hops {
		maxHop = max(maxHop, h.Hop)
		if a := hopAddr(h); a.IsValid() && h.Info != nil {
			info[a] = *h.Info
		}
	}
	seq := make([][]netip.Addr, min(maxHop, 64))
	for _, h := range hops {
		if h.Hop >= 1 && h.Hop <= len(seq) {
			seq[h.Hop-1] = append(seq[h.Hop-1], hopAddr(h))
		}
	}
	cl.Seq(seq)
	if route, ok := tr.Route(ctx, target); ok {
		cl.Route(route.Hops)
	}
	for _, c := range changes {
		cl.Route(c.From)
		cl.Route(c.To)
	}
	lookup := func(a netip.Addr) model.HopInfo {
		if !a.IsValid() {
			return model.HopInfo{}
		}
		a = a.Unmap()
		if i, ok := info[a]; ok {
			return i
		}
		i, _, _ := tr.HopInfo(ctx, a)
		info[a] = i
		return i
	}
	// The ISP: the first public hop with a known owner.
	if r.ispASN == 0 {
		for _, h := range hops {
			a := hopAddr(h)
			if a.IsValid() && !redact.IsPrivate(a) {
				if i := lookup(a); i.ASN != 0 {
					r.ispASN, r.ispOwner = i.ASN, i.Owner
				}
				break
			}
		}
	}
	v := &traceView{Target: target, Why: why}
	for _, h := range hops {
		a := hopAddr(h)
		rh := cl.Hop(a, lookup(a), r.o.Redact)
		row := hopRow{Hop: h.Hop, RDNS: rh.RDNS, Addr: rh.IP}
		switch {
		case rh.NoReply:
			row.Note = "no reply"
		case rh.Private && r.o.Redact:
			row.Note = "private (hidden)"
		case rh.Private:
			row.Note = "private"
		case rh.Masked:
			row.Note = "masked"
		}
		if rh.ASN != 0 {
			row.Owner = fmt.Sprintf("AS%d %s", rh.ASN, rh.Owner)
		}
		switch {
		case h.N == 0 && h.Lost > 0:
			row.Loss, row.Verdict = "–", "does not answer probes (normal for some routers)"
		case h.N+h.Lost == 0:
			row.Loss = "–"
		default:
			row.Loss = fmtPct(h.Loss)
			row.Mean, row.P95 = fmtMs(h.MeanMs), fmtMs(h.P95Ms)
		}
		switch {
		case h.N == 0:
			// A silent router: neither real loss nor rate limiting.
		case h.LossContinues:
			row.Verdict, row.Class = "real loss: continues to the destination", "real"
		case h.RateLimited:
			row.Verdict, row.Class = "not real: router limits its replies", "ratelimit"
		}
		v.Hops = append(v.Hops, row)
	}
	path := func(hs []netip.Addr, first int) string {
		if first < 1 {
			first = 1
		}
		var out []string
		for i := first - 1; i < len(hs) && len(out) < 6; i++ {
			rh := cl.Hop(hs[i], lookup(hs[i]), r.o.Redact)
			s := rh.IP
			switch {
			case rh.NoReply:
				s = "*"
			case rh.Private && s == "":
				s = "private"
			}
			out = append(out, s)
		}
		if len(hs)-(first-1) > 6 {
			out = append(out, "…")
		}
		if len(out) == 0 {
			return "(ends)"
		}
		return joinPath(out)
	}
	for i, c := range changes {
		if i >= MaxRouteChanges {
			v.More = len(changes) - i
			break
		}
		v.Changes = append(v.Changes, changeRow{At: fmtTableTime(c.At), FirstDiff: c.FirstDiff,
			From: path(c.From, c.FirstDiff), To: path(c.To, c.FirstDiff)})
	}
	return v
}

func joinPath(p []string) string {
	s := ""
	for i, x := range p {
		if i > 0 {
			s += " › "
		}
		s += x
	}
	return s
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func isNaN32(f float32) bool { return f != f }
