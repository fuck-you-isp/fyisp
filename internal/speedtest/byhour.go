package speedtest

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Speed by hour of day. Congestion shows as a dip at the same hours every
// day (typically evenings). Patterns use each test's score: its result
// relative to its server's own baseline (Baseline), so far servers that
// always read lower neither hide nor fake a dip, and only scheduled tests:
// people press "Test now" when things feel slow, which would bias the
// hours. The API, Home, the finding and the report share these numbers.

// zone is loc, or UTC for nil.
func zone(loc *time.Location) *time.Location {
	if loc == nil {
		return time.UTC
	}
	return loc
}

// localHour is t's hour of day in loc (nil: UTC): with an IANA zone, right
// on both sides of a DST change.
func localHour(t time.Time, loc *time.Location) int { return t.In(zone(loc)).Hour() }

// MinPerHour is the tests an hour needs for its medians.
const MinPerHour = 3

// HourCell is one local hour of day of a row (a region or a server).
type HourCell struct {
	Hour      int `json:"hour"`
	N         int `json:"n"` // ok tests
	NFallback int `json:"n_fallback"`
	NFailed   int `json:"n_failed"` // busy, unreachable or failed
	NCapped   int `json:"n_capped"` // capped by the server's link
	// Medians with at least MinPerHour tests, else nil.
	DownMedian *float64 `json:"down_median_bps"`
	UpMedian   *float64 `json:"up_median_bps"`
	Score      *float64 `json:"score_median"`    // download score
	UpScore    *float64 `json:"up_score_median"` // upload score
	// Servers counts the servers that answered (ok tests).
	Servers map[string]int `json:"servers"`
}

// Stats summarize the scheduled tests of one row.
type Stats struct {
	N             int        `json:"n"` // ok tests
	Busy          int        `json:"busy"`
	Unreachable   int        `json:"unreachable"`
	Failed        int        `json:"failed"`
	Fallback      int        `json:"fallback"` // ok tests on a fallback server
	FallbackShare float64    `json:"fallback_share"`
	Capped        int        `json:"capped_by_server"`
	LatestAt      *time.Time `json:"-"`
	ServerNow     string     `json:"server_now"` // the server of the newest ok test
	DownMedian    *float64   `json:"down_median_bps"`
	DownP10       *float64   `json:"down_p10_bps"`
	UpMedian      *float64   `json:"up_median_bps"`
	UpP10         *float64   `json:"up_p10_bps"`
	// BestDown is the p90 of the downloads (the row's own "best").
	BestDown *float64 `json:"best_down_bps"`
	Score    *float64 `json:"score_median"` // median download score (nil while learning)
	// Dip and UpDip are the row's own time-of-day dips (scores).
	Dip   *Pattern   `json:"dip"`
	UpDip *Pattern   `json:"up_dip"`
	Hours []HourCell `json:"hours"`
	// LowerBounds counts the ok tests whose download was a lower bound
	// ("at least" values: left out of the speeds above), RampCappedShare
	// their share. TooFast: every server of the row is too fast to measure
	// from here (the coverage guard), so it's out of the patterns.
	LowerBounds     int     `json:"n_lower_bound,omitempty"`
	RampCappedShare float64 `json:"ramp_capped_share,omitempty"`
	TooFast         bool    `json:"too_fast,omitempty"`
}

func quant(v []float64, p float64) *float64 {
	if len(v) == 0 {
		return nil
	}
	x := math.Round(callq.Quantile(v, p))
	return &x
}

func medianOf(v []float64, digits float64) *float64 {
	if len(v) < MinPerHour {
		return nil
	}
	x := math.Round(callq.Median(v)*digits) / digits
	return &x
}

