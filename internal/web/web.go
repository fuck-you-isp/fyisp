// Package web serves the fyisp dashboard: an embedded static UI plus a small
// read-only JSON/CSV API over store.Reader.
//
// Local is the only handler: the owner's listener (default
// 127.0.0.1:3000). It adds /metrics and the write routes (on-demand traces,
// speed tests), protected by a Host allowlist, a same-origin check, a
// per-process CSRF token and, where the dashboard is reachable from
// elsewhere (--listen beyond loopback, --allow-host), an admin token. There
// is no public listener (v0.8.0 removed the share link) and no reports
// (v0.10.0 removed them): live access is the owner's own tunnel or proxy.
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
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/probe"
	"github.com/fuck-you-isp/fyisp/internal/store"
	"github.com/fuck-you-isp/fyisp/internal/tiles"
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
	// Baselines answers each series' normal (baseline.Source).
	Baselines BaselineSource
	// Slow lists targets currently slower than their normal (the verdict
	// engine's baseline comparison), worst first.
	Slow func() []SlowTarget
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
// latest tick, a tile's 24-hour chart and the problems of the last week.
type TilesSource interface {
	Ready() bool
	Snapshot() *tiles.Snapshot
	Chart(id string) (*tiles.Chart, bool)
	Problems(from time.Time) []tiles.Episode
	LateSamples() int // samples that arrived too late to count
}

// Status is what /api/status reports.
type Status struct {
	Version string      `json:"version"`
	Started time.Time   `json:"started"`
	Caps    probe.Caps  `json:"caps"`
	Targets int         `json:"targets"` // targets in the profile
	Ready   int         `json:"ready"`   // targets that have reported at least one sample (see Warmup)
	Store   store.Stats `json:"store"`
	// SpeedMasked is the share of the last 24 hours under speed test
	// (probes then are stored as not measured); nil without speed tests.
	SpeedMasked *float64 `json:"speed_masked_share_24h,omitempty"`
	// TilesLate counts the samples the Home tiles engine ignored because
	// they arrived too late (TilesSource.LateSamples); nil without it.
	TilesLate *int `json:"tiles_late_samples,omitempty"`
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
