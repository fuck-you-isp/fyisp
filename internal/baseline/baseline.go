// Package baseline answers "is this worse than usual?": it caches each
// series' normal (store.BaselineReader) and compares current values to it.
//
// Two tables are kept per series: the overall normal of the past Window
// (default 7 days) and one normal per UTC hour of day. Get prefers the
// hour-of-day normal once it rests on at least MinHours (default 6) past
// instances of that hour, so congestion that happens every evening is
// "normal" for the evening, and falls back to the overall normal before
// that. Both need at least MinSamples samples.
//
// Refreshes (all in Run): at start, both tables for every key (the overall
// one plus one hour-of-day query per UTC hour); when the UTC hour changes,
// the overall table and the new hour's table for every key (keys that
// disappeared are dropped); and every Check, any key that keys() returns for
// the first time gets both tables at once. A failed refresh keeps the old
// tables and is retried at the next Check.
//
// Requirement on the reader: Baseline.From and To must span the data the
// statistics were computed from (From = the earliest sample used, not the
// window start), since the number of same-hour hours is counted from them.
// A zero From or To disqualifies an hour-of-day baseline.
package baseline

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// Source is what the verdict engine, the web API and reports use.
//
//	func New(r store.BaselineReader, keys func() []model.SeriesKey, o Options) Source
type Source interface {
	Run(ctx context.Context) error
	// Get returns the series' baseline for time `at` (time-of-day aware once
	// 7 days of history exist), and whether one is known yet.
	Get(key model.SeriesKey, at time.Time) (model.Baseline, bool)
}

// Defaults for Options.
const (
	DefaultWindow     = 7 * 24 * time.Hour
	DefaultCheck      = time.Minute
	DefaultMinSamples = 30 // a baseline from fewer samples is not used
	DefaultMinHours   = 6  // same-UTC-hour hours needed before the hour-of-day normal is used
)

// Options configures a Source. Zero values mean the defaults.
type Options struct {
	Now        func() time.Time
	Window     time.Duration // history the normals are computed over
	Check      time.Duration // how often Run looks for new keys and a new hour
	MinSamples int64
	MinHours   int
	Log        *slog.Logger
}

type tables struct {
	overall map[model.SeriesKey]model.Baseline
	hourly  [24]map[model.SeriesKey]model.Baseline // by UTC hour
}

type source struct {
	r    store.BaselineReader
	keys func() []model.SeriesKey
	o    Options
	t    atomic.Pointer[tables] // replaced whole, never mutated

	// Only touched by Run.
	asked map[model.SeriesKey]bool // keys whose tables were filled for every hour
	hour  time.Time                // UTC hour of the last successful hourly refresh
}

// New returns a Source reading from r. keys returns the series to keep
// normals for (the probe series of the current profile, not trace hops; it
// may change on reload). Get answers nothing until Run's first refresh.
func New(r store.BaselineReader, keys func() []model.SeriesKey, o Options) Source {
	return newSource(r, keys, o)
}

func newSource(r store.BaselineReader, keys func() []model.SeriesKey, o Options) *source {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	if o.Check <= 0 {
		o.Check = DefaultCheck
	}
	if o.MinSamples <= 0 {
		o.MinSamples = DefaultMinSamples
	}
	if o.MinHours <= 0 {
		o.MinHours = DefaultMinHours
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	s := &source{r: r, keys: keys, o: o, asked: map[model.SeriesKey]bool{}}
	s.t.Store(&tables{overall: map[model.SeriesKey]model.Baseline{}})
	return s
}

// Get returns the hour-of-day normal for at's UTC hour when it qualifies,
// else the overall normal. It is a lock-free map lookup; safe for
// concurrent use.
func (s *source) Get(key model.SeriesKey, at time.Time) (model.Baseline, bool) {
	t := s.t.Load()
	if b, ok := t.hourly[at.UTC().Hour()][key]; ok {
		return b, true
	}
	b, ok := t.overall[key]
	return b, ok
}

// Run refreshes at start, then checks every Check until ctx is done.
func (s *source) Run(ctx context.Context) error {
	s.check(ctx)
	tk := time.NewTicker(s.o.Check)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tk.C:
			s.check(ctx)
		}
	}
}

// check does whatever refresh is due at Now.
func (s *source) check(ctx context.Context) {
	now := s.o.Now()
	keys := s.keys()
	hour := now.UTC().Truncate(time.Hour)
	if s.hour.IsZero() {
		if s.refresh(ctx, now, keys, true, true) {
			s.hour = hour
		}
		return
	}
	if !hour.Equal(s.hour) && s.refresh(ctx, now, keys, false, true) {
		s.hour = hour
	}
	var fresh []model.SeriesKey
	for _, k := range keys {
		if !s.asked[k] {
			fresh = append(fresh, k)
		}
	}
	if len(fresh) > 0 {
		s.refresh(ctx, now, fresh, true, false)
	}
}

