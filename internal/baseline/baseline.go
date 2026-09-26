// Package baseline answers "is this worse than usual?": it caches each
// series' normal (store.BaselineReader) and compares current values to it.
package baseline

import (
	"context"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Source is what the verdict engine, the web API and reports use.
//
//	func New(r store.BaselineReader, o Options) Source   // refreshes hourly in Run
type Source interface {
	Run(ctx context.Context) error
	// Get returns the series' baseline for time `at` (time-of-day aware once
	// 7 days of history exist), and whether one is known yet.
	Get(key model.SeriesKey, at time.Time) (model.Baseline, bool)
}
