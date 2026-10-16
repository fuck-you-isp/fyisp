// Package trace runs unprivileged traceroutes: always-on for targets marked
// Trace in the profile, and on demand (Investigate) for any target. Each hop
// becomes a KindTrace series; routes and route changes are reported.
//
// Always-on traces send ICMP echo requests. An on-demand trace may send TCP
// SYNs or UDP datagrams instead (model.TraceMode, where hops.Supported
// allows): its hops are KindTraceTCP or KindTraceUDP series, and its route
// is kept in memory only (Route), never reported to the sink. So is the
// route of a host outside the profile (model.HostTarget), which Investigate
// traces as a temporary target.
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
	model.Sink                              // per-hop samples: Key{Target, mode.Kind(), Hop}
	ObserveHop(info model.HopInfo)          // a hop address was seen (cheap; deduplicated by caller or store)
	ObserveRoute(r model.Route)             // current ICMP route after each round
	ObserveRouteChange(c model.RouteChange) // ICMP path changed (after it held for 2 rounds)
}

// Investigation is one on-demand trace session.
type Investigation interface {
	// Stop ends the session (idempotent).
	Stop()
	// Progress returns the rounds traced for this session so far and when
	// it ended (zero while it runs).
	Progress() (rounds int, ended time.Time)
}

// Tracer runs traces while Run runs.
//
//	func New(o Options) Tracer
type Tracer interface {
	Run(ctx context.Context, p *model.Profile, sink Sink) error
	// Investigate traces target with mode every Options.Investigate for at
	// most rounds rounds (on-demand, for any profile target): the session
	// ends by itself after its rounds, on Stop, or at a safety deadline of
	// twice the rounds' time. Concurrent sessions of one target and mode
	// share one trace; it slows back down (or stops, for an untraced target
	// or a TCP or UDP trace) when the last one ends. Errors: not running,
	// unknown target, rounds <= 0, a mode this OS does not support.
	//
	// A model.HostTarget name traces a host outside the profile (the
	// caller checked its syntax and its first resolution): a temporary
	// target, dropped when its last session ends (its route stays known to
	// Route for an hour). Its route is kept in memory only, never reported
	// to the sink. Every resolution must give only public addresses
	// (netinfo.Traceable); otherwise its sessions end.
	Investigate(target string, mode model.TraceMode, rounds int) (Investigation, error)
	// Routes returns the current ICMP route of every traced target.
	Routes() []model.Route
	// Route returns target's current route with mode.
	Route(target string, mode model.TraceMode) (model.Route, bool)
	// Interval is the current slot interval of target's hop series of kind
	// (Options.Investigate while investigated; else Options.Interval for
	// KindTrace and Options.Investigate for the others), for the store's
	// per-series interval.
	Interval(target string, kind model.ProbeKind) time.Duration
}
