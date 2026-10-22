package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

var speedNow = time.Date(2026, 10, 10, 18, 0, 0, 0, time.UTC)

// speedCatalog: two regions of two providers each, the US one with a
// nearby city, and an opt-in region.
func speedCatalog(t *testing.T) *speedtest.Catalog {
	t.Helper()
	c, err := speedtest.LoadCatalog(fstest.MapFS{
		"clouvider.yml": {Data: []byte(`provider: clouvider
display: Clouvider
sources: [x]
servers:
  - {id: Clouvider-London, host: lon.speedtest.clouvider.net, ports: 5200-5209, city: London, country: GB, metro: london, capacity_gbps: 10, geo: eu, verified: {date: 2026-10-10}}
  - {id: Clouvider-NewYork, host: nyc.speedtest.clouvider.net, ports: 5200-5209, city: New York, country: US, metro: new-york, geo: na, verified: {date: 2026-10-10}}
`)},
		"leaseweb.yml": {Data: []byte(`provider: leaseweb
display: Leaseweb
sources: [x]
servers:
  - {id: Leaseweb-London, host: speedtest.lon12.uk.leaseweb.net, ports: 5201-5210, city: London, country: GB, metro: london, geo: eu, verified: {date: 2026-10-10}}
  - {id: Leaseweb-NewYork, host: speedtest.nyc1.us.leaseweb.net, ports: 5201-5210, city: New York, country: US, metro: new-york, geo: na, verified: {date: 2026-10-10}}
  - {id: Leaseweb-Washington, host: speedtest.wdc2.us.leaseweb.net, ports: 5201-5210, city: Washington, country: US, metro: washington, geo: na, verified: {date: 2026-10-10}}
`)},
		"regions.yml": {Data: []byte(`regions:
  - {id: uk, title: UK, servers: [Clouvider-London, Leaseweb-London]}
  - {id: us-east, title: US East, servers: [Leaseweb-NewYork, Clouvider-NewYork], nearby: [Leaseweb-Washington]}
  - {id: us-mid-atlantic, title: US Mid-Atlantic, servers: [Leaseweb-Washington], default: false}
`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// speedRig is a real Scheduler over a fake network and an in-memory store
// with 30 days of history: both regions every step, the UK near and fast,
// US East slower, both 40% slower from 19:00 to 23:00 UTC with either
// provider; the UK on its fallback most of the last day; a manual test
// against the own server now and then; a busy test every 50th. The clock
// stands still, so nothing is scheduled; each queued test's direction takes
// a token (or runs once release is closed), until it is cancelled.
type speedRig struct {
	s       *speedtest.Scheduler
	st      *speedtest.MemStore
	release chan struct{}
	tokens  chan struct{}
}

// rigOpts: the bytes each direction moves, and noRun to leave the queue
// stopped (entries stay waiting).
type rigOpts struct {
	wire  int64
	noRun bool
}

func newSpeedRig(t *testing.T, step time.Duration) *speedRig {
	t.Helper()
	return newSpeedRigWith(t, step, rigOpts{})
}

func newSpeedRigWith(t *testing.T, step time.Duration, ro rigOpts) *speedRig {
	t.Helper()
	ctx := context.Background()
	cat := speedCatalog(t)
	custom, err := speedtest.Custom("192.168.1.20:5201", cat)
	if err != nil {
		t.Fatal(err)
	}
	g := &speedRig{st: &speedtest.MemStore{}, release: make(chan struct{}), tokens: make(chan struct{}, 100)}
	if ro.wire == 0 {
		ro.wire = 90e6 // per direction: within the slot's 200 MB
	}
	g.st.SetSetting(ctx, speedtest.SettingKey, "regions")
	g.st.SetSetting(ctx, "speedtest_seed", "7")
	f := func(v float64) *float64 { return &v }
	start := speedNow.Add(-30 * 24 * time.Hour)
	i := 0
	for at := start; at.Before(speedNow); at = at.Add(step) {
		for _, reg := range cat.Regions[:2] {
			i++
			x := model.SpeedTest{Start: at, End: at.Add(18 * time.Second), Trigger: "schedule", Outcome: model.SpeedOK,
				Region: reg.ID, Port: 5201 + i%10, Attempts: 1, FallbackLevel: speedtest.FallbackNone, DownSecs: 4, UpSecs: 3.2, Bytes: 140 << 20}
			base := 400e6
			if reg.ID == "us-east" {
				base = 150e6
				x.Start = at.Add(1800 * time.Second)
				x.End = x.Start.Add(18 * time.Second)
			}
			chain := reg.Chain(cat, x.Start, 7)
			x.Server = chain[0].Server.ID
			if reg.ID == "uk" && speedNow.Sub(at) < 24*time.Hour && i%3 != 0 {
				x.Fallback, x.FallbackLevel, x.Server = true, speedtest.FallbackSameMetro, chain[1].Server.ID
			}
			switch {
			case i%50 == 7:
				x.Outcome, x.Attempts, x.Bytes, x.DownSecs, x.UpSecs = model.SpeedBusy, 3, 0, 0, 0
			case i%97 == 5:
				x.Outcome, x.Attempts, x.Bytes, x.DownSecs, x.UpSecs = model.SpeedUnreachable, 1, 0, 0, 0
			default:
				sc := 1 + 0.03*float64(i%7-3)
				if h := x.Start.Hour(); h >= 19 && h <= 22 {
					sc *= 0.6
				}
				down, up := base*sc, base*sc/10
				re := int64(i % 40)
				x.DownBps, x.UpBps, x.DownRetrans, x.ScoreDown, x.ScoreUp = &down, &up, &re, f(sc), f(sc)
				x.Conn = model.ConnWired
				x.Latency = &model.SpeedLatency{
					Edge:    model.LoadPhases{Idle: &model.LoadStats{N: 40, P50Ms: 8.2, P95Ms: 9.9}, Down: &model.LoadStats{N: 30, Lost: 1, Loss: 0.033, P50Ms: 21, P95Ms: 34}, Up: &model.LoadStats{N: 25, P50Ms: 48, P95Ms: 90}},
					Gateway: model.LoadPhases{Idle: &model.LoadStats{N: 40, P50Ms: 1.1, P95Ms: 1.9}, Down: &model.LoadStats{N: 30, P50Ms: 1.4, P95Ms: 2}, Up: &model.LoadStats{N: 25, P50Ms: 1.5, P95Ms: 2.2}}}
				x.Handoff = &model.Handoff{ASN: 3356, Name: "LEVEL3", ISPASN: 3320, At: x.Start}
				x.Extra = `{"ramp_ms":[600,400]}`
			}
			g.st.SaveSpeedTest(ctx, &x)
		}
		if i%20 == 0 {
			down, up := 900e6, 900e6
			g.st.SaveSpeedTest(ctx, &model.SpeedTest{Start: at.Add(60 * time.Second), End: at.Add(75 * time.Second), Trigger: "manual",
				Outcome: model.SpeedOK, Server: "192.168.1.20:5201", Port: 5201, Attempts: 1, DownBps: &down, UpBps: &up, FallbackLevel: speedtest.FallbackNone})
		}
	}
	g.s = speedtest.New(speedtest.Options{Catalog: cat, Custom: custom, Store: g.st, Now: func() time.Time { return speedNow },
		Fake: &speedtest.Fake{Seed: 1,
			Run: func(ctx context.Context, c speedtest.Config) (speedtest.Result, error) {
				select {
				case <-ctx.Done():
					return speedtest.Result{}, ctx.Err()
				case <-g.release:
				case <-g.tokens:
				}
				return speedtest.Result{Direction: c.Direction, Bytes: 80e6, Elapsed: 4 * time.Second, Streams: 8, TotalBytes: ro.wire, WireBytes: ro.wire}, nil
			},
			Sleep: func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil },
			RTT: func(_ context.Context, host string, _ int) (time.Duration, error) {
				if strings.Contains(host, "lon") {
					return 9 * time.Millisecond, nil
				}
				return 80 * time.Millisecond, nil
			}}})
	if ro.noRun { // the queue stays stopped; the mode is regions as saved
		if _, err := g.s.SetMode(ctx, speedtest.ModeRegions, nil); err != nil {
			t.Fatal(err)
		}
		return g
	}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { g.s.Run(rctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, func() bool { return g.s.State().Mode == speedtest.ModeRegions })
	return g
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func speedHandlers(t *testing.T, g *speedRig, o LocalOptions) (*localHandler, Deps) {
	_, d := fixture(t, speedNow)
	d.Speed = g.s
	if o.Addr == "" {
		o.Addr = "127.0.0.1:3000"
	}
	o.CSRFToken = "tok123"
	loc := Local(d, o).(*localHandler)
	loc.now = func() time.Time { return speedNow }
	return loc, d
}

var speedHost = map[string]string{"Host": "127.0.0.1:3000"}

// TestSpeedGolden pins api/speed's summary (7 days, a 2-hour time zone; a
// test every 5 hours keeps the file small): go test ./internal/web -run
// TestSpeedGolden -update.
func TestSpeedGolden(t *testing.T) {
	loc, _ := speedHandlers(t, newSpeedRig(t, 5*time.Hour), LocalOptions{})
	for name, raw := range map[string]string{
		"local": do(loc, "GET", "/api/speed?days=7&tz=120", speedHost).Body.String(),
	} {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("%s: %v: %s", name, err, raw)
		}
		got, _ := json.MarshalIndent(v, "", " ")
		got = append(got, '\n')
		path := filepath.Join("testdata", "speed", name+".json")
		if *update {
			os.MkdirAll(filepath.Dir(path), 0o755)
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -update)", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the golden file; run go test ./internal/web -run TestSpeedGolden -update and review the diff", path)
		}
	}
}

func TestSpeedSummary(t *testing.T) {
	g := newSpeedRig(t, 15*time.Minute)
	loc, d := speedHandlers(t, g, LocalOptions{})
	var s speedJSON
	if err := json.Unmarshal(do(loc, "GET", "/api/speed?days=30", speedHost).Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Mode != "regions" || !s.Enabled || !s.Configurable || s.SetByFlag || s.IntervalS != 3600 || s.Warning != nil || s.NextAt == nil || s.NextRow == nil ||
		len(s.Servers) != 6 || len(s.Regions) != 3 || len(s.PerRegion) != 2 || len(s.Rows) != 2 || len(s.Plans) != 3 || s.BytesMonth == 0 || s.Connection.Type != "wired" {
		t.Fatalf("summary: mode %s set by flag %v interval %d warning %v next %v %v, %d servers %d regions %d per region %d rows %d plans, bytes %d, %+v",
			s.Mode, s.SetByFlag, s.IntervalS, s.Warning, s.NextAt, s.NextRow, len(s.Servers), len(s.Regions), len(s.PerRegion), len(s.Rows), len(s.Plans), s.BytesMonth, s.Connection)
	}
	uk, us := s.PerRegion[0], s.PerRegion[1]
	if uk.ID != "uk" || uk.Label != "UK" || !uk.Scheduled || uk.N < 2500 || uk.Busy == 0 || uk.Fallback == 0 || uk.Hours[20].Score == nil || *uk.Hours[20].Score > 0.7 ||
		len(uk.Hours[20].Servers) != 2 {
		t.Errorf("uk row: %s %s %v n %d busy %d fallback %d, hour 20 %+v", uk.ID, uk.Label, uk.Scheduled, uk.N, uk.Busy, uk.Fallback, uk.Hours[20])
	}
	if uk.FallbackShare24h == nil || *uk.FallbackShare24h < 0.5 || uk.Note == nil || us.Note != nil || *us.FallbackShare24h != 0 {
		t.Errorf("fallback: uk %v %v, us %v", uk.FallbackShare24h, uk.Note, us.Note)
	}
	if uk.Dip == nil || us.Dip == nil || *us.DownMedian > 160e6 || *uk.BestDown < 400e6 || len(us.Servers) != 3 {
		t.Errorf("rows: uk dip %v us dip %v", uk.Dip, us.Dip)
	}
	// The pooled pattern and the finding: every region, both providers.
	if s.Pattern == nil || s.Pattern.Text != "Evenings are slower: about 40% below your daytime speed." || len(s.Pattern.Hours) != 4 || s.Pattern.Hours[0] != 19 {
		t.Errorf("pattern %+v", s.Pattern)
	}
	if s.Finding.Case != speedtest.CaseWidespread || strings.Contains(s.Finding.Text, "provider's own network") || s.Finding.Evidence == "" {
		t.Errorf("finding %+v", s.Finding)
	}
	if s.Coverage[20].N < 30 || s.Coverage[20].Providers != 2 {
		t.Errorf("coverage %+v", s.Coverage[20])
	}
	if len(s.UnderLoad.Text) != 3 ||
		!strings.Contains(s.UnderLoad.Text[1], "rises from 8 to 48 ms while uploading (+40 ms): a small rise, fine for calls") {
		t.Errorf("under load %+v", s.UnderLoad)
	}
	if len(s.PerServer) != 4 || s.Latest == nil || s.Servers[0].Host == "" || s.Servers[0].Ports != "5200-5209" || s.Servers[0].Capacity == nil ||
		*s.Servers[0].Capacity != 10000 || s.Servers[0].Display != "UK · Clouvider, London" {
		t.Errorf("servers: %d per server, latest %v, %+v", len(s.PerServer), s.Latest, s.Servers[0])
	}
	// An IANA zone: hours by it (DST-correct), echoed.
	var z speedJSON
	json.Unmarshal(do(loc, "GET", "/api/speed?days=7&tz=Europe/Berlin", speedHost).Body.Bytes(), &z)
	if z.TZName != "Europe/Berlin" || z.TZ != 120 || len(z.Coverage) != 24 {
		t.Errorf("tz=Europe/Berlin: %q %d", z.TZName, z.TZ)
	}
	// Fallback tests left out of the region rows on request.
	var x speedJSON
	json.Unmarshal(do(loc, "GET", "/api/speed?days=30&exclude_fallback=1", speedHost).Body.Bytes(), &x)
	if x.PerRegion[0].Fallback != 0 || x.PerRegion[0].N >= uk.N || !x.ExcludeFallback {
		t.Errorf("exclude_fallback: %+v", x.PerRegion[0].Stats.N)
	}
	// One row: a region, a server.
	var one speedOneJSON
	json.Unmarshal(do(loc, "GET", "/api/speed?region=us-east&days=7", speedHost).Body.Bytes(), &one)
	if one.Kind != "region" || one.Row.Label != "US East" || !one.Row.Scheduled || len(one.Tests) < 7*96 || len(one.Windows) != len(one.Tests) || one.Tests[0].Region == nil ||
		one.Tests[0].Latency == nil || one.Tests[0].Conn != "wired" || one.Tests[0].Handoff == nil {
		t.Errorf("region: %s %s %d tests", one.Kind, one.Row.Label, len(one.Tests))
	}
	one = speedOneJSON{}
	json.Unmarshal(do(loc, "GET", "/api/speed?row=uk&days=7", speedHost).Body.Bytes(), &one)
	if one.Kind != "region" || one.Row.ID != "uk" {
		t.Errorf("row=uk: %+v", one.Row)
	}
	one = speedOneJSON{}
	json.Unmarshal(do(loc, "GET", "/api/speed?server=192.168.1.20:5201&days=30", speedHost).Body.Bytes(), &one)
	if one.Kind != "server" || one.Row.ID != "192.168.1.20:5201" || len(one.Tests) == 0 || one.Tests[0].Server != "192.168.1.20:5201" || !one.Tests[0].OnDemand {
		t.Errorf("own server: %+v", one.Row)
	}
	for path, code := range map[string]int{
		"/api/speed?server=Nope":                     404,
		"/api/speed?server=a&region=b":               400,
		"/api/speed?days=14":                         400,
		"/api/speed?tz=900":                          400,
		"/api/speed?tz=Mars/Olympus":                 400,
		"/api/speed?tz=../../etc/passwd":             400,
		"/api/speed?tz=Local":                        400,
		"/api/speed?part=x":                          400,
		"/api/speed?part=windows&days=7":             400,
		"/api/speed?from=now-1h":                     400,
		"/api/speed?bogus=1":                         400,
		"/api/speed?exclude_fallback=yes":            400,
		"/api/speed?part=windows&from=now&to=now-1h": 400,
		"/api/speed?part=entry":                      400,
		"/api/speed?part=entry&id=99":                404,
		"/api/speed?part=queue&days=7":               400,
	} {
		w := do(loc, "GET", path, speedHost)
		var e struct{ Code string }
		json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != code || e.Code != errCode(code) {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	var w struct{ Windows []speedWindow }
	json.Unmarshal(do(loc, "GET", "/api/speed?part=windows&from=now-1h&to=now", speedHost).Body.Bytes(), &w)
	if len(w.Windows) < 4 || w.Windows[0].End-w.Windows[0].Start != 18000 {
		t.Errorf("windows %+v", w.Windows)
	}
	if r := do(loc, "GET", "/api/speed?part=sweep", speedHost); r.Code != 200 || !strings.Contains(r.Body.String(), `"entry":null`) {
		t.Errorf("no sweep yet: %d %s", r.Code, r.Body.String())
	}
	// features.speed, and without a source every speed route is 404.
	if b := do(loc, "GET", "/api/profile", speedHost).Body.String(); !strings.Contains(b, `"speed":true`) {
		t.Errorf("features: %s", b)
	}
	d.Speed = nil
	loc2 := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"})
	if w := do(loc2, "GET", "/api/speed", speedHost); w.Code != 404 {
		t.Errorf("no source: %d", w.Code)
	}
	if w := do(loc2, "POST", "/api/speed/run", speedPOSTHeaders()); w.Code != 404 {
		t.Errorf("no source run: %d", w.Code)
	}
	if b := do(loc2, "GET", "/api/profile", speedHost).Body.String(); !strings.Contains(b, `"speed":false`) {
		t.Errorf("features without a source: %s", b)
	}
}

func speedPOSTHeaders() map[string]string {
	return map[string]string{"Host": "127.0.0.1:3000", "Origin": "http://127.0.0.1:3000", "Sec-Fetch-Site": "same-origin", HeaderCSRF: "tok123"}
}

// speedSend is do with a body.
func speedSend(h http.Handler, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range hdr {
		if k == "Host" {
			r.Host = v
			continue
		}
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type speedRunResp struct {
	Entry     speedEntryJSON
	Duplicate bool
	Code      string
}

// TestSpeedWrites: the owner's Test now, sweeps and cancels (the queue is
// stopped: entries wait).
func TestSpeedWrites(t *testing.T) {
	g := newSpeedRigWith(t, 5*time.Hour, rigOpts{noRun: true})
	loc, d := speedHandlers(t, g, LocalOptions{})
	good := speedPOSTHeaders()
	run := func(path string) (int, speedRunResp) {
		w := do(loc, "POST", path, good)
		var r speedRunResp
		json.Unmarshal(w.Body.Bytes(), &r)
		return w.Code, r
	}
	if c, r := run("/api/speed/run?id=us-east"); c != 202 || r.Duplicate || r.Entry.Kind != "single" || r.Entry.Target == nil ||
		r.Entry.Target.ID != "us-east" || r.Entry.Target.Label != "US East" || r.Entry.Who != "you" || r.Entry.State != "waiting" || r.Entry.Position != 1 {
		t.Fatalf("run: %d %+v", c, r)
	}
	if c, r := run("/api/speed/run?kind=single&id=us-east"); c != 202 || !r.Duplicate || r.Entry.ID != 1 {
		t.Errorf("duplicate: %d %+v", c, r)
	}
	if c, r := run("/api/speed/run"); c != 202 || r.Duplicate || r.Entry.Target != nil || r.Entry.Position != 2 {
		t.Errorf("nearest: %d %+v", c, r)
	}
	if c, r := run("/api/speed/run?kind=sweep"); c != 202 || r.Entry.Kind != "sweep" || r.Entry.Steps != 6 || r.Entry.Origin != "owner" {
		t.Errorf("sweep: %d %+v", c, r)
	}
	for _, p := range []string{"/api/speed/run?kind=sweep&id=us-east", "/api/speed/run?kind=fast", "/api/speed/run?id=a&id=b", "/api/speed/run?server=x"} {
		if c, _ := run(p); c != 400 {
			t.Errorf("%s: %d", p, c)
		}
	}
	if c, _ := run("/api/speed/run?id=Nope"); c != 404 {
		t.Errorf("unknown id: %d", c)
	}
	// Cancel (admin-gated like the other speed controls).
	if w := do(loc, "POST", "/api/speed/cancel?id=2", good); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"cancelled"`) {
		t.Errorf("cancel: %d %s", w.Code, w.Body.String())
	}
	for _, p := range []string{"/api/speed/cancel", "/api/speed/cancel?id=x", "/api/speed/cancel?public=1"} {
		if w := do(loc, "POST", p, good); w.Code != 400 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if w := do(loc, "POST", "/api/speed/cancel?id=99", good); w.Code != 404 {
		t.Errorf("cancel unknown: %d", w.Code)
	}
	bad := map[string]string{"Host": "127.0.0.1:3000", "Origin": "http://evil.com", HeaderCSRF: "tok123"}
	for _, p := range []string{"/api/speed/run", "/api/speed/cancel?id=1"} {
		if w := do(loc, "POST", p, bad); w.Code != 403 {
			t.Errorf("%s cross-origin: %d", p, w.Code)
		}
	}
	if w := do(loc, "GET", "/api/speed/run", good); w.Code != 405 {
		t.Errorf("GET run: %d", w.Code)
	}
	// An admin token is required when set.
	adm := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123", AdminToken: "sekrit"})
	if w := do(adm, "POST", "/api/speed/run", good); w.Code != 403 {
		t.Errorf("without admin token: %d", w.Code)
	}
	var s speedJSON
	json.Unmarshal(do(adm, "GET", "/api/speed", speedHost).Body.Bytes(), &s)
	if s.Configurable {
		t.Error("configurable without the admin token")
	}
	withAdm := map[string]string{"X-FYISP-Admin": "sekrit"}
	for k, v := range good {
		withAdm[k] = v
	}
	if w := do(adm, "POST", "/api/speed/run?id=uk", withAdm); w.Code != 202 {
		t.Errorf("with admin token: %d %s", w.Code, w.Body.String())
	}
}

// TestSweepResults: a running sweep lists each server with its status and
// the results so far.
func TestSweepResults(t *testing.T) {
	g := newSpeedRig(t, 5*time.Hour)
	for range 4 { // two servers, both directions
		g.tokens <- struct{}{}
	}
	loc, _ := speedHandlers(t, g, LocalOptions{})
	w := do(loc, "POST", "/api/speed/run?kind=sweep", speedPOSTHeaders())
	var started struct{ Entry speedEntryJSON }
	if json.Unmarshal(w.Body.Bytes(), &started); w.Code != 202 {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	var e speedEntryJSON
	waitUntil(t, func() bool {
		e = speedEntryJSON{}
		json.Unmarshal(do(loc, "GET", fmt.Sprintf("/api/speed?part=entry&id=%d", started.Entry.ID), speedHost).Body.Bytes(), &e)
		return len(e.Results) == 6 && e.Results[2].Status == "testing"
	})
	want := []string{"ok", "ok", "testing", "waiting", "waiting", "waiting"}
	for i, r := range e.Results {
		if r.Status != want[i] || r.Server.ID == "" || r.Server.Label == "" || r.Display == "" {
			t.Errorf("result %d: %+v, want %s", i, r, want[i])
		}
	}
	if r := e.Results[0]; r.Test == nil || r.DownBps == nil || *r.DownBps != 160e6 || r.Region == nil || r.Provider == "" {
		t.Errorf("ok result: %+v", r)
	}
	var sw struct{ Running *speedEntryJSON }
	json.Unmarshal(do(loc, "GET", "/api/speed?part=sweep", speedHost).Body.Bytes(), &sw)
	if sw.Running == nil || len(sw.Running.Results) != 6 {
		t.Errorf("part=sweep running: %+v", sw.Running)
	}
	var run struct{ Running *speedRunning }
	json.Unmarshal(do(loc, "GET", "/api/speed?part=running", speedHost).Body.Bytes(), &run)
	if run.Running == nil || run.Running.Kind != "sweep" || run.Running.Step != 3 || run.Running.Steps != 6 || run.Running.Server == nil {
		t.Errorf("running %+v", run.Running)
	}
}

// TestSweepCancelledStaysReadable: the owner cancels a running sweep; the
// cancel says why, and part=sweep keeps it as the last sweep, its results
// so far, the rest skipped.
func TestSweepCancelledStaysReadable(t *testing.T) {
	g := newSpeedRig(t, 5*time.Hour)
	for range 4 { // two servers, both directions
		g.tokens <- struct{}{}
	}
	loc, _ := speedHandlers(t, g, LocalOptions{})
	w := do(loc, "POST", "/api/speed/run?kind=sweep", speedPOSTHeaders())
	var started struct{ Entry speedEntryJSON }
	if json.Unmarshal(w.Body.Bytes(), &started); w.Code != 202 {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	waitUntil(t, func() bool {
		var e speedEntryJSON
		json.Unmarshal(do(loc, "GET", fmt.Sprintf("/api/speed?part=entry&id=%d", started.Entry.ID), speedHost).Body.Bytes(), &e)
		return len(e.Results) == 6 && e.Results[2].Status == "testing"
	})
	w = do(loc, "POST", fmt.Sprintf("/api/speed/cancel?id=%d", started.Entry.ID), speedPOSTHeaders())
	var c struct{ Entry speedEntryJSON }
	if json.Unmarshal(w.Body.Bytes(), &c); w.Code != 200 || c.Entry.StoppedReason == nil || *c.Entry.StoppedReason != "cancelled" {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	var sw struct {
		Running *speedEntryJSON
		Entry   *speedEntryJSON
		Tests   []speedTestJSON
	}
	waitUntil(t, func() bool {
		sw.Running, sw.Entry, sw.Tests = nil, nil, nil
		json.Unmarshal(do(loc, "GET", "/api/speed?part=sweep", speedHost).Body.Bytes(), &sw)
		return sw.Entry != nil
	})
	e := sw.Entry
	if e.ID != started.Entry.ID || e.State != "cancelled" || e.StoppedReason == nil || *e.StoppedReason != "cancelled" || sw.Running != nil {
		t.Fatalf("part=sweep: %+v (running %+v)", e, sw.Running)
	}
	want := []string{"ok", "ok", "cancelled", "skipped", "skipped", "skipped"}
	for i, r := range e.Results {
		if r.Status != want[i] {
			t.Errorf("result %d: %s, want %s", i, r.Status, want[i])
		}
	}
	if len(e.Results) != 6 || len(sw.Tests) != 2 {
		t.Errorf("results %d, tests %d", len(e.Results), len(sw.Tests))
	}
}

func TestSpeedConfig(t *testing.T) {
	g := newSpeedRig(t, 5*time.Hour)
	loc, _ := speedHandlers(t, g, LocalOptions{})
	cfg := func(body string) *httptest.ResponseRecorder {
		return speedSend(loc, "POST", "/api/speed/config", body, speedPOSTHeaders())
	}
	if w := cfg(`{"mode": "off"}`); w.Code != 200 || g.s.State().Mode != "off" || !strings.Contains(w.Body.String(), `"next_at":null`) {
		t.Errorf("off: %d %s", w.Code, w.Body.String())
	}
	if w := cfg(`{"mode": "regions", "extra": ["us-mid-atlantic"]}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"next_at":17`) {
		t.Errorf("regions: %d %s", w.Code, w.Body.String())
	}
	var s speedJSON
	json.Unmarshal(do(loc, "GET", "/api/speed", speedHost).Body.Bytes(), &s)
	if i := slices.IndexFunc(s.Rows, func(r speedRow) bool { return r.Why == "added" }); len(s.Rows) != 3 || i < 0 || s.Rows[i].ID != "us-mid-atlantic" || len(g.s.State().Extra) != 1 {
		t.Errorf("added region: %d rows, extra %v", len(s.Rows), g.s.State().Extra)
	}
	if v, _, _ := g.st.Setting(context.Background(), speedtest.SettingKey); v != "regions" {
		t.Errorf("saved %q", v)
	}
	if w := cfg(`{"mode": "nearby"}`); w.Code != 200 || g.s.State().Mode != "nearby" {
		t.Errorf("nearby: %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{``, `{}`, `{"mode": true}`, `{"mode": "fast"}`, `{"enabled": true}`, `{"mode": "off", "x": 1}`,
		`{"mode": "regions", "extra": ["mars"]}`, `{"extra": ["canada"]}`, `{"mode": "nearest"}`, `{"public": true}`} {
		if w := cfg(body); w.Code != 400 {
			t.Errorf("%q: %d", body, w.Code)
		}
	}
}

// TestHomeSpeedLine: Home's speed block reads the stored speed tests
// (s.d.Speed): the latest, the usual of the ones before it, and whether
// tests are scheduled.
func TestHomeSpeedLine(t *testing.T) {
	home := func(g *speedRig) HomeSpeed {
		_, d := speedHandlers(t, g, LocalOptions{})
		d.Tiles = scenario(t, "good")
		loc := Local(d, LocalOptions{Addr: "127.0.0.1:3000"}).(*localHandler)
		loc.now = func() time.Time { return speedNow }
		var n HomeNow
		json.Unmarshal(do(loc, "GET", "/api/home?part=now", speedHost).Body.Bytes(), &n)
		return n.Speed
	}
	sp := home(newSpeedRig(t, 15*time.Minute))
	if !sp.Available || sp.Latest == nil || !strings.Contains(sp.Latest.Text, "usual") || !strings.HasPrefix(sp.Latest.Text, "Your speed: about ") ||
		!strings.Contains(sp.EmptyText, "within the hour") {
		t.Errorf("speed %+v %+v", sp, sp.Latest)
	}
	g := newSpeedRig(t, 15*time.Minute)
	g.s.SetMode(context.Background(), speedtest.ModeOff, nil)
	if sp := home(g); !sp.Available || strings.Contains(sp.EmptyText, "within the hour") {
		t.Errorf("speed while off: %+v", sp)
	}
}

// TestUnderLoadWords: the words match the numbers.
func TestUnderLoadWords(t *testing.T) {
	ls := func(p50 float64) *model.LoadStats { return &model.LoadStats{N: 30, P50Ms: p50, P95Ms: p50 * 1.5} }
	ts := []model.SpeedTest{{Outcome: model.SpeedOK, Latency: &model.SpeedLatency{Edge: model.LoadPhases{Idle: ls(9), Down: ls(36), Up: ls(117)}}}}
	got := speedUnderLoadOf(ts).Text
	want := []string{
		"Delay to your provider's first router rises from 9 to 36 ms while downloading (+27 ms): a small rise, fine for calls.",
		"Delay to your provider's first router rises from 9 to 117 ms while uploading (+108 ms): enough to break up calls while the line is busy." +
			" This usually builds up in the queue on your line (your modem, or the provider's first router), not further out.",
	}
	if !slices.Equal(got, want) {
		t.Errorf("%q", got)
	}
}

// TestCancelledWindow: a cancelled test's stored row (no speeds) keeps its
// masked minutes explained: its window is in part=windows.
func TestCancelledWindow(t *testing.T) {
	g := newSpeedRigWith(t, 5*time.Hour, rigOpts{noRun: true})
	g.st.SaveSpeedTest(context.Background(), &model.SpeedTest{Start: speedNow.Add(-36 * time.Minute), End: speedNow.Add(-29 * time.Minute),
		Trigger: "manual", Outcome: model.SpeedCancelled, Server: "Leaseweb-London"})
	loc, _ := speedHandlers(t, g, LocalOptions{})
	var w struct{ Windows []speedWindow }
	json.Unmarshal(do(loc, "GET", "/api/speed?part=windows&from=now-1h&to=now", speedHost).Body.Bytes(), &w)
	if !slices.ContainsFunc(w.Windows, func(w speedWindow) bool { return w.Start == speedNow.Add(-36*time.Minute).UnixMilli() }) {
		t.Errorf("windows %+v lack the cancelled test's", w.Windows)
	}
}

// TestWithVia: the exchange a handoff crossed is added only to tests whose
// handoff is the server's latest.
func TestWithVia(t *testing.T) {
	st := speedtest.State{Vias: map[string]model.Handoff{"A": {ASN: 60781, ISPASN: 3320, Via: "DE-CIX"}}}
	ts := []model.SpeedTest{
		{Server: "A", Handoff: &model.Handoff{ASN: 60781, ISPASN: 3320}},
		{Server: "A", Handoff: &model.Handoff{ASN: 3356, ISPASN: 3320}}, // an older route
		{Server: "B", Handoff: &model.Handoff{ASN: 60781, ISPASN: 3320}},
		{Server: "A"},
	}
	got := withVia(ts, st)
	if got[0].Handoff.Via != "DE-CIX" || got[1].Handoff.Via != "" || got[2].Handoff.Via != "" || got[3].Handoff != nil || ts[0].Handoff.Via != "" {
		t.Errorf("via: %+v %+v %+v", got[0].Handoff, got[1].Handoff, got[2].Handoff)
	}
}

// TestSweepBytes: "Test every server" is estimated from each server's
// recent median data, else the line's rate both ways for MaxMeasure;
// skipped servers are left out.
func TestSweepBytes(t *testing.T) {
	st := speedtest.State{Servers: []speedtest.ServerState{
		{Server: speedtest.Server{ID: "A"}}, {Server: speedtest.Server{ID: "B"}}, {Server: speedtest.Server{ID: "C"}, Skipped: true}}}
	d, u := 80e6, 40e6
	ts := []model.SpeedTest{
		{Server: "A", Outcome: model.SpeedOK, Bytes: 100e6, DownBps: &d, UpBps: &u, Trigger: "schedule"},
		{Server: "A", Outcome: model.SpeedOK, Bytes: 300e6, DownBps: &d, UpBps: &u, Trigger: "manual"},
		{Server: "B", Outcome: model.SpeedBusy, Bytes: 1e3, Trigger: "schedule"},
	}
	if b, n := sweepBytes(st, ts); n != 2 || b != 200e6+150e6 {
		t.Errorf("%d bytes over %d servers", b, n)
	}
	if b, _ := sweepBytes(st, nil); b != 2*defaultTestBytes {
		t.Errorf("no tests: %d", b)
	}
}
