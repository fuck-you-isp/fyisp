package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/tunnel"
	"github.com/fuck-you-isp/fyisp/internal/web"
)

// fakeTunnel's Run fails with err, or blocks until release is closed (it
// ignores ctx, like a tunnel that does not shut down in time).
type fakeTunnel struct {
	err     error
	release chan struct{}
	mu      sync.Mutex
	state   tunnel.State
}

func (f *fakeTunnel) Run(ctx context.Context) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	f.state = tunnel.State{Phase: tunnel.Connected, URL: "https://x.trycloudflare.com"}
	f.mu.Unlock()
	<-f.release
	f.mu.Lock()
	f.state = tunnel.State{Phase: tunnel.Disabled}
	f.mu.Unlock()
	return nil
}

func (f *fakeTunnel) State() tunnel.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeTunnel) Subscribe() (<-chan tunnel.Event, func()) {
	ch := make(chan tunnel.Event)
	var once sync.Once
	return ch, func() { once.Do(func() { close(ch) }) }
}

func newTestShare(ts ...*fakeTunnel) *shareControl {
	s := newShareControl(context.Background(), "http://127.0.0.1:1", "secret", "auto", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.stopWait = 50 * time.Millisecond
	s.newTunnel = func(tunnel.Options) tunnel.Tunnel {
		t := ts[0]
		ts = ts[1:]
		return t
	}
	return s
}

func waitPhase(t *testing.T, s *shareControl, want string) web.ShareState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := s.State()
		if st.Phase == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("share state %+v, want phase %q", st, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestShareRunErrorShown: when tunnel.Run fails (e.g. ErrAlreadyRunning),
// the state is an error with the reason, not "starting" forever; a later
// Start tries again.
func TestShareRunErrorShown(t *testing.T) {
	ok := &fakeTunnel{release: make(chan struct{})}
	s := newTestShare(&fakeTunnel{err: tunnel.ErrAlreadyRunning}, ok)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := waitPhase(t, s, web.ShareError)
	if st.LastErr != tunnel.ErrAlreadyRunning.Error() {
		t.Fatalf("last error %q", st.LastErr)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, s, web.ShareConnected)
	close(ok.release)
	s.Stop()
	waitPhase(t, s, web.ShareOff)
}

// TestShareStartAfterSlowStop: Stop gave up waiting for the tunnel; Start
// then waits for it within its deadline, or returns an error the UI shows,
// instead of starting a second tunnel that fails.
func TestShareStartAfterSlowStop(t *testing.T) {
	slow := &fakeTunnel{release: make(chan struct{})}
	next := &fakeTunnel{release: make(chan struct{})}
	defer close(next.release)
	s := newTestShare(slow, next)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, s, web.ShareConnected)
	s.Stop() // times out: slow ignores ctx
	if st := s.State(); st.Phase != web.ShareOff {
		t.Fatalf("after Stop: %+v", st)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := s.Start(ctx)
	cancel()
	if !errors.Is(err, errShareStopping) {
		t.Fatalf("Start while the old tunnel is still running: %v", err)
	}
	close(slow.release)
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, s, web.ShareConnected)
}