// refresh queries the reader for keys: the overall normal and the
// hour-of-day normal of the current hour, plus the 23 other hours when
// allHours. With replace the result is the new table for the current key
// set (keys not in it are dropped); otherwise it is merged in. It reports
// success; on failure the tables are unchanged.
func (s *source) refresh(ctx context.Context, now time.Time, keys []model.SeriesKey, allHours, replace bool) bool {
	overall, err := s.r.Baselines(ctx, keys, now, s.o.Window, false)
	if err != nil {
		s.o.Log.Warn("baseline: refresh", "err", err)
		return false
	}
	hours := 1
	if allHours {
		hours = 24
	}
	hourStart := now.UTC().Truncate(time.Hour)
	var hourly [24]map[model.SeriesKey]model.Baseline
	var got [24]bool
	for i := range hours {
		at := now
		if i > 0 {
			// The latest past instance of that hour: its window ends at
			// that hour's start, as the current hour's does.
			at = hourStart.Add(-time.Duration(i) * time.Hour)
		}
		h := at.UTC().Hour()
		m, err := s.r.Baselines(ctx, keys, at, s.o.Window, true)
		if err != nil {
			s.o.Log.Warn("baseline: refresh", "hour", h, "err", err)
			return false
		}
		hourly[h], got[h] = map[model.SeriesKey]model.Baseline{}, true
		for k, b := range m {
			if s.hourOK(b, h) {
				b.Key, b.HourOfDay = k, true
				hourly[h][k] = b
			}
		}
	}

	live := make(map[model.SeriesKey]bool, len(keys))
	for _, k := range keys {
		live[k] = true
	}
	old := s.t.Load()
	nt := &tables{overall: map[model.SeriesKey]model.Baseline{}}
	if !replace {
		for k, b := range old.overall {
			if !live[k] {
				nt.overall[k] = b
			}
		}
	}
	for k, b := range overall {
		if live[k] && s.enough(b) {
			b.Key, b.HourOfDay = k, false
			nt.overall[k] = b
		}
	}
	for h := range 24 {
		nt.hourly[h] = map[model.SeriesKey]model.Baseline{}
		for k, b := range old.hourly[h] {
			// replace: keep the other hours of keys still wanted;
			// merge: keep everything not being refreshed now.
			if (replace && live[k] && !got[h]) || (!replace && (!live[k] || !got[h])) {
				nt.hourly[h][k] = b
			}
		}
		for k, b := range hourly[h] {
			if live[k] {
				nt.hourly[h][k] = b
			}
		}
	}
	s.t.Store(nt)
	if replace {
		for k := range s.asked {
			if !live[k] {
				delete(s.asked, k)
			}
		}
	}
	if allHours {
		for _, k := range keys {
			s.asked[k] = true
		}
	}
	return true
}

func (s *source) enough(b model.Baseline) bool {
	return b.Samples >= s.o.MinSamples && b.MedianMs > 0
}

// hourOK: enough samples over at least MinHours past instances of hour h.
func (s *source) hourOK(b model.Baseline, h int) bool {
	return s.enough(b) && sameHourHours(b.From, b.To, h) >= s.o.MinHours
}

// sameHourHours counts the instances of UTC hour-of-day h that [from, to)
// touches.
func sameHourHours(from, to time.Time, h int) int {
	if from.IsZero() || to.IsZero() || !to.After(from) {
		return 0
	}
	first := from.UTC().Truncate(time.Hour)
	first = first.Add(time.Duration((h-first.Hour()+24)%24) * time.Hour)
	if !first.Before(to) {
		return 0
	}
	return int((to.Sub(first)-1)/(24*time.Hour)) + 1
}

// Ratio is how many times slower the current median RTT is than the
// normal median (0 when either is unknown).
func Ratio(currentMedianMs float64, b model.Baseline) float64 {
	if b.MedianMs <= 0 || currentMedianMs <= 0 || math.IsNaN(currentMedianMs) || math.IsInf(currentMedianMs, 0) {
		return 0
	}
	return currentMedianMs / b.MedianMs
}

// Describe puts a Ratio in words: "" when unknown, "faster than your
// normal" below 0.8, "about your normal" below 1.25, else "2.3× your
// normal" (see FormatRatio).
func Describe(ratio float64) string {
	switch {
	case ratio <= 0 || math.IsNaN(ratio) || math.IsInf(ratio, 0):
		return ""
	case ratio < 0.8:
		return "faster than your normal"
	case ratio < 1.25:
		return "about your normal"
	}
	return FormatRatio(ratio) + "× your normal"
}

// FormatRatio formats a ratio for people: one decimal below 10 without a
// trailing ".0" ("2.3", "3"), whole numbers from 10 ("14"). The verdict
// engine formats the ratios in its summaries the same way.
func FormatRatio(r float64) string {
	if r >= 9.95 {
		return strconv.Itoa(int(math.Round(r)))
	}
	return strconv.FormatFloat(math.Round(r*10)/10, 'f', -1, 64)
}
