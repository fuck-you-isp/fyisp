package probe

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSlotMath(t *testing.T) {
	iv, ph := 15*time.Second, 4*time.Second
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		now  time.Duration // after base
		slot time.Duration // after base
	}{
		{0, -15 * time.Second},                       // before this slot's fire time
		{3999 * time.Millisecond, -15 * time.Second}, // still before
		{4 * time.Second, 0},                         // fires at slot+phase
		{18 * time.Second, 0},
		{19 * time.Second, 15 * time.Second},
	}
	for _, c := range cases {
		n := slotIndex(base.Add(c.now), iv, ph)
		if got := slotStart(n, iv); !got.Equal(base.Add(c.slot)) {
			t.Errorf("now=+%s: slot %s, want +%s", c.now, got, c.slot)
		}
		if ft := fireTime(n, iv, ph); ft.After(base.Add(c.now)) || base.Add(c.now).Sub(ft) >= iv {
			t.Errorf("now=+%s: fire time %s not in (now-iv, now]", c.now, ft)
		}
	}
	if s := slotStart(slotIndex(base.Add(20*time.Second), iv, ph), iv); s.Location() != time.UTC {
		t.Errorf("slot not UTC: %v", s)
	}
	// Phases spread targets evenly and stay inside the interval.
	seen := map[time.Duration]bool{}
	for i := range 87 {
		p := phaseOf(i, 87, iv, 0.5)
		if p < 0 || p >= iv || seen[p] {
			t.Fatalf("phase %d = %s", i, p)
		}
		seen[p] = true
	}
}

// fakeClock advances by exactly the requested sleep, plus optional jumps.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps int
	jumps  map[int]time.Duration // sleep number -> jump applied after it
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) sleep(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	c.sleeps++
	c.t = c.t.Add(c.jumps[c.sleeps])
	return true
}

type fired struct{ slot, at time.Time }

func runSchedule(t *testing.T, c *fakeClock, iv, ph time.Duration, n int) []fired {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out []fired
	schedule(ctx, clock{now: c.now, sleep: c.sleep}, iv, ph, func(slot time.Time) {
		out = append(out, fired{slot, c.now()})
		if len(out) == n {
			cancel()
		}
	})
	return out
}

func checkSlots(t *testing.T, fs []fired, iv, ph time.Duration) {
	t.Helper()
	for i, f := range fs {
		if f.slot.UnixNano()%int64(iv) != 0 {
			t.Errorf("slot %s not aligned to %s", f.slot, iv)
		}
		if i > 0 && !f.slot.After(fs[i-1].slot) {
			t.Errorf("slot %d (%s) not after previous (%s): duplicate or backwards", i, f.slot, fs[i-1].slot)
		}
		// fireTime(n) <= at < fireTime(n+1)
		if d := f.at.Sub(f.slot); d < ph || d >= iv+ph {
			t.Errorf("slot %s fired at +%s, want in [phase %s, interval+phase %s)", f.slot, d, ph, iv+ph)
		}
	}
}

func TestScheduleSteady(t *testing.T) {
	iv, ph := 15*time.Second, 7*time.Second
	start := time.Date(2026, 9, 26, 12, 0, 1, 0, time.UTC)
	c := &fakeClock{t: start}
	fs := runSchedule(t, c, iv, ph, 10)
	checkSlots(t, fs, iv, ph)
	for i, f := range fs {
		want := start.Truncate(iv).Add(time.Duration(i) * iv)
		if !f.slot.Equal(want) || !f.at.Equal(want.Add(ph)) {
			t.Errorf("fire %d: slot %s at %s, want slot %s at %s", i, f.slot, f.at, want, want.Add(ph))
		}
	}
}

func TestScheduleForwardJump(t *testing.T) {
	iv, ph := 5*time.Second, 2*time.Second
	start := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	c := &fakeClock{t: start, jumps: map[int]time.Duration{4: time.Hour + 1234*time.Millisecond}}
	fs := runSchedule(t, c, iv, ph, 10)
	checkSlots(t, fs, iv, ph)
	var gaps int
	for i := 1; i < len(fs); i++ {
		d := fs[i].slot.Sub(fs[i-1].slot)
		if d != iv {
			gaps++
			if d < time.Hour {
				t.Errorf("unexpected gap %s", d)
			}
		}
	}
	if gaps != 1 {
		t.Errorf("want exactly one gap (no samples for skipped slots), got %d: %v", gaps, fs)
	}
}

func TestScheduleBackwardJump(t *testing.T) {
	iv, ph := 5*time.Second, time.Second
	start := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	c := &fakeClock{t: start, jumps: map[int]time.Duration{4: -50 * time.Second}}
	fs := runSchedule(t, c, iv, ph, 12)
	checkSlots(t, fs, iv, ph) // strictly increasing: no slot fires twice
	for i := 1; i < len(fs); i++ {
		if d := fs[i].slot.Sub(fs[i-1].slot); d != iv {
			t.Errorf("gap %s between %s and %s: slots after catching up must resume in order", d, fs[i-1].slot, fs[i].slot)
		}
	}
	// The clock needed 50s to catch up: the fire after the jump happened
	// only once the clock had passed the last slot again.
	if c.sleeps < 4+int(50*time.Second/iv) {
		t.Errorf("only %d sleeps; the schedule did not wait for the clock to catch up", c.sleeps)
	}
}

// TestScheduleLargeBackwardJump: after the wall clock is stepped back 2h
// (it was wrong and got corrected), probing resumes within about one
// interval instead of 2h later.
func TestScheduleLargeBackwardJump(t *testing.T) {
	for _, iv := range []time.Duration{5 * time.Second, 15 * time.Second, 60 * time.Second} {
		ph := iv / 3
		start := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
		c := &fakeClock{t: start, jumps: map[int]time.Duration{4: -2 * time.Hour}}
		var fs []fired
		ctx, cancel := context.WithCancel(context.Background())
		schedule(ctx, clock{now: c.now, sleep: c.sleep}, iv, ph, func(slot time.Time) {
			fs = append(fs, fired{slot, c.now()})
			if len(fs) == 10 || c.sleeps > 100000 {
				cancel()
			}
		})
		cancel()
		var before, after []fired
		for _, f := range fs {
			if f.at.Before(start) {
				after = append(after, f)
			} else {
				before = append(before, f)
			}
		}
		if len(before) == 0 || len(after) == 0 {
			t.Fatalf("iv %s: fired %v", iv, fs)
		}
		// The step happened within one interval after the last fire before it.
		stepped := before[len(before)-1].at.Add(-2 * time.Hour)
		if d := after[0].at.Sub(stepped); d > 3*iv {
			t.Errorf("iv %s: first probe %s after the step back, want within ~1 interval", iv, d)
		}
		checkSlots(t, after, iv, ph)
	}
}
