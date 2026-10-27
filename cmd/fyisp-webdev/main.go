// Command fyisp-webdev serves the web UI over probe.Fake and store.Fake for
// UI development and manual checks. It is not part of any release: the
// release matrix builds ./cmd/fyisp only.
//
//	fyisp-webdev -listen 0.0.0.0:3000 -backfill 6h
//	fyisp-webdev -profile all -backfill 90m   # a catalog profile: the Overview
//	fyisp-webdev -verdict ok -calls poor      # the Calls view: good, poor or blocked
//	fyisp-webdev -home blip                   # a Home scenario (/api/home; homescen.Names)
//	fyisp-webdev -spikes                      # fixed spikes and drops on one service (api/probes)
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/probe"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/fuck-you-isp/fyisp/internal/store"
	"github.com/fuck-you-isp/fyisp/internal/tiles"
	"github.com/fuck-you-isp/fyisp/internal/web"
	"github.com/fuck-you-isp/fyisp/internal/web/homescen"
)

// udpBase are base RTTs (ms) of the STUN targets' fake bursts (see
// probe.FakeBurst), the Teams relay included (its region shows in the
// state hook).
var udpBase = map[string]float64{
	"Meet-STUN": 9, "Cloudflare-STUN": 6, "Cloudflare-UDP53": 6, "Twilio-STUN-global": 24, "Teams-Relay": 31,
}

// devProfile is the default profile, network path included. Probes and
// traces are fake: no host is ever contacted.
func devProfile() (*model.Profile, error) {
	p, err := profile.Default()
	if err != nil {
		return nil, err
	}
	p.Name = "dev"
	return p, nil
}

// Synthetic history windows, as fractions of the backfill range before now.
// The ISP outage and the LAN blip match fakeVerdict's incidents.
type window struct{ from, to time.Time }

func (w window) has(t time.Time) bool { return !t.Before(w.from) && t.Before(w.to) }

func windows(d time.Duration, now time.Time) (hole, isp, lan window) {
	hole = window{now.Add(-d * 6 / 10), now.Add(-d*6/10 + d/20)}
	isp = window{now.Add(-d * 3 / 10), now.Add(-d*3/10 + d/40)}
	lan = window{now.Add(-d * 8 / 10), now.Add(-d*8/10 + d/80)}
	return
}

// backfill writes synthetic history: a not-measured hole (fyisp "stopped"),
// an ISP outage (gateway fine, everything past it lossy, mixed reasons) and
// a short LAN blip (everything lossy), so the UI has something to show.
func backfill(st model.Sink, p *model.Profile, d time.Duration, now time.Time, spikes bool) {
	r := mrand.New(mrand.NewPCG(1, 2))
	hole, isp, lan := windows(d, now)
	spikeName := ""
	if spikes {
		spikeName = spikeTarget(p)
	}
	reasons := []model.Reason{model.ReasonTimeout, model.ReasonRefused, model.ReasonReset, model.ReasonDNS, model.ReasonTLS, model.ReasonUnreachable, model.ReasonNoNetwork}
	for i, t := range p.Targets {
		base := 5 + float64(i%30)*5
		switch t.Layer {
		case model.LayerGateway:
			base = 1.5
		case model.LayerEdge:
			base = 7
		case model.LayerAnycast:
			base = 12 + float64(i%3)*2
		}
		if b, ok := udpBase[t.Name]; ok {
			base = b
		}
		for _, k := range t.ProbeKinds() {
			iv := t.Interval
			if k == model.KindICMP {
				iv /= 3
			}
			for ts := now.Add(-d).Truncate(iv); ts.Before(now.Add(-2 * time.Second)); ts = ts.Add(iv) {
				if hole.has(ts) {
					continue
				}
				if k == model.KindUDP {
					// Bursts: the ISP outage loses most requests, the
					// LAN blip nearly all; otherwise 0.3% per request.
					lossP := 0.003
					switch {
					case isp.has(ts):
						lossP = 0.5
					case lan.has(ts):
						lossP = 0.8
					}
					for _, s := range probe.FakeBurst(r, t.Name, ts, base, lossP) {
						st.Observe(s)
					}
					continue
				}
				s := model.Sample{Key: model.SeriesKey{Target: t.Name, Kind: k}, Slot: ts}
				lossP := 0.003
				reason := reasons[(i+int(k))%len(reasons)]
				switch {
				case isp.has(ts) && t.Layer != model.LayerGateway:
					lossP = 0.6
				case lan.has(ts):
					lossP, reason = 0.8, model.ReasonTimeout
				}
				if r.Float64() < lossP {
					s.Lost, s.Reason = true, reason
				} else {
					ms := base*(1+0.08*math.Sin(float64(ts.Unix())/900+float64(i))) + r.ExpFloat64()*1.5
					if k == model.KindICMP {
						ms *= 0.9
					}
					s.RTT = time.Duration(ms * float64(time.Millisecond))
				}
				if t.Name == spikeName && k == model.KindTCP {
					spike(&s, now, iv, base)
				}
				st.Observe(s)
			}
		}
	}
}

