package store

import (
	"context"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store/blob"
)

// Fake's baselines: the same statistics as *SQLite's, from the kept
// samples.

// Baselines summarizes the kept samples per hour, as the real store's
// hourly summaries, and combines them the same way.
func (f *Fake) Baselines(_ context.Context, keys []model.SeriesKey, at time.Time, window time.Duration, hourOfDay bool) (map[model.SeriesKey]model.Baseline, error) {
	b := newBaselineSet(keys, at, window, hourOfDay)
	f.mu.RLock()
	defer f.mu.RUnlock()
	for k := range b.accs {
		hours := map[int64][]model.Sample{}
		for _, x := range f.data[k] {
			if !x.Slot.Before(b.from) && x.Slot.Before(b.to) {
				h := floorHour(x.Slot.UnixMilli())
				hours[h] = append(hours[h], x)
			}
		}
		for h, ss := range hours {
			slots := make([]blob.Slot, 0, len(ss))
			for _, x := range ss {
				slots = append(slots, toSlot(x))
			}
			b.add(k, h, summarize(slots))
		}
	}
	return b.result(), nil
}

var _ BaselineReader = (*Fake)(nil)
