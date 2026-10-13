package main

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
	"github.com/fuck-you-isp/fyisp/internal/verdict"
)

// slowdown is the synthetic "evening slowdown": these targets run slower
// than their normal for the last 45 minutes of the backfill and live.
var slowdown = map[string]float64{"M365-Outlook": 2.4, "CDN-Akamai": 1.8, "Discord": 1.6}

const slowFor = 45 * time.Minute

// slowSink multiplies the RTT of the slowdown targets from `since` on.
func slowSink(inner model.Sink, since time.Time) model.Sink {
	return model.SinkFunc(func(s model.Sample) {
		if f, ok := slowdown[s.Key.Target]; ok && !s.Lost && !s.Slot.Before(since) && s.Key.Kind != model.KindUDPLoss {
			s.RTT = time.Duration(float64(s.RTT) * f)
		}
		inner.Observe(s)
	})
}

// ---------- baselines ----------

// fakeBaselines implements web.BaselineSource: each series' median, p95
// and loss over the backfilled history before the last hour (so the
// evening slowdown is not part of "normal").
type fakeBaselines struct {
	m map[model.SeriesKey]model.Baseline
}

func newFakeBaselines(ctx context.Context, st store.Reader, p *model.Profile, now time.Time, bf time.Duration) *fakeBaselines {
	fb := &fakeBaselines{m: map[model.SeriesKey]model.Baseline{}}
	from, to := now.Add(-bf), now.Add(-time.Hour)
	if !to.After(from) {
		return fb
	}
	for _, t := range p.Targets {
		for _, k := range t.ProbeKinds() {
			key := model.SeriesKey{Target: t.Name, Kind: k}
			var rtts []float64
			var lost int64
			_ = st.Raw(ctx, []model.SeriesKey{key}, from, to, func(rp store.RawPoint) error {
				switch {
				case rp.Lost && rp.Reason == model.ReasonGap:
				case rp.Lost:
					lost++
				default:
					rtts = append(rtts, rp.RTTms)
				}
				return nil
			})
			if len(rtts) < 20 {
				continue
			}
			sort.Float64s(rtts)
			n := int64(len(rtts))
			fb.m[key] = model.Baseline{Key: key, MedianMs: rtts[len(rtts)/2], P95Ms: rtts[len(rtts)*95/100],
				Loss: float64(lost) / float64(n+lost), Samples: n + lost, From: from, To: to}
		}
	}
	return fb
}

func (f *fakeBaselines) Get(key model.SeriesKey, _ time.Time) (model.Baseline, bool) {
	b, ok := f.m[key]
	return b, ok
}

// slow lists TCP series whose last 5 minutes are ≥1.5× their median.
func (f *fakeBaselines) slow(st store.Reader, p *model.Profile) func() []verdict.SlowTarget {
	var mu sync.Mutex
	var at time.Time
	var last []verdict.SlowTarget
	return func() []verdict.SlowTarget {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		if now.Sub(at) < 15*time.Second {
			return last
		}
		var keys []model.SeriesKey
		for _, t := range p.Targets {
			k := model.SeriesKey{Target: t.Name, Kind: model.KindTCP}
			if _, ok := f.m[k]; ok {
				keys = append(keys, k)
			}
		}
		res, err := st.Panel(context.Background(), store.PanelQuery{Keys: keys, From: now.Add(-5 * time.Minute), To: now, MaxPoints: 1})
		if err != nil {
			return last
		}
		var out []verdict.SlowTarget
		for _, c := range res.Series {
			var sum float64
			var n float64
			for i, m := range c.Mean {
				if i < len(c.N) && c.N[i] > 0 && !math.IsNaN(float64(m)) {
					sum += float64(m) * float64(c.N[i])
					n += float64(c.N[i])
				}
			}
			b := f.m[c.Key]
			if n == 0 || b.MedianMs <= 0 {
				continue
			}
			cur := sum / n
			if r := cur / b.MedianMs; r >= 1.5 {
				out = append(out, verdict.SlowTarget{Target: c.Key.Target, Kind: c.Key.Kind, Ratio: r, NowMs: cur, NormalMs: b.MedianMs})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Ratio > out[j].Ratio })
		at, last = now, out
		return out
	}
}
