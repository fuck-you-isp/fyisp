// Package tunnel publishes a local HTTP origin on a Cloudflare quick tunnel
// (https://<random>.trycloudflare.com) by running cloudflared's supervisor
// in-process. No cloudflared binary, account or config file is needed.
//
// Only one Tunnel may Run at a time per process: cloudflared keeps
// process-wide state (Prometheus registrations, the connection observer), see
// session.go.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// CloudflaredVersion is the cloudflared release linked into this binary. It is
// reported to Cloudflare's edge and to the quick tunnel service. Keep it in
// lockstep with go.mod (TestCloudflaredVersionMatchesGoMod checks this).
const CloudflaredVersion = "2026.9.3"

// cloudflaredCommit is the commit go.mod pins for CloudflaredVersion.
const cloudflaredCommit = "96d39adbc812"

// Phase is the coarse tunnel state shown to users.
type Phase uint8

const (
	// Disabled: Run is not running.
	Disabled Phase = iota
	// Provisioning: requesting a new quick tunnel (hostname + credentials).
	Provisioning
	// Connecting: provisioned (URL known) but no edge connection yet.
	Connecting
	// Connected: at least one edge connection is registered; URL is live.
	Connected
	// Reconnecting: the edge connection was lost; cloudflared is retrying.
	Reconnecting
	// Failed: provisioning failed; Run retries after a backoff (see LastErr).
	Failed
)

func (p Phase) String() string {
	switch p {
	case Disabled:
		return "disabled"
	case Provisioning:
		return "provisioning"
	case Connecting:
		return "connecting"
	case Connected:
		return "connected"
	case Reconnecting:
		return "reconnecting"
	case Failed:
		return "failed"
	}
	return fmt.Sprintf("phase(%d)", uint8(p))
}

// State is a snapshot of the tunnel.
type State struct {
	Phase    Phase
	URL      string // https://<name>.trycloudflare.com once provisioned
	Protocol string // "quic" or "http2" while Connected
	Location string // edge colo, e.g. "fra08", while Connected
	Since    time.Time
	LastErr  string
}

// Event is published to subscribers whenever State changes.
type Event struct {
	Time  time.Time
	State State
}

// Options configures a Tunnel. Zero values get defaults.
type Options struct {
	// OriginURL is the local HTTP server to publish, e.g. "http://127.0.0.1:3000".
	OriginURL string
	// Protocol is "auto" (default: QUIC, falling back to HTTP/2), "quic" or "http2".
	Protocol string
	// QuickService is the quick tunnel API (default https://api.trycloudflare.com).
	QuickService string
	// ReprovisionAfter re-provisions (new URL) after this long without an
	// active edge connection (default 5m).
	ReprovisionAfter time.Duration
	// FallbackAfter applies to Protocol "auto" only: after this long without
	// an edge connection, cloudflared is restarted with HTTP/2 for the rest of
	// the current quick tunnel (same URL). cloudflared's own auto fallback
	// takes minutes when UDP is silently dropped. Default 30s; negative
	// disables the fast fallback.
	FallbackAfter time.Duration
	// Log receives tunnel state changes (Info) and cloudflared's own logs:
	// cloudflared debug -> Debug-4, info -> Debug, warn and error -> Warn.
	// Default: discard.
	Log *slog.Logger
}

const (
	defaultQuickService     = "https://api.trycloudflare.com"
	defaultReprovisionAfter = 5 * time.Minute
	defaultFallbackAfter    = 30 * time.Second
)

