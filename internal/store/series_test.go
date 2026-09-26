package store

import (
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TestSeriesFirstIsFirstRealSample: a series that starts mid-hour is stored
// in a block starting at the hour, padded with not-measured slots. Series
// must report its first real sample (a measurement or a loss), not the
// block start, whether the hour is in memory, flushed, closed or reopened;
// Last is the last real sample. The Fake agrees.
func TestSeriesFirstIsFirstRealSample(t *testing.T) {
	a := model.SeriesKey{Target: "a", Kind: model.KindHTTPS}
	b := model.SeriesKey{Target: "b", Kind: model.KindICMP}
	iv := func(k model.SeriesKey) time.Duration {
		if k == b {
			return 5 * time.Second
		}
		return 15 * time.Second
	}
	now := t0
	dir := t.TempDir()
	o := Options{Interval: iv, Now: func() time.Time { return now }}
	s := openT(t, dir, o)
	defer func() { s.Close() }()
	f := NewFake()
	obs := func(x model.Sample) {
		s.Observe(x)
		f.Observe(x)
		if x.Slot.After(now) {
			now = x.Slot
		}
	}
	// a: a not-measured slot, then its first measurement at 20m15s; b: a
	// loss first, at 41m35s (both mid-hour).
	aFirst := t0.Add(20*time.Minute + 15*time.Second)
	bFirst := t0.Add(41*time.Minute + 35*time.Second)
	obs(model.Sample{Key: a, Slot: aFirst.Add(-15 * time.Second), Lost: true, Reason: model.ReasonGap})
	for i := range 10 {
		obs(model.Sample{Key: a, Slot: aFirst.Add(time.Duration(i) * 15 * time.Second), RTT: 12 * time.Millisecond})
	}
	obs(model.Sample{Key: b, Slot: bFirst, Lost: true, Reason: model.ReasonTimeout})
	obs(model.Sample{Key: b, Slot: bFirst.Add(5 * time.Second), RTT: 3 * time.Millisecond})
	aLast, bLast := aFirst.Add(9*15*time.Second), bFirst.Add(5*time.Second)

	check := func(stage string) {
		t.Helper()
		want := map[model.SeriesKey][2]time.Time{a: {aFirst, aLast}, b: {bFirst, bLast}}
		for name, r := range map[string]Reader{"sqlite": s, "fake": f} {
			infos, err := r.Series(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(infos) != len(want) {
				t.Fatalf("%s %s: %d series, want %d", stage, name, len(infos), len(want))
			}
			for _, in := range infos {
				w := want[in.Key]
				if !in.First.Equal(w[0]) || !in.Last.Equal(w[1]) {
					t.Errorf("%s %s %v: first %v last %v, want %v %v", stage, name, in.Key,
						in.First.Format(time.TimeOnly), in.Last.Format(time.TimeOnly), w[0].Format(time.TimeOnly), w[1].Format(time.TimeOnly))
				}
			}
		}
		// Raw's first non-gap point agrees.
		for k, w := range want {
			var first time.Time
			_ = s.Raw(ctx, []model.SeriesKey{k}, t0, t0.Add(3*time.Hour), func(p RawPoint) error {
				if first.IsZero() && !(p.Lost && p.Reason == model.ReasonGap) {
					first = p.TS
				}
				return nil
			})
			if !first.Equal(w[0]) {
				t.Errorf("%s raw %v: first %v, want %v", stage, k, first, w[0])
			}
		}
	}
	check("in memory")
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	check("flushed")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openT(t, dir, o)
	check("reopened")

	// The next hour: a's later samples extend Last only.
	next := t0.Add(time.Hour + 30*time.Second)
	obs(model.Sample{Key: a, Slot: next, RTT: 10 * time.Millisecond})
	aLast = next
	check("next hour in memory")
	now = t0.Add(2*time.Hour + 5*time.Minute) // closes every hour
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	check("closed")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openT(t, dir, o)
	check("closed reopened")
}