// spikeTable holds fixed probes of spikeTarget, by age at startup, as ms
// added to the slot's value: three of about 1 s (a resent SYN), six of
// 10-50 ms and one timeout. They change the slot after its random draws, so
// every other sample is unchanged.
var spikeTable = []struct {
	age   time.Duration
	extra float64 // 0: lost
}{
	{3 * time.Minute, 23}, {5 * time.Minute, 1000}, {8 * time.Minute, 48}, {10 * time.Minute, 0},
	{12 * time.Minute, 1005}, {15 * time.Minute, 17}, {20 * time.Minute, 35}, {25 * time.Minute, 12},
	{90 * time.Minute, 29}, {2 * time.Hour, 1010},
}

// spikeTarget is the first service probed with TCP (not the network path).
func spikeTarget(p *model.Profile) string {
	for _, t := range p.Targets {
		if t.Layer == "" && slices.Contains(t.ProbeKinds(), model.KindTCP) {
			return t.Name
		}
	}
	return ""
}

// spike changes s when a spikeTable entry falls in its slot; a randomly lost
// slot becomes base plus the extra, so every spike shows.
func spike(s *model.Sample, now time.Time, iv time.Duration, base float64) {
	for _, x := range spikeTable {
		if !now.Add(-x.age).Truncate(iv).Equal(s.Slot) {
			continue
		}
		switch {
		case x.extra == 0:
			s.Lost, s.Reason, s.RTT = true, model.ReasonTimeout, 0
		case s.Lost:
			s.Lost, s.Reason, s.RTT = false, 0, time.Duration((base+x.extra)*float64(time.Millisecond))
		default:
			s.RTT += time.Duration(x.extra * float64(time.Millisecond))
		}
	}
}

// oldestStore reports when the synthetic history starts, like the real store.
type oldestStore struct {
	*store.Fake
	oldest time.Time
}

func (s oldestStore) Stats(ctx context.Context) (store.Stats, error) {
	st, err := s.Fake.Stats(ctx)
	st.Oldest = s.oldest
	return st, err
}

// devAdminToken is the default admin token: the dashboard serves fake
// data, and the UI tests sign in with it (test/ui/playwright.config.js).
const devAdminToken = "fyisp-webdev-not-a-secret"

