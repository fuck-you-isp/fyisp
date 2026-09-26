// Command fyisp-webdev serves the web UI over probe.Fake and store.Fake for
// UI development and manual checks. It is not part of any release: the
// release matrix builds ./cmd/fyisp only.
//
//	fyisp-webdev -listen 0.0.0.0:3000 -public 0.0.0.0:3001 -backfill 6h
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/probe"
	"github.com/fuck-you-isp/fyisp/internal/store"
	"github.com/fuck-you-isp/fyisp/internal/web"
)

var names = strings.Fields(`Google-Meet Microsoft-Teams Discord google-dns cloudflare-dns quad9-dns open-dns dev-GHCR dev-GitHub
DevTunnel-Global DevTunnel-UkSouth DevTunnel-NorthEurope DevTunnel-WestEurope DevTunnel-EastUs DevTunnel-EastUs2 DevTunnel-WestUs3
DevTunnel-WestUs2 DevTunnel-SouthEastAsia DevTunnel-BrazilSouth DevTunnel-CentralIndia DevTunnel-AustraliaEast DevTunnel-AustraliaCentral
Hetzner-Falkenstein Hetzner-Nuremberg Hetzner-Helsinki Hetzner-Ashburn Hetzner-Hillsboro Hetzner-Singapore
AWS-us-east-1 AWS-us-east-2 AWS-us-west-1 AWS-us-west-2 AWS-af-south-1 AWS-ap-east-1 AWS-ap-east-2 AWS-ap-south-1 AWS-ap-south-2
AWS-ap-southeast-1 AWS-ap-southeast-2 AWS-ap-southeast-3 AWS-ap-southeast-4 AWS-ap-southeast-5 AWS-ap-southeast-6 AWS-ap-southeast-7
AWS-ap-northeast-1 AWS-ap-northeast-2 AWS-ap-northeast-3 AWS-ca-central-1 AWS-ca-west-1 AWS-eu-central-1 AWS-eu-central-2 AWS-eu-west-1
AWS-eu-west-2 AWS-eu-west-3 AWS-eu-north-1 AWS-eu-south-1 AWS-eu-south-2 AWS-il-central-1 AWS-mx-central-1 AWS-me-south-1 AWS-me-central-1
AWS-sa-east-1 GCP-us-west1 GCP-us-west2 GCP-us-west3 GCP-us-west4 GCP-us-central1 GCP-us-east1 GCP-us-east4 GCP-us-east5 GCP-us-south1
GCP-northamerica-northeast1 GCP-northamerica-northeast2 GCP-southamerica-east1 GCP-southamerica-west1 GCP-europe-west1 GCP-europe-west2
GCP-europe-west3 GCP-europe-west4 GCP-europe-west6 GCP-europe-west8 GCP-europe-west9 GCP-europe-west10 GCP-europe-west12
GCP-europe-north1 GCP-europe-southwest1 GCP-europe-central2`)

// devProfile mirrors the default profile's 87 targets and 7 groups.
func devProfile() *model.Profile {
	p := &model.Profile{Name: "dev", Groups: []model.Group{
		{ID: "common", Title: "Common Services (<100ms is good for audio/video calls)", Order: 1},
		{ID: "dns", Title: "DNS", Order: 2},
		{ID: "devtunnels", Title: "DevTunnels", Order: 3},
		{ID: "dev", Title: "Various Dev Related Services", Order: 4},
		{ID: "aws", Title: "Amazon Web Services", Order: 5},
		{ID: "hetzner", Title: "Hetzner", Order: 6},
		{ID: "gcp", Title: "Google Cloud Platform", Order: 7},
	}}
	for _, n := range names {
		g := "common"
		switch {
		case strings.HasSuffix(n, "-dns"):
			g = "dns"
		case strings.HasPrefix(n, "DevTunnel-"):
			g = "devtunnels"
		case strings.HasPrefix(n, "dev-"):
			g = "dev"
		case strings.HasPrefix(n, "AWS-"):
			g = "aws"
		case strings.HasPrefix(n, "Hetzner-"):
			g = "hetzner"
		case strings.HasPrefix(n, "GCP-"):
			g = "gcp"
		}
		p.Targets = append(p.Targets, model.Target{Name: n, Host: strings.ToLower(n) + ".invalid", Group: g, Interval: 15 * time.Second})
	}
	return p
}

