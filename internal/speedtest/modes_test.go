package speedtest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TestSchedulerQueue: Test now and sweeps run through the queue with the
// scheduler's servers (a region's rotated chain, every catalog server once)
// and triggers; a sweep's tests are on demand, never in patterns.
func TestSchedulerQueue(t *testing.T) {
	h := newHarness(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	q := h.s.Queue()
	if _, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "Nope", Who: WhoOwner}); !errors.Is(err, ErrUnknownID) {
		t.Errorf("unknown id: %v", err)
	}
	e, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "home", Who: WhoOwner})
	if err != nil || e.Steps != 1 || len(e.Servers) != 1 || e.Servers[0] == "" {
		t.Fatalf("%+v %v", e, err)
	}
	waitFor(t, func() bool { x, _ := q.Get(e.ID); return x.State == StateDone })
	sw, _, err := q.Enqueue(Request{Kind: KindSweep, Who: WhoOwner})
	if err != nil || sw.Steps != len(h.s.o.Catalog.Servers) {
		t.Fatalf("sweep %+v %v", sw, err)
	}
	waitFor(t, func() bool { x, _ := q.Get(sw.ID); return x.State == StateDone })
	x, _ := q.Get(sw.ID)
	h.st.mu.Lock()
	defer h.st.mu.Unlock()
	if len(h.st.tests) != 1+len(h.s.o.Catalog.Servers) || len(x.Tests) != len(h.s.o.Catalog.Servers) {
		t.Fatalf("%d tests, sweep %+v", len(h.st.tests), x)
	}
	if first := h.st.tests[0]; first.Trigger != "manual" || first.Region != "home" || !first.OnDemand() {
		t.Errorf("Test now %+v", first)
	}
	for _, ts := range h.st.tests[1:] {
		if ts.Trigger != "sweep" || ts.Region != "" || ts.FallbackLevel != FallbackNone {
			t.Errorf("sweep test %+v", ts)
		}
	}
}

// TestWireBytes: a test's bytes count every attempt's data phase on the
// wire (ramp-up included), not only the measured window: the share link's
// budget charges what was sent.
func TestWireBytes(t *testing.T) {
	h := newHarness(t, Options{})
	h.setRun(func(c Config, n int) (Result, error) {
		if n == 1 { // the first download attempt fails after its data phase started
			return Result{WireBytes: 7e6}, ErrBusy
		}
		return Result{Direction: c.Direction, Bytes: 10e6, WireBytes: 12e6, Elapsed: time.Second}, nil
	})
	_, _, bytes := h.s.execJob(context.Background(), h.s.slotJob(target{server: "Near", nearby: -1}), "manual")
	if bytes != 7e6+12e6+12e6 {
		t.Errorf("bytes %d, want %d", bytes, int64(7e6+12e6+12e6))
	}
}

