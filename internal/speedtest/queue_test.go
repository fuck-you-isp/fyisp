package speedtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

func TestQueueOrderAndLimits(t *testing.T) {
	var q queue
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	add := func(kind, who, client, target string) (Entry, bool, error) {
		return q.add(&Entry{Kind: kind, Who: who, Client: client, Target: target}, now)
	}
	v1, _, _ := add(KindTest, WhoVisitor, "a", "")
	v2, _, _ := add(KindSweep, WhoVisitor, "b", "")
	o1, _, _ := add(KindTest, WhoOwner, "", "uk")
	s1, _, _ := add(KindScheduled, WhoScheduled, "", "us-east")
	var order []int64
	for _, e := range q.snapshot().Waiting {
		order = append(order, e.ID)
	}
	// Owner and schedule ahead of visitors, each in arrival order.
	if want := []int64{o1.ID, s1.ID, v1.ID, v2.ID}; !equalIDs(order, want) {
		t.Errorf("order %v, want %v", order, want)
	}
	// Equivalent requests return the waiting entry.
	if e, dup, err := add(KindTest, WhoVisitor, "c", "uk"); err != nil || !dup || e.ID != o1.ID {
		t.Errorf("dedupe: %+v %v %v", e, dup, err)
	}
	if e, dup, _ := add(KindSweep, WhoOwner, "", ""); !dup || e.ID != v2.ID {
		t.Errorf("sweep dedupe: %+v", e)
	}
	// A visitor may have one waiting entry.
	if _, _, err := add(KindTest, WhoVisitor, "a", "Mid"); !errors.Is(err, ErrQueueFull) {
		t.Errorf("second entry of a visitor: %v", err)
	}
	// One runs at a time; a scheduled slot that waited too long is dropped.
	e := q.pop(now)
	if e == nil || e.ID != o1.ID || q.pop(now) != nil {
		t.Fatalf("pop %+v", e)
	}
	q.done(e, now)
	if e := q.pop(now.Add(ScheduledTTL + time.Second)); e == nil || e.ID != v1.ID {
		t.Fatalf("after the drop: %+v", e)
	}
	if got, ok := q.entry(s1.ID); !ok || got.State != StateDropped {
		t.Errorf("scheduled slot %+v", got)
	}
	// Cancel a waiting entry; clear the visitors'.
	if err := q.cancel(v2.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := q.cancel(999, now); !errors.Is(err, ErrNoEntry) {
		t.Errorf("cancel unknown: %v", err)
	}
	add(KindTest, WhoVisitor, "q", "Near")
	add(KindTest, WhoOwner, "", "Near2")
	if n := q.clear(WhoVisitor, now); n != 1 || len(q.snapshot().Waiting) != 1 {
		t.Errorf("clear: %d, %d left", n, len(q.snapshot().Waiting))
	}
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSweepAndCancel: Run takes queued entries one at a time; a sweep tests
// every catalog server once (stored as trigger sweep, never in patterns)
// and reports progress; the owner can cancel the running entry.
func TestSweepAndCancel(t *testing.T) {
	h := newHarness(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { h.s.Run(ctx); close(done) }()
	e, _, err := h.s.Enqueue(KindSweep, "", WhoVisitor, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.s.Enqueue(KindTest, "Nope", WhoOwner, ""); !errors.Is(err, ErrUnknown) {
		t.Errorf("unknown target: %v", err)
	}
	waitFor(t, func() bool { x, _ := h.s.QueueEntry(e.ID); return x.State == StateDone })
	x, _ := h.s.QueueEntry(e.ID)
	if len(x.Results) != 6 || x.Progress.Total != 6 {
		t.Fatalf("sweep %+v", x)
	}
	for _, r := range x.Results {
		if r.Status != "done" || r.DownBps == nil {
			t.Errorf("result %+v", r)
		}
	}
	if ls := h.s.Queue().LastSweep; ls == nil || ls.ID != e.ID {
		t.Errorf("last sweep %+v", ls)
	}
	h.st.mu.Lock()
	for _, ts := range h.st.tests {
		if ts.Trigger != "sweep" || ts.Entry != e.ID {
			t.Errorf("stored %+v", ts)
		}
	}
	h.st.mu.Unlock()
	// Cancel a running test: nothing is stored for it.
	release := make(chan struct{})
	h.setRun(func(c Config, n int) (Result, error) {
		<-release
		return Result{}, context.Canceled
	})
	n := len(h.st.tests)
	e2, _, _ := h.s.Enqueue(KindTest, "Mid", WhoOwner, "")
	waitFor(t, func() bool { return h.s.Queue().Running != nil })
	if err := h.s.Cancel(e2.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitFor(t, func() bool {
		x, _ := h.s.QueueEntry(e2.ID)
		return x.State == StateCancelled && h.s.Queue().Running == nil
	})
	if len(h.st.tests) != n {
		t.Errorf("a cancelled test was stored")
	}
	cancel()
	<-done
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
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