func (o Options) withDefaults() Options {
	if o.Protocol == "" {
		o.Protocol = "auto"
	}
	if o.QuickService == "" {
		o.QuickService = defaultQuickService
	}
	if o.ReprovisionAfter <= 0 {
		o.ReprovisionAfter = defaultReprovisionAfter
	}
	if o.FallbackAfter == 0 {
		o.FallbackAfter = defaultFallbackAfter
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return o
}

func (o Options) validate() error {
	switch o.Protocol {
	case "auto", "quic", "http2":
	default:
		return fmt.Errorf("tunnel: unknown protocol %q (want auto, quic or http2)", o.Protocol)
	}
	u, err := url.Parse(o.OriginURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("tunnel: origin URL %q must be http(s)://host:port", o.OriginURL)
	}
	if u, err := url.Parse(o.QuickService); err != nil || u.Host == "" {
		return fmt.Errorf("tunnel: quick service URL %q is invalid", o.QuickService)
	}
	return nil
}

// Tunnel is a quick tunnel that keeps itself connected while Run runs.
type Tunnel interface {
	// Run provisions and serves the tunnel until ctx is cancelled, then shuts
	// down and returns nil. It returns an error only for invalid Options or if
	// another Tunnel is already running in this process.
	Run(ctx context.Context) error
	// State returns the current state.
	State() State
	// Subscribe returns a channel of state changes and a func to stop. The
	// channel is buffered; a slow reader loses intermediate events but always
	// receives the newest one.
	Subscribe() (<-chan Event, func())
}

// ErrAlreadyRunning is returned by Run when another Tunnel is running.
var ErrAlreadyRunning = errors.New("tunnel: another tunnel is already running in this process")

var running atomic.Bool

// New returns a Tunnel. Nothing happens until Run is called.
func New(o Options) Tunnel {
	o = o.withDefaults()
	return &tunnel{
		opts:  o,
		log:   o.Log.With("component", "tunnel"),
		state: State{Phase: Disabled, Since: time.Now()},
		subs:  map[int]chan Event{},
	}
}

type tunnel struct {
	opts Options
	log  *slog.Logger

	mu     sync.Mutex
	state  State
	subs   map[int]chan Event
	nextID int
}

func (t *tunnel) State() State {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

func (t *tunnel) Subscribe() (<-chan Event, func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.nextID
	t.nextID++
	ch := make(chan Event, 16)
	t.subs[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			delete(t.subs, id)
			close(ch)
		})
	}
}

// update applies fn to the state under the lock and publishes the result if
// anything changed. Since is bumped when the phase changes.
func (t *tunnel) update(fn func(s *State)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.updateLocked(fn)
}

func (t *tunnel) updateLocked(fn func(s *State)) {
	old := t.state
	s := old
	fn(&s)
	if s == old {
		return
	}
	now := time.Now()
	if s.Phase != old.Phase {
		s.Since = now
	}
	t.state = s
	if s.Phase != old.Phase || s.URL != old.URL || s.Protocol != old.Protocol {
		attrs := []any{"phase", s.Phase.String(), "url", s.URL}
		if s.Protocol != "" {
			attrs = append(attrs, "protocol", s.Protocol, "location", s.Location)
		}
		if s.LastErr != "" && s.Phase != Connected {
			attrs = append(attrs, "err", s.LastErr)
		}
		t.log.Info("tunnel state", attrs...)
	}
	ev := Event{Time: now, State: s}
	for _, ch := range t.subs {
		select {
		case ch <- ev:
		default: // full: drop the oldest so the newest state always arrives
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- ev:
			default:
			}
		}
	}
}

func (t *tunnel) Run(ctx context.Context) error {
	if err := t.opts.validate(); err != nil {
		return err
	}
	if !running.CompareAndSwap(false, true) {
		return ErrAlreadyRunning
	}
	defer running.Store(false)
	activeLog.Store(t.opts.Log)
	setupProcessGlobals()
	defer t.update(func(s *State) { *s = State{Phase: Disabled, LastErr: s.LastErr} })

	failures := 0
	for ctx.Err() == nil {
		t.update(func(s *State) { *s = State{Phase: Provisioning, LastErr: s.LastErr} })
		qt, err := provision(ctx, t.opts.QuickService)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			failures++
			wait := provisionBackoff(failures, err)
			t.update(func(s *State) { s.Phase, s.LastErr = Failed, err.Error() })
			t.log.Warn("quick tunnel provisioning failed", "err", err, "retry_in", wait.Round(time.Second))
			if !sleepCtx(ctx, wait) {
				break
			}
			continue
		}
		failures = 0
		t.serveQuickTunnel(ctx, qt)
	}
	return nil
}

