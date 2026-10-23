// Package tunnel publishes a local HTTP origin on a Cloudflare quick tunnel
// (https://<random>.trycloudflare.com) by running cloudflared's supervisor
// in-process. No cloudflared binary, account or config file is needed.
//
// Only one Tunnel may run at a time per process: cloudflared keeps
// process-wide state (Prometheus registrations, the connection observer), see
// session.go.
//
// The package imports nothing from fyisp, so it can move to a module of its
// own; an importer must then repeat cloudflared's go.mod replace directives
// (Go ignores them in dependencies).
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
// reported to Cloudflare's edge and to the quick tunnel service. Keep it and
// cloudflaredCommit in lockstep with go.mod (TestCloudflaredVersionMatchesGoMod
// checks both against the linked module).
const CloudflaredVersion = "2026.10.3"

// cloudflaredCommit is the commit go.mod pins for CloudflaredVersion.
const cloudflaredCommit = "de6376c5af4a"

// Options configure a tunnel.
type Options struct {
	// Origin is the local HTTP server to publish, e.g.
	// "http://127.0.0.1:3000". Required.
	Origin string
	// OnURL, if set, is called with each new public URL
	// ("https://<random>.trycloudflare.com") once its tunnel has connected.
	// A tunnel that stays down for 5 minutes is replaced by a new one with a
	// new URL. Called from cloudflared's event goroutine, never concurrently;
	// it must return promptly (a blocked OnURL holds up the tunnel's events
	// and its stop).
	OnURL func(url string)
	// Log receives the tunnel's state changes (Info) and cloudflared's own
	// logs, capped and redacted (see zlog.go): cloudflared info -> Debug,
	// warn and error -> Warn. Default: discard.
	Log *slog.Logger
}

// ErrAlreadyRunning is returned by Start when another tunnel is running in
// this process.
var ErrAlreadyRunning = errors.New("tunnel: another tunnel is already running in this process")

// Tunnel is a running quick tunnel. Its methods are safe for concurrent use.
type Tunnel struct {
	t    *tunnel
	done chan struct{}
}

// Start provisions a quick tunnel to o.Origin and keeps one connected until
// ctx ends: a lost connection is retried (QUIC, falling back to HTTP/2 when
// UDP is blocked), and a tunnel down for 5 minutes is replaced. It returns
// an error only for invalid options or ErrAlreadyRunning; the URL comes
// later (URL, or Options.OnURL).
func Start(ctx context.Context, o Options) (*Tunnel, error) {
	return start(ctx, o, settings{})
}

// URL is the public URL once its tunnel has connected; it stays while the
// tunnel reconnects. "" before that, while a replacement is provisioned,
// and after Done.
func (t *Tunnel) URL() string {
	t.t.mu.Lock()
	defer t.t.mu.Unlock()
	return t.t.announced
}

// Done is closed once the tunnel has stopped: ctx ended, and cloudflared has
// unregistered (or been cut after 5 s).
func (t *Tunnel) Done() <-chan struct{} { return t.done }

// Err is the last provisioning or connection error, nil while connected
// and after Done.
func (t *Tunnel) Err() error {
	st := t.t.getState()
	if st.LastErr == "" || st.Phase == phaseConnected {
		return nil
	}
	return errors.New(st.LastErr)
}

// settings hold the tunnel's internals; tests change them.
type settings struct {
	// Protocol is "auto" (default: QUIC, falling back to HTTP/2), "quic" or
	// "http2".
	Protocol string
	// QuickService is the quick tunnel API (default
	// https://api.trycloudflare.com).
	QuickService string
	// ReprovisionAfter re-provisions (new URL) after this long without an
	// active edge connection (default 5m).
	ReprovisionAfter time.Duration
	// FallbackAfter applies to Protocol "auto" only: after this long
	// without an edge connection, cloudflared is restarted with HTTP/2 for
	// the rest of the current quick tunnel (same URL). cloudflared's own
	// auto fallback takes minutes when UDP is silently dropped. Default 30s;
	// negative disables the fast fallback.
	FallbackAfter time.Duration
}

const (
	defaultQuickService     = "https://api.trycloudflare.com"
	defaultReprovisionAfter = 5 * time.Minute
	defaultFallbackAfter    = 30 * time.Second
)

