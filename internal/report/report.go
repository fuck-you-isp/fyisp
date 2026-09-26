// Package report builds evidence reports: a self-contained HTML document
// (no scripts; inline CSS and SVG charts; prints to PDF) that summarises a
// time range for an ISP: verdicts, outage log, charts, per-group statistics,
// traceroute evidence and annotations.
package report

import (
	"context"
	"time"
)

// Options selects what goes into a report.
type Options struct {
	From, To time.Time
	Title    string
	// Redact applies the public-link rules (no private addresses, ISP edge
	// masked to /24, no reverse DNS for the first public hops, public
	// annotations only). Snapshots served on the public link are always
	// built with Redact.
	Redact bool
	Now    func() time.Time
}

// Builder renders reports from the running instance's data.
//
//	func New(deps Deps) Builder   // Deps: profile, store readers, verdict incidents, trace readers, baselines, annotations, version
type Builder interface {
	Build(ctx context.Context, o Options) (html []byte, err error)
}