// serveQuickTunnel keeps one provisioned quick tunnel connected, restarting
// cloudflared's supervisor when it gives up, until ctx is done or there has
// been no active connection for ReprovisionAfter.
func (t *tunnel) serveQuickTunnel(ctx context.Context, qt *quickTunnel) {
	t.update(func(s *State) {
		*s = State{Phase: Connecting, URL: "https://" + qt.Hostname, LastErr: s.LastErr}
	})
	w := &watchdog{after: t.opts.ReprovisionAfter, downSince: time.Now()}
	proto := t.opts.Protocol
	for restarts := 0; ; restarts++ {
		if fallbackDue(proto, t.opts.FallbackAfter, w.active(), w.downFor()) {
			proto = "http2"
			t.log.Info("no tunnel connection yet, falling back to HTTP/2", "after", t.opts.FallbackAfter)
		}
		why, err := t.runSupervisor(ctx, qt, w, proto)
		switch why {
		case stopCtx:
			return
		case stopWatchdog:
			t.log.Warn("no tunnel connection, provisioning a new quick tunnel", "down_for", t.opts.ReprovisionAfter)
			return
		case stopFallback:
			restarts = -1 // restart at once with HTTP/2
			continue
		}
		// The supervisor gave up on its own (e.g. initial connection retries
		// exhausted). Retry with the same credentials (same URL) until the
		// watchdog says it is time for a new tunnel.
		msg := "tunnel supervisor exited"
		if err != nil {
			msg = err.Error()
		}
		t.update(func(s *State) {
			s.Phase, s.Protocol, s.Location, s.LastErr = Reconnecting, "", "", msg
		})
		wait := min(10*time.Second<<min(restarts, 3), time.Minute)
		if left := w.left(); left < wait {
			wait = left
		}
		if !sleepCtx(ctx, wait) {
			return
		}
		if w.expired() {
			t.log.Warn("no tunnel connection, provisioning a new quick tunnel", "down_for", t.opts.ReprovisionAfter)
			return
		}
	}
}

// watchdog tracks how long the current quick tunnel has had no connection.
// It spans supervisor restarts for the same quick tunnel.
type watchdog struct {
	mu        sync.Mutex
	after     time.Duration
	up        int // active connections
	downSince time.Time
}

func (w *watchdog) connected() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.up++
}

func (w *watchdog) disconnected() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.up > 0 {
		w.up--
		if w.up == 0 {
			w.downSince = time.Now()
		}
	}
}

func (w *watchdog) active() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.up
}

func (w *watchdog) left() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.up > 0 {
		return w.after
	}
	return max(w.after-time.Since(w.downSince), 0)
}

func (w *watchdog) expired() bool { return w.left() <= 0 }

// downFor is how long there has been no active connection (0 while connected).
func (w *watchdog) downFor() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.up > 0 {
		return 0
	}
	return time.Since(w.downSince)
}

// fallbackDue reports whether a supervisor running protocol proto should be
// restarted with HTTP/2: only "auto" falls back, and only once there has been
// no connection for at least after (<= 0 disables the fast fallback).
func fallbackDue(proto string, after time.Duration, active int, downFor time.Duration) bool {
	return proto == "auto" && after > 0 && active == 0 && downFor >= after
}

func provisionBackoff(failures int, err error) time.Duration {
	d := min(5*time.Second<<min(failures-1, 6), 5*time.Minute)
	var rl *rateLimitedError
	if errors.As(err, &rl) && rl.retryAfter > d {
		d = rl.retryAfter
	}
	// +-20% jitter so many instances behind one NAT don't retry in lockstep.
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-tm.C:
		return true
	}
}
