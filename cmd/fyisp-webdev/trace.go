package main

import (
	"context"
	"hash/fnv"
	"math"
	mrand "math/rand/v2"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
	"github.com/fuck-you-isp/fyisp/internal/web"
)

// fakeTrace implements web.TraceSource over store.Fake. Every target gets a
// realistic 12-hop route: the home router, the ISP's private CMTS hop, five
// ISP hops in one ASN (AS7922), an IX (or, before a route change, Lumen
// transit), then the destination's cloud. Hop 5 drops ~25% of replies all
// the time (ICMP rate limiting: the loss does not continue), and every 20
// minutes, plus during the backfilled ISP outage, hops 3.. lose packets all
// the way to the destination (real loss). Google-Meet's route changed 12
// minutes before start, Cloudflare-anycast's during the backfill.
type fakeTrace struct {
	st    *store.Fake
	start time.Time
	iv    time.Duration
	isp   window // backfilled ISP outage: heavy real loss
	hole  window // fyisp "not running": no trace rounds

	mu      sync.Mutex
	invest  map[string]time.Time
	changes []model.RouteChange
	info    map[netip.Addr]model.HopInfo
}

var tracedTargets = []string{"Cloudflare-anycast", "Google-anycast", "Quad9-anycast", "Google-Meet", "AWS-us-east-1", "Hetzner-Falkenstein", "dev-GitHub"}

type fhop struct {
	ip    string
	rdns  string
	asn   uint32
	owner string
}

// ISP part (hops 1-7), identical for every target.
var ispHops = []fhop{
	{"192.168.1.1", "_gateway", 0, ""},
	{"10.94.160.1", "", 0, ""},
	{"198.51.100.1", "edge1.city.isp.example", 64500, "EXAMPLE-ISP"},
	{"198.51.100.5", "agg1.city.isp.example", 64500, "EXAMPLE-ISP"},
	{"198.51.100.9", "agg2.region.isp.example", 64500, "EXAMPLE-ISP"},
	{"198.51.100.13", "core1.region.isp.example", 64500, "EXAMPLE-ISP"},
	{"198.51.100.17", "pe1.border.isp.example", 64500, "EXAMPLE-ISP"},
}

var ixHop = fhop{"192.0.2.33", "ix.exchange.example", 0, "Example IX"}
var transitHops = []fhop{
	{"203.0.113.65", "edge1.transit.example", 64501, "EXAMPLE-TRANSIT"},
	{"203.0.113.66", "edge2.transit.example", 64501, "EXAMPLE-TRANSIT"},
}

// cloud describes the last hops (9-12 via the IX; 10-12 via transit).
type cloud struct {
	asn   uint32
	owner string
	hops  []string // 3 core hops then the destination
	rdns  string   // destination rDNS
	rtt   float64  // destination RTT, ms
}

var clouds = map[string]cloud{
	"Cloudflare-anycast":  {13335, "CLOUDFLARENET", []string{"172.71.144.3", "162.158.60.1", "172.68.188.2", "1.1.1.1"}, "one.one.one.one", 14},
	"Google-anycast":      {15169, "GOOGLE", []string{"108.170.242.225", "142.251.65.143", "108.170.243.1", "8.8.8.8"}, "dns.google", 15},
	"Quad9-anycast":       {19281, "QUAD9-AS-1", []string{"74.63.18.10", "74.63.18.66", "74.63.18.90", "9.9.9.9"}, "dns9.quad9.net", 17},
	"Google-Meet":         {64510, "EXAMPLE-CLOUD-A", []string{"192.0.2.101", "192.0.2.102", "192.0.2.103", "192.0.2.104"}, "meet.cloud-a.example", 16},
	"AWS-us-east-1":       {64511, "EXAMPLE-CLOUD-B", []string{"192.0.2.111", "192.0.2.112", "192.0.2.113", "192.0.2.114"}, "", 71},
	"Hetzner-Falkenstein": {64502, "EXAMPLE-HOSTING", []string{"192.0.2.121", "192.0.2.122", "192.0.2.123", "192.0.2.124"}, "server.hosting.example", 158},
	"dev-GitHub":          {64503, "EXAMPLE-CODEHOST", []string{"192.0.2.131", "192.0.2.132", "192.0.2.133", "192.0.2.134"}, "lb.code-host.example", 68},
}

func cloudFor(target string) cloud {
	if c, ok := clouds[target]; ok {
		return c
	}
	h := fnv.New32a()
	h.Write([]byte(target))
	x := h.Sum32()
	b := [2]byte{byte(x), byte(x >> 8)}
	return cloud{64511, "EXAMPLE-CLOUD-B", []string{"192.0.2.141", "192.0.2.142",
		netip.AddrFrom4([4]byte{203, 0, 113, b[0]}).String(), netip.AddrFrom4([4]byte{198, 51, 100, b[1] | 1}).String()}, "", 20 + float64(x%150)}
}

