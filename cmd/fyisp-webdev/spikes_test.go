package main

import (
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TestSpikesKeepRNG: -spikes changes only spikeTable's slots of one TCP
// series; every other sample of the backfill stays the same.
func TestSpikesKeepRNG(t *testing.T) {
	p, err := devProfile()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)
	run := func(spikes bool) map[model.SeriesKey]map[time.Time]model.Sample {
		out := map[model.SeriesKey]map[time.Time]model.Sample{}
		backfill(model.SinkFunc(func(s model.Sample) {
			if out[s.Key] == nil {
				out[s.Key] = map[time.Time]model.Sample{}
			}
			out[s.Key][s.Slot] = s
		}), p, 6*time.Hour, now, spikes)
		return out
	}
	plain, spiky := run(false), run(true)
	key := model.SeriesKey{Target: spikeTarget(p), Kind: model.KindTCP}
	var iv time.Duration
	for _, tg := range p.Targets {
		if tg.Name == key.Target {
			iv = tg.Interval
		}
	}
	if iv == 0 {
		t.Fatal("no TCP service to spike")
	}
	want := map[time.Time]float64{}
	for _, x := range spikeTable {
		want[now.Add(-x.age).Truncate(iv)] = x.extra
	}
	seen := 0
	for k, ss := range plain {
		for slot, s := range ss {
			got := spiky[k][slot]
			extra, spiked := want[slot]
			switch {
			case k == key && spiked && extra == 0:
				seen++
				if !got.Lost || got.Reason != model.ReasonTimeout {
					t.Errorf("%v %v = %+v, want a timeout", k, slot, got)
				}
			case k == key && spiked:
				seen++
				add := time.Duration(extra * float64(time.Millisecond))
				if got.Lost || !s.Lost && got.RTT != s.RTT+add || s.Lost && got.RTT <= add {
					t.Errorf("%v %v = %+v, want %+v plus %v ms", k, slot, got, s, extra)
				}
			case got != s:
				t.Errorf("%v %v changed: %+v -> %+v", k, slot, s, got)
			}
		}
	}
	if seen != len(spikeTable) {
		t.Errorf("%d of %d spikes landed", seen, len(spikeTable))
	}
}
