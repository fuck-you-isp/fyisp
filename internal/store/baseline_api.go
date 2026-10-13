package store

import (
	"context"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// BaselineReader computes each series' normal from stored history.
type BaselineReader interface {
	// Baselines for keys as of `at`, over the `window` before the current
	// hour (default 7 days). hourOfDay selects same-UTC-hour statistics.
	Baselines(ctx context.Context, keys []model.SeriesKey, at time.Time, window time.Duration, hourOfDay bool) (map[model.SeriesKey]model.Baseline, error)
}