func (c settings) withDefaults() settings {
	if c.Protocol == "" {
		c.Protocol = "auto"
	}
	if c.QuickService == "" {
		c.QuickService = defaultQuickService
	}
	if c.ReprovisionAfter <= 0 {
		c.ReprovisionAfter = defaultReprovisionAfter
	}
	if c.FallbackAfter == 0 {
		c.FallbackAfter = defaultFallbackAfter
	}
	return c
}

func validate(o Options, c settings) error {
	switch c.Protocol {
	case "auto", "quic", "http2":
	default:
		return fmt.Errorf("tunnel: unknown protocol %q (want auto, quic or http2)", c.Protocol)
	}
	u, err := url.Parse(o.Origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("tunnel: origin %q must be http(s)://host:port", o.Origin)
	}
	if u, err := url.Parse(c.QuickService); err != nil || u.Host == "" {
		return fmt.Errorf("tunnel: quick service URL %q is invalid", c.QuickService)
	}
	return nil
}

// phase is the coarse tunnel state.
type phase uint8

const (
	// phaseDisabled: not running.
	phaseDisabled phase = iota
	// phaseProvisioning: requesting a new quick tunnel (hostname + credentials).
	phaseProvisioning
	// phaseConnecting: provisioned (URL known) but no edge connection yet.
	phaseConnecting
	// phaseConnected: at least one edge connection is registered; URL is live.
	phaseConnected
	// phaseReconnecting: the edge connection was lost; cloudflared is retrying.
	phaseReconnecting
	// phaseFailed: provisioning failed; the loop retries after a backoff.
	phaseFailed
)

func (p phase) String() string {
	switch p {
	case phaseDisabled:
		return "disabled"
	case phaseProvisioning:
		return "provisioning"
	case phaseConnecting:
		return "connecting"
	case phaseConnected:
		return "connected"
	case phaseReconnecting:
		return "reconnecting"
	case phaseFailed:
		return "failed"
	}
	return fmt.Sprintf("phase(%d)", uint8(p))
}

// state is a snapshot of the tunnel.
type state struct {
	Phase    phase
	URL      string // https://<name>.trycloudflare.com once provisioned
	Protocol string // "quic" or "http2" while connected
	Location string // edge colo, e.g. "fra08", while connected
	LastErr  string
}

var running atomic.Bool

type tunnel struct {
	opts Options
	cfg  settings
	log  *slog.Logger

	// runSup runs one supervisor (runSupervisor; tests replace it).
	runSup func(ctx context.Context, qt *quickTunnel, w *watchdog, proto string) (stopReason, error)

	mu        sync.Mutex
	state     state
	announced string // the URL OnURL last heard, while it is live
}

func start(ctx context.Context, o Options, c settings) (*Tunnel, error) {
	c = c.withDefaults()
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if err := validate(o, c); err != nil {
		return nil, err
	}
	if !running.CompareAndSwap(false, true) {
		return nil, ErrAlreadyRunning
	}
	t := &tunnel{
		opts:  o,
		cfg:   c,
		log:   o.Log.With("component", "tunnel"),
		state: state{Phase: phaseDisabled},
	}
	t.runSup = t.runSupervisor
	T := &Tunnel{t: t, done: make(chan struct{})}
	go func() {
		defer close(T.done)
		defer running.Store(false)
		t.run(ctx)
	}()
	return T, nil
}

