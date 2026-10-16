package main

import (
	"context"
	"net/netip"
	"runtime"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo/hops"
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

func (w *webTrace) Hops(ctx context.Context, target string, mode model.TraceMode, from, to time.Time) ([]web.HopStat, error) {
	var route []netip.Addr // the store keeps ICMP routes only
	if mode != model.TraceICMP {
		if r, ok := w.tr.Route(target, mode); ok {
			route = r.Hops
		}
	}
	hs, err := w.st.HopsKind(ctx, target, mode.Kind(), route, from, to)
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
			Last: h.LastMs, StDev: h.StDevMs, Jmax: h.JmaxMs,
			LossContinues: h.LossContinues, RateLimited: h.RateLimited,
		}
		if h.Info != nil {
			o.Info = *h.Info
		}
		out = append(out, o)
	}
	return out, nil
}

func (w *webTrace) Route(ctx context.Context, target string, mode model.TraceMode) (model.Route, bool) {
	if r, ok := w.tr.Route(target, mode); ok || mode != model.TraceICMP {
		return r, ok
	}
	return w.st.Route(ctx, target)
}

func (w *webTrace) RouteChanges(ctx context.Context, from, to time.Time) ([]model.RouteChange, error) {
	return w.st.RouteChanges(ctx, from, to)
}

func (w *webTrace) Investigate(target string, mode model.TraceMode, rounds int) (web.Investigation, error) {
	return w.tr.Investigate(target, mode, rounds)
}

// Modes reports what this OS can trace with (hops.Supported).
func (w *webTrace) Modes() []web.TraceModeInfo { return traceModes(hops.Supported(), runtime.GOOS) }

func traceModes(s hops.Support, goos string) []web.TraceModeInfo {
	why := func(ok bool) string {
		switch {
		case ok:
			return ""
		case goos == "windows":
			return "windows_firewall"
		}
		return "os"
	}
	tcp := web.TraceModeInfo{Mode: "tcp", Port: model.TraceTCPPort, Available: s.TCP, Reason: why(s.TCP)}
	if s.TCPViaICMP {
		tcp.HopsVia = "icmp"
	}
	return []web.TraceModeInfo{{Mode: "icmp", Available: true}, tcp,
		{Mode: "udp", Port: model.TraceUDPPort, Available: s.UDP, Reason: why(s.UDP)}}
}

func (w *webTrace) Traced() []string { return w.traced }

// Interval is the always-on trace round interval (timeline bucket floor).
func (w *webTrace) Interval() time.Duration { return trace.DefaultInterval }

var (
	_ trace.Sink      = traceSinks(nil)
	_ web.TraceSource = (*webTrace)(nil)
)
