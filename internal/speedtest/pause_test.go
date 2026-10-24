package speedtest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPauseSchedule: while paused, scheduled slots are skipped (the grid
// moves on, nothing piles up for later) and Test now still runs; a timed
// pause ends by itself and is cleared from the store; resuming restores the
// schedule.
func TestPauseSchedule(t *testing.T) {
	h := newHarness(t, Options{Mode: ModeRegions})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return !h.s.State().Next.IsZero() })

	if err := h.s.SetPause(ctx, true, time.Time{}); err != nil {
		t.Fatal(err)
	}
	st := h.s.State()
	if st.Pause == nil || !st.Pause.Until.IsZero() || !st.Pause.Since.Equal(h.clock()) {
		t.Fatalf("paused state %+v", st.Pause)
	}
	if v, _, _ := h.st.Setting(ctx, settingPause); !strings.Contains(v, `"until":0`) {
		t.Errorf("stored pause %q", v)
	}
	// Three cycles go by: every slot is taken and skipped.
	h.mu.Lock()
	h.now = h.now.Add(3 * h.s.IntervalFor(ModeRegions))
	h.mu.Unlock()
	h.s.poke()
	waitFor(t, func() bool { return h.s.State().Next.After(h.clock()) })
	h.mu.Lock()
	calls := len(h.calls)
	h.mu.Unlock()
	if calls != 0 || len(h.s.Queue().State().Waiting) != 0 {
		t.Fatalf("paused: %d runs, queue %+v", calls, h.s.Queue().State())
	}
	// Test now still runs.
	if _, _, err := h.s.Queue().Enqueue(Request{Kind: KindSingle, Target: "home"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { h.st.mu.Lock(); defer h.st.mu.Unlock(); return len(h.st.tests) == 1 })

	// A timed pause ends by itself, and the schedule goes on. Changing the
	// end time keeps the start.
	since := h.s.State().Pause.Since
	until := h.clock().Add(time.Hour)
	if err := h.s.SetPause(ctx, true, until); err != nil {
		t.Fatal(err)
	}
	if p := h.s.State().Pause; p == nil || !p.Until.Equal(until) || !p.Since.Equal(since) {
		t.Fatalf("timed pause %+v, since %v", p, since)
	}
	// fyisp only gets to the loop after the end (a suspend): the slots due
	// during the pause are still skipped.
	h.mu.Lock()
	h.now = until.Add(time.Second)
	h.mu.Unlock()
	if h.s.State().Pause != nil {
		t.Error("State reports a pause past its end time")
	}
	h.s.poke()
	waitFor(t, func() bool { v, _, _ := h.st.Setting(ctx, settingPause); return v == "" })
	waitFor(t, func() bool { return h.s.State().Next.After(h.clock()) })
	h.st.mu.Lock()
	for _, x := range h.st.tests {
		if x.Trigger == "schedule" {
			t.Errorf("a slot due during the pause ran after it: %+v", x)
		}
	}
	h.st.mu.Unlock()
	h.mu.Lock()
	h.now = h.now.Add(h.s.IntervalFor(ModeRegions))
	h.mu.Unlock()
	h.s.poke()
	waitFor(t, func() bool {
		h.st.mu.Lock()
		defer h.st.mu.Unlock()
		return len(h.st.tests) > 1 && h.st.tests[len(h.st.tests)-1].Trigger == "schedule"
	})
	// No burst: at most the cycle that elapsed after the pause ran (2
	// regions), not the skipped ones.
	h.st.mu.Lock()
	n := 0
	for _, x := range h.st.tests {
		if x.Trigger == "schedule" {
			n++
		}
	}
	h.st.mu.Unlock()
	if n < 1 || n > 2 {
		t.Errorf("%d scheduled tests ran after the pause, want 1 or 2", n)
	}
}

// TestPauseDropsWaitingSlot: pausing drops the slot waiting in the queue;
// the test running finishes.
func TestPauseDropsWaitingSlot(t *testing.T) {
	h := newHarness(t, Options{Mode: ModeRegions})
	release := make(chan struct{})
	h.setRun(func(c Config, n int) (Result, error) {
		<-release
		return Result{Direction: c.Direction, Bytes: 1e6, Elapsed: time.Second}, nil
	})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.s.Run(ctx); close(done) }()
	defer func() { free(); cancel(); <-done }()
	waitFor(t, func() bool { return !h.s.State().Next.IsZero() })
	if _, _, err := h.s.Queue().Enqueue(Request{Kind: KindSingle, Target: "away"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return h.s.Queue().State().Running != nil })
	next := h.s.State().Next
	h.mu.Lock()
	h.now = next.Add(time.Second)
	h.mu.Unlock()
	h.s.poke()
	waitFor(t, func() bool {
		w := h.s.Queue().State().Waiting
		return len(w) == 1 && w[0].Kind == KindScheduled
	})
	if err := h.s.SetPause(ctx, true, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if qs := h.s.Queue().State(); len(qs.Waiting) != 0 || qs.Running == nil || qs.Running.Kind != KindSingle {
		t.Fatalf("after pause: %+v", qs)
	}
	free()
	waitFor(t, func() bool { return h.s.Queue().State().Running == nil })
	h.st.mu.Lock()
	defer h.st.mu.Unlock()
	for _, x := range h.st.tests {
		if x.Trigger == "schedule" {
			t.Errorf("a scheduled test ran while paused: %+v", x)
		}
	}
}

// TestPauseBoundsAndLoad: end times out of bounds are refused; a saved pause
// is restored at start, and one that ended while fyisp was stopped is
// cleared.
func TestPauseBoundsAndLoad(t *testing.T) {
	h := newHarness(t, Options{Mode: ModeRegions})
	ctx := context.Background()
	now := h.clock()
	for _, until := range []time.Time{now.Add(30 * time.Second), now.Add(MaxPause + time.Minute), now.Add(-time.Hour)} {
		if err := h.s.SetPause(ctx, true, until); !errors.Is(err, ErrPause) {
			t.Errorf("until %v: %v", until.Sub(now), err)
		}
	}
	if err := h.s.SetPause(ctx, true, now.Add(MaxPause)); err != nil {
		t.Errorf("until MaxPause: %v", err)
	}

	for _, c := range []struct {
		name, saved string
		paused      bool
	}{
		{"until resumed", `{"since":1791633600000,"until":0}`, true},
		{"until later", `{"since":1791633600000,"until":1791640800000}`, true},
		{"ended while stopped", `{"since":1791633600000,"until":1791634000000}`, false},
		{"garbage", `nope`, false},
	} {
		h := newHarness(t, Options{Mode: ModeRegions}) // 2026-10-10 12:00 UTC = 1791633600000
		h.mu.Lock()
		h.now = time.UnixMilli(1791633600000 + 1800_000).UTC()
		h.mu.Unlock()
		_ = h.st.SetSetting(ctx, settingPause, c.saved)
		h.s.mu.Lock()
		h.s.load(ctx)
		h.s.mu.Unlock()
		if got := h.s.State().Pause != nil; got != c.paused {
			t.Errorf("%s: paused %v, want %v", c.name, got, c.paused)
		}
		if v, _, _ := h.st.Setting(ctx, settingPause); c.name == "ended while stopped" && v != "" {
			t.Errorf("%s: setting %q not cleared", c.name, v)
		}
	}
}
