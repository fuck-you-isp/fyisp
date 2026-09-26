package report

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"net/netip"
	"sort"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// Planted values that must never appear in a redacted report.
const (
	secretNote    = "SECRET-PRIVATE-NOTE router 192.168.1.1 rebooted"
	publicNote    = "Opened ticket 4711 with the ISP"
	gwRDNS        = "secret-home-gw.lan"
	cgnatRDNS     = "cgnat-secret.isp.example"
	edgeRDNS      = "edge1.secret-town.isp.example"
	hop4RDNS      = "core2.secret-city.isp.example"
	farRDNS       = "ae1.far-away.transit.example"
	testVersion   = "v9.9.9-secretbuild"
	summarySecret = "gateway 10.0.0.1 unreachable"
)

var (
	gwIP    = netip.MustParseAddr("192.168.1.1")
	cgnatIP = netip.MustParseAddr("100.64.0.1")
	edgeIP  = netip.MustParseAddr("203.0.113.9")
	hop4IP  = netip.MustParseAddr("198.51.100.4")
	hop5IP  = netip.MustParseAddr("198.51.100.5")
	hop5bIP = netip.MustParseAddr("198.51.100.55")
	farIP   = netip.MustParseAddr("192.0.2.77")
	dstIP   = netip.MustParseAddr("1.1.1.1")
)

// fakeNotes is an in-memory store.AnnotationStore.
type fakeNotes struct{ list []model.Annotation }

