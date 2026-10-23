// Package web serves the fyisp dashboard: an embedded static UI, a JSON/CSV
// API over store.Reader and a few write routes.
//
// Local is the only handler: the owner's listener (default
// 127.0.0.1:3000). It adds /metrics and the write routes (on-demand traces,
// speed tests). Everything but the page and its static files needs the
// admin token; all of it is behind a Host allowlist, and the write routes
// also behind a same-origin check and a CSRF token derived from the admin
// token. There is one listener; live access from elsewhere is fyisp's
// quick tunnel (TunnelHost) or the owner's own tunnel or proxy
// (--allow-host).
//
// The caller owns the http.Server. Recommended settings:
//
//	&http.Server{
//		Handler:           h,
//		ReadHeaderTimeout: 5 * time.Second,
//		ReadTimeout:       10 * time.Second,
//		WriteTimeout:      30 * time.Second,
//		IdleTimeout:       60 * time.Second,
//		MaxHeaderBytes:    16 << 10,
//	}
package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/probe"
	"github.com/fuck-you-isp/fyisp/internal/store"
	"github.com/fuck-you-isp/fyisp/internal/tiles"
	"github.com/fuck-you-isp/fyisp/internal/verdict"
)

// Deps is everything the handlers need. Profile, Store and Status are
// required; Metrics may be nil (the route then answers 404).
// Verdict may be nil: the verdict banner and outage log are then hidden.
// Trace may be nil: the trace routes then answer 404 and the UI hides the
// Investigate view. Baselines may be nil: its route then answers 404 and
// the UI hides the "normal" badges. Slow may be nil (no "slower than your normal" line). UDP may be
// nil (no live state of the UDP targets, e.g. the Teams relay's region).
type Deps struct {
	Profile func() *model.Profile
	Store   store.Reader
	Status  func() Status
	Metrics http.Handler
	Verdict VerdictSource // the verdict engine (internal/verdict.Engine)
	Trace   TraceSource   // per-hop stats, routes and on-demand traces
	// Resolve looks up the IPv4 addresses of a host a viewer asks to trace
	// (POST /api/investigate?host=), to refuse non-public ones. Nil: the
	// system resolver (net.DefaultResolver.LookupNetIP "ip4"), as the
	// tracer resolves targets.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// Baselines answers each series' normal (baseline.Source).
	Baselines BaselineSource
	// Slow lists targets currently slower than their normal (the verdict
	// engine's baseline comparison), worst first.
	Slow func() []verdict.SlowTarget
	// UDP reports the live state of the UDP (STUN) targets: the relay
	// region from the name's CNAME and address-change epochs (the probe
	// runner: probe.Runner implements probe.UDPStateSource).
	UDP probe.UDPStateSource
	// Speed runs the speed tests (speedtest.Scheduler); nil: the speed
	// routes answer 404 and the UI hides Speed.
	Speed SpeedSource
	// Path is the current network path of this machine (netinfo.Watcher's
	// Current), including how it connects (Path.Conn); may be nil.
	Path func() netinfo.Path
	// Tiles is the Home tiles engine (tiles.Engine); may be nil.
	Tiles TilesSource
	// CustomProfile: the profile is a --config file (an off tile is turned
	// on with a tile: tag there, not with --profile).
	CustomProfile bool
	Log           *slog.Logger

	firsts *firstCache // set by the handlers; nil queries the store each time
}

// TilesSource is the Home tiles engine (tiles.Engine): the snapshot of the
// latest tick and a tile's 24-hour chart.
type TilesSource interface {
	Ready() bool
	Snapshot() *tiles.Snapshot
	Chart(id string) (*tiles.Chart, bool)
}

// Status is what /api/status reports.
type Status struct {
	Version string      `json:"version"`
	Started time.Time   `json:"started"`
	Caps    probe.Caps  `json:"caps"`
	Targets int         `json:"targets"` // targets in the profile
	Ready   int         `json:"ready"`   // targets that have reported at least one sample (see Warmup)
	Store   store.Stats `json:"store"`
}

// Warmup counts distinct targets that have reported a sample (lost or not).
// Put it in the sink fan-out and use Ready() for Status.Ready.
type Warmup struct {
	// Only, when set, limits counting to these target names. Path targets
	// (gateway, ISP edge) may never report, e.g. before the edge is found,
	// so they are left out of the warm-up count.
	Only map[string]bool

	mu   sync.Mutex
	seen map[string]struct{}
}

// Observe implements model.Sink.
func (w *Warmup) Observe(s model.Sample) {
	if s.Lost && s.Reason == model.ReasonGap {
		return
	}
	if w.Only != nil && !w.Only[s.Key.Target] {
		return
	}
	w.mu.Lock()
	if w.seen == nil {
		w.seen = map[string]struct{}{}
	}
	w.seen[s.Key.Target] = struct{}{}
	w.mu.Unlock()
}

// Ready returns how many distinct targets have reported.
func (w *Warmup) Ready() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.seen)
}

func (d *Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.New(slog.DiscardHandler)
}
