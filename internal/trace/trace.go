// Package trace runs unprivileged traceroutes: always-on for targets marked
// Trace in the profile, and on demand (Investigate) for any target. Each hop
// becomes a KindTrace series; routes and route changes are reported.
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
	// Investigate traces target every Interval until the returned stop
	// function is called or ttl passes (on-demand, for any profile target).
	Investigate(target string, ttl time.Duration) (stop func(), err error)
	// Routes returns the current route of every traced target.
	Routes() []model.Route
}
