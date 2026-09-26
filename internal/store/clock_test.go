package store

import (
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// measured counts the stored points of k in [from, to] that have a value.
func measured(t *testing.T, s *SQLite, k model.SeriesKey, from, to time.Time) (n int, rtts []float64) {
	t.Helper()
	for _, p := range rawAll(t, s, []model.SeriesKey{k}, from, to) {
		if !p.Lost {
			n++
			rtts = append(rtts, p.RTTms)
		}
	}
	return n, rtts
}

// TestFutureSampleDoesNotPoison: one sample stamped a year ahead (clock
// briefly wrong) must not make the series reject every later, correctly
// stamped sample, neither now nor after a restart.
func TestFutureSampleDoesNotPoison(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	o := Options{Now: func() time.Time { return now }}
	k := model.SeriesKey{Target: "A", Kind: model.KindHTTPS}
	s := openT(t, dir, o)
	s.Observe(model.Sample{Key: k, Slot: now.Add(365 * 24 * time.Hour), RTT: time.Millisecond})
	for i := 1; i <= 10; i++ {
		now = now.Add(15 * time.Second)
		s.Observe(model.Sample{Key: k, Slot: now, RTT: time.Millisecond})
	}
	if st, _ := s.Stats(ctx); st.Rejected != 0 {
		t.Fatalf("correct samples after a future one: rejected=%d, want 0", st.Rejected)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openT(t, dir, o)
	defer s.Close()
	for i := 11; i <= 20; i++ {
		now = now.Add(15 * time.Second)
		s.Observe(model.Sample{Key: k, Slot: now, RTT: time.Millisecond})
	}
	if st, _ := s.Stats(ctx); st.Rejected != 0 {
		t.Fatalf("after restart: rejected=%d, want 0", st.Rejected)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	h := now.Truncate(time.Hour)
	if n, _ := measured(t, s, k, h, h.Add(time.Hour)); n != 20 {
		t.Fatalf("stored %d samples in the current hour, want 20", n)
	}
}

// TestClockStepBackMerges: the clock jumps a year ahead, then back. The
// samples recorded before the jump in the current hour stay; the samples
// after the correction are added to the same hour; a sample for a slot that
// is already stored does not replace it.
func TestClockStepBackMerges(t *testing.T) {
	dir := t.TempDir()
	h := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	now := h
	o := Options{Now: func() time.Time { return now }, Interval: func(model.SeriesKey) time.Duration { return 15 * time.Second }}
	k := model.SeriesKey{Target: "A", Kind: model.KindTCP}
	s := openT(t, dir, o)
	defer func() { s.Close() }()
	for i := range 20 { // 10:00:00 .. 10:04:45, 1 ms
		now = h.Add(time.Duration(i) * 15 * time.Second)
		s.Observe(model.Sample{Key: k, Slot: now, RTT: time.Millisecond})
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// Clock a year ahead for a few samples (flushed), then back at 10:04:30.
	for i := range 4 {
		now = h.Add(365*24*time.Hour + time.Duration(i)*15*time.Second)
		s.Observe(model.Sample{Key: k, Slot: now, RTT: 9 * time.Millisecond})
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 18; i < 40; i++ { // 10:04:30 .. 10:09:45, 2 ms; the first two are stored already
		now = h.Add(time.Duration(i) * 15 * time.Second)
		s.Observe(model.Sample{Key: k, Slot: now, RTT: 2 * time.Millisecond})
	}
	if st, _ := s.Stats(ctx); st.Rejected != 0 {
		t.Fatalf("after the clock came back: rejected=%d", st.Rejected)
	}
	check := func(when string) {
		t.Helper()
		n, rtts := measured(t, s, k, h, h.Add(time.Hour))
		if n != 40 {
			t.Fatalf("%s: %d samples in the hour, want 40", when, n)
		}
		for i, v := range rtts {
			want := 1.0
			if i >= 20 {
				want = 2
			}
			if v != want {
				t.Fatalf("%s: sample %d = %vms, want %vms (stored samples must win)", when, i, v, want)
			}
		}
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	check("after flush")
	// The block now continues the stored hour.
	now = h.Add(40 * 15 * time.Second)
	s.Observe(model.Sample{Key: k, Slot: now, RTT: 2 * time.Millisecond})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openT(t, dir, o)
	n, _ := measured(t, s, k, h, h.Add(time.Hour))
	if n != 41 {
		t.Fatalf("after reopen: %d samples, want 41", n)
	}
	// The future hour is kept (the clock could be the wrong one now), but
	// ignored: recording continues.
	now = now.Add(15 * time.Second)
	s.Observe(model.Sample{Key: k, Slot: now, RTT: 2 * time.Millisecond})
	if st, _ := s.Stats(ctx); st.Rejected != 0 {
		t.Fatalf("after reopen: rejected=%d", st.Rejected)
	}
}

// TestSmallStepBackRejected: a duplicate or a step back while the clock is
// not ahead of the data is still rejected (no churn for NTP slews).
func TestSmallStepBackRejected(t *testing.T) {
	h := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	now := h.Add(10 * time.Minute)
	s := openT(t, t.TempDir(), Options{Now: func() time.Time { return now }})
	defer s.Close()
	k := model.SeriesKey{Target: "A", Kind: model.KindHTTPS}
	s.Observe(model.Sample{Key: k, Slot: now, RTT: time.Millisecond})
	s.Observe(model.Sample{Key: k, Slot: now, RTT: time.Millisecond})
	s.Observe(model.Sample{Key: k, Slot: now.Add(-30 * time.Second), RTT: time.Millisecond})
	if st, _ := s.Stats(ctx); st.Rejected != 2 {
		t.Fatalf("rejected=%d, want 2", st.Rejected)
	}
}

// TestIntervalChangeMidHour: the interval changed between runs (15s -> 10s)
// and fyisp restarted mid-hour. The restored hour must take the new samples
// at their exact times without dropping any, and keep the earlier ones.
func TestIntervalChangeMidHour(t *testing.T) {
	dir := t.TempDir()
	h := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	k := model.SeriesKey{Target: "A", Kind: model.KindHTTPS}
	s := openT(t, dir, Options{Now: func() time.Time { return h.Add(10 * time.Minute) },
		Interval: func(model.SeriesKey) time.Duration { return 15 * time.Second }})
	for i := range 40 {
		s.Observe(model.Sample{Key: k, Slot: h.Add(time.Duration(i) * 15 * time.Second), RTT: time.Millisecond})
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openT(t, dir, Options{Now: func() time.Time { return h.Add(30 * time.Minute) },
		Interval: func(model.SeriesKey) time.Duration { return 10 * time.Second }})
	defer s.Close()
	start := h.Add(20 * time.Minute)
	for i := range 60 {
		s.Observe(model.Sample{Key: k, Slot: start.Add(time.Duration(i) * 10 * time.Second), RTT: time.Millisecond})
	}
	if st, _ := s.Stats(ctx); st.Rejected != 0 {
		t.Fatalf("rejected=%d, want 0", st.Rejected)
	}
	check := func(when string) {
		t.Helper()
		pts := rawAll(t, s, []model.SeriesKey{k}, start, start.Add(10*time.Minute-time.Millisecond))
		var got []time.Time
		for _, p := range pts {
			if !p.Lost {
				got = append(got, p.TS)
			}
		}
		if len(got) != 60 {
			t.Fatalf("%s: %d measured points in the new window, want 60", when, len(got))
		}
		for i, ts := range got {
			if want := start.Add(time.Duration(i) * 10 * time.Second); !ts.Equal(want) {
				t.Fatalf("%s: point %d at %s, want %s", when, i, ts, want)
			}
		}
		if n, _ := measured(t, s, k, h, start.Add(-time.Millisecond)); n != 40 {
			t.Fatalf("%s: %d earlier points, want 40", when, n)
		}
	}
	check("in memory")
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	check("flushed")
}

// TestPruneClockAhead: a clock far ahead (before time sync) must not delete
// the history the retention would keep; the retention counts back from the
// newest stored hour.
func TestPruneClockAhead(t *testing.T) {
	now := t0
	s := openT(t, t.TempDir(), Options{Now: func() time.Time { return now }})
	defer s.Close()
	k := model.SeriesKey{Target: "A", Kind: model.KindICMP}
	for i := range 48 * 12 { // 48 hours, one sample every 5 minutes
		now = t0.Add(time.Duration(i) * 5 * time.Minute)
		s.Observe(model.Sample{Key: k, Slot: now, RTT: time.Millisecond})
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	st1, _ := s.Stats(ctx)
	now = t0.Add(400 * 24 * time.Hour) // clock a year ahead
	if err := s.Prune(ctx, now.Add(-90*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Stats(ctx); !st.Oldest.Equal(st1.Oldest) {
		t.Fatalf("clock ahead pruned history: oldest %v -> %v", st1.Oldest, st.Oldest)
	}
	// A 24h retention keeps the 24 newest hours.
	if err := s.Prune(ctx, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Stats(ctx); !st.Oldest.Equal(t0.Add(24 * time.Hour)) {
		t.Fatalf("24h retention with the clock ahead: oldest %v, want %v", st.Oldest, t0.Add(24*time.Hour))
	}
}

// TestPruneClockBehind: when every stored hour is after the current time
// (the clock is behind, e.g. an RTC reset), nothing is pruned.
func TestPruneClockBehind(t *testing.T) {
	now := t0
	dir := t.TempDir()
	s := openT(t, dir, Options{Now: func() time.Time { return now }})
	k := model.SeriesKey{Target: "A", Kind: model.KindICMP}
	for i := range 12 * 6 {
		now = t0.Add(time.Duration(i) * 5 * time.Minute)
		s.Observe(model.Sample{Key: k, Slot: now, RTT: time.Millisecond})
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	s = openT(t, dir, Options{Now: func() time.Time { return now }})
	defer s.Close()
	if err := s.Prune(ctx, t0.Add(3*time.Hour)); err != nil { // a bogus "before"
		t.Fatal(err)
	}
	if st, _ := s.Stats(ctx); !st.Oldest.Equal(t0) {
		t.Fatalf("clock behind pruned: oldest %v, want %v", st.Oldest, t0)
	}
}