// TestModes: off by default; a saved mode, a flag over it; the opt-in
// regions are validated and saved.
func TestModes(t *testing.T) {
	h := newHarness(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	go h.s.Run(ctx)
	set := func(k string) string { v, _, _ := h.st.Setting(ctx, k); return v }
	waitFor(t, func() bool { return set(settingSeed) != "" })
	if st := h.s.State(); st.Mode != ModeOff || !st.Next.IsZero() || len(st.Modes) != 3 || st.SetByFlag || len(st.Plans) != 2 {
		t.Fatalf("off by default: %+v", st)
	}
	if _, err := h.s.SetMode(ctx, ModeCustom, nil); !errors.Is(err, ErrMode) {
		t.Errorf("custom without servers: %v", err)
	}
	if _, err := h.s.SetMode(ctx, ModeRegions, []string{"nowhere"}); !errors.Is(err, ErrMode) {
		t.Errorf("unknown region: %v", err)
	}
	next, err := h.s.SetMode(ctx, ModeRegions, []string{"us-east"})
	if err != nil || next.IsZero() || set(SettingKey) != "regions" || set(settingExtra) != "us-east" {
		t.Errorf("regions: %v %v", next, err)
	}
	if st := h.s.State(); st.Mode != ModeRegions || len(st.Rows) != 3 || st.NextRow == "" || st.Rows[2].Why != "added" && st.Rows[0].Why != "added" && st.Rows[1].Why != "added" {
		t.Errorf("state %+v", st)
	}
	cancel()
	h2 := newHarness(t, Options{Mode: ModeNearby})
	h2.st.settings = map[string]string{SettingKey: "off", settingSeed: "42"}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go h2.s.Run(ctx2)
	waitFor(t, func() bool { return h2.s.State().Mode == ModeNearby })
	if st := h2.s.State(); !st.SetByFlag || h2.s.seedNow() != 42 {
		t.Errorf("flag over saved: %+v, seed %d", st.SetByFlag, h2.s.seedNow())
	}
}

// TestMask: exactly the probes that ran during a test (from Sample.At to
// their arrival) or within MaskTail after it are blanked.
func TestMask(t *testing.T) {
	w := &Windows{}
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	w.Add(Window{Start: t0, End: t0.Add(20 * time.Second)})
	now := t0
	var got []model.Sample
	m := Mask{Inner: model.SinkFunc(func(s model.Sample) { got = append(got, s) }), Windows: w, Now: func() time.Time { return now }}
	gw := model.SeriesKey{Target: "Gateway", Kind: model.KindICMP}
	for _, c := range []struct {
		at, arrive time.Duration // from t0
		masked     bool
	}{
		{-10 * time.Second, -9 * time.Second, false},
		{-2 * time.Second, 100 * time.Millisecond, true},
		{10 * time.Second, 11 * time.Second, true},
		{21 * time.Second, 21500 * time.Millisecond, true}, // the 2 s tail
		{22500 * time.Millisecond, 23 * time.Second, false},
	} {
		now = t0.Add(c.arrive)
		m.Observe(model.Sample{Key: gw, Slot: t0.Add(c.at).Truncate(time.Second), At: t0.Add(c.at), RTT: 30 * time.Millisecond})
		s := got[len(got)-1]
		if masked := s.Lost && s.Reason == model.ReasonGap && s.RTT == 0; masked != c.masked {
			t.Errorf("probe at %v: masked %v, want %v", c.at, masked, c.masked)
		}
	}
	if sh := w.Share(t0.Add(time.Minute), 5*time.Minute); sh < 21.9/300 || sh > 22.1/300 {
		t.Errorf("share %v", sh)
	}
}

// TestAbortTail: a test cut short masks AbortTail more than one that ended
// normally.
func TestAbortTail(t *testing.T) {
	for _, fail := range []bool{false, true} {
		h := newHarness(t, Options{})
		h.setRun(func(c Config, n int) (Result, error) {
			if fail {
				return Result{}, errors.New("protocol error")
			}
			return Result{Direction: c.Direction, Bytes: 1e6, Elapsed: time.Second}, nil
		})
		ts := h.job(t, h.s.slotJob(target{server: "Near", nearby: -1}), "manual")
		end := ts[0].End
		after := end.Add(MaskTail + time.Second)
		if got := h.s.o.Windows.Overlaps(after, after); got != fail {
			t.Errorf("failed %v: masked %v at end+%v", fail, got, MaskTail+time.Second)
		}
	}
}

// TestSkipAndRest: a skipped server is never tested (chains, sweeps, Test
// now); one busy BusyRun scheduled tests in a row rests for BusyRest.
func TestSkipAndRest(t *testing.T) {
	h := newHarness(t, Options{})
	ctx := context.Background()
	if err := h.s.SetSkip(ctx, []string{"Nope"}); !errors.Is(err, ErrMode) {
		t.Errorf("unknown: %v", err)
	}
	if err := h.s.SetSkip(ctx, []string{"Mid"}); err != nil || h.st.settings[settingSkip] != "Mid" {
		t.Fatalf("%v %v", err, h.st.settings)
	}
	if _, err := h.s.resolve(KindSingle, "Mid"); !errors.Is(err, ErrSkipped) {
		t.Errorf("Test now of a skipped server: %v", err)
	}
	jobs, _ := h.s.resolve(KindSweep, "")
	for _, j := range jobs {
		if j.Chain[0].ID == "Mid" {
			t.Error("a sweep tests a skipped server")
		}
	}
	for range 24 {
		ts := h.region(t, "home")
		if ts.Server == "Mid" {
			t.Fatal("a slot used a skipped server")
		}
		h.mu.Lock()
		h.now = h.now.Add(time.Hour)
		h.mu.Unlock()
	}
	if st := h.s.State(); !st.Servers[slices.IndexFunc(st.Servers, func(s ServerState) bool { return s.ID == "Mid" })].Skipped {
		t.Error("state")
	}
	// Busy three scheduled tests in a row: rests.
	h.setRun(func(c Config, n int) (Result, error) {
		if host, _ := hostPort(c); host == "near.example" {
			return Result{}, ErrBusy
		}
		return Result{Direction: c.Direction, Bytes: 1e6, Elapsed: time.Second}, nil
	})
	for range BusyRun {
		h.slot(t, target{server: "Near"})
	}
	st := h.s.State()
	near := st.Servers[slices.IndexFunc(st.Servers, func(s ServerState) bool { return s.ID == "Near" })]
	if near.RestUntil.Sub(h.clock()) < BusyRest-time.Minute {
		t.Fatalf("rest %v", near.RestUntil)
	}
	if ts := h.job(t, h.s.slotJob(target{server: "Near", nearby: -1}), "schedule"); len(ts) != 0 {
		t.Errorf("a resting server was tested: %+v", ts)
	}
	if ts := h.job(t, h.s.slotJob(target{server: "Near", nearby: -1}), "manual"); len(ts) != 1 {
		t.Error("Test now of a resting server")
	}
}
