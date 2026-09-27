package web

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
	"github.com/fuck-you-isp/fyisp/internal/verdict"
)

// Overview thresholds. Failing and very slow are the verdict engine's
// (verdict.LossBad, verdict.SpikeFactor); lossy and slow match the
// dashboard's REAL_LOSS and SLOW_RATIO (app.js).
const (
	overviewLossy   = 0.01 // app.js REAL_LOSS: loss below 1% is noise
	overviewSlow    = 1.5  // app.js SLOW_RATIO
	overviewMinSeen = 5    // fewer samples (received plus lost): not measured
	// Loss states need evidence: at least overviewLossMinLost lost samples
	// out of at least overviewLossMinSeen. One lost sample out of a handful
	// (a first probe during startup, a single timeout) is not "lossy".
	overviewLossMinLost = 2
	overviewLossMinSeen = 10
)

// OverviewMaxRange caps the /api/overview window (from is moved up to
// to-OverviewMaxRange). One overview query reads every target of a kind:
// with 1300 series (TestPerfOverview in internal/store) 30 days take about
// 0.3 s, 7 days 0.1 s and 24 hours 0.1 s; 90 days would take about 1 s,
// every refresh.
const OverviewMaxRange = 30 * 24 * time.Hour

// Overview row states, worst first.
const (
	stateFailing    = "failing"
	stateLossy      = "lossy"
	stateVerySlow   = "very_slow"
	stateSlow       = "slow"
	stateOK         = "ok"
	stateUnmeasured = "unmeasured"
)

// overviewRow is one target of the selected kind. NowMs is the mean RTT over
// the window (omitted without successful samples); Normal*, Ratio and
// HourOfDay are omitted without a baseline; Reason is the most frequent
// loss reason (only with losses).
type overviewRow struct {
	Target      string   `json:"target"`
	State       string   `json:"state"`
	NowMs       *float64 `json:"now_ms,omitempty"`
	N           int64    `json:"n"`
	Lost        int64    `json:"lost"`
	Loss        float64  `json:"loss"`
	Reason      string   `json:"reason,omitempty"`
	NormalMs    *float64 `json:"normal_ms,omitempty"`
	NormalP95Ms *float64 `json:"normal_p95_ms,omitempty"`
	Ratio       *float64 `json:"ratio,omitempty"`
	HourOfDay   bool     `json:"hour_of_day,omitempty"`
}

// overviewSummary counts rows. Slow counts slow and very slow rows;
// affected rows are slow, very slow, lossy or failing, and the provider and
// geo counts are over rows with a provider (geo: a known geo).
type overviewSummary struct {
	Targets           int `json:"targets"`
	Measured          int `json:"measured"`
	WithNormal        int `json:"with_normal"`
	Slow              int `json:"slow"`
	Lossy             int `json:"lossy"`
	Failing           int `json:"failing"`
	Providers         int `json:"providers"`
	ProvidersAffected int `json:"providers_affected"`
	Geos              int `json:"geos"`
	GeosAffected      int `json:"geos_affected"`
}

type overviewJSON struct {
	From int64  `json:"from"`
	To   int64  `json:"to"`
	Kind string `json:"kind"`
	// Capped reports that the window was shortened to OverviewMaxRange.
	Capped  bool            `json:"capped,omitempty"`
	Summary overviewSummary `json:"summary"`
	Rows    []overviewRow   `json:"rows"`
}

// overviewParams is a parsed /api/overview query.
type overviewParams struct {
	kind     model.ProbeKind
	from, to time.Time
	capped   bool
	key      string // cache key
}

