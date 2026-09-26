// Package store keeps every sample for up to 90 days in one SQLite file:
// hourly zstd-compressed blobs per series plus hourly summary rows.
package store

import (
	"context"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Tier names returned in PanelResult.Tier.
const (
	TierRaw    = "raw" // ranges up to RawMaxRange read raw blobs
	TierHourly = "1h"  // longer ranges read summary_1h
)

// RawMaxRange is the longest range served from raw samples.
const RawMaxRange = 48 * time.Hour

// Writer accepts samples. Observe buffers the current hour in memory; Flush
// persists it (every 60s and on shutdown); Prune enforces retention.
type Writer interface {
	model.Sink
	Flush(ctx context.Context) error
	Prune(ctx context.Context, before time.Time) error
}

// PanelQuery asks for several series over one range, bucketed to at most
// MaxPoints points. Implementations must use one SQL query per call.
type PanelQuery struct {
	Keys      []model.SeriesKey
	From, To  time.Time
	MaxPoints int
}

// PanelResult is columnar: bucket i covers [Start+i*Step, Start+(i+1)*Step).
// Buckets with no successful samples have NaN Mean/Min/Max.
type PanelResult struct {
	Tier   string        `json:"tier"`
	Start  time.Time     `json:"start"`
	Step   time.Duration `json:"step"`
	Series []SeriesCols  `json:"series"`
}

// SeriesCols holds one series' buckets. Times are milliseconds.
type SeriesCols struct {
	Key    model.SeriesKey           `json:"key"`
	Mean   []float32                 `json:"mean"`
	Min    []float32                 `json:"min"`
	Max    []float32                 `json:"max"`
	N      []uint32                  `json:"n"`    // successful samples
	Lost   []uint32                  `json:"lost"` // lost samples (excludes not-measured)
	LostBy map[model.Reason][]uint32 `json:"lost_by,omitempty"`
}

// RawPoint is one stored sample, as exported.
type RawPoint struct {
	Key    model.SeriesKey
	TS     time.Time
	RTTms  float64
	Lost   bool
	Reason model.Reason // ReasonGap means not measured
}

// SeriesInfo describes a stored series.
type SeriesInfo struct {
	ID          int64
	Key         model.SeriesKey
	Interval    time.Duration
	First, Last time.Time
}

// Stats summarizes the store for the status page.
type Stats struct {
	Dir          string    `json:"-"` // never exposed publicly
	FileBytes    int64     `json:"file_bytes"`
	Oldest       time.Time `json:"oldest"`
	LastFlush    time.Time `json:"last_flush"`
	LastFlushErr string    `json:"last_flush_err,omitempty"`
}

// Reader serves queries. Safe for concurrent use.
type Reader interface {
	Panel(ctx context.Context, q PanelQuery) (*PanelResult, error)
	Raw(ctx context.Context, keys []model.SeriesKey, from, to time.Time, fn func(RawPoint) error) error
	Series(ctx context.Context) ([]SeriesInfo, error)
	Stats(ctx context.Context) (Stats, error)
}

// Store is both.
type Store interface {
	Writer
	Reader
	Close() error
}
