package store

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store/blob"
)

// TestSummaryDecode: a day row in the format version 2, with an unknown
// median (0) and with one; version 1 rows (fyisp v0.1/v0.2) are refused.
func TestSummaryDecode(t *testing.T) {
	// hour 5, n=3, lost=0, min 100, max-min 50, p95-min 40, sum 390, p50 unknown.
	unknown := []byte{summaryV2, 5, 3, 0, 100, 50, 40, 0x86, 0x03, 0}
	hs, err := decodeDay(nil, 10, unknown)
	if err != nil || len(hs) != 1 {
		t.Fatalf("%v %+v", err, hs)
	}
	u := hs[0].s
	if u.p50 != -1 || u.median() != 130 || u.min != 100 || u.max != 150 || u.p95 != 140 || u.sum != 390 {
		t.Fatalf("%+v median %d", u, u.median())
	}
	again, err := decodeDay(nil, 10, appendDay(nil, 10, hs))
	if err != nil || !slices.Equal(again, hs) {
		t.Fatalf("re-encoded: %v %+v", err, again)
	}
	if _, err := decodeDay(nil, 10, []byte{1, 5, 3, 0, 100, 50, 40, 0x86, 0x03}); err == nil {
		t.Fatal("a version 1 row was read")
	}
	// v2 with a median.
	hs[0].s.p50 = 120
	enc := appendDay(nil, 10, hs)
	if enc[0] != summaryV2 {
		t.Fatal("not v2")
	}
	again, err = decodeDay(nil, 10, enc)
	if err != nil || again[0].s.p50 != 120 || again[0].s.median() != 120 {
		t.Fatalf("v2: %v %+v", err, again)
	}
	// A median above max is corrupt.
	bad := slices.Clone(enc)
	bad[len(bad)-1] = 52 // p50-min+1 = 52 > max-min+1
	if _, err := decodeDay(nil, 10, bad); err == nil {
		t.Fatal("p50 > max accepted")
	}
	if s := summarize([]blob.Slot{blob.Val(5), blob.Val(1), blob.Val(9), blob.Val(3)}); s.p50 != 3 || s.p95 != 9 {
		t.Fatalf("summarize p50 %+v", s)
	}
	if s := summarize([]blob.Slot{blob.Lost(1)}); s.p50 != -1 {
		t.Fatalf("summarize all lost %+v", s)
	}
}

// Baseline math, over synthetic hourly summaries written straight into
// summary_1h.

// synthHour is a synthetic hour: offset from the hour of `at` (-1 is the last
// closed hour), n successful samples with median p50 and p95 (ms), lost.
type synthHour struct {
	off      int
	n, lost  int64
	p50, p95 float64
	noP50    bool // stored without a median (p50 unknown); mean = p50+1
}

func (h synthHour) summary() summary {
	u := summary{n: h.n, lost: h.lost, p50: -1}
	u.by[1] = h.lost
	if h.n > 0 {
		p50, p95 := int64(h.p50*100), int64(h.p95*100) // ms -> 10 µs
		u.min, u.max, u.p95 = p50/2, p95*2, p95
		mean := p50
		if h.noP50 {
			mean = p50 + 100
		} else {
			u.p50 = p50
		}
		u.sum = mean * h.n
	}
	return u
}