func newFakeTrace(st *store.Fake, start time.Time, backfill time.Duration) *fakeTrace {
	f := &fakeTrace{st: st, start: start, iv: 10 * time.Second, invest: map[string]time.Time{}, info: map[netip.Addr]model.HopInfo{}}
	f.hole, f.isp, _ = windows(backfill, start)
	add := func(h fhop) {
		a := netip.MustParseAddr(h.ip)
		f.info[a] = model.HopInfo{IP: a, RDNS: h.rdns, ASN: h.asn, Owner: h.owner, FirstSeen: start.Add(-backfill), LastSeen: start}
	}
	for _, h := range ispHops {
		add(h)
	}
	add(ixHop)
	for _, h := range transitHops {
		add(h)
	}
	for _, n := range tracedTargets {
		c := cloudFor(n)
		for i, ip := range c.hops {
			r := ""
			if i == len(c.hops)-1 {
				r = c.rdns
			}
			add(fhop{ip, r, c.asn, c.owner})
		}
	}
	chg := func(id int64, target string, at time.Time) {
		f.changes = append(f.changes, model.RouteChange{ID: id, Target: target, At: at,
			From: f.routeVia(target, false), To: f.routeVia(target, true), FirstDiff: 8})
	}
	if backfill > 0 {
		chg(1, "Cloudflare-anycast", start.Add(-backfill*4/10))
	}
	chg(2, "Google-Meet", start.Add(-12*time.Minute))
	if backfill > 0 {
		for _, n := range tracedTargets {
			f.write(n, start.Add(-backfill), start)
		}
	}
	return f
}

// routeVia returns the 12-hop route through the IX (ix) or Lumen transit.
func (f *fakeTrace) routeVia(target string, ix bool) []netip.Addr {
	var out []netip.Addr
	for _, h := range ispHops {
		out = append(out, netip.MustParseAddr(h.ip))
	}
	c := cloudFor(target)
	if ix {
		out = append(out, netip.MustParseAddr(ixHop.ip))
		for _, ip := range c.hops {
			out = append(out, netip.MustParseAddr(ip))
		}
	} else {
		for _, h := range transitHops {
			out = append(out, netip.MustParseAddr(h.ip))
		}
		for _, ip := range c.hops[1:] {
			out = append(out, netip.MustParseAddr(ip))
		}
	}
	return out
}

func (f *fakeTrace) routeAt(target string, t time.Time) ([]netip.Addr, time.Time) {
	since := time.Time{}
	for _, c := range f.changes {
		if c.Target == target {
			if t.Before(c.At) {
				return c.From, since
			}
			return c.To, c.At
		}
	}
	return f.routeVia(target, true), since
}

// realLoss reports the loss fraction that starts at hop 3 and continues to
// the destination at t.
func (f *fakeTrace) realLoss(t time.Time) float64 {
	if f.isp.has(t) {
		return 0.35
	}
	if t.Unix()%1200 < 180 { // 3 minutes every 20
		return 0.12
	}
	return 0.002
}

// write stores one trace round per interval for target in [from, to).
func (f *fakeTrace) write(target string, from, to time.Time) {
	h := fnv.New64a()
	h.Write([]byte(target))
	r := mrand.New(mrand.NewPCG(h.Sum64(), uint64(from.UnixNano())))
	c := cloudFor(target)
	for ts := from.Truncate(f.iv); ts.Before(to); ts = ts.Add(f.iv) {
		if f.hole.has(ts) {
			continue
		}
		route, _ := f.routeAt(target, ts)
		real := f.realLoss(ts)
		for i := range route {
			hop := i + 1
			s := model.Sample{Key: model.SeriesKey{Target: target, Kind: model.KindTrace, Hop: uint8(hop)}, Slot: ts}
			var base, lossP float64
			switch {
			case hop == 1:
				base, lossP = 1.2, 0.001
			case hop == 2:
				base, lossP = 8.5, 0.002
			case hop <= 7:
				base = 9 + float64(hop-3)*1.1
				lossP = real
			default:
				// hops 8..12 ramp towards the destination RTT
				k := float64(hop-8) / 4
				base = 13.5 + (c.rtt-13.5)*math.Pow(k, 1.5)
				lossP = real
			}
			if hop == 5 {
				// The router's control plane: slow, jittery and rate-limited
				// ICMP replies, while forwarding is fine.
				lossP = 1 - (1-lossP)*(1-0.25)
				base += 14 + 10*r.Float64()
			}
			if real > 0.1 && hop >= 3 {
				base *= 1.6 // queueing during the outage
			}
			if r.Float64() < lossP {
				s.Lost, s.Reason = true, model.ReasonTimeout
			} else {
				ms := base*(1+0.05*math.Sin(float64(ts.Unix())/700+float64(hop))) + r.ExpFloat64()*0.3*math.Sqrt(base)
				s.RTT = time.Duration(ms * float64(time.Millisecond))
			}
			f.st.Observe(s)
		}
	}
}