// backfill writes synthetic history: a not-measured hole (fyisp "stopped")
// and an outage with mixed reasons, so the UI has something to show.
func backfill(st model.Sink, p *model.Profile, d time.Duration, now time.Time) {
	r := mrand.New(mrand.NewPCG(1, 2))
	holeFrom, holeTo := now.Add(-d*6/10), now.Add(-d*6/10+d/20)
	outFrom, outTo := now.Add(-d*3/10), now.Add(-d*3/10+d/40)
	reasons := []model.Reason{model.ReasonTimeout, model.ReasonRefused, model.ReasonReset, model.ReasonDNS, model.ReasonTLS, model.ReasonUnreachable, model.ReasonNoNetwork}
	for i, t := range p.Targets {
		base := 5 + float64(i%30)*5
		for _, k := range []model.ProbeKind{model.KindHTTPS, model.KindTCP, model.KindICMP} {
			iv := t.Interval
			if k == model.KindICMP {
				iv /= 3
			}
			for ts := now.Add(-d).Truncate(iv); ts.Before(now.Add(-2 * time.Second)); ts = ts.Add(iv) {
				if !ts.Before(holeFrom) && ts.Before(holeTo) {
					continue
				}
				s := model.Sample{Key: model.SeriesKey{Target: t.Name, Kind: k}, Slot: ts}
				lossP := 0.003
				if !ts.Before(outFrom) && ts.Before(outTo) {
					lossP = 0.6
				}
				if r.Float64() < lossP {
					s.Lost, s.Reason = true, reasons[(i+int(k))%len(reasons)]
				} else {
					ms := base*(1+0.08*math.Sin(float64(ts.Unix())/900+float64(i))) + r.ExpFloat64()*1.5
					if k == model.KindICMP {
						ms *= 0.9
					}
					s.RTT = time.Duration(ms * float64(time.Millisecond))
				}
				st.Observe(s)
			}
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

// devShare simulates the tunnel: starting for 2s, then connected.
type devShare struct {
	mu  sync.Mutex
	st  web.ShareState
	url string
}

func (s *devShare) State() web.ShareState { s.mu.Lock(); defer s.mu.Unlock(); return s.st }
func (s *devShare) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Phase == web.ShareConnected || s.st.Phase == web.ShareStarting {
		return nil
	}
	s.st = web.ShareState{Phase: web.ShareStarting}
	time.AfterFunc(2*time.Second, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.st.Phase == web.ShareStarting {
			s.st = web.ShareState{Phase: web.ShareConnected, URL: s.url, Protocol: "quic"}
		}
	})
	return nil
}
func (s *devShare) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st = web.ShareState{Phase: web.ShareOff}
	return nil
}

func main() {
	listen := flag.String("listen", "127.0.0.1:3000", "local listener")
	public := flag.String("public", "127.0.0.1:3001", "public listener (the tunnel origin)")
	publicURL := flag.String("public-url", "http://localhost:3001", "base URL the fake share reports")
	bf := flag.Duration("backfill", 6*time.Hour, "synthetic history to generate")
	loss := flag.Float64("loss", 0.01, "live loss rate")
	noICMP := flag.Bool("no-icmp", false, "report ICMP as unavailable")
	admin := flag.String("admin-token", "", "admin token for share controls")
	secret := flag.String("secret", "", "public path secret (random if empty)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	p := devProfile()
	st := store.NewFake()
	var warm web.Warmup
	now := time.Now()
	if *bf > 0 {
		log.Info("backfilling", "range", *bf)
		backfill(st, p, *bf, now)
	}
	pr := &probe.Fake{Seed: 42, LossRate: *loss}
	go func() { _ = pr.Run(ctx, p, model.Fanout{st, &warm}) }()

	sec := *secret
	if sec == "" {
		var b [16]byte
		_, _ = rand.Read(b[:])
		sec = hex.EncodeToString(b[:])
	}
	share := &devShare{st: web.ShareState{Phase: web.ShareOff}, url: *publicURL + "/s/" + sec + "/"}
	caps := pr.Caps()
	if *noICMP {
		caps.ICMP, caps.ICMPHint = "unavailable", "Allow unprivileged ICMP with: sudo sysctl -w net.ipv4.ping_group_range='0 2147483647'"
	}
	d := web.Deps{
		Profile: func() *model.Profile { return p },
		Store:   oldestStore{st, now.Add(-*bf)},
		Status: func() web.Status {
			return web.Status{Version: "dev", Started: now, Caps: caps, Targets: len(p.Targets), Ready: warm.Ready()}
		},
		Metrics: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintln(w, "# fyisp-webdev: no real metrics")
		}),
		Share: share,
		Log:   log,
	}
	srv := func(addr string, h http.Handler) *http.Server {
		return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
			WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	}
	ls := srv(*listen, web.Local(d, web.LocalOptions{Addr: *listen, AdminToken: *admin}))
	ps := srv(*public, web.Public(d, sec))
	for _, s := range []*http.Server{ls, ps} {
		go func() {
			if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Error("listen", "addr", s.Addr, "err", err)
				stop()
			}
		}()
	}
	log.Info("serving", "local", "http://"+*listen+"/", "public", share.url)
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ls.Shutdown(sctx)
	_ = ps.Shutdown(sctx)
}
