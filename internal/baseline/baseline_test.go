package baseline

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

var _ store.BaselineReader = (*fakeReader)(nil)

// t0 is 18:20 UTC.
var t0 = time.Date(2026, 9, 26, 18, 20, 0, 0, time.UTC)

var (
	kA = model.SeriesKey{Target: "A", Kind: model.KindICMP}
	kB = model.SeriesKey{Target: "B", Kind: model.KindHTTPS}
	kC = model.SeriesKey{Target: "C", Kind: model.KindTCP}
)

// fakeReader serves fixed statistics: overall[key], and hourly[h][key] for
// queries whose `at` falls in UTC hour h.
type fakeReader struct {
	mu      sync.Mutex
	overall map[model.SeriesKey]model.Baseline
	hourly  map[int]map[model.SeriesKey]model.Baseline
	err     error
	calls   []call
}

type call struct {
	keys      []model.SeriesKey
	at        time.Time
	window    time.Duration
	hourOfDay bool
}

func newFakeReader() *fakeReader {
	return &fakeReader{overall: map[model.SeriesKey]model.Baseline{}, hourly: map[int]map[model.SeriesKey]model.Baseline{}}
}

func (f *fakeReader) Baselines(_ context.Context, keys []model.SeriesKey, at time.Time, window time.Duration, hourOfDay bool) (map[model.SeriesKey]model.Baseline, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{slices.Clone(keys), at, window, hourOfDay})
	if f.err != nil {
		return nil, f.err
	}
	src := f.overall
	if hourOfDay {
		src = f.hourly[at.UTC().Hour()]
	}
	out := map[model.SeriesKey]model.Baseline{}
	for _, k := range keys {
		if b, ok := src[k]; ok {
			out[k] = b
		}
	}
	return out, nil
}

func (f *fakeReader) setHour(h int, k model.SeriesKey, b model.Baseline) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hourly[h] == nil {
		f.hourly[h] = map[model.SeriesKey]model.Baseline{}
	}
	f.hourly[h][k] = b
}

func (f *fakeReader) takeCalls() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

// overallB is a 7-day normal; hourB one over `days` past instances of the
// hour h (data from `days` days before t0's day at h to the current hour).
func overallB(med float64) model.Baseline {
	return model.Baseline{MedianMs: med, P95Ms: med * 1.5, Samples: 5000, From: t0.Add(-7 * 24 * time.Hour), To: t0.Truncate(time.Hour)}
}

func hourB(med float64, h, days int) model.Baseline {
	day := t0.Truncate(24 * time.Hour)
	from := day.Add(time.Duration(h)*time.Hour - time.Duration(days)*24*time.Hour)
	if h < t0.Hour() { // today's instance is already over: it is the last one
		from = from.Add(24 * time.Hour)
	}
	return model.Baseline{MedianMs: med, P95Ms: med * 1.5, Samples: int64(days) * 700, HourOfDay: true, From: from, To: t0.Truncate(time.Hour)}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type keyList struct {
	mu   sync.Mutex
	keys []model.SeriesKey
}

func (k *keyList) get() []model.SeriesKey {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.keys)
}
func (k *keyList) set(v ...model.SeriesKey) {
	k.mu.Lock()
	k.keys = v
	k.mu.Unlock()
}

func setup(keys ...model.SeriesKey) (*fakeReader, *clock, *keyList, *source) {
	r, c, kl := newFakeReader(), &clock{t: t0}, &keyList{keys: keys}
	return r, c, kl, newSource(r, kl.get, Options{Now: c.now})
}

var ctx = context.Background()

func TestSameHourHours(t *testing.T) {
	d := func(day, hour, min int) time.Time { return time.Date(2026, 9, day, hour, min, 0, 0, time.UTC) }
	cases := []struct {
		from, to time.Time
		h, want  int
	}{
		{d(20, 18, 0), d(26, 18, 0), 18, 6},  // 20..25 at 18:00
		{d(20, 18, 30), d(26, 18, 0), 18, 6}, // data started mid-hour: that hour counts
		{d(20, 19, 0), d(26, 18, 0), 18, 5},  // started after the 20th's 18:00
		{d(19, 18, 0), d(26, 18, 0), 18, 7},
		{d(20, 18, 0), d(26, 18, 0), 3, 6}, // 21..26 at 03:00
		{d(26, 17, 0), d(26, 18, 0), 17, 1},
		{d(26, 17, 0), d(26, 18, 0), 18, 0},
		{time.Time{}, d(26, 18, 0), 18, 0},
		{d(26, 18, 0), d(26, 18, 0), 18, 0},
	}
	for _, c := range cases {
		if got := sameHourHours(c.from, c.to, c.h); got != c.want {
			t.Errorf("sameHourHours(%v, %v, %d) = %d, want %d", c.from, c.to, c.h, got, c.want)
		}
	}
}

