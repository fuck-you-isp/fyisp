// Command genfixture writes the database fixture of the last release
// (testdata/<release>.db): the dbfixture Dockerfile stage copies it into a
// checkout of that release's tag and runs it there, so the file is written
// by the release's own store. It uses a fixed clock and fixed values, which
// upgrade_test.go recomputes (relSamples and the others): keep both in
// step.
//
//	go run ./cmd/genfixture -out DIR   # writes DIR/fyisp.db
package main

import (
	"context"
	"flag"
	"log"
	"net/netip"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

var (
	t0  = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end = t0.Add(3*time.Hour + 20*time.Minute) // three closed hours and part of a fourth
)

func interval(k model.SeriesKey) time.Duration {
	if k.Kind == model.KindHTTPS || k.Kind == model.KindTCP {
		return 15 * time.Second
	}
	return 5 * time.Second
}

// keys are one series of every probe kind.
var keys = []model.SeriesKey{
	{Target: "github", Kind: model.KindHTTPS}, {Target: "github", Kind: model.KindTCP}, {Target: "github", Kind: model.KindICMP},
	{Target: "github", Kind: model.KindTrace, Hop: 1}, {Target: "github", Kind: model.KindTrace, Hop: 2}, {Target: "github", Kind: model.KindTrace, Hop: 3},
	{Target: "Meet-STUN", Kind: model.KindUDP}, {Target: "Meet-STUN", Kind: model.KindUDPJitter}, {Target: "Meet-STUN", Kind: model.KindUDPLoss},
}

func samples() []model.Sample {
	var out []model.Sample
	for i, k := range keys {
		n := 0
		for ts := t0; ts.Before(end); ts = ts.Add(interval(k)) {
			x := model.Sample{Key: k, Slot: ts, RTT: time.Duration(1000+n%100*37+i*100) * 10 * time.Microsecond}
			switch {
			case k.Kind == model.KindUDPLoss:
				x.RTT = time.Duration(n%5*20) * time.Millisecond // loss in percent as milliseconds
			case n%200 == 13:
				x = model.Sample{Key: k, Slot: ts, Lost: true, Reason: model.ReasonGap}
			case n%50 == 7:
				x = model.Sample{Key: k, Slot: ts, Lost: true, Reason: model.Reason(1 + n%15)}
			}
			out = append(out, x)
			n++
		}
	}
	return out
}

func incidents() []model.Incident {
	return []model.Incident{
		{Start: t0.Add(10 * time.Minute), End: t0.Add(25 * time.Minute), Kind: model.VerdictISP, Summary: "closed", Targets: []string{"github"}, PeakLoss: 0.5},
		{Start: t0.Add(3 * time.Hour), Kind: model.VerdictUpstream, Summary: "ongoing", PeakLoss: 0.25},
	}
}

func trace() (model.Route, model.RouteChange, model.HopInfo) {
	a, b, c := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("203.0.113.9")
	r := model.Route{Target: "github", Hops: []netip.Addr{a, c, b}, Since: t0.Add(time.Hour)}
	ch := model.RouteChange{Target: "github", At: t0.Add(time.Hour), From: []netip.Addr{a, b, b}, To: []netip.Addr{a, c, b}, FirstDiff: 2}
	h := model.HopInfo{IP: c, RDNS: "edge.example.net", ASN: 64500, Owner: "EXAMPLE-AS", FirstSeen: t0, LastSeen: t0.Add(3 * time.Hour)}
	return r, ch, h
}

func speedTest() model.SpeedTest {
	down, up, sc, re := 4.12e8, 3.5e7, 0.93, int64(112)
	return model.SpeedTest{Start: t0.Add(2 * time.Hour), End: t0.Add(2*time.Hour + 33*time.Second), Trigger: "schedule", Outcome: model.SpeedOK,
		Region: "uk", Server: "Clouvider-London", Port: 5203, Attempts: 2, DownBps: &down, UpBps: &up, DownRetrans: &re,
		Latency: &model.SpeedLatency{Edge: model.LoadPhases{Idle: &model.LoadStats{N: 40, P50Ms: 8.2, P95Ms: 9.9},
			Down: &model.LoadStats{N: 58, Lost: 3, Loss: 0.052, P50Ms: 140, P95Ms: 1000}}},
		Conn: model.ConnWired, Handoff: &model.Handoff{ASN: 3356, Name: "LEVEL3", ISPASN: 3320, At: t0.Add(time.Hour)},
		FallbackLevel: "none", DownSecs: 0.8, UpSecs: 3.1, Bytes: 140 << 20, ScoreDown: &sc, Extra: `{"ramp_ms":[400,300]}`}
}

func main() {
	out := flag.String("out", "", "data directory to write fyisp.db into")
	flag.Parse()
	ctx := context.Background()
	s, err := store.Open(*out, store.Options{Version: "v0.14.0", Interval: interval, Now: func() time.Time { return end }})
	if err != nil {
		log.Fatal(err)
	}
	for _, x := range samples() {
		s.Observe(x)
	}
	for _, in := range incidents() {
		if err := s.SaveIncident(ctx, &in); err != nil {
			log.Fatal(err)
		}
	}
	r, ch, h := trace()
	s.ObserveRoute(r)
	s.ObserveRouteChange(ch)
	s.ObserveHop(h)
	st := speedTest()
	if err := s.SaveSpeedTest(ctx, &st); err != nil {
		log.Fatal(err)
	}
	if err := s.SetSetting(ctx, "owner_tz", "Europe/Berlin"); err != nil {
		log.Fatal(err)
	}
	if err := s.Close(); err != nil {
		log.Fatal(err)
	}
}