func (t *tunnel) getState() state {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

// update applies fn to the state under the lock and logs the result if
// anything changed. A newly connected URL goes to OnURL, outside the lock.
func (t *tunnel) update(fn func(s *state)) {
	t.mu.Lock()
	announce := t.updateLocked(fn)
	t.mu.Unlock()
	if announce != "" && t.opts.OnURL != nil {
		t.opts.OnURL(announce)
	}
}

func (t *tunnel) updateLocked(fn func(s *state)) (announce string) {
	old := t.state
	s := old
	fn(&s)
	if s == old {
		return ""
	}
	t.state = s
	switch {
	case s.Phase == phaseConnected && s.URL != t.announced:
		t.announced, announce = s.URL, s.URL
	case s.URL != t.announced && s.Phase != phaseReconnecting:
		t.announced = "" // a new tunnel, not connected yet, or stopped
	}
	if s.Phase != old.Phase || s.URL != old.URL || s.Protocol != old.Protocol {
		attrs := []any{"phase", s.Phase.String(), "url", s.URL}
		if s.Protocol != "" {
			attrs = append(attrs, "protocol", s.Protocol, "location", s.Location)
		}
		if s.LastErr != "" && s.Phase != phaseConnected {
			attrs = append(attrs, "err", s.LastErr)
		}
		t.log.Info("tunnel state", attrs...)
	}
	return announce
}

// run provisions and serves quick tunnels until ctx is cancelled.
func (t *tunnel) run(ctx context.Context) {
	activeLog.Store(t.opts.Log)
	setupProcessGlobals()
	defer t.update(func(s *state) { *s = state{Phase: phaseDisabled} })
	failures := 0
	for ctx.Err() == nil {
		t.update(func(s *state) { *s = state{Phase: phaseProvisioning, LastErr: s.LastErr} })
		qt, err := provision(ctx, t.cfg.QuickService)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			failures++
			wait := provisionBackoff(failures, err)
			msg := redact(err.Error()) // a network error quotes this machine's address
			t.update(func(s *state) { s.Phase, s.LastErr = phaseFailed, msg })
			t.log.Warn("quick tunnel provisioning failed", "err", msg, "retry_in", wait.Round(time.Second))
			if !sleepCtx(ctx, wait) {
				break
			}
			continue
		}
		failures = 0
		t.serveQuickTunnel(ctx, qt)
	}
}

// serveQuickTunnel keeps one provisioned quick tunnel connected, restarting
// cloudflared's supervisor when it gives up, until ctx is done or there has
// been no active connection for ReprovisionAfter.
func (t *tunnel) serveQuickTunnel(ctx context.Context, qt *quickTunnel) {
	t.update(func(s *state) {
		*s = state{Phase: phaseConnecting, URL: "https://" + qt.Hostname, LastErr: s.LastErr}
	})
	w := &watchdog{after: t.cfg.ReprovisionAfter, downSince: time.Now()}
	proto := t.cfg.Protocol
	for restarts := 0; ; restarts++ {
		if fallbackDue(proto, t.cfg.FallbackAfter, w.everUp(), w.active(), w.downFor()) {
			proto = "http2"
			t.log.Info("no tunnel connection yet, falling back to HTTP/2", "after", t.cfg.FallbackAfter)
		}
		why, err := t.runSup(ctx, qt, w, proto)
		switch why {
		case stopCtx:
			return
		case stopWatchdog:
			t.log.Warn("no tunnel connection, provisioning a new quick tunnel", "down_for", t.cfg.ReprovisionAfter)
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
			msg = redact(err.Error())
		}
		t.update(func(s *state) {
			s.Phase, s.Protocol, s.Location, s.LastErr = phaseReconnecting, "", "", msg
		})
		wait := min(10*time.Second<<min(restarts, 3), time.Minute)
		if left := w.left(); left < wait {
			wait = left
		}
		if !sleepCtx(ctx, wait) {
			return
		}
		if w.expired() {
			t.log.Warn("no tunnel connection, provisioning a new quick tunnel", "down_for", t.cfg.ReprovisionAfter)
			return
		}
	}
}

// watchdog tracks how long the current quick tunnel has had no connection.
// It spans supervisor restarts for the same quick tunnel.
type watchdog struct {
	mu        sync.Mutex
	after     time.Duration
	up        int  // active connections
	ever      bool // connected at least once
	downSince time.Time
}

func (w *watchdog) connected() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.up++
	w.ever = true
}

// everUp reports a connection since the watchdog started (this quick
// tunnel).
func (w *watchdog) everUp() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ever
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
// restarted with HTTP/2: only "auto" falls back, only while this quick tunnel
// has never connected (QUIC that worked once is not blocked: a later outage
// is the network's), and only once there has been no connection for at
// least after (<= 0 disables the fast fallback).
func fallbackDue(proto string, after time.Duration, ever bool, active int, downFor time.Duration) bool {
	return proto == "auto" && after > 0 && !ever && active == 0 && downFor >= after
}

func provisionBackoff(failures int, err error) time.Duration {
	d := min(5*time.Second<<min(failures-1, 6), 5*time.Minute)
	// +-20% jitter so many instances behind one NAT don't retry in lockstep.
	d = time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
	var rl *rateLimitedError
	if errors.As(err, &rl) && rl.retryAfter > 0 {
		// Never before the service's Retry-After; jitter on top of it.
		d = max(d, rl.retryAfter+time.Duration(0.2*rand.Float64()*float64(rl.retryAfter)))
	}
	return d
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