func TestSelection(t *testing.T) {
	r, _, _, s := setup(kA, kB, kC)
	r.overall[kA] = overallB(20)
	r.overall[kB] = overallB(40)
	r.overall[kC] = model.Baseline{MedianMs: 30, P95Ms: 40, Samples: 10, From: t0.Add(-time.Hour), To: t0.Truncate(time.Hour)} // too few samples
	r.setHour(18, kA, hourB(60, 18, 6))                                                                                        // evenings are slow, 6 evenings known: used
	r.setHour(18, kB, hourB(90, 18, 5))                                                                                        // only 5 evenings: not yet
	r.setHour(3, kA, hourB(10, 3, 6))                                                                                          // night, 6 nights
	r.setHour(4, kA, model.Baseline{MedianMs: 11, Samples: 5000})                                                              // no From/To: not used
	s.check(ctx)

	evening, night := t0, time.Date(2026, 9, 26, 3, 40, 0, 0, time.UTC)
	check := func(k model.SeriesKey, at time.Time, wantMed float64, wantHour, wantOK bool) {
		t.Helper()
		b, ok := s.Get(k, at)
		if ok != wantOK || (ok && (b.MedianMs != wantMed || b.HourOfDay != wantHour || b.Key != k)) {
			t.Errorf("Get(%v, %v) = %+v %v, want median %v hourOfDay %v ok %v", k, at, b, ok, wantMed, wantHour, wantOK)
		}
	}
	check(kA, evening, 60, true, true)
	check(kA, evening.In(time.FixedZone("x", -5*3600)), 60, true, true) // hour of day is UTC
	check(kA, night, 10, true, true)
	check(kA, night.Add(time.Hour), 20, false, true) // 04:xx: no From/To
	check(kA, night.Add(2*time.Hour), 20, false, true)
	check(kB, evening, 40, false, true) // falls back to overall
	check(kC, evening, 0, false, false) // not enough data at all
	check(model.SeriesKey{Target: "D"}, evening, 0, false, false)

	// The first refresh asked for the overall table and all 24 hours, each
	// hour at its latest past instance.
	calls := r.takeCalls()
	if len(calls) != 25 {
		t.Fatalf("%d calls, want 25", len(calls))
	}
	hours := map[int]bool{}
	for _, c := range calls {
		if c.window != DefaultWindow || len(c.keys) != 3 {
			t.Errorf("call %+v", c)
		}
		if c.hourOfDay {
			if c.at.After(t0) || t0.Sub(c.at) >= 24*time.Hour {
				t.Errorf("hour query at %v", c.at)
			}
			hours[c.at.UTC().Hour()] = true
		}
	}
	if len(hours) != 24 {
		t.Errorf("hours asked: %v", hours)
	}
}

func TestMinimumHoursOption(t *testing.T) {
	r, c, kl := newFakeReader(), &clock{t: t0}, &keyList{keys: []model.SeriesKey{kB}}
	s := newSource(r, kl.get, Options{Now: c.now, MinHours: 5})
	r.overall[kB] = overallB(40)
	r.setHour(18, kB, hourB(90, 18, 5))
	s.check(ctx)
	if b, _ := s.Get(kB, t0); !b.HourOfDay || b.MedianMs != 90 {
		t.Errorf("MinHours 5: %+v", b)
	}
}

func TestHourlyRefresh(t *testing.T) {
	r, c, kl, s := setup(kA, kB)
	r.overall[kA] = overallB(20)
	r.overall[kB] = overallB(40)
	r.setHour(3, kB, hourB(10, 3, 6))
	s.check(ctx)
	r.takeCalls()

	// Same hour: nothing to do.
	c.add(30 * time.Minute) // 18:50
	s.check(ctx)
	if n := len(r.takeCalls()); n != 0 {
		t.Fatalf("%d calls within the hour", n)
	}

	// Next hour: the overall table and the new hour's, for every key.
	r.overall[kA] = overallB(25)
	r.setHour(19, kA, hourB(50, 19, 6))
	kl.set(kA)              // kB left the profile
	c.add(15 * time.Minute) // 19:05
	s.check(ctx)
	calls := r.takeCalls()
	if len(calls) != 2 || calls[0].hourOfDay || !calls[1].hourOfDay || calls[1].at.UTC().Hour() != 19 {
		t.Fatalf("calls %+v", calls)
	}
	if b, _ := s.Get(kA, c.now()); b.MedianMs != 50 || !b.HourOfDay {
		t.Errorf("new hour: %+v", b)
	}
	if b, _ := s.Get(kA, t0); b.MedianMs != 25 {
		t.Errorf("overall refreshed: %+v", b)
	}
	if _, ok := s.Get(kB, t0); ok {
		t.Error("removed key still has an overall normal")
	}
	if _, ok := s.Get(kB, time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)); ok {
		t.Error("removed key still has an hour-of-day normal")
	}
}

