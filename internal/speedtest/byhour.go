package speedtest

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Speed by hour of day: congestion shows as a dip at the same hours every
// day (typically evenings). The API, Home and the report share these
// numbers and the wording.

// MinPerHour is the tests an hour needs for its medians.
const MinPerHour = 3

// HourStat is one local hour of day over many days. Bits per second; nil
// with fewer than MinPerHour tests.
type HourStat struct {
	Hour       int      `json:"hour"`
	N          int      `json:"n"`
	DownMedian *float64 `json:"down_median_bps"`
	DownP10    *float64 `json:"down_p10_bps"`
	UpMedian   *float64 `json:"up_median_bps"`
	UpP10      *float64 `json:"up_p10_bps"`
}

// ByHour groups the ok tests by local hour of day (tz: offset east of UTC).
func ByHour(tests []model.SpeedTest, tz time.Duration) []HourStat {
	var down, up [24][]float64
	var n [24]int
	for _, t := range tests {
		if t.Outcome != model.SpeedOK {
			continue
		}
		h := t.Start.Add(tz).UTC().Hour()
		n[h]++
		if t.DownBps != nil {
			down[h] = append(down[h], *t.DownBps)
		}
		if t.UpBps != nil {
			up[h] = append(up[h], *t.UpBps)
		}
	}
	out := make([]HourStat, 24)
	q := func(v []float64, p float64) *float64 {
		if len(v) < MinPerHour {
			return nil
		}
		x := math.Round(callq.Quantile(slices.Clone(v), p))
		return &x
	}
	for h := range 24 {
		out[h] = HourStat{Hour: h, N: n[h], DownMedian: q(down[h], 0.5), DownP10: q(down[h], 0.1), UpMedian: q(up[h], 0.5), UpP10: q(up[h], 0.1)}
	}
	return out
}

// Pattern is a clear time-of-day dip in download speed.
type Pattern struct {
	Text    string  `json:"text"`
	Level   string  `json:"level"`
	DipFrom int     `json:"-"`     // local hour
	DipTo   int     `json:"-"`     // exclusive; wraps past midnight
	Drop    float64 `json:"drop"`  // rounded to 0.1
	Hours   []int   `json:"hours"` // the local hours of the dip, in order
}

// Pattern rule: enough hours, and a run of ≥ 2 consecutive hours whose
// download median is at most dipRatio of the median of the other hours.
const (
	minHours    = 12
	minRun      = 2
	dipRatio    = 0.75
	PatternSpan = 30 * 24 * time.Hour // the span the pattern is judged over
)

// FindPattern returns the clearest dip of hs (ByHour), or nil.
func FindPattern(hs []HourStat) *Pattern {
	med := make([]float64, 24)
	var valid []float64
	for i, h := range hs {
		med[i] = math.NaN()
		if h.DownMedian != nil {
			med[i] = *h.DownMedian
			valid = append(valid, med[i])
		}
	}
	if len(valid) < minHours {
		return nil
	}
	ref := callq.Median(slices.Clone(valid))
	low := func(h int) bool { return !math.IsNaN(med[h]) && med[h] <= dipRatio*ref }
	// Runs of low hours, circular. Start each run after a hour that is not low.
	var best *Pattern
	for start := range 24 {
		if !low(start) || low((start+23)%24) {
			continue
		}
		var run []int
		for h := start; low(h) && len(run) < 24; h = (h + 1) % 24 {
			run = append(run, h)
		}
		if len(run) < minRun || len(run) > 12 {
			continue
		}
		var in, out []float64
		for h := range 24 {
			if math.IsNaN(med[h]) {
				continue
			}
			if slices.Contains(run, h) {
				in = append(in, med[h])
			} else {
				out = append(out, med[h])
			}
		}
		others := callq.Median(out)
		drop := 1 - callq.Median(in)/others
		if others <= 0 || callq.Median(in) > dipRatio*others {
			continue
		}
		if best == nil || len(run) > len(best.Hours) || (len(run) == len(best.Hours) && drop > best.Drop) {
			best = &Pattern{Hours: run, DipFrom: run[0], DipTo: (run[len(run)-1] + 1) % 24, Drop: drop, Level: "info"}
		}
	}
	if best == nil {
		return nil
	}
	best.Drop = math.Round(best.Drop*10) / 10
	mid := (best.DipFrom + len(best.Hours)/2) % 24
	part, ref2 := "Mornings", "usual"
	switch {
	case mid >= 18:
		part, ref2 = "Evenings", "daytime"
	case mid < 6:
		part, ref2 = "Nights", "daytime"
	case mid >= 12:
		part = "Afternoons"
	}
	best.Text = fmt.Sprintf("%s are slower: about %d%% below your %s speed.", part, int(best.Drop*100+0.5), ref2)
	return best
}

