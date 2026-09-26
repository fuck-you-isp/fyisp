package store

import (
	"context"
	"database/sql"
	"math"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

func fixtureDirV2(t *testing.T) string {
	dir := t.TempDir()
	copyFile(t, filepath.Join("testdata", "v2.db"), filepath.Join(dir, DBName))
	return dir
}

// seriesIDs reads the series table of a database file.
func seriesIDs(t *testing.T, db *sql.DB) map[model.SeriesKey]int64 {
	t.Helper()
	rows, err := db.Query(`SELECT id, target, kind FROM series`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[model.SeriesKey]int64{}
	for rows.Next() {
		var id int64
		var k model.SeriesKey
		if err := rows.Scan(&id, &k.Target, &k.Kind); err != nil {
			t.Fatal(err)
		}
		out[k] = id
	}
	return out
}

func checkV3Schema(t *testing.T, s *SQLite, bakFrom int) {
	t.Helper()
	if v := pragmaInt(t, s.db.w, "user_version"); v != int64(SchemaVersion()) || SchemaVersion() < 3 {
		t.Fatalf("user_version %d, SchemaVersion %d", v, SchemaVersion())
	}
	for _, tbl := range []string{"hop_info", "route_changes", "routes", "incidents"} {
		var n int
		if err := s.db.r.QueryRow(`SELECT count(*) FROM ` + tbl).Scan(&n); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
	}
	var idx int
	s.db.r.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'index' AND name = 'route_changes_at'`).Scan(&idx)
	if idx != 1 {
		t.Fatal("route_changes_at index missing")
	}
	checkV4Schema(t, s)
	var hops int
	if err := s.db.r.QueryRow(`SELECT count(*) FROM series WHERE hop = 0`).Scan(&hops); err != nil || hops != 3 {
		t.Fatalf("series hop column: %d %v", hops, err)
	}
	bak := filepath.Join(s.dir, DBName+".bak-v"+string(rune('0'+bakFrom)))
	b, err := sql.Open("sqlite", bak)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var uv int
	if err := b.QueryRow(`PRAGMA user_version`).Scan(&uv); err != nil || uv != bakFrom {
		t.Fatalf("backup %s: user_version %d %v", bak, uv, err)
	}
}

// TestFixtureV2ToV3 upgrades the v2 fixture: samples, series ids and
// incidents survive, a v2 backup is written, trace series can be added.
func TestFixtureV2ToV3(t *testing.T) {
	dir := fixtureDirV2(t)
	pre, err := sql.Open("sqlite", filepath.Join(dir, DBName))
	if err != nil {
		t.Fatal(err)
	}
	before := seriesIDs(t, pre)
	pre.Close()
	if len(before) != 3 {
		t.Fatalf("fixture series %v", before)
	}

	now := t0.Add(2*time.Hour + 30*time.Minute)
	s := openT(t, dir, Options{Now: func() time.Time { return now }})
	defer func() { s.Close() }()
	checkV3Schema(t, s, 2)
	checkFixture(t, s)
	if after := seriesIDs(t, s.db.r); !mapsEqual(after, before) {
		t.Fatalf("series ids changed: %v -> %v", before, after)
	}
	got, err := s.Incidents(ctx, t0, t0.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := fixtureIncidents()
	if len(got) != len(want) {
		t.Fatalf("incidents %+v", got)
	}
	for i, w := range want {
		g := got[len(got)-1-i] // newest first
		if !g.Start.Equal(w.Start) || !g.End.Equal(w.End) || g.Kind != w.Kind || g.Summary != w.Summary ||
			!slices.Equal(g.Targets, w.Targets) || g.PeakLoss != w.PeakLoss {
			t.Fatalf("incident %d = %+v, want %+v", i, g, w)
		}
	}

	// Same target and kind, several hops: distinct series.
	for hop := uint8(1); hop <= 3; hop++ {
		s.Observe(model.Sample{Key: model.SeriesKey{Target: "github", Kind: model.KindTrace, Hop: hop}, Slot: now, RTT: time.Duration(hop) * time.Millisecond})
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.r.QueryRow(`SELECT count(*) FROM series WHERE target = 'github' AND kind = ?`, int(model.KindTrace)).Scan(&n)
	if n != 3 {
		t.Fatalf("%d trace series", n)
	}
	s.Close()
	s = openT(t, dir, Options{Now: func() time.Time { return now }})
	checkFixture(t, s)
	pts := rawAll(t, s, []model.SeriesKey{{Target: "github", Kind: model.KindTrace, Hop: 2}}, now, now)
	if len(pts) != 1 || pts[0].RTTms != 2 {
		t.Fatalf("hop 2 after reopen: %+v", pts)
	}
}

func mapsEqual[K comparable, V comparable](a, b map[K]V) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TestFixtureV1ToV3 upgrades the v1 fixture through v2 to v3.
func TestFixtureV1ToV3(t *testing.T) {
	dir := fixtureDir(t)
	s := openT(t, dir, Options{Now: func() time.Time { return t0.Add(2*time.Hour + 30*time.Minute) }})
	defer s.Close()
	checkV3Schema(t, s, 1)
	checkFixture(t, s)
}

// traceSamples is n rounds of a 4-hop trace every 5s from t0: hop 2 drops
// every 4th probe (rate limiting), hop 4 (the destination) every 10th,
// with a not-measured slot now and then.
func traceSamples(target string, rounds int) []model.Sample {
	var out []model.Sample
	for r := range rounds {
		ts := t0.Add(time.Duration(r) * 5 * time.Second)
		for hop := uint8(1); hop <= 4; hop++ {
			k := model.SeriesKey{Target: target, Kind: model.KindTrace, Hop: hop}
			x := model.Sample{Key: k, Slot: ts, RTT: time.Duration(int(hop)*1000+r%7*100) * 10 * time.Microsecond}
			switch {
			case r%97 == 50:
				x = model.Sample{Key: k, Slot: ts, Lost: true, Reason: model.ReasonGap}
			case hop == 2 && r%4 == 0:
				x = model.Sample{Key: k, Slot: ts, Lost: true, Reason: model.ReasonTimeout}
			case hop == 4 && r%10 == 3:
				x = model.Sample{Key: k, Slot: ts, Lost: true, Reason: model.ReasonUnreachable}
			}
			out = append(out, x)
		}
	}
	return out
}

func traceKeysN(target string, n int) []model.SeriesKey {
	var out []model.SeriesKey
	for hop := 1; hop <= n; hop++ {
		out = append(out, model.SeriesKey{Target: target, Kind: model.KindTrace, Hop: uint8(hop)})
	}
	return out
}

// TestTraceSeriesRoundTrip: hop series (default 5s interval) next to an
// ordinary series of the same target go through Panel (one query), Raw and
// Series like any other, in memory, flushed and after a reopen; Fake agrees.
func TestTraceSeriesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := t0
	o := Options{Now: func() time.Time { return now }} // nil Interval: trace hops default to 5s
	s := openT(t, dir, o)
	defer func() { s.Close() }()
	f := NewFake()
	icmp := model.SeriesKey{Target: "gh", Kind: model.KindICMP}
	for _, x := range traceSamples("gh", 1000) { // ~83 min
		s.Observe(x)
		f.Observe(x)
		if x.Key.Hop == 1 {
			y := model.Sample{Key: icmp, Slot: x.Slot, RTT: 7 * time.Millisecond}
			s.Observe(y)
			f.Observe(y)
		}
		now = x.Slot
	}
	keys := append(traceKeysN("gh", 4), icmp)
	end := t0.Add(2 * time.Hour)
	check := func(stage string) {
		t.Helper()
		equalRaw(t, stage, rawAll(t, s, keys, t0, end), rawAll(t, f, keys, t0, end))
		for _, mp := range []int{10, 1000} {
			q := PanelQuery{Keys: keys, From: t0, To: end, MaxPoints: mp}
			equalPanel(t, stage, panel(t, s, q), panel(t, f, q), 1e-6)
		}
		infos, err := s.Series(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(infos) != 5 {
			t.Fatalf("%s: %d series", stage, len(infos))
		}
		if infos[0].Key != icmp { // sorted: icmp (3) before trace (4)
			t.Fatalf("%s: series order %v", stage, infos)
		}
		for i, in := range infos[1:] {
			if in.Key != keys[i] || (stage != "memory" && in.Interval != 5*time.Second) {
				t.Fatalf("%s: series %d = %+v", stage, i, in)
			}
		}
		// Fake orders by hop too.
		finfos, _ := f.Series(ctx)
		for i := range finfos {
			if finfos[i].Key != infos[i].Key {
				t.Fatalf("%s: fake series order %v", stage, finfos)
			}
		}
	}
	check("memory")
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	check("flushed")
	now = t0.Add(90 * time.Minute)
	if err := s.Flush(ctx); err != nil { // closes hour 0
		t.Fatal(err)
	}
	check("closed")
	s.Close()
	s = openT(t, dir, o)
	check("reopened")
	// The hourly tier (summaries) too.
	q := PanelQuery{Keys: keys, From: t0.Add(-72 * time.Hour), To: end, MaxPoints: 100}
	equalPanel(t, "hourly", panel(t, s, q), panel(t, f, q), 1e-6)
}

// TestClassifyLoss: the "real loss" rule, table-driven.
func TestClassifyLoss(t *testing.T) {
	type hop struct {
		n, lost int64
	}
	cases := []struct {
		name      string
		hops      []hop
		continues []bool
		limited   []bool
	}{
		{"clean", []hop{{100, 0}, {100, 0}, {100, 0}}, []bool{false, false, false}, []bool{false, false, false}},
		{"rate limited middle", []hop{{100, 0}, {60, 40}, {100, 0}}, []bool{false, false, false}, []bool{false, true, false}},
		{"real loss from hop 2", []hop{{100, 0}, {80, 20}, {85, 15}, {80, 20}}, []bool{false, true, true, true}, []bool{false, false, false, false}},
		{"destination loses half", []hop{{100, 0}, {60, 40}, {80, 20}}, []bool{false, true, true}, []bool{false, false, false}},
		{"destination loses less than half", []hop{{100, 0}, {60, 40}, {81, 19}}, []bool{false, false, true}, []bool{false, true, false}},
		{"below 5 points ignored", []hop{{100, 0}, {96, 4}, {96, 4}}, []bool{false, false, false}, []bool{false, false, false}},
		{"exactly 5 points", []hop{{95, 5}, {95, 5}}, []bool{true, true}, []bool{false, false}},
		{"destination only", []hop{{100, 0}, {100, 0}, {50, 50}}, []bool{false, false, true}, []bool{false, false, false}},
		{"silent hop", []hop{{100, 0}, {0, 100}, {100, 0}}, []bool{false, false, false}, []bool{false, true, false}},
		{"trailing hop without probes", []hop{{100, 0}, {50, 50}, {50, 50}, {0, 0}}, []bool{false, true, true, false}, []bool{false, false, false, false}},
		{"no probes", []hop{{0, 0}, {0, 0}}, []bool{false, false}, []bool{false, false}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var hs []HopStat
			for i, h := range c.hops {
				st := HopStat{Hop: i + 1, N: h.n, Lost: h.lost}
				if h.n+h.lost > 0 {
					st.Loss = float64(h.lost) / float64(h.n+h.lost)
				}
				hs = append(hs, st)
			}
			classifyLoss(hs)
			for i := range hs {
				if hs[i].LossContinues != c.continues[i] || hs[i].RateLimited != c.limited[i] {
					t.Errorf("hop %d (loss %.2f): continues %v limited %v, want %v %v",
						i+1, hs[i].Loss, hs[i].LossContinues, hs[i].RateLimited, c.continues[i], c.limited[i])
				}
			}
		})
	}
}

// TestHops: per-hop statistics, addresses and loss classification from a
// real store and the Fake, over the raw and the hourly tier.
func TestHops(t *testing.T) {
	dir := t.TempDir()
	now := t0
	s := openT(t, dir, Options{Now: func() time.Time { return now }})
	defer s.Close()
	f := NewFake()
	type sink interface {
		model.Sink
		ObserveHop(model.HopInfo)
		ObserveRoute(model.Route)
		ObserveRouteChange(model.RouteChange)
	}
	all := []sink{s, f}
	samples := traceSamples("gh", 1440) // 2 hours
	for _, x := range samples {
		for _, k := range all {
			k.Observe(x)
		}
	}
	ip := func(s string) netip.Addr { return netip.MustParseAddr(s) }
	oldR := []netip.Addr{ip("192.168.1.1"), ip("10.0.0.1"), ip("100.64.9.9"), ip("140.82.112.3")}
	newR := []netip.Addr{ip("192.168.1.1"), ip("10.0.0.1"), ip("100.64.0.1"), ip("140.82.112.3")}
	change := t0.Add(100 * time.Minute) // the old route held for most of the range
	for _, k := range all {
		k.ObserveRoute(model.Route{Target: "gh", Hops: oldR, Since: t0})
		k.ObserveRouteChange(model.RouteChange{Target: "gh", At: change, From: oldR, To: newR, FirstDiff: 3})
		k.ObserveRoute(model.Route{Target: "gh", Hops: newR, Since: change})
		k.ObserveHop(model.HopInfo{IP: ip("100.64.0.1"), ASN: 7922, Owner: "COMCAST-7922", LastSeen: change})
		k.ObserveHop(model.HopInfo{IP: ip("10.0.0.1"), RDNS: "gw.isp.example", LastSeen: change})
	}
	now = t0.Add(2*time.Hour + 5*time.Minute)

	// Expected numbers straight from the samples.
	type exp struct {
		n, lost int64
		rtts    []float64
	}
	want := map[uint8]*exp{}
	for _, x := range samples {
		e := want[x.Key.Hop]
		if e == nil {
			e = &exp{}
			want[x.Key.Hop] = e
		}
		switch {
		case !x.Lost:
			e.n++
			e.rtts = append(e.rtts, float64(x.RTT)/1e6)
		case x.Reason != model.ReasonGap:
			e.lost++
		}
	}
	check := func(name string, r TraceReader, hourly bool) {
		t.Helper()
		from, to := t0, t0.Add(2*time.Hour)
		if hourly {
			from = t0.Add(-3 * 24 * time.Hour)
		}
		hs, err := r.Hops(ctx, "gh", from, to)
		if err != nil {
			t.Fatal(err)
		}
		if len(hs) != 4 {
			t.Fatalf("%s: %d hops", name, len(hs))
		}
		for i, h := range hs {
			e := want[uint8(i+1)]
			if h.Hop != i+1 || h.N != e.n || h.Lost != e.lost {
				t.Fatalf("%s: hop %+v, want n %d lost %d", name, h, e.n, e.lost)
			}
			if math.Abs(h.Loss-float64(e.lost)/float64(e.n+e.lost)) > 1e-12 {
				t.Fatalf("%s: hop %d loss %v", name, h.Hop, h.Loss)
			}
			v := slices.Clone(e.rtts)
			slices.Sort(v)
			var sum float64
			for _, x := range v {
				sum += x
			}
			if !closeTo(h.MinMs, v[0]) || !closeTo(h.MaxMs, v[len(v)-1]) || !closeTo(h.MeanMs, sum/float64(len(v))) {
				t.Fatalf("%s: hop %d rtt %v/%v/%v", name, h.Hop, h.MinMs, h.MeanMs, h.MaxMs)
			}
			if !hourly {
				if !closeTo(h.P95Ms, v[(len(v)*95+99)/100-1]) {
					t.Fatalf("%s: hop %d p95 %v", name, h.Hop, h.P95Ms)
				}
				if h.JitterMs <= 0 {
					t.Fatalf("%s: hop %d jitter %v", name, h.Hop, h.JitterMs)
				}
			} else if h.P95Ms < h.MinMs || h.P95Ms > h.MaxMs || h.JitterMs != 0 {
				t.Fatalf("%s: hourly hop %d p95 %v jitter %v", name, h.Hop, h.P95Ms, h.JitterMs)
			}
			if h.IP != newR[i] {
				t.Fatalf("%s: hop %d ip %v", name, h.Hop, h.IP)
			}
		}
		// Hop 2 loses 25% (rate limiting: the destination loses 10%).
		if !hs[1].RateLimited || hs[1].LossContinues || !hs[3].LossContinues || hs[0].RateLimited {
			t.Fatalf("%s: classification %+v", name, hs)
		}
		if !hourly && hs[2].MostCommonIP != oldR[2] {
			t.Fatalf("%s: most common hop 3 %v", name, hs[2].MostCommonIP)
		}
		if in := hs[2].Info; in == nil || in.ASN != 7922 || in.Owner != "COMCAST-7922" {
			t.Fatalf("%s: hop 3 info %+v", name, in)
		}
		if in := hs[1].Info; in == nil || in.RDNS != "gw.isp.example" {
			t.Fatalf("%s: hop 2 info %+v", name, in)
		}
		if hs[0].Info != nil {
			t.Fatalf("%s: hop 1 info %+v", name, hs[0].Info)
		}
	}
	check("fake", f, false)
	check("sqlite memory", s, false)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	check("sqlite flushed", s, false)
	check("sqlite hourly", s, true)

	// Unknown target: nothing.
	if hs, err := s.Hops(ctx, "nope", t0, now); err != nil || len(hs) != 0 {
		t.Fatalf("unknown target: %v %v", hs, err)
	}
	// A current route longer than the samples shows its hops, empty.
	s.ObserveRoute(model.Route{Target: "gh", Hops: append(slices.Clone(newR), ip("140.82.112.4"))})
	if hs, _ := s.Hops(ctx, "gh", t0, t0.Add(2*time.Hour)); len(hs) != 5 || hs[4].N+hs[4].Lost != 0 || hs[4].IP != ip("140.82.112.4") {
		t.Fatalf("route-only hop: %+v", hs)
	}
}

func closeTo(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

// TestMostCommonHops reconstructs the routes in effect over a range.
func TestMostCommonHops(t *testing.T) {
	a, b, c := netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.3")
	cur := model.Route{Target: "x", Hops: []netip.Addr{a, c}, Since: t0.Add(50 * time.Minute)}
	changes := []model.RouteChange{ // newest first
		{At: t0.Add(50 * time.Minute), From: []netip.Addr{a, b}, To: []netip.Addr{a, c}},
		{At: t0.Add(10 * time.Minute), From: []netip.Addr{a, c}, To: []netip.Addr{a, b}},
	}
	for _, tc := range []struct {
		from, to time.Duration
		want     netip.Addr
	}{
		{0, 60 * time.Minute, b},                // b held 40 of 60 minutes
		{0, 20 * time.Minute, b},                // c [0,10], b [10,20]: a tie, the lower address wins
		{45 * time.Minute, 90 * time.Minute, c}, // mostly the current route
		{15 * time.Minute, 30 * time.Minute, b}, // inside one segment
	} {
		got := mostCommonHops(cur, true, changes, t0.Add(tc.from), t0.Add(tc.to))
		if len(got) != 2 || got[0] != a {
			t.Fatalf("%v-%v: %v", tc.from, tc.to, got)
		}
		if got[1] != tc.want {
			t.Fatalf("%v-%v: hop 2 %v, want %v", tc.from, tc.to, got[1], tc.want)
		}
	}
}

// TestRoutes: routes are written only when they change, survive a reopen
// and are readable read-only; route changes are listed newest first,
// before and after Flush; Prune drops old changes and hop addresses.
func TestRoutes(t *testing.T) {
	dir := t.TempDir()
	now := t0
	o := Options{Now: func() time.Time { return now }}
	s := openT(t, dir, o)
	defer func() { s.Close() }()
	ip := func(s string) netip.Addr { return netip.MustParseAddr(s) }
	r1 := []netip.Addr{ip("192.168.1.1"), {}, ip("8.8.8.8")}
	r2 := []netip.Addr{ip("192.168.1.1"), ip("100.64.0.1"), ip("8.8.8.8")}

	s.ObserveRoute(model.Route{Target: "a", Hops: r1, Since: t0})
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	s.ObserveRoute(model.Route{Target: "a", Hops: slices.Clone(r1), Since: t0.Add(time.Minute)}) // unchanged: nothing queued
	s.mu.Lock()
	queued := len(s.tr.routes)
	s.mu.Unlock()
	if queued != 0 {
		t.Fatal("unchanged route queued")
	}
	if r, ok := s.Route(ctx, "a"); !ok || !slices.Equal(r.Hops, r1) || !r.Since.Equal(t0) {
		t.Fatalf("route %+v %v", r, ok)
	}
	if _, ok := s.Route(ctx, "b"); ok {
		t.Fatal("route of an unknown target")
	}

	var changes []model.RouteChange
	for i := range 5 {
		c := model.RouteChange{Target: "a", At: t0.Add(time.Duration(i) * 24 * time.Hour), From: r1, To: r2, FirstDiff: 2}
		if i%2 == 1 {
			c.From, c.To, c.Target = r2, r1, "b"
		}
		changes = append(changes, c)
		s.ObserveRouteChange(c)
	}
	s.ObserveRoute(model.Route{Target: "a", Hops: r2, Since: t0.Add(4 * 24 * time.Hour)})
	checkChanges := func(stage string, r TraceReader, flushed bool) {
		t.Helper()
		got, err := r.RouteChanges(ctx, t0.Add(24*time.Hour), t0.Add(3*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("%s: %d changes", stage, len(got))
		}
		for i, g := range got {
			w := changes[3-i]
			if g.Target != w.Target || !g.At.Equal(w.At) || !slices.Equal(g.From, w.From) || !slices.Equal(g.To, w.To) || g.FirstDiff != w.FirstDiff {
				t.Fatalf("%s: change %d = %+v, want %+v", stage, i, g, w)
			}
			if flushed != (g.ID != 0) {
				t.Fatalf("%s: change id %d", stage, g.ID)
			}
		}
	}
	checkChanges("pending", s, false)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	checkChanges("flushed", s, true)
	s.ObserveRouteChange(model.RouteChange{Target: "a", At: t0.Add(2 * 24 * time.Hour), From: r1, To: r2, FirstDiff: 2})
	if got, _ := s.RouteChanges(ctx, t0.Add(2*24*time.Hour), t0.Add(2*24*time.Hour)); len(got) != 2 || got[0].ID != 0 || got[1].ID == 0 {
		t.Fatalf("pending before flushed at the same time: %+v", got)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	s.Close()
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := ro.Route(ctx, "a"); !ok || !slices.Equal(r.Hops, r2) || !r.Since.Equal(t0.Add(4*24*time.Hour)) {
		t.Fatalf("read-only route %+v %v", r, ok)
	}
	if got, err := ro.RouteChanges(ctx, t0, t0.Add(10*24*time.Hour)); err != nil || len(got) != 6 {
		t.Fatalf("read-only changes %d %v", len(got), err)
	}
	ro.Close()

	now = t0.Add(4*24*time.Hour + time.Hour)
	s = openT(t, dir, o)
	if r, ok := s.Route(ctx, "a"); !ok || !slices.Equal(r.Hops, r2) {
		t.Fatalf("reopened route %+v %v", r, ok)
	}
	s.ObserveRoute(model.Route{Target: "a", Hops: r2}) // loaded: still unchanged
	s.mu.Lock()
	queued = len(s.tr.routes)
	s.mu.Unlock()
	if queued != 0 {
		t.Fatal("route unchanged across reopen queued")
	}

	// Prune everything before day 2. Hop addresses last seen before the
	// cut go too.
	s.ObserveHop(model.HopInfo{IP: ip("100.64.0.1"), LastSeen: t0})
	s.ObserveHop(model.HopInfo{IP: ip("8.8.8.8"), LastSeen: now})
	// Prune counts retention back from the newest stored hour: store one.
	s.Observe(model.Sample{Key: model.SeriesKey{Target: "a", Kind: model.KindTrace, Hop: 1}, Slot: now, RTT: time.Millisecond})
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(ctx, t0.Add(2*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.RouteChanges(ctx, t0, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if c.At.Before(t0.Add(2 * 24 * time.Hour)) {
			t.Fatalf("change at %v kept", c.At)
		}
	}
	if len(got) != 4 {
		t.Fatalf("%d changes after prune", len(got))
	}
	if _, ok, _ := s.HopInfo(ctx, ip("100.64.0.1")); ok {
		t.Fatal("stale hop kept")
	}
	if _, ok, _ := s.HopInfo(ctx, ip("8.8.8.8")); !ok {
		t.Fatal("recent hop pruned")
	}
	if _, ok := s.Route(ctx, "a"); !ok {
		t.Fatal("current route pruned")
	}

	// The Fake prunes the same way.
	f := NewFake()
	for _, c := range changes {
		f.ObserveRouteChange(c)
	}
	f.ObserveHop(model.HopInfo{IP: ip("100.64.0.1"), LastSeen: t0})
	f.Prune(ctx, t0.Add(2*24*time.Hour))
	if got, _ := f.RouteChanges(ctx, t0, now); len(got) != 3 || got[0].ID != 5 {
		t.Fatalf("fake changes after prune %+v", got)
	}
	if _, ok, _ := f.HopInfo(ctx, ip("100.64.0.1")); ok {
		t.Fatal("fake kept stale hop")
	}
}

// hopRow reads one hop_info row.
func hopRow(t *testing.T, s *SQLite, ip string) (rdns sql.NullString, asn sql.NullInt64, owner sql.NullString, first, last int64) {
	t.Helper()
	if err := s.db.r.QueryRow(`SELECT rdns, asn, owner, first_seen_ms, last_seen_ms FROM hop_info WHERE ip = ?`, ip).
		Scan(&rdns, &asn, &owner, &first, &last); err != nil {
		t.Fatal(err)
	}
	return
}

// TestHopInfoCoalescing: repeated sightings rewrite last_seen at most every
// HopSeenInterval, new metadata at once; empty metadata never erases.
func TestHopInfoCoalescing(t *testing.T) {
	dir := t.TempDir()
	now := t0
	o := Options{Now: func() time.Time { return now }}
	s := openT(t, dir, o)
	defer func() { s.Close() }()
	a := netip.MustParseAddr("100.64.0.1")
	queued := func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.tr.hops)
	}
	s.ObserveHop(model.HopInfo{IP: a}) // LastSeen = now
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 10; i++ { // every minute for 9 minutes: nothing to write
		now = t0.Add(time.Duration(i) * time.Minute)
		s.ObserveHop(model.HopInfo{IP: a})
		if queued() != 0 {
			t.Fatalf("minute %d: sighting queued", i)
		}
	}
	if h, ok, err := s.HopInfo(ctx, a); err != nil || !ok || !h.LastSeen.Equal(t0) || !h.FirstSeen.Equal(t0) {
		t.Fatalf("hop %+v %v %v", h, ok, err)
	}
	now = t0.Add(10 * time.Minute)
	s.ObserveHop(model.HopInfo{IP: a})
	if queued() != 1 {
		t.Fatal("stale sighting not queued")
	}
	// Unflushed state is visible.
	if h, _, _ := s.HopInfo(ctx, a); !h.LastSeen.Equal(now) {
		t.Fatalf("pending last seen %v", h.LastSeen)
	}
	now = t0.Add(11 * time.Minute)
	s.ObserveHop(model.HopInfo{IP: a, ASN: 7922, Owner: "COMCAST-7922"}) // new metadata: at once
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	rdns, asn, owner, first, last := hopRow(t, s, a.String())
	if rdns.Valid || asn.Int64 != 7922 || owner.String != "COMCAST-7922" || first != t0.UnixMilli() || last != now.UnixMilli() {
		t.Fatalf("row %v %v %v %d %d", rdns, asn, owner, first, last)
	}
	now = t0.Add(12 * time.Minute)
	s.ObserveHop(model.HopInfo{IP: a, ASN: 7922}) // same metadata: coalesced
	if queued() != 0 {
		t.Fatal("same metadata queued")
	}
	now = t0.Add(30 * time.Minute)
	s.ObserveHop(model.HopInfo{IP: a, RDNS: "edge.example"}) // no asn/owner: kept
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	rdns, asn, owner, _, last = hopRow(t, s, a.String())
	if rdns.String != "edge.example" || asn.Int64 != 7922 || owner.String != "COMCAST-7922" || last != now.UnixMilli() {
		t.Fatalf("row %v %v %v %d", rdns, asn, owner, last)
	}
	// An IPv4-mapped IPv6 address is the same router.
	if h, ok, _ := s.HopInfo(ctx, netip.AddrFrom16(a.As16())); !ok || h.ASN != 7922 {
		t.Fatalf("mapped lookup %+v %v", h, ok)
	}

	// The coalescing memory survives a restart.
	s.Close()
	now = t0.Add(35 * time.Minute)
	s = openT(t, dir, o)
	s.ObserveHop(model.HopInfo{IP: a})
	if queued() != 0 {
		t.Fatal("sighting after reopen queued")
	}
	// Invalid addresses are ignored.
	s.ObserveHop(model.HopInfo{})
	if queued() != 0 {
		t.Fatal("invalid address queued")
	}
}

// TestTraceFlushFailure: queued trace items survive a failed flush and are
// written by the next one.
func TestTraceFlushFailure(t *testing.T) {
	dir := t.TempDir()
	now := t0
	s := openT(t, dir, Options{Now: func() time.Time { return now }})
	defer s.Close()
	a := netip.MustParseAddr("10.1.1.1")
	s.ObserveHop(model.HopInfo{IP: a, ASN: 1})
	s.ObserveRoute(model.Route{Target: "x", Hops: []netip.Addr{a}})
	s.ObserveRouteChange(model.RouteChange{Target: "x", At: t0, To: []netip.Addr{a}, FirstDiff: 1})
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Flush(cctx); err == nil {
		t.Fatal("flush with a canceled context succeeded")
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.r.QueryRow(`SELECT (SELECT count(*) FROM hop_info) + (SELECT count(*) FROM routes) + (SELECT count(*) FROM route_changes)`).Scan(&n)
	if n != 3 {
		t.Fatalf("%d rows written", n)
	}
	s.mu.Lock()
	left := len(s.tr.hops) + len(s.tr.routes) + len(s.tr.changes)
	s.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d items still queued", left)
	}
}
