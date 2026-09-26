package netinfo

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"time"
)

// Defaults for Options.
const (
	DefaultRecheck     = 15 * time.Minute
	DefaultPollGateway = 10 * time.Second
	// retryUnknown re-runs edge discovery sooner while the edge is unknown.
	retryUnknown = time.Minute
)

// DefaultProbe is the default traceroute destination.
var DefaultProbe = netip.MustParseAddr("1.1.1.1")

// New returns a Watcher for this machine. Nothing happens until Run.
func New(o Options) Watcher {
	return newWatcher(o, defaultRoute, openTracer)
}

func newWatcher(o Options, rt func() (route, error), tr func() (tracer, error)) *watcher {
	if o.Recheck <= 0 {
		o.Recheck = DefaultRecheck
	}
	if o.PollGateway <= 0 {
		o.PollGateway = DefaultPollGateway
	}
	if !o.Probe.IsValid() {
		o.Probe = DefaultProbe
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &watcher{
		o: o, route: rt, tracer: tr, now: time.Now, retry: retryUnknown,
		kick: make(chan struct{}, 1), subs: map[chan Path]struct{}{},
	}
}

type watcher struct {
	o      Options
	route  func() (route, error)
	tracer func() (tracer, error)
	now    func() time.Time
	retry  time.Duration
	kick   chan struct{} // run edge discovery now

	mu      sync.Mutex
	cur     Path
	gwErr   string
	edgeErr string
	lastGW  netip.Addr // last known gateway (kept while there is no route)
	hadGW   bool       // the previous poll found a default route
	subs    map[chan Path]struct{}
}

func (w *watcher) Current() Path {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cur
}

// Subscribe returns a channel that receives the Path after every change of
// Gateway, Iface or Edge. It holds only the latest Path: a slow reader
// skips intermediate ones. The returned func unsubscribes and closes it.
func (w *watcher) Subscribe() (<-chan Path, func()) {
	ch := make(chan Path, 1)
	w.mu.Lock()
	w.subs[ch] = struct{}{}
	w.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			w.mu.Lock()
			delete(w.subs, ch)
			close(ch)
			w.mu.Unlock()
		})
	}
}

// Run polls the gateway and rediscovers the edge until ctx is done.
func (w *watcher) Run(ctx context.Context) error {
	w.pollGateway()
	w.trigger()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); w.edgeLoop(ctx) }()
	t := time.NewTicker(w.o.PollGateway)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case <-t.C:
			w.pollGateway()
		}
	}
}

func (w *watcher) trigger() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// pollGateway reads the default route. A new gateway forgets the edge (it
// belongs to another network) and triggers edge discovery; losing the route
// keeps the last edge, so that probes of it report the outage.
func (w *watcher) pollGateway() {
	r, err := w.route()
	w.mu.Lock()
	defer w.mu.Unlock()
	prev := w.cur
	next := w.cur
	next.Updated = w.now()
	kick := false
	switch {
	case errors.Is(err, errNoRoute):
		next.Gateway, next.Iface = netip.Addr{}, ""
		w.gwErr = ErrNoRoute
		w.hadGW = false
	case err != nil:
		next.Gateway, next.Iface = netip.Addr{}, ""
		w.gwErr = "gateway discovery: " + err.Error()
		w.hadGW = false
	default:
		next.Gateway, next.Iface = r.Gateway, r.Iface
		w.gwErr = ""
		if r.Gateway.IsValid() && w.lastGW.IsValid() && r.Gateway != w.lastGW {
			next.Edge, next.EdgeHop = netip.Addr{}, 0
			w.edgeErr = ""
		}
		kick = !w.hadGW || r.Gateway != prev.Gateway || r.Iface != prev.Iface
		if r.Gateway.IsValid() {
			w.lastGW = r.Gateway
		}
		w.hadGW = true
	}
	next.Err = w.errLocked()
	w.setLocked(prev, next)
	if kick {
		w.trigger()
	}
}

func (w *watcher) errLocked() string {
	if w.gwErr != "" {
		return w.gwErr
	}
	return w.edgeErr
}

// setLocked stores next and notifies subscribers if the path changed.
func (w *watcher) setLocked(prev, next Path) {
	w.cur = next
	if prev.Gateway == next.Gateway && prev.Iface == next.Iface && prev.Edge == next.Edge {
		return
	}
	w.o.Log.Info("network path", "gateway", next.Gateway, "iface", next.Iface, "edge", next.Edge, "edge_hop", next.EdgeHop)
	for ch := range w.subs {
		select {
		case <-ch: // drop the stale value
		default:
		}
		select {
		case ch <- next:
		default:
		}
	}
}

func (w *watcher) edgeLoop(ctx context.Context) {
	t := time.NewTimer(w.o.Recheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.kick:
		case <-t.C:
		}
		w.discover(ctx)
		if ctx.Err() != nil {
			return
		}
		next := w.o.Recheck
		if !w.Current().Edge.IsValid() {
			next = min(next, w.retry)
		}
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(next)
	}
}

// discover runs one edge discovery. Without a route it does nothing (a
// kick follows when the route returns). A failed discovery keeps the last
// edge: during an outage the edge series must keep reporting loss.
func (w *watcher) discover(ctx context.Context) {
	start := w.Current()
	if start.NoRoute() {
		return
	}
	edge, n, err := w.trace(ctx)
	if ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur.Gateway != start.Gateway || w.cur.Iface != start.Iface {
		return // the network changed meanwhile; a new discovery is queued
	}
	prev, next := w.cur, w.cur
	next.Updated = w.now()
	if err != nil {
		if w.edgeErr == "" || prev.Edge.IsValid() {
			w.o.Log.Debug("edge discovery failed", "err", err, "keeping", prev.Edge)
		}
		w.edgeErr = "edge discovery: " + err.Error()
	} else {
		next.Edge, next.EdgeHop = edge, n
		w.edgeErr = ""
	}
	next.Err = w.errLocked()
	w.setLocked(prev, next)
}

func (w *watcher) trace(ctx context.Context) (netip.Addr, int, error) {
	tr, err := w.tracer()
	if err != nil {
		return netip.Addr{}, 0, err
	}
	defer tr.Close()
	return discoverEdge(ctx, tr, w.o.Probe)
}