// Summarize computes Stats over a row's tests (only scheduled ones count),
// hours local to loc (nil: UTC).
//
// Scores are censored (Censored). Lower bounds are left out of the speeds'
// quantiles: they are "at least" values, never the row's slow end.
func Summarize(tests []model.SpeedTest, loc *time.Location) Stats {
	fast := tooFast(tests, loc)
	tests = Censored(tests, loc)
	servers := map[string]bool{}
	for i := range tests {
		if patternRow(&tests[i], false) {
			servers[tests[i].Server] = true
		}
	}
	var st Stats
	st.TooFast = len(servers) > 0
	for sv := range servers {
		st.TooFast = st.TooFast && fast[0][sv]
	}
	var down, up, score []float64
	var hd, hu, hs, hus [24][]float64
	cells := make([]HourCell, 24)
	for h := range cells {
		cells[h] = HourCell{Hour: h, Servers: map[string]int{}}
	}
	var newestOK time.Time
	for _, t := range tests {
		if t.OnDemand() || t.Outcome == model.SpeedCancelled {
			continue
		}
		if st.LatestAt == nil || t.Start.After(*st.LatestAt) {
			at := t.Start
			st.LatestAt = &at
		}
		c := &cells[localHour(t.Start, loc)]
		switch t.Outcome {
		case model.SpeedOK:
			st.N++
			c.N++
			c.Servers[t.Server]++
			if t.Fallback {
				st.Fallback++
				c.NFallback++
			}
			if t.CappedByServer {
				st.Capped++
				c.NCapped++
			}
			if !t.Start.Before(newestOK) {
				newestOK, st.ServerNow = t.Start, t.Server
			}
		case model.SpeedBusy:
			st.Busy++
			c.NFailed++
		case model.SpeedUnreachable:
			st.Unreachable++
			c.NFailed++
		default:
			st.Failed++
			c.NFailed++
		}
		if t.Outcome != model.SpeedOK && t.DownBps == nil {
			continue
		}
		// A partial result (the upload failed) keeps its download.
		h := c.Hour
		lbDown, lbUp := LowerBounds(&t)
		if lbDown && t.Outcome == model.SpeedOK {
			st.LowerBounds++
		}
		if t.DownBps != nil && !lbDown {
			down = append(down, *t.DownBps)
			hd[h] = append(hd[h], *t.DownBps)
		}
		if t.UpBps != nil && !lbUp {
			up = append(up, *t.UpBps)
			hu[h] = append(hu[h], *t.UpBps)
		}
		if t.ScoreDown != nil && !t.CappedByServer {
			score = append(score, *t.ScoreDown)
			hs[h] = append(hs[h], *t.ScoreDown)
		}
		if t.ScoreUp != nil && !t.CappedByServer {
			hus[h] = append(hus[h], *t.ScoreUp)
		}
	}
	if st.N > 0 {
		st.RampCappedShare = math.Round(float64(st.LowerBounds)/float64(st.N)*100) / 100
		st.FallbackShare = math.Round(float64(st.Fallback)/float64(st.N)*100) / 100
	}
	st.DownMedian, st.DownP10, st.BestDown = quant(down, 0.5), quant(down, 0.1), quant(down, 0.9)
	st.UpMedian, st.UpP10 = quant(up, 0.5), quant(up, 0.1)
	st.Score = medianOf(score, 1000)
	var dm, um [24]float64
	for h := range 24 {
		c := &cells[h]
		c.DownMedian, c.UpMedian = medianOf(hd[h], 1), medianOf(hu[h], 1)
		c.Score, c.UpScore = medianOf(hs[h], 1000), medianOf(hus[h], 1000)
		dm[h], um[h] = math.NaN(), math.NaN()
		if c.Score != nil {
			dm[h] = *c.Score
		}
		if c.UpScore != nil {
			um[h] = *c.UpScore
		}
	}
	st.Hours = cells
	st.Dip, st.UpDip = FindDip(dm, false), FindDip(um, true)
	return st
}

// HourMedians is the median score per local hour of the scheduled ok
// tests (NaN with fewer than MinPerHour), scores censored (Censored); up
// picks the upload score.
func HourMedians(tests []model.SpeedTest, loc *time.Location, up bool) [24]float64 {
	return hourMedians(Censored(tests, loc), loc, up)
}

