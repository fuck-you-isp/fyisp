package store

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Fake is an in-memory Store for tests and UI development. It keeps every
// sample and implements the same bucketing semantics as the real store.
type Fake struct {
	mu   sync.RWMutex
	data map[model.SeriesKey][]model.Sample
}

func NewFake() *Fake { return &Fake{data: map[model.SeriesKey][]model.Sample{}} }

func (f *Fake) Observe(s model.Sample) {
	f.mu.Lock()
	f.data[s.Key] = append(f.data[s.Key], s)
	f.mu.Unlock()
}

func (f *Fake) Flush(context.Context) error { return nil }
func (f *Fake) Close() error                { return nil }

func (f *Fake) Prune(_ context.Context, before time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, ss := range f.data {
		i := sort.Search(len(ss), func(i int) bool { return !ss[i].Slot.Before(before) })
		f.data[k] = ss[i:]
	}
	return nil
}

// niceSteps are the allowed bucket sizes. Buckets are aligned to multiples
// of the step (UTC), so they line up with clock boundaries and do not shift
// between refreshes.
var niceSteps = []time.Duration{
	time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second,
	time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

// BucketStep returns the smallest nice step giving at most maxPoints buckets
// (default 1000). The hourly tier never goes below one hour.
func BucketStep(from, to time.Time, maxPoints int, tier string) time.Duration {
	if maxPoints <= 0 {
		maxPoints = 1000
	}
	want := to.Sub(from) / time.Duration(maxPoints)
	for _, s := range niceSteps {
		if tier == TierHourly && s < time.Hour {
			continue
		}
		if s >= want {
			return s
		}
	}
	return niceSteps[len(niceSteps)-1]
}

func (f *Fake) Panel(_ context.Context, q PanelQuery) (*PanelResult, error) {
	tier := TierRaw
	if q.To.Sub(q.From) > RawMaxRange {
		tier = TierHourly
	}
	step := BucketStep(q.From, q.To, q.MaxPoints, tier)
	start := q.From.Truncate(step)
	n := int(q.To.Sub(start)/step) + 1
	res := &PanelResult{Tier: tier, Start: start, Step: step}
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, k := range q.Keys {
		c := SeriesCols{Key: k, Mean: make([]float32, n), Min: make([]float32, n), Max: make([]float32, n),
			N: make([]uint32, n), Lost: make([]uint32, n)}
		sum := make([]float64, n)
		for i := range c.Min {
			c.Min[i], c.Max[i] = float32(math.Inf(1)), float32(math.Inf(-1))
		}
		for _, s := range f.data[k] {
			if s.Slot.Before(q.From) || s.Slot.After(q.To) {
				continue
			}
			b := int(s.Slot.Sub(start) / step)
			if s.Lost {
				if s.Reason == model.ReasonGap {
					continue
				}
				c.Lost[b]++
				if c.LostBy == nil {
					c.LostBy = map[model.Reason][]uint32{}
				}
				if c.LostBy[s.Reason] == nil {
					c.LostBy[s.Reason] = make([]uint32, n)
				}
				c.LostBy[s.Reason][b]++
				continue
			}
			ms := float32(float64(s.RTT) / 1e6)
			c.N[b]++
			sum[b] += float64(ms)
			c.Min[b] = min(c.Min[b], ms)
			c.Max[b] = max(c.Max[b], ms)
		}
		for i := range c.Mean {
			if c.N[i] == 0 {
				nan := float32(math.NaN())
				c.Mean[i], c.Min[i], c.Max[i] = nan, nan, nan
				continue
			}
			c.Mean[i] = float32(sum[i] / float64(c.N[i]))
		}
		res.Series = append(res.Series, c)
	}
	return res, nil
}

func (f *Fake) Raw(_ context.Context, keys []model.SeriesKey, from, to time.Time, fn func(RawPoint) error) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, k := range keys {
		for _, s := range f.data[k] {
			if s.Slot.Before(from) || s.Slot.After(to) {
				continue
			}
			p := RawPoint{Key: k, TS: s.Slot, Lost: s.Lost, Reason: s.Reason}
			if !s.Lost {
				p.RTTms = float64(s.RTT) / 1e6
			}
			if err := fn(p); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *Fake) Series(context.Context) ([]SeriesInfo, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var out []SeriesInfo
	for k, ss := range f.data {
		if len(ss) == 0 {
			continue
		}
		// Like SQLite: the first and last real samples (a measurement or a
		// loss), else the bounds when every sample is "not measured".
		lo, hi := 0, len(ss)-1
		isGap := func(x model.Sample) bool { return x.Lost && x.Reason == model.ReasonGap }
		for lo < hi && isGap(ss[lo]) {
			lo++
		}
		for hi > lo && isGap(ss[hi]) {
			hi--
		}
		if isGap(ss[lo]) {
			lo, hi = 0, len(ss)-1
		}
		out = append(out, SeriesInfo{Key: k, First: ss[lo].Slot, Last: ss[hi].Slot})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.Target != out[j].Key.Target {
			return out[i].Key.Target < out[j].Key.Target
		}
		return out[i].Key.Kind < out[j].Key.Kind
	})
	return out, nil
}

func (f *Fake) Stats(context.Context) (Stats, error) { return Stats{LastFlush: time.Now()}, nil }

var _ Store = (*Fake)(nil)
