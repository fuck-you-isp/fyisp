package main

import (
	"context"
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

	mu     sync.Mutex
	t      tunnel.Tunnel
	cancel context.CancelFunc
	done   chan struct{}
}

func newShareControl(parent context.Context, origin, secret, proto string, log *slog.Logger) *shareControl {
	return &shareControl{parent: parent, origin: origin, secret: secret, proto: proto, log: log}
}

func (s *shareControl) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.t != nil {
		return nil
	}
	t := tunnel.New(tunnel.Options{OriginURL: s.origin, Protocol: s.proto, Log: s.log})
	ctx, cancel := context.WithCancel(s.parent)
	done := make(chan struct{})
	events, unsubscribe := t.Subscribe()
	go func() {
		defer close(done)
		defer unsubscribe()
		if err := t.Run(ctx); err != nil {
			s.log.Error("public link failed", "err", err)
		}
	}()
	go s.announce(events)
	s.t, s.cancel, s.done = t, cancel, done
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
	cancel, done := s.cancel, s.done
	s.t, s.cancel, s.done = nil, nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		s.log.Warn("public link did not stop within 10s")
	}
	s.log.Info("public link stopped")
	return nil
}

func (s *shareControl) State() web.ShareState {
	s.mu.Lock()
	t := s.t
	s.mu.Unlock()
	if t == nil {
		return web.ShareState{Phase: web.ShareOff}
	}
	st := t.State()
	out := web.ShareState{Protocol: st.Protocol, LastErr: st.LastErr}
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