func main() {
	listen := flag.String("listen", "127.0.0.1:3000", "local listener")
	bf := flag.Duration("backfill", 6*time.Hour, "synthetic history to generate")
	loss := flag.Float64("loss", 0.01, "live loss rate")
	noICMP := flag.Bool("no-icmp", false, "report ICMP as unavailable")
	admin := flag.String("admin-token", devAdminToken, "admin token the dashboard requires (fake data, so a fixed, well-known one by default)")
	vCycle := flag.Duration("verdict-cycle", 20*time.Second, "how long each fake verdict kind lasts")
	vKind := flag.String("verdict", "", "stay on one verdict kind (ok, lan, isp, upstream, dns, service, no_network, udp_blocked, stun_blocked (quiet), udp_degraded, warming_up)")
	noVerdict := flag.Bool("no-verdict", false, "run without a verdict source (banner and outage log hidden)")
	noEdge := flag.Bool("edge-undiscovered", false, "never report ISP edge samples (shows \"discovering…\")")
	conn := flag.String("conn", model.ConnWiFi, "how this machine connects (the network path's Conn): wifi, wired or unknown")
	noTrace := flag.Bool("no-trace", false, "run without a trace source (Investigate view hidden)")
	noBaselines := flag.Bool("no-baselines", false, "run without baselines (no \"normal\" badges, band or slow line)")
	noSpeed := flag.Bool("no-speed", false, "run without speed tests (Speed hidden)")
	storageScen := flag.String("storage", storageTargets, "storage tests: none (Storage hidden), empty (no targets) or targets")
	callsMode := flag.String("calls", callsGood, "call-path scenario for the Calls view: good, poor or blocked")
	named := flag.String("profile", "", "named profiles instead of the dev profile, e.g. all (keep -backfill short: every series is in memory)")
	spikes := flag.Bool("spikes", false, "fixed slow and lost probes on the first TCP service in the last 2 h (needs -backfill 2h or more)")
	home := flag.String("home", "", "a Home scenario served by /api/home (the rest stays fake probes): "+strings.Join(homescen.Names, ", "))
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *conn != model.ConnWiFi && *conn != model.ConnWired && *conn != model.ConnUnknown {
		log.Error("-conn: want wifi, wired or unknown", "got", *conn)
		os.Exit(2)
	}
	if *admin == "" {
		log.Error("-admin-token: the dashboard needs one")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM) // SIGTERM: docker stop (the storage dir is removed)
	defer stop()

	p, err := devProfile()
	if err != nil {
		log.Error("profile", "err", err)
		os.Exit(2)
	}
	if *named != "" {
		reg, err := profile.Builtin()
		if err == nil {
			p, err = reg.Resolve(strings.Split(*named, ","), profile.ResolveOptions{})
		}
		if err != nil {
			log.Error("profile", "err", err)
			os.Exit(2)
		}
		// A "service" verdict naming many targets, as on a big profile.
		var bad []string
		for _, t := range p.Targets {
			if t.Layer == "" && len(bad) < 44 {
				bad = append(bad, t.Name)
			}
		}
		if len(bad) > 3 {
			script[0].targets = bad
			script[0].summary = fmt.Sprintf("%d services look affected: %s and %d more.", len(bad), strings.Join(bad[:3], ", "), len(bad)-3)
		}
		// Slow a spread of targets down too, so the Overview has something to show.
		for i, t := range p.Targets {
			switch {
			case t.Layer != "":
			case i%23 == 0:
				slowdown[t.Name] = 3.5
			case i%7 == 0:
				slowdown[t.Name] = 1.8
			}
		}
	}
	now := time.Now()
	// -home: the Home's scenario, going on as time passes; the rest of the
	// dashboard runs on the fake probes over its profile. Speed tests are
	// the fake scheduler's (Test now works), not the scenario's.
	var live *homescen.Live
	if *home != "" {
		sc, err := homescen.Build(*home, now)
		if err != nil {
			log.Error("-home", "err", err)
			os.Exit(2)
		}
		p = sc.Profile
		live = &homescen.Live{Name: *home, Now: time.Now}
	}
	// sink adds the evening slowdown and drops ISP edge samples with
	// -edge-undiscovered.
	sink := func(inner model.Sink) model.Sink {
		inner = callsSink(slowSink(inner, now.Add(-slowFor)), *callsMode, now)
		if !*noEdge {
			return inner
		}
		return model.SinkFunc(func(s model.Sample) {
			if s.Key.Target != "ISP edge" {
				inner.Observe(s)
			}
		})
	}
	st := store.NewFake()
	var warm web.Warmup
	var vs web.VerdictSource // read by tl once it runs
	pr := &probe.Fake{Seed: 42, LossRate: *loss}
	if *bf > 0 {
		log.Info("backfilling", "range", *bf)
		backfill(sink(st), p, *bf, now, *spikes)
	}
	// The Home tiles: the real engine over the fake store and probes.
	tl := tiles.New(p, profile.ResolveTiles(p), tiles.Sources{Store: st,
		Verdict: func() model.Verdict {
			if vs == nil {
				return model.Verdict{Kind: model.VerdictOK}
			}
			return vs.Current()
		},
		Incidents: func(ctx context.Context, from, to time.Time) ([]model.Incident, error) {
			if vs == nil {
				return nil, nil
			}
			return vs.Incidents(ctx, from, to)
		}}, tiles.Options{Log: log})
	go func() { _ = pr.Run(ctx, p, sink(model.Fanout{st, &warm, tl})) }()

	caps := pr.Caps()
	if *noICMP {
		caps.ICMP, caps.ICMPHint = "unavailable", "Allow unprivileged ICMP with: sudo sysctl -w net.ipv4.ping_group_range='0 2147483647'"
	}
	started := now
	if live != nil {
		started = now.Add(-time.Hour) // a scenario has been running a while: its stale is stale, not a fresh start
	}
	d := web.Deps{
		Profile: func() *model.Profile { return p },
		Store:   oldestStore{st, now.Add(-*bf)},
		Status: func() web.Status {
			return web.Status{Version: "dev", Started: started, Caps: caps, Targets: len(p.Targets), Ready: warm.Ready()}
		},
		Metrics: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintln(w, "# fyisp-webdev: no real metrics")
		}),
		UDP:  pr,
		Path: func() netinfo.Path { return fakePath(*conn, *noEdge) },
		Log:  log,
	}
	if !*noTrace {
		ft := newFakeTrace(st, now, *bf)
		go ft.run(ctx)
		d.Trace = ft
		d.Resolve = fakeResolve
	}
	if !*noVerdict {
		d.Verdict = callsVerdict{newFakeVerdict(now, *bf, *vCycle, model.VerdictKind(*vKind), *noEdge), *callsMode, now}
	}
	vs = d.Verdict
	d.Tiles = tl
	go func() { _ = tl.Run(ctx) }()
	if live != nil {
		d.Tiles = live
		d.Path = func() netinfo.Path { return live.Current().Path }
	}
	if !*noSpeed {
		d.Speed = newFakeSpeed(ctx, now, *bf, log)
	}
	switch *storageScen {
	case storageNone, storageEmpty, storageTargets:
	default:
		fmt.Fprintf(os.Stderr, "-storage %q: want none, empty or targets\n", *storageScen)
		os.Exit(2)
	}
	if *storageScen != storageNone {
		var q *speedtest.Queue
		if sp, ok := d.Speed.(*speedtest.Scheduler); ok {
			q = sp.Queue()
		} else {
			q = speedtest.NewQueue(speedtest.QueueOptions{})
			go q.Run(ctx)
		}
		ss, camps, db, cleanup := newFakeStorage(ctx, *storageScen, q, log)
		defer cleanup()
		d.Storage = ss
		if camps != nil {
			d.Campaigns, d.StorageData = camps, db
		}
	}
	if !*noBaselines {
		fb := newFakeBaselines(ctx, st, p, now, *bf)
		d.Baselines = fb
		d.Slow = fb.slow(st, p)
	}
	srv := func(addr string, h http.Handler) *http.Server {
		return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
			WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	}
	ls := srv(*listen, web.Local(d, web.LocalOptions{Addr: *listen, AdminToken: *admin}))
	go func() {
		if err := ls.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("listen", "addr", ls.Addr, "err", err)
			stop()
		}
	}()
	log.Info("serving", "local", "http://"+*listen+"/", "sign_in", "http://"+*listen+"/#admin="+*admin)
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ls.Shutdown(sctx)
}

// fakePath is the network path the fake data describes (see the Network
// details page): conn says how this machine connects.
func fakePath(conn string, noEdge bool) netinfo.Path {
	p := netinfo.Path{Gateway: netip.MustParseAddr("192.168.1.1"), Iface: "eth0", Conn: conn}
	if conn == model.ConnWiFi {
		p.Iface = "wlan0"
	}
	if !noEdge {
		p.Edge, p.EdgeHop = netip.MustParseAddr("198.51.100.1"), 2
	}
	return p
}
