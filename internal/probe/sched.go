package probe

import (
	"context"
	"time"
)

// Slots are aligned to multiples of the interval since the Unix epoch (UTC),
// so they line up with hour boundaries for every interval that divides an
// hour. A series fires at slotStart+phase; phases spread the targets evenly
// across the interval so probes never burst.
//
//	slot index n = floor((now - epoch - phase) / interval)
//	Sample.Slot  = epoch + n*interval
//	fire time    = epoch + n*interval + phase
//
// Only wall-clock time is used for slots; RTTs use the monotonic clock.

func slotIndex(now time.Time, iv, phase time.Duration) int64 {
	x := now.UnixNano() - int64(phase)
	n := x / int64(iv)
	if x%int64(iv) < 0 {
		n--
	}
	return n
}

func slotStart(n int64, iv time.Duration) time.Time {
	return time.Unix(0, n*int64(iv)).UTC()
}

func fireTime(n int64, iv, phase time.Duration) time.Time {
	return time.Unix(0, n*int64(iv)+int64(phase))
}

// phaseOf spreads item i of n evenly over iv, shifted by frac (0..1) of one
// step so different kinds of the same target don't fire together.
func phaseOf(i, n int, iv time.Duration, frac float64) time.Duration {
	if n <= 0 {
		return 0
	}
	step := float64(iv) / float64(n)
	p := time.Duration((float64(i) + frac) * step)
	if p >= iv || p < 0 {
		p = 0
	}
	return p
}

type clock struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) bool // false when ctx is done
}

func realSleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// schedule calls fire once per slot, at the slot's fire time, until ctx is
// done. It starts with the first slot that begins after the call. fire runs
// synchronously: a probe that overruns its interval skips slots.
//
// Clock jumps: after a forward jump (or suspend) only the current slot is
// fired; skipped slots get no sample (the store records them as not
// measured). After a small backward jump (up to backStep(iv)) nothing fires
// until the clock passes the last fired slot again, so slots never repeat.
// After a larger one (the clock was wrong and got corrected) the schedule
// restarts from the current slot instead of waiting for the old time.
func schedule(ctx context.Context, c clock, iv, phase time.Duration, fire func(slot time.Time)) {
	last := slotIndex(c.now(), iv, phase)
	for {
		if ctx.Err() != nil {
			return
		}
		now := c.now()
		n := slotIndex(now, iv, phase)
		if n > last {
			last = n
			fire(slotStart(n, iv))
			continue
		}
		if time.Duration(last-n)*iv > backStep(iv) {
			last = n
		}
		wait := fireTime(last+1, iv, phase).Sub(now)
		// Re-check at least once per interval: the wall clock may have been
		// stepped back, and monotonic timers stop during suspend.
		wait = min(wait, iv)
		if !c.sleep(ctx, wait) {
			return
		}
	}
}

// backStep is how far the clock may step back before the schedule restarts
// from the current slot: two intervals, and at least the minute within which
// the store still rejects samples behind its last one (see store.clockSlack).
func backStep(iv time.Duration) time.Duration { return max(2*iv, time.Minute) }