// parseOverview parses /api/overview?from=&to=&kind= with /api/panel's time
// rules. kind is one of tcp, https, icmp (default tcp: every shipped profile
// probes its targets with TCP only). On the public link
// absolute bounds are rounded outward to the minute so that viewers share
// cache entries.
func parseOverview(q url.Values, now time.Time, public bool) (overviewParams, error) {
	var p overviewParams
	if err := checkParams(q, "from", "to", "kind"); err != nil {
		return p, err
	}
	switch ks := q.Get("kind"); ks {
	case "", "tcp":
		p.kind = model.KindTCP
	case "https":
		p.kind = model.KindHTTPS
	case "icmp":
		p.kind = model.KindICMP
	default:
		return p, badReq("bad kind %q: want tcp, https or icmp", ks)
	}
	fs, ts := q.Get("from"), q.Get("to")
	if fs == "" {
		fs = "now-30m"
	}
	if ts == "" {
		ts = "now"
	}
	var fk, tk string
	var err error
	if p.from, fk, err = parseTime(fs, now); err != nil {
		return p, err
	}
	if p.to, tk, err = parseTime(ts, now); err != nil {
		return p, err
	}
	if p.to.After(now) {
		p.to, tk = now, "now"
	}
	if public {
		if !strings.HasPrefix(tk, "now") {
			to := p.to.Truncate(time.Minute)
			if to.Before(p.to) {
				to = to.Add(time.Minute)
			}
			if to.After(now) {
				to, tk = now, "now"
			} else {
				tk = strconv.FormatInt(to.UnixMilli(), 10)
			}
			p.to = to
		}
		if !strings.HasPrefix(fk, "now") {
			p.from = p.from.Truncate(time.Minute)
			fk = strconv.FormatInt(p.from.UnixMilli(), 10)
		}
	}
	if !p.from.Before(p.to) {
		return p, badReq("from must be before to")
	}
	if p.to.Sub(p.from) > OverviewMaxRange {
		p.from, fk, p.capped = p.to.Add(-OverviewMaxRange), tk+"-cap", true
	}
	p.key = "ov\x00" + p.kind.String() + "\x00" + fk + "\x00" + tk
	return p, nil
}

// serveOverview answers /api/overview: every non-path target of one kind
// with its state over the window, and summary counts. Numbers and profile
// target names only; one store query.
func (s *server) serveOverview(w http.ResponseWriter, r *http.Request) {
	p, err := parseOverview(r.URL.Query(), s.now(), s.public)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	s.runCached(w, r, p.key, func(ctx context.Context) (*cached, error) {
		o, err := s.overview(ctx, p)
		if err != nil {
			return nil, err
		}
		return jsonCached(o)
	})
}

