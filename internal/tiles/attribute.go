package tiles

import (
	"slices"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// Who is the likely cause of a problem.
type Who string

const (
	WhoRouter    Who = "router"     // the home router (or the Wi-Fi)
	WhoOffline   Who = "offline"    // no network at all
	WhoProvider  Who = "provider"   // the internet provider
	WhoInternet  Who = "internet"   // beyond the provider
	WhoNames     Who = "names"      // name lookups (DNS)
	WhoApp       Who = "app"        // the app's (or its company's) side
	WhoCallsPath Who = "calls_path" // the path calls take, while everything else works
	WhoSpeedtest Who = "speedtest"  // a speed test fyisp was running
	WhoUnknown   Who = "unknown"
)

// Evidence codes (Cause.Evidence); the web package words them.
const (
	EvRouterDown    = "router_down"    // the router stopped answering
	EvRouterLossy   = "router_lossy"   // the router lost packets
	EvNoNetwork     = "no_network"     // no route at all
	EvProviderDrop  = "provider_drop"  // the router answered, the provider's side did not
	EvProviderLossy = "provider_lossy" // the router was clean, the provider's side lost packets
	EvBeyond        = "beyond"         // the provider answered, the internet beyond it did not
	EvDNS           = "dns"            // name lookups failed
	EvVendorOnly    = "vendor_only"    // only one company's apps were affected
	EvAppOnly       = "app_only"       // only this app was affected
	EvCallsPath     = "calls_path"     // only calls were affected
	EvSpeedtest     = "speedtest"      // during a fyisp speed test
)

// Cause is a problem's likely cause.
type Cause struct {
	Who      Who
	Vendor   string // the company, for WhoApp
	Evidence string // an evidence code
}

// Cause kinds: the buckets of the problem summaries.
const (
	KindProvider  = "provider"
	KindHome      = "home"
	KindApp       = "app"
	KindSpeedtest = "speedtest"
	KindOther     = "other"
)

// Kind is the cause's summary bucket: the summaries count provider, home
// and app; speedtest and other count in the total only.
func (c Cause) Kind() string {
	switch c.Who {
	case WhoRouter, WhoOffline:
		return KindHome
	case WhoProvider:
		return KindProvider
	case WhoApp:
		return KindApp
	case WhoSpeedtest:
		return KindSpeedtest
	}
	return KindOther
}

// speedOverlap reports whether [from, to] (to zero: now) overlaps a fyisp
// speed test window, extended by speedtest.MaskTail as the live mask is.
func speedOverlap(from, to time.Time, wins []speedtest.Window, now time.Time) bool {
	if to.IsZero() {
		to = now
	}
	return slices.ContainsFunc(wins, func(w speedtest.Window) bool {
		end := w.End
		if end.IsZero() {
			end = now // running
		}
		return !w.Start.After(to) && !end.Add(speedtest.MaskTail).Before(from)
	})
}

// layerEv is one path layer over an episode.
type layerEv struct {
	known   bool    // measured during the episode
	dropped bool    // lost everything for connDropNow or more during it
	loss    float64 // share of its checks lost
}

// layerEvidence sums a layer's series (gateway; edge; the anycast
// resolvers) over [from, to] (to zero: now). The layer dropped when every
// series with checks had a run of total loss of connDropNow or more in it.
func layerEvidence(ts []tail, from, to, now time.Time) layerEv {
	ep := window{from, to}
	var n, lost int
	ev := layerEv{dropped: true}
	for _, t := range ts {
		has, dropped := false, false
		for _, c := range t.sorted() {
			if c.at.Before(from) || c.at.After(ep.end(now)) {
				continue
			}
			has = true
			n++
			if c.lost {
				lost++
			}
		}
		if !has {
			continue
		}
		for _, r := range runs(t) {
			if r.est() >= connDropNow && r.window().overlaps(ep, now) {
				dropped = true
			}
		}
		ev.known = true
		ev.dropped = ev.dropped && dropped
	}
	if !ev.known {
		return layerEv{}
	}
	ev.loss = float64(lost) / float64(n)
	return ev
}

// tileEv is one covered tile over an episode.
type tileEv struct {
	id     string
	vendor string
	kind   profile.TileKind
	worst  State // the worst word or minute in the episode; None without data
}

// attrIn is what attribute weighs for one episode.
type attrIn struct {
	from, to  time.Time // to zero: ongoing
	now       time.Time
	incidents []model.Incident
	layers    [nLayers]layerEv
	tiles     []tileEv // covered tiles only
	speed     []speedtest.Window
}

// attribute finds an episode's likely cause, first match:
//
//  0. it overlaps a fyisp speed test: speedtest;
//  1. a verdict incident overlapping [from − causeSlack, to + causeSlack]:
//     lan → router, no_network → offline, isp → provider, upstream →
//     internet, dns → names (the earliest such incident);
//  2. the path layers: gateway lost everything → router; else the edge →
//     provider; else the anycast resolvers → internet; gateway loss ≥
//     layerLossy → router; gateway under layerClean and edge loss ≥
//     layerLossy → provider;
//  3. only one vendor's tiles were bad, and a tile of another vendor was
//     good: that app (app_only for one tile, vendor_only for several);
//  4. every covered call tile (two or more) was bad and nothing else: the
//     call path;
//  5. unknown.
func attribute(in attrIn) Cause {
	if speedOverlap(in.from, in.to, in.speed, in.now) {
		return Cause{Who: WhoSpeedtest, Evidence: EvSpeedtest}
	}
	ep := window{in.from.Add(-causeSlack), in.to}
	if !in.to.IsZero() {
		ep.to = in.to.Add(causeSlack)
	}
	gw, edge, ac := in.layers[layerGateway], in.layers[layerEdge], in.layers[layerAnycast]
	var inc *model.Incident
	for i, x := range in.incidents {
		switch x.Kind {
		case model.VerdictLAN, model.VerdictNoNetwork, model.VerdictISP, model.VerdictUpstream, model.VerdictDNS:
		default:
			continue
		}
		if (window{x.Start, x.End}).overlaps(ep, in.now) && (inc == nil || x.Start.Before(inc.Start)) {
			inc = &in.incidents[i]
		}
	}
	if inc != nil {
		full := inc.PeakLoss >= fullLoss
		switch inc.Kind {
		case model.VerdictLAN:
			if full || gw.dropped {
				return Cause{Who: WhoRouter, Evidence: EvRouterDown}
			}
			return Cause{Who: WhoRouter, Evidence: EvRouterLossy}
		case model.VerdictNoNetwork:
			return Cause{Who: WhoOffline, Evidence: EvNoNetwork}
		case model.VerdictISP:
			if full || edge.dropped {
				return Cause{Who: WhoProvider, Evidence: EvProviderDrop}
			}
			return Cause{Who: WhoProvider, Evidence: EvProviderLossy}
		case model.VerdictUpstream:
			return Cause{Who: WhoInternet, Evidence: EvBeyond}
		case model.VerdictDNS:
			return Cause{Who: WhoNames, Evidence: EvDNS}
		}
	}
	switch {
	case gw.dropped:
		return Cause{Who: WhoRouter, Evidence: EvRouterDown}
	case edge.dropped:
		return Cause{Who: WhoProvider, Evidence: EvProviderDrop}
	case ac.dropped:
		return Cause{Who: WhoInternet, Evidence: EvBeyond}
	case gw.loss >= layerLossy:
		return Cause{Who: WhoRouter, Evidence: EvRouterLossy}
	case gw.loss < layerClean && edge.loss >= layerLossy:
		return Cause{Who: WhoProvider, Evidence: EvProviderLossy}
	}
	var bad []tileEv
	vendors := map[string]bool{}
	for _, t := range in.tiles {
		if t.worst.Bad() {
			bad = append(bad, t)
			vendors[t.vendor] = true
		}
	}
	if len(bad) == 0 {
		return Cause{Who: WhoUnknown}
	}
	if len(vendors) == 1 && slices.ContainsFunc(in.tiles, func(t tileEv) bool { return t.vendor != bad[0].vendor && t.worst == Good }) {
		ev := EvVendorOnly
		if len(bad) == 1 {
			ev = EvAppOnly
		}
		return Cause{Who: WhoApp, Vendor: bad[0].vendor, Evidence: ev}
	}
	calls := 0
	for _, t := range in.tiles {
		if t.kind == profile.TileCall {
			calls++
		}
	}
	if calls >= 2 && len(bad) == calls && !slices.ContainsFunc(bad, func(t tileEv) bool { return t.kind != profile.TileCall }) {
		return Cause{Who: WhoCallsPath, Evidence: EvCallsPath}
	}
	return Cause{Who: WhoUnknown}
}
