package web

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// BaselineSource answers each series' normal. baseline.Source implements
// it: wire it as Deps.Baselines. Nil hides the "normal" badges and band:
// /api/baselines answers 404.
type BaselineSource interface {
	// Get returns the series' baseline for time `at` and whether one is
	// known yet.
	Get(key model.SeriesKey, at time.Time) (model.Baseline, bool)
}

// SlowTarget is one target slower than its normal (Deps.Slow). It mirrors
// the verdict engine's baseline comparison; wire an adapter that copies
// the fields.
type SlowTarget struct {
	Target   string          `json:"target"`
	Kind     model.ProbeKind `json:"-"`
	Ratio    float64         `json:"ratio"`     // now / normal median
	NowMs    float64         `json:"now_ms"`    // current RTT
	NormalMs float64         `json:"normal_ms"` // baseline median
}

// Baseline limits.
const (
	// BaselineNowWindow is the span before `at` whose mean RTT is compared
	// with the baseline median (ratio_now).
	BaselineNowWindow = 5 * time.Minute
	// MaxSlow caps the "slower than your normal" list.
	MaxSlow = 8
)

// slowJSON is one /api/verdict "slow" item.
type slowJSON struct {
	Target   string  `json:"target"`
	Kind     string  `json:"kind"`
	Ratio    float64 `json:"ratio"`
	NowMs    float64 `json:"now_ms"`
	NormalMs float64 `json:"normal_ms"`
}

// slow returns Deps.Slow's list, limited to profile targets and finite
// numbers (it is served on the public link).
func (s *server) slow() []slowJSON {
	if s.d.Slow == nil {
		return nil
	}
	known := targetFilter(s.d.Profile())
	var out []slowJSON
	for _, x := range s.d.Slow() {
		if len(out) >= MaxSlow {
			break
		}
		if len(known([]string{x.Target})) == 0 || !finite(x.Ratio, x.NowMs, x.NormalMs) || x.Ratio <= 0 {
			continue
		}
		out = append(out, slowJSON{Target: x.Target, Kind: x.Kind.String(), Ratio: round2(x.Ratio), NowMs: round2(x.NowMs), NormalMs: round2(x.NormalMs)})
	}
	return out
}

func finite(v ...float64) bool {
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
	}
	return true
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// baselineJSON is one /api/baselines series.
type baselineJSON struct {
	Target    string   `json:"target"`
	Kind      string   `json:"kind"`
	MedianMs  float64  `json:"median_ms"`
	P95Ms     float64  `json:"p95_ms"`
	Loss      float64  `json:"loss"`
	Samples   int64    `json:"samples"`
	HourOfDay bool     `json:"hour_of_day"`
	NowMs     *float64 `json:"now_ms,omitempty"`
	RatioNow  *float64 `json:"ratio_now,omitempty"`
}

type baselinesJSON struct {
	Group  string         `json:"group"`
	At     int64          `json:"at"`
	Series []baselineJSON `json:"series"`
}

// serveBaselines answers /api/baselines?group=&at=: each series' normal
// (median, p95, loss) as of `at` (default now), and the ratio of the mean
// RTT over the BaselineNowWindow before `at` to the median. Numbers and
// profile target names only; one store query.
func (s *server) serveBaselines(w http.ResponseWriter, r *http.Request) {
	if s.d.Baselines == nil {
		notFound(w, r)
		return
	}
	q := r.URL.Query()
	if err := checkParams(q, "group", "at"); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	group := q.Get("group")
	if group == "" || len(group) > 64 {
		writeErr(w, r, http.StatusBadRequest, "group is required")
		return
	}
	now := s.now()
	as := q.Get("at")
	if as == "" {
		as = "now"
	}
	at, ak, err := parseTime(as, now)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if at.After(now) {
		at, ak = now, "now"
	}
	if s.public && !strings.HasPrefix(ak, "now") {
		// Share cache entries between viewers: a minute is plenty.
		at = at.Truncate(time.Minute)
		ak = strconv.FormatInt(at.UnixMilli(), 10)
	}
	s.runCached(w, r, "base\x00"+group+"\x00"+ak, func(ctx context.Context) (*cached, error) {
		b, err := s.baselines(ctx, group, at)
		if err != nil {
			return nil, err
		}
		return jsonCached(b)
	})
}

func (s *server) baselines(ctx context.Context, group string, when time.Time) (*baselinesJSON, error) {
	_, series, _, err := resolveGroup(s.d.Profile(), group)
	if err != nil {
		return nil, errUnknownGroup
	}
	out := &baselinesJSON{Group: group, At: when.UnixMilli(), Series: []baselineJSON{}}
	var keys []model.SeriesKey
	for _, ps := range series {
		b, ok := s.d.Baselines.Get(ps.key, when)
		if !ok || !finite(b.MedianMs, b.P95Ms, b.Loss) || b.MedianMs <= 0 {
			continue
		}
		out.Series = append(out.Series, baselineJSON{Target: ps.key.Target, Kind: ps.key.Kind.String(),
			MedianMs: round2(b.MedianMs), P95Ms: round2(b.P95Ms), Loss: math.Min(1, math.Max(0, b.Loss)),
			Samples: b.Samples, HourOfDay: b.HourOfDay})
		keys = append(keys, ps.key)
	}
	if len(keys) == 0 {
		return out, nil
	}
	res, err := s.d.Store.Panel(ctx, store.PanelQuery{Keys: keys, From: when.Add(-BaselineNowWindow), To: when, MaxPoints: 1})
	if err != nil {
		return nil, err
	}
	cur := map[model.SeriesKey]float64{}
	for _, c := range res.Series {
		var sum float64
		var n int64
		for i, m := range c.Mean {
			k := int64(at(c.N, i))
			if k == 0 || math.IsNaN(float64(m)) {
				continue
			}
			sum += float64(m) * float64(k)
			n += k
		}
		if n > 0 {
			cur[c.Key] = sum / float64(n)
		}
	}
	for i := range out.Series {
		b := &out.Series[i]
		v, ok := cur[keys[i]]
		if !ok {
			continue
		}
		nv, rv := round2(v), round2(v/b.MedianMs)
		b.NowMs, b.RatioNow = &nv, &rv
	}
	return out, nil
}