// hourMedians is HourMedians of tests already censored.
func hourMedians(tests []model.SpeedTest, loc *time.Location, up bool) [24]float64 {
	var v [24][]float64
	for _, t := range tests {
		sc := t.ScoreDown
		if up {
			sc = t.ScoreUp
		}
		usable := t.Outcome == model.SpeedOK || (!up && t.DownBps != nil) // a partial result keeps its download
		if t.OnDemand() || !usable || sc == nil || t.CappedByServer {
			continue
		}
		h := localHour(t.Start, loc)
		v[h] = append(v[h], *sc)
	}
	var out [24]float64
	for h := range 24 {
		out[h] = math.NaN()
		if len(v[h]) >= MinPerHour {
			out[h] = callq.Median(v[h])
		}
	}
	return out
}

// Pattern is a clear time-of-day dip.
type Pattern struct {
	Text    string  `json:"text"`
	Level   string  `json:"level"`
	DipFrom int     `json:"-"`     // local hour
	DipTo   int     `json:"-"`     // exclusive; wraps past midnight
	Drop    float64 `json:"drop"`  // rounded to 0.1
	Hours   []int   `json:"hours"` // the local hours of the dip, in order
	Up      bool    `json:"up"`    // an upload dip
}

// Pattern rule: enough hours, and a run of ≥ 2 consecutive hours whose
// median is at most dipRatio of the median of the other hours.
const (
	minHours    = 12
	minRun      = 2
	dipRatio    = 0.75
	mildRatio   = 0.9                 // the finding's "a little slower": a 10-25% dip
	PatternSpan = 30 * 24 * time.Hour // the span patterns are judged over
)

// FindDip returns the clearest dip of 24 hourly medians (NaN: no data), or
// nil.
func FindDip(med [24]float64, up bool) *Pattern { return findDip(med, up, dipRatio) }

// findDip is FindDip with the low hours at most ratio of the others'
// median (mildRatio: the finding's "a little slower").
func findDip(med [24]float64, up bool, ratio float64) *Pattern {
	var valid []float64
	for _, v := range med {
		if !math.IsNaN(v) {
			valid = append(valid, v)
		}
	}
	if len(valid) < minHours {
		return nil
	}
	ref := callq.Median(valid)
	low := func(h int) bool { return !math.IsNaN(med[h]) && med[h] <= ratio*ref }
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
		if others <= 0 || callq.Median(in) > ratio*others {
			continue
		}
		drop := 1 - callq.Median(in)/others
		if best == nil || len(run) > len(best.Hours) || (len(run) == len(best.Hours) && drop > best.Drop) {
			best = &Pattern{Hours: run, DipFrom: run[0], DipTo: (run[len(run)-1] + 1) % 24, Drop: drop, Level: "info", Up: up}
		}
	}
	if best == nil {
		return nil
	}
	best.Drop = math.Round(best.Drop*10) / 10
	part, ref2 := partOfDay(best)
	what := "are slower"
	if up {
		what = "uploads are slower"
		part = lowerFirst(part)
		best.Text = fmt.Sprintf("In the %s, %s: about %d%% below your %s speed.", part, what, int(best.Drop*100+0.5), ref2)
		return best
	}
	best.Text = fmt.Sprintf("%s %s: about %d%% below your %s speed.", part, what, int(best.Drop*100+0.5), ref2)
	return best
}

// partOfDay names a dip by its middle hour.
func partOfDay(p *Pattern) (part, ref string) {
	mid := (p.DipFrom + len(p.Hours)/2) % 24
	switch {
	case mid >= 18:
		return "Evenings", "daytime"
	case mid < 6:
		return "Nights", "daytime"
	case mid >= 12:
		return "Afternoons", "usual"
	}
	return "Mornings", "usual"
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]+'a'-'A') + s[1:]
}

// GroupKey is a test's row: its region, else its server.
func GroupKey(t model.SpeedTest) string {
	if t.Region != "" {
		return t.Region
	}
	return t.Server
}

// PooledPattern is the time-of-day dip of every scheduled test's score
// pooled (fallback tests left out): your own line's pattern.
func PooledPattern(tests []model.SpeedTest, loc *time.Location) *Pattern {
	var keep []model.SpeedTest
	for _, t := range tests {
		if !t.Fallback {
			keep = append(keep, t)
		}
	}
	return FindDip(hourMedians(Censored(keep, loc), loc, false), false)
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
