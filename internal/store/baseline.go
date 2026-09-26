package store

import (
	"context"
	"slices"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Baselines (BaselineReader) are computed from the hourly summaries
// (summary_1h, plus closed hours still in memory), never from raw samples,
// so a 7-day baseline of every series costs one SQL query over 8 day rows
// per series.
//
// Per series, over the hours whose start is in [From, To) where
// To = the start of the hour of `at` and From = To - window:
//
//   - MedianMs: the n-weighted median of the hourly medians. Each hour is
//     one value (its median RTT) weighted by its number of successful
//     samples; the result is the value where the cumulative weight reaches
//     half of all samples. Hours written before fyisp v0.3 (summary day
//     format v1) have no median; their mean stands in for it.
//   - P95Ms: the same over the hourly p95s (as the Investigate view).
//   - Loss: lost / (successful + lost) samples; not-measured slots are
//     neither.
//   - Samples: the number of successful samples.
//   - From, To: the span of the data actually used: the start of the
//     earliest and the end of the latest hour that contributed (not the
//     window bounds; the baseline cache counts same-hour days from them).
//
// An hour counts as data when it has at least one successful or lost
// sample. A series needs MinBaselineHours such hours (MinSameHourHours with
// hourOfDay, which keeps only hours with the UTC hour of day of `at`), or it
// is left out of the result.

// DefaultBaselineWindow is the window used when Baselines gets window <= 0.
const DefaultBaselineWindow = 7 * 24 * time.Hour

// Minimum hours of data for a baseline.
const (
	MinBaselineHours = 1
	MinSameHourHours = 6
)

// baselineRange returns [from, to) of the baseline of `at`.
func baselineRange(at time.Time, window time.Duration) (from, to time.Time) {
	if window <= 0 {
		window = DefaultBaselineWindow
	}
	to = at.UTC().Truncate(time.Hour)
	return to.Add(-window), to
}

// weightedValue is one hour's value weighted by its sample count.
type weightedValue struct {
	v float64
	n int64
}

// weightedQuantile returns the smallest value whose cumulative weight
// reaches pct% of total (vs is reordered).
func weightedQuantile(vs []weightedValue, total int64, pct int64) float64 {
	if len(vs) == 0 {
		return 0
	}
	slices.SortFunc(vs, func(x, y weightedValue) int { return cmpF(x.v, y.v) })
	need := max(1, (total*pct+99)/100)
	var c int64
	var v float64
	for _, x := range vs {
		c += x.n
		v = x.v
		if c >= need {
			break
		}
	}
	return v
}

type baselineAcc struct {
	hours       int
	first, last int64 // unix hours of the earliest and latest contributing hour
	n, lost     int64
	p50s, p95   []weightedValue
}

func (a *baselineAcc) add(hour int64, u *summary) {
	if u.n+u.lost == 0 {
		return
	}
	if a.hours == 0 || hour < a.first {
		a.first = hour
	}
	if a.hours == 0 || hour > a.last {
		a.last = hour
	}
	a.hours++
	a.lost += u.lost
	if u.n == 0 {
		return
	}
	a.n += u.n
	a.p50s = append(a.p50s, weightedValue{unitMs(u.median()), u.n})
	a.p95 = append(a.p95, weightedValue{unitMs(u.p95), u.n})
}

// baselineSet accumulates the hourly summaries of a Baselines call.
type baselineSet struct {
	from, to  time.Time
	hourOfDay bool
	hod       int64 // UTC hour of day of `at`
	accs      map[model.SeriesKey]*baselineAcc
}

func newBaselineSet(keys []model.SeriesKey, at time.Time, window time.Duration, hourOfDay bool) *baselineSet {
	from, to := baselineRange(at, window)
	b := &baselineSet{from: from, to: to, hourOfDay: hourOfDay, hod: int64(to.Hour()),
		accs: make(map[model.SeriesKey]*baselineAcc, len(keys))}
	for _, k := range keys {
		b.accs[k] = &baselineAcc{}
	}
	return b
}

// add takes the summary of the hour starting at unix hour `hour`.
func (b *baselineSet) add(k model.SeriesKey, hour int64, u *summary) {
	start := hour * hourMs
	if start < b.from.UnixMilli() || start >= b.to.UnixMilli() {
		return
	}
	if b.hourOfDay && ((hour%24)+24)%24 != b.hod {
		return
	}
	if a := b.accs[k]; a != nil {
		a.add(hour, u)
	}
}

func (b *baselineSet) result() map[model.SeriesKey]model.Baseline {
	need := MinBaselineHours
	if b.hourOfDay {
		need = MinSameHourHours
	}
	out := map[model.SeriesKey]model.Baseline{}
	for k, a := range b.accs {
		if a.hours < need {
			continue
		}
		bl := model.Baseline{Key: k, Samples: a.n, HourOfDay: b.hourOfDay,
			From: msTime(a.first * hourMs), To: msTime((a.last + 1) * hourMs),
			MedianMs: weightedQuantile(a.p50s, a.n, 50), P95Ms: weightedQuantile(a.p95, a.n, 95)}
		if t := a.n + a.lost; t > 0 {
			bl.Loss = float64(a.lost) / float64(t)
		}
		out[k] = bl
	}
	return out
}

// Baselines computes each key's baseline as of `at` (see the top of this
// file) with one SQL query over the day rows of the window.
func (s *SQLite) Baselines(ctx context.Context, keys []model.SeriesKey, at time.Time, window time.Duration, hourOfDay bool) (map[model.SeriesKey]model.Baseline, error) {
	b := newBaselineSet(keys, at, window, hourOfDay)
	if len(b.accs) == 0 {
		return map[model.SeriesKey]model.Baseline{}, nil
	}
	uniq := make([]model.SeriesKey, 0, len(b.accs))
	for k := range b.accs {
		uniq = append(uniq, k)
	}
	// hourlySummaries includes an hour when its start is in [from, to]:
	// stop one millisecond before the current hour.
	err := s.hourlySummaries(ctx, uniq, b.from, b.to.Add(-time.Millisecond), b.add)
	if err != nil {
		return nil, err
	}
	return b.result(), nil
}

var _ BaselineReader = (*SQLite)(nil)