// HourCell is one hour of day of a region or server: its download median
// relative to that row's best (p90), for heatmaps.
type HourCell struct {
	Hour       int      `json:"hour"`
	N          int      `json:"n"`
	DownMedian *float64 `json:"down_median_bps"`
	UpMedian   *float64 `json:"up_median_bps"`
	DownNorm   *float64 `json:"down_norm"` // DownMedian / Stats.BestDown, rounded to 0.01
}

// Stats summarize the tests of one region or server.
type Stats struct {
	N           int        `json:"n"` // ok tests
	Busy        int        `json:"busy"`
	Unreachable int        `json:"unreachable"`
	Failed      int        `json:"failed"`
	Fallback    int        `json:"fallback"` // ok tests on a fallback server (regions)
	LatestAt    *time.Time `json:"-"`
	DownMedian  *float64   `json:"down_median_bps"`
	DownP10     *float64   `json:"down_p10_bps"`
	UpMedian    *float64   `json:"up_median_bps"`
	UpP10       *float64   `json:"up_p10_bps"`
	// BestDown is the p90 of the downloads: the row's "best" for DownNorm.
	BestDown *float64   `json:"best_down_bps"`
	Dip      *Pattern   `json:"dip"`
	Hours    []HourCell `json:"hours"`
}

func quant(v []float64, p float64) *float64 {
	if len(v) == 0 {
		return nil
	}
	x := math.Round(callq.Quantile(v, p))
	return &x
}

// Summarize computes Stats over tests (all of one region or server), hours
// local to tz.
func Summarize(tests []model.SpeedTest, tz time.Duration) Stats {
	var st Stats
	var down, up []float64
	for _, t := range tests {
		if st.LatestAt == nil || t.Start.After(*st.LatestAt) {
			at := t.Start
			st.LatestAt = &at
		}
		switch t.Outcome {
		case model.SpeedOK:
			st.N++
			if t.Fallback {
				st.Fallback++
			}
		case model.SpeedBusy:
			st.Busy++
		case model.SpeedUnreachable:
			st.Unreachable++
		default:
			st.Failed++
		}
		if t.Outcome == model.SpeedOK && t.DownBps != nil {
			down = append(down, *t.DownBps)
		}
		if t.Outcome == model.SpeedOK && t.UpBps != nil {
			up = append(up, *t.UpBps)
		}
	}
	st.DownMedian, st.DownP10, st.BestDown = quant(down, 0.5), quant(down, 0.1), quant(down, 0.9)
	st.UpMedian, st.UpP10 = quant(up, 0.5), quant(up, 0.1)
	hs := ByHour(tests, tz)
	for _, h := range hs {
		c := HourCell{Hour: h.Hour, N: h.N, DownMedian: h.DownMedian, UpMedian: h.UpMedian}
		if h.DownMedian != nil && st.BestDown != nil && *st.BestDown > 0 {
			x := math.Round(*h.DownMedian / *st.BestDown * 100) / 100
			c.DownNorm = &x
		}
		st.Hours = append(st.Hours, c)
	}
	st.Dip = FindPattern(hs)
	return st
}

// GroupKey is a test's row: its region, else its server.
func GroupKey(t model.SpeedTest) string {
	if t.Region != "" {
		return t.Region
	}
	return t.Server
}

// PooledPattern is the time-of-day pattern of the user's own line: each ok
// test as a fraction of its region's (or server's) p90 download, pooled, so
// far servers that always read lower don't hide or fake a dip. Fallback
// tests are left out (another server, another baseline), and so are
// on-demand ones (Test now, sweeps).
func PooledPattern(tests []model.SpeedTest, tz time.Duration) *Pattern {
	tests = Scheduled(tests)
	by := map[string][]float64{}
	for _, t := range tests {
		if t.Outcome == model.SpeedOK && t.DownBps != nil && !t.Fallback {
			k := GroupKey(t)
			by[k] = append(by[k], *t.DownBps)
		}
	}
	best := map[string]float64{}
	for k, v := range by {
		best[k] = callq.Quantile(v, 0.9)
	}
	var norm []model.SpeedTest
	for _, t := range tests {
		if t.Outcome != model.SpeedOK || t.DownBps == nil || t.Fallback || best[GroupKey(t)] <= 0 {
			continue
		}
		r := *t.DownBps / best[GroupKey(t)] * 1e6 // ByHour rounds: keep resolution
		norm = append(norm, model.SpeedTest{Start: t.Start, Outcome: model.SpeedOK, DownBps: &r})
	}
	return FindPattern(ByHour(norm, tz))
}

// Scheduled keeps the schedule's tests: patterns and per-row hours leave
// on-demand tests (Test now, sweeps) out.
func Scheduled(tests []model.SpeedTest) []model.SpeedTest {
	var out []model.SpeedTest
	for _, t := range tests {
		if !t.OnDemand() {
			out = append(out, t)
		}
	}
	return out
}
