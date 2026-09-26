// Package store keeps every sample for up to 90 days in one SQLite file:
// hourly zstd-compressed blobs per series plus hourly summaries (packed per
// day and series).
//
// Wiring (cmd/fyisp):
//
//	st, err := store.Open(dataDir, store.Options{Version: version, URL: localURL,
//		Interval: intervalOf /* the probe schedule: Target.Interval, ICMP /3 */, Log: log})
//	// errors.Is(err, store.ErrLocked): errors.As(err, &*store.LockedError) has the
//	// running instance's PID and URL; exit 3.
//	st.SetURL(url)                        // whenever the URL to show changes
//	sink := model.Fanout{st, metrics}     // Observe never blocks
//	tracer.Run(ctx, prof, traceSinks{st, metrics}) // both are trace.Sink (KindTrace
//	                                      // hop samples, routes, route changes, hop info);
//	                                      // Interval must return the trace round
//	                                      // interval for KindTrace keys (0: 5s)
//	web: st also implements TraceReader (Hops, Route, RouteChanges, HopInfo)
//	v0.3: st (and *Fake) also implement AnnotationStore, ReportStore and
//	      BaselineReader (v3api.go). Errors: ErrNotFound (404), ErrInvalid
//	      (400, validation), writes on OpenReadOnly fail. Prune drops old
//	      annotations; reports are never pruned (capped at MaxReports).
//	every 60s:  st.Flush(ctx)             // error: shown via Stats().LastFlushErr, retried next time
//	every 1h:   st.Prune(ctx, time.Now().Add(-retention))
//	shutdown:   stop probes; st.Flush(ctx); close tunnel; stop HTTP; st.Close()
//	            (Close flushes again, checkpoints, closes, releases the lock)
//
// `fyisp export` uses store.OpenReadOnly(dataDir), which takes no lock and
// sees what the running instance has flushed.
//
// The store starts no goroutines. Interval must match the scheduler: slots
// are Slot0 + i*Interval within each hour, so a too-long interval makes
// samples collide (dropped, Stats.Rejected) and a too-short one leaves
// not-measured slots.
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
	// BufferedBytes is the memory held by hours not yet in the database
	// (the current hour, plus closed hours while flushes fail).
	BufferedBytes int64 `json:"buffered_bytes"`
	Dropped       int64 `json:"dropped,omitempty"`  // samples discarded: write buffer full
	Rejected      int64 `json:"rejected,omitempty"` // samples ignored: duplicate slot or clock stepped back
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
