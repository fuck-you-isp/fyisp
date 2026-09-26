package probe

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TestDefaultTimeouts: TCP and ICMP wait 3s by default (a 1s timeout records
// slow successes on bufferbloated links as loss), never longer than the
// series' interval; HTTPS waits 5s; an explicit Options.Timeout is used as is.
func TestDefaultTimeouts(t *testing.T) {
	r := &runner{}
	cases := []struct {
		kind model.ProbeKind
		iv   time.Duration
		want time.Duration
	}{
		{model.KindTCP, 15 * time.Second, 3 * time.Second},
		{model.KindICMP, 5 * time.Second, 3 * time.Second},
		{model.KindICMP, 2 * time.Second, 2 * time.Second}, // 6s interval: ICMP every 2s
		{model.KindHTTPS, 15 * time.Second, 5 * time.Second},
	}
	for _, c := range cases {
		s := &series{key: model.SeriesKey{Kind: c.kind}, iv: c.iv}
		got := r.timeout(c.kind)
		if c.kind != model.KindHTTPS {
			got = r.seriesTimeout(s)
		}
		if got != c.want {
			t.Errorf("%s every %s: timeout %s, want %s", c.kind, c.iv, got, c.want)
		}
	}
	r = &runner{o: Options{Timeout: 3 * time.Second}}
	if got := r.seriesTimeout(&series{key: model.SeriesKey{Kind: model.KindTCP}, iv: 500 * time.Millisecond}); got != 3*time.Second {
		t.Errorf("explicit timeout: %s", got)
	}
}

// TestScheduleNoOverlap: a probe that runs longer than the interval (a 3s
// timeout on a 2s ICMP interval) never overlaps the next probe of the same
// series; the slots it overran are skipped.
func TestScheduleNoOverlap(t *testing.T) {
	iv, ph := 2*time.Second, 500*time.Millisecond
	c := &fakeClock{t: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
	var running atomic.Int32
	var fs []fired
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	schedule(ctx, clock{now: c.now, sleep: c.sleep}, iv, ph, func(slot time.Time) {
		if running.Add(1) != 1 {
			t.Error("probes of one series overlap")
		}
		fs = append(fs, fired{slot, c.now()})
		c.sleep(ctx, 3*time.Second) // the probe times out
		running.Add(-1)
		if len(fs) == 10 {
			cancel()
		}
	})
	for i := 1; i < len(fs); i++ {
		if !fs[i].at.After(fs[i-1].at.Add(3*time.Second - time.Nanosecond)) {
			t.Errorf("fire %d at %s, before the previous probe (at %s) finished", i, fs[i].at, fs[i-1].at)
		}
		if !fs[i].slot.After(fs[i-1].slot) {
			t.Errorf("slot %d repeats", i)
		}
	}
}