func (f *fakeNotes) AddAnnotation(_ context.Context, a *model.Annotation) error {
	a.ID = int64(len(f.list) + 1)
	f.list = append(f.list, *a)
	return nil
}
func (f *fakeNotes) UpdateAnnotation(context.Context, *model.Annotation) error { return nil }
func (f *fakeNotes) DeleteAnnotation(context.Context, int64) error             { return nil }
func (f *fakeNotes) Annotations(_ context.Context, from, to time.Time, publicOnly bool) ([]model.Annotation, error) {
	var out []model.Annotation
	for _, a := range f.list {
		end := a.End
		if end.IsZero() {
			end = a.At
		}
		if (publicOnly && !a.Public) || a.At.After(to) || end.Before(from) {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

type fixture struct {
	prof      *model.Profile
	st        *store.Fake
	incidents []model.Incident
	notes     *fakeNotes
	from, to  time.Time
}

// ispDown reports whether t is inside a synthetic ISP incident: 19:00 UTC
// for 20 + 5*day minutes on the first 6 days.
func (f *fixture) ispDown(t time.Time) bool {
	for _, in := range f.incidents {
		if in.Kind == model.VerdictISP && !t.Before(in.Start) && t.Before(in.End) {
			return true
		}
	}
	return false
}

// newFixture builds a store.Fake with `days` days of samples every `step`
// for every target of the default profile, ISP incidents every evening
// but the last, one service incident, traces with private hops and a
// route change, and notes (one private).
func newFixture(t testing.TB, days int, step time.Duration) *fixture {
	t.Helper()
	prof, err := profile.Default()
	if err != nil {
		t.Fatal(err)
	}
	to := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	from := to.Add(-time.Duration(days) * 24 * time.Hour)
	f := &fixture{prof: prof, st: store.NewFake(), notes: &fakeNotes{}, from: from, to: to}
	id := int64(0)
	for d := 0; d < days; d++ {
		if days > 1 && d == days-1 {
			break
		}
		id++
		start := from.Add(time.Duration(d)*24*time.Hour + 19*time.Hour)
		f.incidents = append(f.incidents, model.Incident{ID: id, Start: start, End: start.Add(time.Duration(20+5*d) * time.Minute),
			Kind: model.VerdictISP, Summary: "Your ISP's network is losing packets: your router answers, the first hop past it does not.",
			PeakLoss: 0.2 + 0.05*float64(d)})
	}
	id++
	s := from.Add(10 * time.Hour)
	f.incidents = append(f.incidents, model.Incident{ID: id, Start: s, End: s.Add(12 * time.Minute), Kind: model.VerdictService,
		Summary: "Discord is not answering; everything else is fine. " + summarySecret, Targets: []string{"Discord"}, PeakLoss: 1})

	rng := rand.New(rand.NewPCG(1, 2))
	for ti, tg := range prof.Targets {
		k := model.SeriesKey{Target: tg.Name, Kind: primaryKind(tg)}
		base := 8 + float64(ti%17)*6
		if tg.Layer == model.LayerGateway {
			base = 2
		}
		for ts := from; ts.Before(to); ts = ts.Add(step) {
			// Not measured: a 3-hour hole on day 2.
			if ts.Sub(from) >= 50*time.Hour && ts.Sub(from) < 53*time.Hour {
				f.st.Observe(model.Sample{Key: k, Slot: ts, Lost: true, Reason: model.ReasonGap})
				continue
			}
			evening := 1.0
			if h := ts.Hour(); h >= 18 && h < 22 {
				evening = 1.6
			}
			down := f.ispDown(ts) && tg.Layer != model.LayerGateway
			lossP := 0.001
			if down {
				lossP = 0.35
			}
			if tg.Name == "Discord" && ts.Sub(s) >= 0 && ts.Sub(s) < 12*time.Minute {
				lossP = 1
			}
			if rng.Float64() < lossP {
				reason := model.ReasonTimeout
				if rng.Float64() < 0.2 {
					reason = model.ReasonReset
				}
				f.st.Observe(model.Sample{Key: k, Slot: ts, Lost: true, Reason: reason})
				continue
			}
			ms := base*evening*(1+0.15*rng.NormFloat64()) + rng.ExpFloat64()*2
			if down {
				ms *= 2.5
			}
			f.st.Observe(model.Sample{Key: k, Slot: ts, RTT: time.Duration(math.Max(0.3, ms) * 1e6)})
		}
	}

	// Traces: cloudflare-dns and Discord (both traced in the default profile).
	routeA := []netip.Addr{gwIP, cgnatIP, edgeIP, hop4IP, hop5IP, {}, farIP, dstIP}
	routeB := []netip.Addr{gwIP, cgnatIP, edgeIP, hop4IP, hop5bIP, {}, farIP, dstIP}
	for _, h := range []model.HopInfo{
		{IP: gwIP, RDNS: gwRDNS},
		{IP: cgnatIP, RDNS: cgnatRDNS},
		{IP: edgeIP, RDNS: edgeRDNS, ASN: 64500, Owner: "EXAMPLE-ISP"},
		{IP: hop4IP, RDNS: hop4RDNS, ASN: 64500, Owner: "EXAMPLE-ISP"},
		{IP: hop5IP, ASN: 64500, Owner: "EXAMPLE-ISP"},
		{IP: hop5bIP, ASN: 64500, Owner: "EXAMPLE-ISP"},
		{IP: farIP, RDNS: farRDNS, ASN: 64501, Owner: "TRANSIT-NET"},
		{IP: dstIP, RDNS: "one.one.one.one", ASN: 13335, Owner: "CLOUDFLARENET"},
	} {
		h.FirstSeen, h.LastSeen = from, to
		f.st.ObserveHop(h)
	}
	changeAt := from.Add(time.Duration(days) * 12 * time.Hour)
	for _, target := range []string{"cloudflare-dns", "Discord"} {
		f.st.ObserveRoute(model.Route{Target: target, Hops: routeA, Since: from})
		f.st.ObserveRoute(model.Route{Target: target, Hops: routeB, Since: changeAt})
		f.st.ObserveRouteChange(model.RouteChange{Target: target, At: changeAt, From: routeA, To: routeB, FirstDiff: 5})
		for ts := from; ts.Before(to); ts = ts.Add(step) {
			down := f.ispDown(ts)
			for hop := 1; hop <= len(routeA); hop++ {
				k := model.SeriesKey{Target: target, Kind: model.KindTrace, Hop: uint8(hop)}
				lost := false
				switch {
				case hop == 6:
					lost = true // never answers
				case hop == 4:
					lost = rng.Float64() < 0.3 // rate-limits its replies
				case hop >= 3 && down:
					lost = rng.Float64() < 0.35
				case hop >= 5:
					lost = rng.Float64() < 0.07 // real loss from hop 5 on
				}
				if lost {
					f.st.Observe(model.Sample{Key: k, Slot: ts, Lost: true, Reason: model.ReasonTimeout})
					continue
				}
				ms := 1 + float64(hop)*2.5 + rng.ExpFloat64()
				f.st.Observe(model.Sample{Key: k, Slot: ts, RTT: time.Duration(ms * 1e6)})
			}
		}
	}

	ctx := context.Background()
	_ = f.notes.AddAnnotation(ctx, &model.Annotation{At: from.Add(19*time.Hour + 5*time.Minute), Text: publicNote, Public: true})
	_ = f.notes.AddAnnotation(ctx, &model.Annotation{At: from.Add(20 * time.Hour), End: from.Add(21 * time.Hour), Text: secretNote})
	return f
}

func (f *fixture) deps() Deps {
	return Deps{
		Profile: func() *model.Profile { return f.prof },
		Store:   f.st,
		Trace:   f.st,
		Incidents: func(_ context.Context, from, to time.Time) ([]model.Incident, error) {
			var out []model.Incident
			for _, in := range f.incidents {
				if !in.Start.After(to) && (in.End.IsZero() || !in.End.Before(from)) {
					out = append(out, in)
				}
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Start.After(out[j].Start) }) // newest first, like the store
			return out, nil
		},
		Annotations: f.notes,
		Baselines: func(k model.SeriesKey, at time.Time) (model.Baseline, bool) {
			for ti, tg := range f.prof.Targets {
				if tg.Name == k.Target {
					base := 8 + float64(ti%17)*6
					if tg.Layer == model.LayerGateway {
						base = 2
					}
					return model.Baseline{Key: k, MedianMs: base * 1.05, P95Ms: base * 1.6, Samples: 1000}, true
				}
			}
			return model.Baseline{}, false
		},
		Version: testVersion,
	}
}

func (f *fixture) build(t testing.TB, d Deps, o Options) string {
	t.Helper()
	if o.From.IsZero() {
		o.From, o.To = f.from, f.to
	}
	if o.Now == nil {
		o.Now = func() time.Time { return f.to.Add(time.Hour) }
	}
	b, err := New(d).Build(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var _ = fmt.Sprint
