// Package web serves the fyisp dashboard: an embedded static UI plus a small
// read-only JSON/CSV API over store.Reader.
//
// Two handlers share the same API code:
//
//   - Local is the owner's listener (default 127.0.0.1:3000). It adds /metrics
//     and the POST routes that start and stop the public share link, protected
//     by a Host allowlist, a same-origin check and a per-process CSRF token.
//   - Public is the tunnel's origin. Everything lives under /s/<secret>/, only
//     allowlisted GET/HEAD routes exist, requests are rate limited, panel
//     queries are bounded and cached, and responses are redacted (no version,
//     paths, LAN addresses or error strings).
//
// The caller owns the http.Server. Recommended settings for both listeners:
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
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/probe"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// Deps is everything the handlers need. Profile, Store and Status are
// required; Metrics and Share may be nil (the routes then answer 404).
// Verdict may be nil: the verdict banner and outage log are then hidden.
type Deps struct {
	Profile func() *model.Profile
	Store   store.Reader
	Status  func() Status
	Metrics http.Handler
	Share   ShareControl
	Verdict VerdictSource // the verdict engine (internal/verdict.Engine)
	Log     *slog.Logger

	firsts *firstCache // set by the handlers; nil queries the store each time
}

// Status is what /api/status reports. The public handler redacts it (see
// publicStatus): Version, the ICMP hint, share errors and store errors are
// never served publicly.
type Status struct {
	Version string      `json:"version"`
	Started time.Time   `json:"started"`
	Caps    probe.Caps  `json:"caps"`
	Targets int         `json:"targets"` // targets in the profile
	Ready   int         `json:"ready"`   // targets that have reported at least one sample (see Warmup)
	Share   ShareState  `json:"share"`
	Store   store.Stats `json:"store"`
}

// Share phases used by the UI. Anything else is shown verbatim.
const (
	ShareOff         = "off"
	ShareStarting    = "starting"
	ShareConnected   = "connected"
	ShareReconnected = "reconnecting"
	ShareError       = "error"
)

// ShareState is the public link's state as the UI needs it. URL is the full
// public URL including the /s/<secret>/ path.
type ShareState struct {
	Phase    string `json:"phase"`
	URL      string `json:"url,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	LastErr  string `json:"last_err,omitempty"`
}

// ShareControl starts and stops the public link (implemented over
// internal/tunnel by the caller). Start must return promptly: the tunnel keeps
// running in the background until Stop, independent of ctx, which only bounds
// the Start call itself. Start and Stop must be idempotent.
type ShareControl interface {
	State() ShareState
	Start(ctx context.Context) error
	Stop() error
}

// Warmup counts distinct targets that have reported a sample (lost or not).
// Put it in the sink fan-out and use Ready() for Status.Ready.
type Warmup struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// Observe implements model.Sink.
func (w *Warmup) Observe(s model.Sample) {
	if s.Lost && s.Reason == model.ReasonGap {
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