func (s *server) overview(ctx context.Context, p overviewParams) (*overviewJSON, error) {
	out := &overviewJSON{From: p.from.UnixMilli(), To: p.to.UnixMilli(), Kind: p.kind.String(), Capped: p.capped, Rows: []overviewRow{}}
	prof := s.d.Profile()
	if prof == nil {
		return out, nil
	}
	titles := map[string]string{}
	for _, g := range prof.Groups {
		titles[g.ID] = g.Title
	}
	var targets []model.Target
	var keys []model.SeriesKey
	for _, t := range prof.Targets {
		if isPath(t) || !hasKind(t, p.kind) {
			continue
		}
		targets = append(targets, t)
		keys = append(keys, model.SeriesKey{Target: t.Name, Kind: p.kind})
	}
	cols := map[model.SeriesKey]*store.SeriesCols{}
	if len(keys) > 0 {
		res, err := s.d.Store.Panel(ctx, store.PanelQuery{Keys: keys, From: p.from, To: p.to, MaxPoints: 1})
		if err != nil {
			return nil, err
		}
		for i := range res.Series {
			cols[res.Series[i].Key] = &res.Series[i]
		}
	}
	type seen struct{ all, affected map[string]bool }
	provs := seen{map[string]bool{}, map[string]bool{}}
	geos := seen{map[string]bool{}, map[string]bool{}}
	sm := &out.Summary
	for i, t := range targets {
		row := overviewRow{Target: t.Name}
		var sum float64
		var lostBy [model.MaxReason + 1]int64
		if c := cols[keys[i]]; c != nil {
			for j, m := range c.Mean {
				k := int64(at(c.N, j))
				row.Lost += int64(at(c.Lost, j))
				if k == 0 || math.IsNaN(float64(m)) {
					continue
				}
				sum += float64(m) * float64(k)
				row.N += k
			}
			for rs, v := range c.LostBy {
				if rs != model.ReasonGap && rs <= model.MaxReason {
					for _, x := range v {
						lostBy[rs] += int64(x)
					}
				}
			}
		}
		if row.N > 0 {
			v := round2(sum / float64(row.N))
			row.NowMs = &v
		}
		if row.N+row.Lost > 0 {
			row.Loss = float64(row.Lost) / float64(row.N+row.Lost)
		}
		if row.Lost > 0 {
			row.Reason = dominantReason(lostBy[:])
		}
		if s.d.Baselines != nil {
			if b, ok := s.d.Baselines.Get(keys[i], p.to); ok && finite(b.MedianMs, b.P95Ms) && b.MedianMs > 0 {
				med, p95 := round2(b.MedianMs), round2(b.P95Ms)
				row.NormalMs, row.NormalP95Ms, row.HourOfDay = &med, &p95, b.HourOfDay
				if row.NowMs != nil {
					ratio := round2(sum / float64(row.N) / b.MedianMs)
					row.Ratio = &ratio
				}
				sm.WithNormal++
			}
		}
		row.State = rowState(row)
		prov, _ := providerOf(t, titles)
		provs.all[prov] = true
		if t.Geo != "" {
			geos.all[t.Geo] = true
		}
		sm.Targets++
		switch row.State {
		case stateUnmeasured:
		case stateOK:
			sm.Measured++
		default:
			sm.Measured++
			switch row.State {
			case stateFailing:
				sm.Failing++
			case stateLossy:
				sm.Lossy++
			default:
				sm.Slow++
			}
			provs.affected[prov] = true
			if t.Geo != "" {
				geos.affected[t.Geo] = true
			}
		}
		out.Rows = append(out.Rows, row)
	}
	sm.Providers, sm.ProvidersAffected = len(provs.all), len(provs.affected)
	sm.Geos, sm.GeosAffected = len(geos.all), len(geos.affected)
	return out, nil
}

// rowState classifies a row: too few samples, then loss (with enough
// evidence; a row that is losing without it yet is not measured, never
// ok), then the ratio to the normal.
func rowState(r overviewRow) string {
	judged := r.Lost >= overviewLossMinLost && r.N+r.Lost >= overviewLossMinSeen
	switch {
	case r.N+r.Lost < overviewMinSeen:
		return stateUnmeasured
	case r.Lost >= overviewLossMinLost && r.N+r.Lost < overviewLossMinSeen:
		return stateUnmeasured // losing, but too early to say how much
	case judged && r.Loss >= verdict.LossBad:
		return stateFailing
	case judged && r.Loss >= overviewLossy:
		return stateLossy
	case r.Ratio != nil && *r.Ratio >= verdict.SpikeFactor:
		return stateVerySlow
	case r.Ratio != nil && *r.Ratio >= overviewSlow:
		return stateSlow
	}
	return stateOK
}

// dominantReason returns the slug with the most losses (counts indexed by
// model.Reason); ties go to the first in reasonSlugs order.
func dominantReason(counts []int64) string {
	by := map[string]int64{}
	for rs, v := range counts {
		if v == 0 || model.Reason(rs) == model.ReasonGap {
			continue
		}
		slug := "other"
		for _, x := range reasonSlugs {
			if x.r == model.Reason(rs) {
				slug = x.slug
			}
		}
		by[slug] += v
	}
	best, bestN := "", int64(0)
	for _, x := range reasonSlugs {
		if by[x.slug] > bestN {
			best, bestN = x.slug, by[x.slug]
		}
	}
	return best
}

func hasKind(t model.Target, k model.ProbeKind) bool {
	for _, x := range t.ProbeKinds() {
		if x == k {
			return true
		}
	}
	return false
}
