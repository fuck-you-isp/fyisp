package main

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/asn"
	"github.com/fuck-you-isp/fyisp/internal/metrics"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/netinfo/hops"
	"github.com/fuck-you-isp/fyisp/internal/s3test"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/fuck-you-isp/fyisp/internal/storage"
	"github.com/fuck-you-isp/fyisp/internal/trace"
)

// newStorage opens the storage-test targets in dir and the campaigns that
// run rounds against them in the speed-test queue q, stored in st. A
// targets file fyisp can't use turns the storage tests off (the API
// answers 503, no campaign runs) and is left as it is. Transfers are
// marked in wins, as speed tests are; a check during a queued test says
// its timings may be high.
// A round's line context comes from tap (outside fyisp's own transfers),
// and a failure's traceroute from tr.
func newStorage(dir string, q *speedtest.Queue, st storageDB, wins *speedtest.Windows, tap *speedtest.LoadTap, tr trace.Tracer,
	mc *metrics.Collector, path func() netinfo.Path, log *slog.Logger) (*storage.Service, *storage.Campaigns) {
	e := s3test.New(s3test.Options{UserAgent: "fyisp/" + version, Masker: wins, IsSelf: notEdge(path)})
	s := storage.Open(storage.Options{Dir: dir, Engine: e, Busy: func() bool { return q.State().Running != nil }})
	if err := s.Err(); err != nil {
		log.Warn("storage tests are off", "err", err)
		return s, nil
	}
	sm := metrics.NewStorage()
	mc.Register(sm)
	return s, storage.NewCampaigns(storage.CampaignOptions{Store: st, Queue: q, Engine: e, Targets: s, Log: log, OnRound: sm.Observe,
		Line: func(from, to time.Time) (gw, edge *model.LoadStats) {
			enough := func(s *model.LoadStats) *model.LoadStats {
				if s == nil || s.N < lineSamples {
					return nil // a lost probe or two of a handful says nothing
				}
				return s
			}
			return enough(tap.Stats(speedtest.LayerGateway, from, to, wins)), enough(tap.Stats(speedtest.LayerEdge, from, to, wins))
		},
		Trace:  storageTrace(tr, path),
		Recent: recentSpeeds(st)})
}

// storageDB is what the storage tests read and write in the database
// (*store.SQLite): the campaigns, and the speed tests' recent results.
type storageDB interface {
	storage.Store
	SpeedTests(ctx context.Context, from, to time.Time) ([]model.SpeedTest, error)
}

// recentSpeeds is the scheduled speed tests' median download and upload
// over the last week (0: none), read at most every 10 minutes.
func recentSpeeds(sp storageDB) func() [2]float64 {
	var mu sync.Mutex
	var at time.Time
	var last [2]float64
	return func() [2]float64 {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		if !at.IsZero() && now.Sub(at) < 10*time.Minute {
			return last
		}
		at = now
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ts, err := sp.SpeedTests(ctx, now.Add(-7*24*time.Hour), now)
		if err != nil {
			return last
		}
		s := speedtest.Summarize(ts, nil)
		last = [2]float64{}
		if s.DownMedian != nil {
			last[0] = *s.DownMedian
		}
		if s.UpMedian != nil {
			last[1] = *s.UpMedian
		}
		return last
	}
}

// storageTraceRounds is a storage traceroute's length (2.5 s a round).
const storageTraceRounds = 24

// lineSamples is the probes a round's line context needs (else unknown).
const lineSamples = 20

// storageTrace traceroutes a provider's edge address as Investigate does
// a host (TCP to 443 where this OS can, else ICMP), waits for it, and
// returns its route's hops with their networks and the first network
// past this connection's provider. Hops on this network (private, CGNAT)
// and in this connection's provider's network keep no address (one could
// be the user's public address): only "your network", "your provider".
// The last hop, the traced address itself, is left out (the round keeps
// its /24).
func storageTrace(tr trace.Tracer, path func() netinfo.Path) func(ctx context.Context, ip netip.Addr) ([]model.StorageHop, uint32, bool) {
	return func(ctx context.Context, ip netip.Addr) ([]model.StorageHop, uint32, bool) {
		if !netinfo.Traceable(ip) || !tr.Running() {
			return nil, 0, false
		}
		target := model.HostTarget(ip.String())
		mode := model.TraceTCP
		if !hops.Supported().TCP {
			mode = model.TraceICMP
		}
		inv, err := tr.Investigate(target, mode, storageTraceRounds)
		if err != nil {
			return nil, 0, false
		}
		defer inv.Stop()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			if _, ended := inv.Progress(); !ended.IsZero() {
				break
			}
			select {
			case <-ctx.Done():
				return nil, 0, false
			case <-t.C:
			}
		}
		route, ok := tr.Route(target, mode)
		if !ok || len(route.Hops) == 0 {
			return nil, 0, true // probes went out, nothing answered
		}
		isp := uint32(0)
		if p := path(); p.Edge.IsValid() {
			isp = asn.Lookup(p.Edge).ASN
		}
		hs := route.Hops
		if n := len(hs); n > 0 && hs[n-1] == ip {
			hs = hs[:n-1]
		}
		var out []model.StorageHop
		var handoff uint32
		seenISP := false
		for _, h := range hs {
			if !h.IsValid() {
				out = append(out, model.StorageHop{}) // no answer
				continue
			}
			info := asn.Lookup(h)
			switch {
			case !netinfo.Traceable(h) || storage.IsSelf(h):
				out = append(out, model.StorageHop{Owner: model.HopYourNetwork})
			case isp == 0 || info.ASN == isp:
				// The provider's own routers (or an unknown provider: maybe yours).
				seenISP = true
				out = append(out, model.StorageHop{ASN: info.ASN, Owner: model.HopYourProvider})
			default:
				if seenISP && handoff == 0 && info.ASN != 0 {
					handoff = info.ASN
				}
				out = append(out, model.StorageHop{IP: h.String(), ASN: info.ASN, Owner: info.Owner})
			}
		}
		return out, handoff, true
	}
}

// notEdge reports an address never kept as a provider's edge: this
// machine's, any in this connection's own provider's network (it could be
// the user's public address: a server at home reached by its DDNS name),
// and any of no known network. With the provider unknown, every address
// is. Best effort: a provider that hands out addresses from another of its
// networks isn't recognised.
func notEdge(path func() netinfo.Path) func(netip.Addr) bool {
	return func(a netip.Addr) bool {
		if storage.IsSelf(a) {
			return true
		}
		p := path()
		if !p.Edge.IsValid() {
			return true
		}
		isp, own := asn.Lookup(p.Edge).ASN, asn.Lookup(a).ASN
		return isp == 0 || own == 0 || own == isp
	}
}
