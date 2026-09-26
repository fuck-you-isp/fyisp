package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/tunnel"
	"github.com/fuck-you-isp/fyisp/internal/web"
)

// shareControl implements web.ShareControl over internal/tunnel. The tunnel
// runs until Stop (or process shutdown), independent of the Start caller.
type shareControl struct {
	parent context.Context
	origin string // loopback URL of the public handler
	secret string
	proto  string
	log    *slog.Logger

	newTunnel func(tunnel.Options) tunnel.Tunnel // tunnel.New; tests replace it
	stopWait  time.Duration                      // how long Stop waits for the tunnel

	mu   sync.Mutex
	run  *shareRun // the current run (nil: off)
	prev *shareRun // a stopped run that had not exited when Stop gave up
}

// shareRun is one Start..Stop of the tunnel.
type shareRun struct {
	t      tunnel.Tunnel
	cancel context.CancelFunc
	done   chan struct{} // closed when Run returned
	err    error         // Run's error; read after done
}

func (r *shareRun) exited() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func newShareControl(parent context.Context, origin, secret, proto string, log *slog.Logger) *shareControl {
	return &shareControl{parent: parent, origin: origin, secret: secret, proto: proto, log: log,
		newTunnel: tunnel.New, stopWait: 10 * time.Second}
}

// errShareStopping is returned by Start while the previous tunnel is still
// shutting down (its Stop timed out); starting another one now would fail.
var errShareStopping = errors.New("the previous public link is still shutting down; try again in a few seconds")

func (s *shareControl) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.run != nil && !s.run.exited() {
		s.mu.Unlock()
		return nil
	}
	prev := s.prev
	s.mu.Unlock()
	// Only one tunnel can run per process: wait (within the caller's
	// deadline) for one whose Stop timed out.
	if prev != nil {
		select {
		case <-prev.done:
		case <-ctx.Done():
			return errShareStopping
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prev == prev {
		s.prev = nil
	}
	if s.run != nil && !s.run.exited() {
		return nil // started concurrently
	}
	t := s.newTunnel(tunnel.Options{OriginURL: s.origin, Protocol: s.proto, Log: s.log})
	runCtx, cancel := context.WithCancel(s.parent)
	r := &shareRun{t: t, cancel: cancel, done: make(chan struct{})}
	events, unsubscribe := t.Subscribe()
	go func() {
		defer close(r.done)
		defer unsubscribe()
		if err := t.Run(runCtx); err != nil {
			r.err = err
			s.log.Error("public link failed", "err", err)
		}
	}()
	go s.announce(events)
	s.run = r
	s.log.Warn("public link starting: anyone with the link can view your charts (redacted: no LAN addresses, no settings)")
	return nil
}

// announce logs the public URL each time a new one goes live.
func (s *shareControl) announce(events <-chan tunnel.Event) {
	var last string
	for ev := range events {
		if ev.State.Phase == tunnel.Connected && ev.State.URL != last {
			last = ev.State.URL
			s.log.Info("public link ready", "url", s.publicURL(ev.State.URL), "protocol", ev.State.Protocol)
		}
	}
}

func (s *shareControl) Stop() error {
	s.mu.Lock()
	r := s.run
	s.run = nil
	s.mu.Unlock()
	if r == nil {
		return nil
	}
	r.cancel()
	select {
	case <-r.done:
		s.log.Info("public link stopped")
	case <-time.After(s.stopWait):
		s.log.Warn("public link did not stop within the timeout; it keeps shutting down in the background", "timeout", s.stopWait)
		s.mu.Lock()
		s.prev = r
		s.mu.Unlock()
	}
	return nil
}

func (s *shareControl) State() web.ShareState {
	s.mu.Lock()
	r := s.run
	s.mu.Unlock()
	if r == nil {
		return web.ShareState{Phase: web.ShareOff}
	}
	st := r.t.State()
	out := web.ShareState{Protocol: st.Protocol, LastErr: st.LastErr}
	if r.exited() {
		// Run returned without Stop: it failed to start (or the process is
		// shutting down).
		if r.err != nil {
			return web.ShareState{Phase: web.ShareError, LastErr: r.err.Error()}
		}
		return web.ShareState{Phase: web.ShareOff, LastErr: st.LastErr}
	}
	switch st.Phase {
	case tunnel.Connected:
		out.Phase = web.ShareConnected
	case tunnel.Reconnecting:
		out.Phase = web.ShareReconnected
	case tunnel.Failed:
		out.Phase = web.ShareError
	default:
		out.Phase = web.ShareStarting
	}
	if st.URL != "" {
		out.URL = s.publicURL(st.URL)
	}
	return out
}

func (s *shareControl) publicURL(base string) string { return base + "/s/" + s.secret + "/" }

var _ web.ShareControl = (*shareControl)(nil)