// putHours stores the synthetic hours of k relative to the hour of at.
func putHours(t *testing.T, s *SQLite, k model.SeriesKey, at time.Time, hours []synthHour) {
	t.Helper()
	// Register the series with one sample far outside every window.
	s.Observe(model.Sample{Key: k, Slot: at.Add(-300 * 24 * time.Hour), RTT: time.Millisecond})
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	id := s.ids[k]
	base := at.Unix() / 3600
	days := map[int64][]hourSummary{}
	for _, h := range hours {
		hour := base + int64(h.off)
		days[floorDiv(hour, 24)] = mergeDay(days[floorDiv(hour, 24)], hourSummary{hour, h.summary()})
	}
	for day, list := range days {
		var n int
		s.db.w.QueryRow(`SELECT count(*) FROM summary_1h WHERE day = ? AND series = ?`, day, id).Scan(&n)
		if n != 0 {
			t.Fatalf("day %d already stored", day)
		}
		data := appendDay(nil, day, list)
		if _, err := s.db.w.Exec(`INSERT INTO summary_1h(day, series, data) VALUES(?, ?, ?)`, day, id, data); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBaselines(t *testing.T) {
	at := time.Date(2026, 9, 20, 14, 30, 0, 0, time.UTC) // hour of day 14
	to := at.Truncate(time.Hour)
	k := model.SeriesKey{Target: "github", Kind: model.KindHTTPS}
	sameHour := func(days ...int) []synthHour {
		var out []synthHour
		for _, d := range days {
			out = append(out, synthHour{off: -24 * d, n: 100, p50: float64(10 * d), p95: float64(20 * d)})
			out = append(out, synthHour{off: -24*d + 1, n: 100, p50: 500, p95: 900}) // hour 15: other hours of day
		}
		return out
	}
	cases := []struct {
		name      string
		hours     []synthHour
		window    time.Duration
		hourOfDay bool
		want      *model.Baseline // nil: no baseline
		span      [2]int          // offsets of the first and last contributing hour
	}{
		{name: "no data"},
		{name: "weighted medians", hours: []synthHour{
			{off: -1, n: 100, p50: 10, p95: 20},
			{off: -2, n: 300, lost: 100, p50: 30, p95: 50},
			{off: -3, n: 100, p50: 20, p95: 40},
		}, want: &model.Baseline{MedianMs: 30, P95Ms: 50, Loss: 100.0 / 600, Samples: 500}, span: [2]int{-3, -1}},
		{name: "median not the mean of medians", hours: []synthHour{
			{off: -1, n: 1000, p50: 10, p95: 12},
			{off: -2, n: 10, p50: 900, p95: 1000},
		}, want: &model.Baseline{MedianMs: 10, P95Ms: 12, Samples: 1010}, span: [2]int{-2, -1}},
		{name: "current hour excluded", hours: []synthHour{
			{off: 0, n: 100, p50: 99, p95: 99},
		}},
		{name: "window bounds", window: 3 * time.Hour, hours: []synthHour{
			{off: 0, n: 1000, p50: 99, p95: 99},  // current hour: out
			{off: -1, n: 100, p50: 10, p95: 20},  // in
			{off: -3, n: 100, p50: 12, p95: 22},  // first hour of the window: in
			{off: -4, n: 1000, p50: 99, p95: 99}, // before the window: out
		}, want: &model.Baseline{MedianMs: 10, P95Ms: 22, Samples: 200}, span: [2]int{-3, -1}},
		{name: "default window is 7 days", hours: []synthHour{
			{off: -7 * 24, n: 100, p50: 10, p95: 20},
			{off: -7*24 - 1, n: 1000, p50: 99, p95: 99},
		}, want: &model.Baseline{MedianMs: 10, P95Ms: 20, Samples: 100}, span: [2]int{-168, -168}},
		{name: "loss only", hours: []synthHour{
			{off: -1, lost: 40},
			{off: -2}, // no samples at all (not measured): no data
		}, want: &model.Baseline{Loss: 1}, span: [2]int{-1, -1}},
		{name: "only unmeasured hours", hours: []synthHour{{off: -1}, {off: -2}}},
		{name: "old summaries use the mean", hours: []synthHour{
			{off: -1, n: 100, p50: 10, p95: 20, noP50: true},
			{off: -2, n: 101, p50: 30, p95: 40, noP50: true},
		}, want: &model.Baseline{MedianMs: 31, P95Ms: 40, Samples: 201}, span: [2]int{-2, -1}},
		{name: "same hour", hourOfDay: true, hours: sameHour(1, 2, 3, 4, 5, 6, 7),
			want: &model.Baseline{MedianMs: 40, P95Ms: 140, Samples: 700}, span: [2]int{-168, -24}},
		{name: "same hour, 6 days", hourOfDay: true, hours: sameHour(1, 2, 3, 4, 5, 6),
			want: &model.Baseline{MedianMs: 30, P95Ms: 120, Samples: 600}, span: [2]int{-144, -24}},
		{name: "same hour, 5 days: too few", hourOfDay: true, hours: sameHour(1, 2, 3, 4, 5)},
		{name: "same hour, day 8 outside the window", hourOfDay: true, hours: sameHour(1, 2, 3, 4, 5, 8)},
		{name: "all hours", hours: sameHour(1, 2, 3, 4, 5, 6, 7),
			want: &model.Baseline{MedianMs: 70, P95Ms: 900, Samples: 1400}, span: [2]int{-168, -23}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openT(t, t.TempDir(), Options{Now: func() time.Time { return at }})
			defer s.Close()
			putHours(t, s, k, at, c.hours)
			other := model.SeriesKey{Target: "never", Kind: model.KindTCP}
			got, err := s.Baselines(ctx, []model.SeriesKey{k, other, k}, at, c.window, c.hourOfDay)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := got[other]; ok || len(got) > 1 {
				t.Fatalf("unexpected keys %+v", got)
			}
			b, ok := got[k]
			if c.want == nil {
				if ok {
					t.Fatalf("baseline %+v, want none", b)
				}
				return
			}
			w := *c.want
			w.Key, w.HourOfDay = k, c.hourOfDay
			w.From, w.To = to.Add(time.Duration(c.span[0])*time.Hour), to.Add(time.Duration(c.span[1]+1)*time.Hour)
			if !ok || math.Abs(b.Loss-w.Loss) > 1e-12 {
				t.Fatalf("baseline %+v (%v), want %+v", b, ok, w)
			}
			b.Loss = w.Loss
			if b != w {
				t.Fatalf("baseline\n%+v, want\n%+v", b, w)
			}
		})
	}
}

// TestBaselinesFake: the fake and the real store agree on the same
// samples, including closed hours still in memory.
func TestBaselinesFake(t *testing.T) {
	at := t0.Add(9*24*time.Hour + 14*time.Hour + 20*time.Minute)
	now := at
	s := openT(t, t.TempDir(), Options{Now: func() time.Time { return now }})
	defer s.Close()
	f := NewFake()
	keys := []model.SeriesKey{{Target: "a", Kind: model.KindTCP}, {Target: "b", Kind: model.KindTCP}}
	i := 0
	for ts := at.Add(-9 * 24 * time.Hour); ts.Before(at); ts = ts.Add(15 * time.Second) {
		for j, k := range keys {
			x := model.Sample{Key: k, Slot: ts, RTT: time.Duration(5+(i*7+j*3)%40+ts.Hour()) * time.Millisecond}
			switch {
			case i%23 == 3:
				x = model.Sample{Key: k, Slot: ts, Lost: true, Reason: model.ReasonTimeout}
			case i%101 == 5:
				x = model.Sample{Key: k, Slot: ts, Lost: true, Reason: model.ReasonGap}
			}
			s.Observe(x)
			f.Observe(x)
		}
		i++
		if ts.Minute() == 0 && ts.Second() == 0 && ts.Before(at.Add(-3*time.Hour)) {
			now = ts
			if err := s.Flush(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	now = at
	for _, hod := range []bool{false, true} {
		for _, w := range []time.Duration{0, 5 * time.Hour, 36*time.Hour + 30*time.Minute} {
			got, err := s.Baselines(ctx, keys, at, w, hod)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := f.Baselines(ctx, keys, at, w, hod)
			if len(want) != 2 && (!hod || w == 0) {
				t.Fatalf("fake: %+v", want)
			}
			if !mapsEqual(got, want) {
				t.Fatalf("hourOfDay %v window %v:\n%+v\n%+v", hod, w, got, want)
			}
		}
	}
}
