package store

import (
	"context"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// AnnotationStore keeps timeline notes (implemented by *SQLite and *Fake).
type AnnotationStore interface {
	AddAnnotation(ctx context.Context, a *model.Annotation) error // sets ID, Created, Updated
	UpdateAnnotation(ctx context.Context, a *model.Annotation) error
	DeleteAnnotation(ctx context.Context, id int64) error
	// Annotations overlapping [from, to], oldest first. publicOnly limits
	// the result to notes marked Public.
	Annotations(ctx context.Context, from, to time.Time, publicOnly bool) ([]model.Annotation, error)
}

// ReportStore keeps rendered evidence report snapshots.
type ReportStore interface {
	SaveReport(ctx context.Context, meta model.ReportMeta, html []byte) error
	Report(ctx context.Context, id string) (model.ReportMeta, []byte, error) // ErrNotFound
	Reports(ctx context.Context) ([]model.ReportMeta, error)                 // newest first
	SetReportPublic(ctx context.Context, id string, public bool) error
	DeleteReport(ctx context.Context, id string) error
}

// BaselineReader computes each series' normal from stored history.
type BaselineReader interface {
	// Baselines for keys as of `at`, over the `window` before the current
	// hour (default 7 days). hourOfDay selects same-UTC-hour statistics.
	Baselines(ctx context.Context, keys []model.SeriesKey, at time.Time, window time.Duration, hourOfDay bool) (map[model.SeriesKey]model.Baseline, error)
}