// run writes live rounds for traced and investigated targets.
func (f *fakeTrace) run(ctx context.Context) {
	last := f.start
	t := time.NewTicker(f.iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			f.mu.Lock()
			targets := append([]string(nil), tracedTargets...)
			for n, until := range f.invest {
				if now.After(until) {
					delete(f.invest, n)
					continue
				}
				if !contains(tracedTargets, n) {
					targets = append(targets, n)
				}
			}
			f.mu.Unlock()
			for _, n := range targets {
				f.write(n, last, now)
			}
			last = now.Truncate(f.iv)
		}
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func (f *fakeTrace) Interval() time.Duration { return f.iv }
func (f *fakeTrace) Traced() []string        { return append([]string(nil), tracedTargets...) }

func (f *fakeTrace) Investigate(target string, ttl time.Duration) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	until := time.Now().Add(ttl)
	if until.After(f.invest[target]) {
		f.invest[target] = until
	}
	return func() {}, nil
}

func (f *fakeTrace) Route(_ context.Context, target string) (model.Route, bool) {
	f.mu.Lock()
	_, inv := f.invest[target]
	f.mu.Unlock()
	if !contains(tracedTargets, target) && !inv {
		return model.Route{}, false
	}
	hops, since := f.routeAt(target, time.Now())
	if since.IsZero() {
		since = f.start
	}
	return model.Route{Target: target, Hops: hops, Since: since}, true
}

func (f *fakeTrace) RouteChanges(_ context.Context, from, to time.Time) ([]model.RouteChange, error) {
	var out []model.RouteChange
	for _, c := range f.changes {
		if !c.At.Before(from) && c.At.Before(to) {
			out = append(out, c)
		}
	}
	return out, nil
}

// Hops computes per-(hop, address) statistics from the stored samples, as
// the real store does.
func (f *fakeTrace) Hops(ctx context.Context, target string, from, to time.Time) ([]web.HopStat, error) {
	keys := make([]model.SeriesKey, 16)
	for i := range keys {
		keys[i] = model.SeriesKey{Target: target, Kind: model.KindTrace, Hop: uint8(i + 1)}
	}
	type acc struct {
		hop      int
		ip       netip.Addr
		n, lost  int
		rtts     []float64
		prev     float64
		jit      float64
		jn       int
		min, max float64
		sum      float64
	}
	accs := map[[2]any]*acc{}
	hopN, hopLost := map[int]int{}, map[int]int{}
	err := f.st.Raw(ctx, keys, from, to, func(p store.RawPoint) error {
		if p.Lost && p.Reason == model.ReasonGap {
			return nil
		}
		route, _ := f.routeAt(target, p.TS)
		hop := int(p.Key.Hop)
		var ip netip.Addr
		if hop-1 < len(route) {
			ip = route[hop-1]
		}
		k := [2]any{hop, ip}
		a := accs[k]
		if a == nil {
			a = &acc{hop: hop, ip: ip, min: math.Inf(1), max: math.Inf(-1), prev: math.NaN()}
			accs[k] = a
		}
		if p.Lost {
			a.lost++
			hopLost[hop]++
			return nil
		}
		hopN[hop]++
		a.n++
		a.sum += p.RTTms
		a.min, a.max = min(a.min, p.RTTms), max(a.max, p.RTTms)
		a.rtts = append(a.rtts, p.RTTms)
		if !math.IsNaN(a.prev) {
			a.jit += math.Abs(p.RTTms - a.prev)
			a.jn++
		}
		a.prev = p.RTTms
		return nil
	})
	if err != nil {
		return nil, err
	}
	maxHop := 0
	for h := range hopN {
		maxHop = max(maxHop, h)
	}
	for h := range hopLost {
		maxHop = max(maxHop, h)
	}
	lossOf := func(h int) float64 {
		if t := hopN[h] + hopLost[h]; t > 0 {
			return float64(hopLost[h]) / float64(t)
		}
		return math.NaN()
	}
	var out []web.HopStat
	for _, a := range accs {
		hs := web.HopStat{Hop: a.hop, IP: a.ip, Info: f.info[a.ip], N: a.n, Lost: a.lost,
			Min: math.NaN(), Mean: math.NaN(), Max: math.NaN(), P95: math.NaN(), Jitter: math.NaN()}
		if t := a.n + a.lost; t > 0 {
			hs.Loss = float64(a.lost) / float64(t)
		}
		if a.n > 0 {
			sort.Float64s(a.rtts)
			hs.Min, hs.Max, hs.Mean = a.min, a.max, a.sum/float64(a.n)
			hs.P95 = a.rtts[min(len(a.rtts)-1, int(math.Ceil(0.95*float64(len(a.rtts))))-1)]
			if a.jn > 0 {
				hs.Jitter = a.jit / float64(a.jn)
			}
		}
		// Real loss continues to the destination; hop 5's extra loss is the
		// router rate-limiting its replies. (The fake knows its own model;
		// the real store compares hops.)
		if l := lossOf(a.hop); l > 0.005 {
			hs.LossContinues = a.hop != 5 || l < 0.1
		}
		out = append(out, hs)
	}
	return out, nil
}