func TestNewKeys(t *testing.T) {
	r, c, kl, s := setup(kA)
	r.overall[kA] = overallB(20)
	r.overall[kB] = overallB(40)
	r.setHour(18, kA, hourB(60, 18, 6))
	r.setHour(3, kB, hourB(10, 3, 6))
	s.check(ctx)
	r.takeCalls()
	if _, ok := s.Get(kB, t0); ok {
		t.Fatal("kB known before it was asked for")
	}

	kl.set(kA, kB)
	c.add(time.Minute)
	s.check(ctx)
	calls := r.takeCalls()
	if len(calls) != 25 {
		t.Fatalf("%d calls for a new key, want 25", len(calls))
	}
	for _, cl := range calls {
		if !slices.Equal(cl.keys, []model.SeriesKey{kB}) {
			t.Fatalf("new-key refresh asked for %v", cl.keys)
		}
	}
	if b, ok := s.Get(kB, t0); !ok || b.MedianMs != 40 {
		t.Errorf("kB overall: %+v %v", b, ok)
	}
	if b, _ := s.Get(kB, time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)); b.MedianMs != 10 {
		t.Errorf("kB 03:00: %+v", b)
	}
	if b, _ := s.Get(kA, t0); b.MedianMs != 60 { // untouched by the merge
		t.Errorf("kA: %+v", b)
	}

	// A key without history is asked once, not every Check.
	kl.set(kA, kB, kC)
	c.add(time.Minute)
	s.check(ctx)
	if n := len(r.takeCalls()); n != 25 {
		t.Fatalf("%d calls for kC", n)
	}
	c.add(time.Minute)
	s.check(ctx)
	if n := len(r.takeCalls()); n != 0 {
		t.Fatalf("kC asked again: %d calls", n)
	}
	// The hourly refresh does not re-ask every hour of known keys.
	c.add(time.Hour)
	s.check(ctx)
	if n := len(r.takeCalls()); n != 2 {
		t.Fatalf("hourly refresh: %d calls, want 2", n)
	}
}

func TestRefreshError(t *testing.T) {
	r, c, _, s := setup(kA)
	r.err = errors.New("db locked")
	s.check(ctx)
	if _, ok := s.Get(kA, t0); ok {
		t.Fatal("baseline after a failed refresh")
	}
	// Retried at the next check.
	r.err = nil
	r.overall[kA] = overallB(20)
	c.add(time.Minute)
	s.check(ctx)
	if b, ok := s.Get(kA, t0); !ok || b.MedianMs != 20 {
		t.Fatalf("after retry: %+v %v", b, ok)
	}
	// A failed hourly refresh keeps the old tables and retries.
	r.err = errors.New("db locked")
	c.add(time.Hour)
	s.check(ctx)
	if b, ok := s.Get(kA, t0); !ok || b.MedianMs != 20 {
		t.Fatalf("old table lost: %+v %v", b, ok)
	}
	r.err = nil
	r.overall[kA] = overallB(22)
	r.takeCalls()
	c.add(time.Minute)
	s.check(ctx)
	if b, _ := s.Get(kA, t0); b.MedianMs != 22 {
		t.Fatalf("hourly refresh not retried: %+v", b)
	}
}

func TestRun(t *testing.T) {
	r, c, kl := newFakeReader(), &clock{t: t0}, &keyList{keys: []model.SeriesKey{kA}}
	r.overall[kA] = overallB(20)
	r.overall[kB] = overallB(40)
	s := New(r, kl.get, Options{Now: c.now, Check: 5 * time.Millisecond})
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error)
	go func() { done <- s.Run(cctx) }()
	waitFor(t, func() bool { _, ok := s.Get(kA, t0); return ok })
	kl.set(kA, kB)
	waitFor(t, func() bool { _, ok := s.Get(kB, t0); return ok })
	r.mu.Lock()
	r.overall[kA] = overallB(33)
	r.mu.Unlock()
	c.add(time.Hour)
	waitFor(t, func() bool { b, _ := s.Get(kA, t0); return b.MedianMs == 33 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestConcurrentGet runs Get against refreshes (for -race).
func TestConcurrentGet(t *testing.T) {
	r, c, _, s := setup(kA)
	r.overall[kA] = overallB(20)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					s.Get(kA, t0)
				}
			}
		})
	}
	for range 20 {
		c.add(time.Hour)
		s.check(ctx)
	}
	close(stop)
	wg.Wait()
}

func TestRatioDescribe(t *testing.T) {
	b := model.Baseline{MedianMs: 42}
	if r := Ratio(98, b); r < 2.33 || r > 2.34 {
		t.Errorf("Ratio = %v", r)
	}
	if Ratio(98, model.Baseline{}) != 0 || Ratio(0, b) != 0 {
		t.Error("unknown ratio not 0")
	}
	cases := map[float64]string{
		0:         "",
		-1:        "",
		0.5:       "faster than your normal",
		1:         "about your normal",
		1.2:       "about your normal",
		1.25:      "1.3× your normal",
		98.0 / 42: "2.3× your normal",
		3:         "3× your normal",
		3.04:      "3× your normal",
		9.96:      "10× your normal",
		14.4:      "14× your normal",
	}
	for r, want := range cases {
		if got := Describe(r); got != want {
			t.Errorf("Describe(%v) = %q, want %q", r, got, want)
		}
	}
}
