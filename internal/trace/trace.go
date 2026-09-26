// Package trace runs unprivileged traceroutes: always-on for targets marked
// Trace in the profile, and on demand (Investigate) for any target. Each hop
// becomes a KindTrace series; routes and route changes are reported.
//
// Hops that several traces share (the home gateway, the ISP's routers) are
// measured once per round and that one measurement is reported in every
// sharing trace's series, and each router gets at most Options.RouterRate
// probes per second over all traces: see share.go.
package trace

import (
	"context"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Sink receives trace results (implemented by the store and the web layer).
type Sink interface {
	model.Sink                              // per-hop samples: Key{Target, KindTrace, Hop}
	ObserveHop(info model.HopInfo)          // a hop address was seen (cheap; deduplicated by caller or store)
	ObserveRoute(r model.Route)             // current route after each round
	ObserveRouteChange(c model.RouteChange) // path changed (after it held for 2 rounds)
}

// Tracer runs traces while Run runs.
//
//	func New(o Options) Tracer
type Tracer interface {
	Run(ctx context.Context, p *model.Profile, sink Sink) error
	// Investigate traces target every Options.Investigate until the
	// returned stop function is called or ttl passes (on-demand, for any
	// profile target). Concurrent callers share one trace; it slows back
	// down (or stops, for an untraced target) when the last one ends.
	// Errors: not running, unknown target, ttl <= 0.
	Investigate(target string, ttl time.Duration) (stop func(), err error)
	// Routes returns the current route of every traced target.
	Routes() []model.Route
	// Interval is the current slot interval of target's KindTrace series
	// (Options.Investigate while investigated, else Options.Interval), for
	// the store's per-series interval.
	Interval(target string) time.Duration
}
