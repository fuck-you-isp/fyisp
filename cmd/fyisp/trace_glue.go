package main

import (
	"context"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
	"github.com/fuck-you-isp/fyisp/internal/trace"
	"github.com/fuck-you-isp/fyisp/internal/web"
)

// traceSinks sends traceroute results to several trace sinks.
type traceSinks []trace.Sink

func (f traceSinks) Observe(s model.Sample) {
	for _, k := range f {
		k.Observe(s)
	}
}

func (f traceSinks) ObserveHop(i model.HopInfo) {
	for _, k := range f {
		k.ObserveHop(i)
	}
}

func (f traceSinks) ObserveRoute(r model.Route) {
	for _, k := range f {
		k.ObserveRoute(r)
	}
}

func (f traceSinks) ObserveRouteChange(c model.RouteChange) {
	for _, k := range f {
		k.ObserveRouteChange(c)
	}
}

// webTrace adapts the store's trace reads and the tracer to web.TraceSource.
type webTrace struct {
	st     store.TraceReader
	tr     trace.Tracer
	traced []string
}

func newWebTrace(st store.TraceReader, tr trace.Tracer, prof *model.Profile) *webTrace {
	w := &webTrace{st: st, tr: tr}
	for _, t := range prof.Targets {
		if t.Trace {
			w.traced = append(w.traced, t.Name)
		}
	}
	return w
}

func (w *webTrace) Hops(ctx context.Context, target string, from, to time.Time) ([]web.HopStat, error) {
	hs, err := w.st.Hops(ctx, target, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]web.HopStat, 0, len(hs))
	for _, h := range hs {
		ip := h.IP
		if !ip.IsValid() {
			ip = h.MostCommonIP
		}
		o := web.HopStat{
			Hop: h.Hop, IP: ip, N: int(h.N), Lost: int(h.Lost), Loss: h.Loss,
			Min: h.MinMs, Mean: h.MeanMs, Max: h.MaxMs, P95: h.P95Ms, Jitter: h.JitterMs,
			LossContinues: h.LossContinues,
		}
		if h.Info != nil {
			o.Info = *h.Info
		}
		out = append(out, o)
	}
	return out, nil
}

func (w *webTrace) Route(ctx context.Context, target string) (model.Route, bool) {
	for _, r := range w.tr.Routes() {
		if r.Target == target {
			return r, true
		}
	}
	return w.st.Route(ctx, target)
}

func (w *webTrace) RouteChanges(ctx context.Context, from, to time.Time) ([]model.RouteChange, error) {
	return w.st.RouteChanges(ctx, from, to)
}

func (w *webTrace) Investigate(target string, ttl time.Duration) (func(), error) {
	return w.tr.Investigate(target, ttl)
}

func (w *webTrace) Traced() []string { return w.traced }

// Interval is the always-on trace round interval (timeline bucket floor).
func (w *webTrace) Interval() time.Duration { return trace.DefaultInterval }

var (
	_ trace.Sink      = traceSinks(nil)
	_ web.TraceSource = (*webTrace)(nil)
)
