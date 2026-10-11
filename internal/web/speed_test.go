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
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/fuck-you-isp/fyisp/internal/store"
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

// rigOpts: the share link's opt-in and budget, the bytes each direction
// moves, and noRun to leave the queue stopped (entries stay waiting).
type rigOpts struct {
	public bool
	budget int64
	wire   int64
	noRun  bool
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
				x.IdleGatewayMs, x.LoadedGatewayMs, x.Conn = f(1.1), f(1.5), model.ConnWired
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
		Public: &ro.public, PublicBudget: ro.budget,
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

func speedHandlers(t *testing.T, g *speedRig, o LocalOptions) (*localHandler, *publicHandler, Deps) {
	_, d, _ := fixture(t, speedNow)
	d.Speed = g.s
	if o.Addr == "" {
		o.Addr = "127.0.0.1:3000"
	}
	o.CSRFToken = "tok123"
	loc := Local(d, o).(*localHandler)
	loc.now = func() time.Time { return speedNow }
	pub := Public(d, "0123456789abcdef0123456789abcdef").(*publicHandler)
	pub.now = func() time.Time { return speedNow }
	return loc, pub, d
}

const speedPub = "/s/0123456789abcdef0123456789abcdef"

var speedHost = map[string]string{"Host": "127.0.0.1:3000"}

// TestSpeedGolden pins api/speed's summary on both listeners (7 days, a
// 2-hour time zone; a test every 5 hours keeps the files small): go test
// ./internal/web -run TestSpeedGolden -update.
func TestSpeedGolden(t *testing.T) {
	loc, pub, _ := speedHandlers(t, newSpeedRig(t, 5*time.Hour), LocalOptions{})
	for name, raw := range map[string]string{
		"local":  do(loc, "GET", "/api/speed?days=7&tz=120", speedHost).Body.String(),
		"public": do(pub, "GET", speedPub+"/api/speed?days=7&tz=120", nil).Body.String(),
	} {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("%s: %v: %s", name, err, raw)
		}
		got, _ := json.MarshalIndent(v, "", " ")
		got = append(got, '\n')
		// The share link speaks about someone else's line: no "your" (the
		// own-server label "Your server" aside).
		if name == "public" {
			if l := strings.ReplaceAll(strings.ToLower(string(got)), "your server", ""); strings.Contains(l, "your") {
				i := strings.Index(l, "your")
				t.Errorf("public speed JSON addresses the viewer: …%s…", l[max(0, i-80):min(len(l), i+40)])
			}
			// It never tells tests apart, nor names networks or exchanges.
			if m := regexp.MustCompile(`"public"|public_sweep|"manual"|"sweep"|"via"|DE-CIX|AS[0-9]|"network"`).FindString(string(got)); m != "" {
				t.Errorf("public speed JSON has %s", m)
			}
		}
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
	loc, pub, d := speedHandlers(t, g, LocalOptions{})
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
	if s.Finding.Case != speedtest.CaseISP || !strings.Contains(s.Finding.Text, "your internet provider") || s.Finding.Evidence == "" {
		t.Errorf("finding %+v", s.Finding)
	}
	if s.Coverage[20].N < 30 || s.Coverage[20].Providers != 2 {
		t.Errorf("coverage %+v", s.Coverage[20])
	}
	if len(s.UnderLoad.Layers) != 2 || s.UnderLoad.Layers[0].Layer != "edge" || *s.UnderLoad.Layers[0].UpMs != 48 || len(s.UnderLoad.Text) != 3 ||
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
	// The share link: labels and catalog ids, never hosts, ports, data use
	// or the owner's wording; the own server is custom-1.
	b := do(pub, "GET", speedPub+"/api/speed?days=30", nil).Body.String()
	for _, leak := range []string{"192.168.1.20", "clouvider.net", "leaseweb.net", `"port"`, `"host"`, `"ports"`, `"configurable":true`, "your internet provider", "your daytime", "your provider", "LEVEL3", "3320"} {
		if strings.Contains(b, leak) {
			t.Errorf("share link leaks %s", leak)
		}
	}
	for _, want := range []string{`"id":"custom-1"`, `"label":"Your server"`, `"label":"Leaseweb New York"`, `"Clouvider-London"`, `"bytes_month":0`} {
		if !strings.Contains(b, want) {
			t.Errorf("share link lacks %s", want)
		}
	}
	// One row: a region, a server (local id, and the public custom id).
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
	json.Unmarshal(do(pub, "GET", speedPub+"/api/speed?server=custom-1&days=30", nil).Body.Bytes(), &one)
	if one.Kind != "server" || one.Row.ID != "custom-1" || len(one.Tests) == 0 || one.Tests[0].Server != "custom-1" || one.Tests[0].Port != 0 || !one.Tests[0].OnDemand {
		t.Errorf("public custom server: %+v", one.Row)
	}
	if w := do(pub, "GET", speedPub+"/api/speed?server=192.168.1.20:5201", nil); w.Code != 404 {
		t.Errorf("own server by host on the share link: %d", w.Code)
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

func speedVisitor(ip string) map[string]string {
	return map[string]string{"Host": "x.trycloudflare.com", "Origin": "https://x.trycloudflare.com", "Sec-Fetch-Site": "same-origin", "Cf-Connecting-Ip": ip}
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
	loc, _, d := speedHandlers(t, g, LocalOptions{})
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
	// Cancel (admin-gated like notes): an entry, then the share link's.
	if w := do(loc, "POST", "/api/speed/cancel?id=2", good); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"cancelled"`) {
		t.Errorf("cancel: %d %s", w.Code, w.Body.String())
	}
	if w := do(loc, "POST", "/api/speed/cancel?public=1", good); w.Code != 200 || !strings.Contains(w.Body.String(), `"cancelled":0`) {
		t.Errorf("clear public: %d %s", w.Code, w.Body.String())
	}
	for _, p := range []string{"/api/speed/cancel", "/api/speed/cancel?id=x", "/api/speed/cancel?id=1&public=1"} {
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
	// The share link has its own run route; never config or cancel.
	pub := Public(d, "0123456789abcdef0123456789abcdef")
	for _, p := range []string{"/api/speed/config", "/api/speed/cancel?id=1"} {
		if w := do(pub, "POST", speedPub+p, nil); w.Code != 404 && w.Code != 405 {
			t.Errorf("share link %s: %d", p, w.Code)
		}
	}
}

// TestSpeedPublicQueue: the share link queues tests and sweeps (same
// origin, no body, per-visitor caps and start limits, never cached), sees
// "you" for its own entries and "someone" for everyone else's, and never
// an address, a host or another visitor's key.
func TestSpeedPublicQueue(t *testing.T) {
	g := newSpeedRigWith(t, 5*time.Hour, rigOpts{public: true, noRun: true})
	loc, pub, _ := speedHandlers(t, g, LocalOptions{})
	now := speedNow
	pub.now = func() time.Time { return now }
	post := func(path string, hdr map[string]string) (*httptest.ResponseRecorder, speedRunResp) {
		w := do(pub, "POST", speedPub+path, hdr)
		var r speedRunResp
		json.Unmarshal(w.Body.Bytes(), &r)
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control %q", path, cc)
		}
		return w, r
	}
	for name, c := range map[string]struct {
		path string
		hdr  map[string]string
		code int
		ec   string
	}{
		"cross-origin": {"/api/speed/run", map[string]string{"Host": "x.trycloudflare.com", "Origin": "https://evil.com", "Cf-Connecting-Ip": "1.1.1.1"}, 403, "forbidden"},
		"bad kind":     {"/api/speed/run?kind=all", speedVisitor("1.1.1.1"), 400, "bad_request"},
		"sweep and id": {"/api/speed/run?kind=sweep&id=us-east", speedVisitor("1.1.1.1"), 400, "bad_request"},
		"other param":  {"/api/speed/run?host=8.8.8.8", speedVisitor("1.1.1.1"), 400, "bad_request"},
		"unknown id":   {"/api/speed/run?id=8.8.8.8", speedVisitor("1.1.1.1"), 404, "not_found"},
		"local id":     {"/api/speed/run?id=192.168.1.20:5201", speedVisitor("1.1.1.1"), 404, "not_found"},
	} {
		if w, r := post(c.path, c.hdr); w.Code != c.code || r.Code != c.ec {
			t.Errorf("%s: %d %q (%s)", name, w.Code, r.Code, w.Body.String())
		}
	}
	if w := do(pub, "GET", speedPub+"/api/speed/run", speedVisitor("1.1.1.1")); w.Code != 405 {
		t.Errorf("GET run: %d", w.Code)
	}
	// With speed tests off the share link can't ask.
	g.s.SetMode(context.Background(), speedtest.ModeOff, nil)
	if w, r := post("/api/speed/run", speedVisitor("1.1.1.1")); w.Code != 403 || r.Code != "speed_off" {
		t.Errorf("off: %d %+v", w.Code, r)
	}
	if _, q := speedQueueOf(t, pub, speedVisitor("1.1.1.1")); q.SpeedOn || q.PublicStart {
		t.Errorf("off: %+v", q)
	}
	g.s.SetMode(context.Background(), speedtest.ModeRegions, nil)
	if w := speedSend(pub, "POST", speedPub+"/api/speed/run", "kind=sweep", speedVisitor("1.1.1.1")); w.Code != 400 {
		t.Errorf("body: %d", w.Code)
	}
	// A sweep from visitor 1, a single (own server, by its public id) from 2.
	if w, r := post("/api/speed/run?kind=sweep", speedVisitor("1.1.1.1")); w.Code != 202 || r.Duplicate || r.Entry.Who != "you" || r.Entry.Kind != "sweep" {
		t.Fatalf("sweep: %d %+v", w.Code, r)
	}
	if w, r := post("/api/speed/run?id=custom-1", speedVisitor("2.2.2.2")); w.Code != 202 || r.Entry.Target == nil || r.Entry.Target.ID != "custom-1" {
		t.Fatalf("own server: %d %s", w.Code, w.Body.String())
	}
	if w, r := post("/api/speed/run?kind=sweep", speedVisitor("1.1.1.1")); w.Code != 202 || !r.Duplicate {
		t.Errorf("again: %d %+v", w.Code, r)
	}
	// One waiting entry per visitor; IPv6 visitors per /64.
	if w, r := post("/api/speed/run?id=us-east", speedVisitor("1.1.1.1")); w.Code != 429 || r.Code != "queue_full" {
		t.Errorf("second entry: %d %+v", w.Code, r)
	}
	if w, _ := post("/api/speed/run?id=us-east", speedVisitor("2001:db8:1:2::1")); w.Code != 202 {
		t.Errorf("v6: %d", w.Code)
	}
	if w, r := post("/api/speed/run?id=Leaseweb-NewYork", speedVisitor("2001:db8:1:2::99")); w.Code != 429 || r.Code != "queue_full" {
		t.Errorf("same /64: %d %+v", w.Code, r)
	}
	// The owner goes first.
	lw := do(loc, "POST", "/api/speed/run?id=Leaseweb-NewYork", speedPOSTHeaders())
	var lr speedRunResp
	json.Unmarshal(lw.Body.Bytes(), &lr)
	if lw.Code != 202 || lr.Entry.Position != 1 || lr.Entry.Who != "you" {
		t.Errorf("owner: %d %+v", lw.Code, lr)
	}
	// The start limit: after the owner clears the share link's entries, a
	// visitor can't start again at once.
	if w := do(loc, "POST", "/api/speed/cancel?public=1", speedPOSTHeaders()); w.Code != 200 || !strings.Contains(w.Body.String(), `"cancelled":3`) {
		t.Errorf("clear: %d %s", w.Code, w.Body.String())
	}
	if w, r := post("/api/speed/run?id=us-east", speedVisitor("1.1.1.1")); w.Code != 429 || r.Code != "rate_limited" || w.Header().Get("Retry-After") == "" {
		t.Errorf("start limit: %d %+v", w.Code, r)
	}
	now = now.Add(31 * time.Second)
	if w, _ := post("/api/speed/run?id=us-east", speedVisitor("1.1.1.1")); w.Code != 202 {
		t.Errorf("after 30 s: %d", w.Code)
	}
	// The queue as each viewer sees it.
	get := func(h http.Handler, path string, hdr map[string]string) (string, speedQueueJSON) {
		w := do(h, "GET", path, hdr)
		var q speedQueueJSON
		json.Unmarshal(w.Body.Bytes(), &q)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d %q", path, w.Code, w.Header().Get("Cache-Control"))
		}
		return w.Body.String(), q
	}
	body, q1 := get(pub, speedPub+"/api/speed?part=queue", speedVisitor("1.1.1.1"))
	if len(q1.Waiting) != 2 || q1.Waiting[0].Who != "someone" || q1.Waiting[1].Who != "you" || q1.Mine == nil || *q1.Mine != q1.Waiting[1].ID || q1.Full {
		t.Errorf("visitor 1: %s", body)
	}
	_, q3 := get(pub, speedPub+"/api/speed?part=queue", speedVisitor("3.3.3.3"))
	if q3.Mine != nil || q3.Waiting[1].Who != "someone" {
		t.Errorf("visitor 3: %+v", q3)
	}
	for _, leak := range []string{"1.1.1.1", "2001:db8", "192.168.1.20", "leaseweb.net", "clouvider.net", "visitor", `"origin"`} {
		if strings.Contains(body, leak) {
			t.Errorf("public queue shows %q: %s", leak, body)
		}
	}
	lbody, ql := get(loc, "/api/speed?part=queue", speedHost)
	if ql.Waiting[0].Who != "you" || ql.Waiting[1].Who != "someone" || ql.Waiting[1].Origin != "public" || strings.Contains(lbody, "1.1.1.1") {
		t.Errorf("owner's view: %s", lbody)
	}
	if w := do(pub, "GET", fmt.Sprintf("%s/api/speed?part=entry&id=%d", speedPub, q1.Waiting[1].ID), speedVisitor("1.1.1.1")); w.Code != 200 || !strings.Contains(w.Body.String(), `"who":"you"`) {
		t.Errorf("entry: %d %s", w.Code, w.Body.String())
	}
	if w := do(pub, "GET", speedPub+"/api/speed?part=entry&id=999", speedVisitor("1.1.1.1")); w.Code != 404 {
		t.Errorf("unknown entry: %d", w.Code)
	}
	if w := do(pub, "GET", speedPub+"/api/speed?part=sweep", speedVisitor("1.1.1.1")); w.Code != 200 || !strings.Contains(w.Body.String(), `"entry":null`) {
		t.Errorf("sweep: %d %s", w.Code, w.Body.String())
	}
	if w := do(pub, "GET", speedPub+"/api/speed?part=queue&id=1", speedVisitor("1.1.1.1")); w.Code != 400 {
		t.Errorf("id outside part=entry: %d", w.Code)
	}
}

func speedQueueOf(t *testing.T, pub *publicHandler, hdr map[string]string) (string, speedQueueJSON) {
	t.Helper()
	w := do(pub, "GET", speedPub+"/api/speed?part=queue", hdr)
	var q speedQueueJSON
	json.Unmarshal(w.Body.Bytes(), &q)
	return w.Body.String(), q
}

// TestSpeedPublicOptInBudget: the share link's tests are an owner opt-in
// (403 disabled; the Details toggle turns them on), and spend a 24-hour
// data budget counted on the wire (429 budget_exhausted with resets_at).
func TestSpeedPublicOptInBudget(t *testing.T) {
	g := newSpeedRigWith(t, 5*time.Hour, rigOpts{budget: 500e6, wire: 90e6})
	close(g.release)
	loc, pub, _ := speedHandlers(t, g, LocalOptions{})
	queue := func(h http.Handler, path string, hdr map[string]string) speedQueueJSON {
		var q speedQueueJSON
		json.Unmarshal(do(h, "GET", path, hdr).Body.Bytes(), &q)
		return q
	}
	w := do(pub, "POST", speedPub+"/api/speed/run?id=us-east", speedVisitor("1.1.1.1"))
	if w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"disabled"`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("off: %d %s", w.Code, w.Body.String())
	}
	if q := queue(pub, speedPub+"/api/speed?part=queue", speedVisitor("1.1.1.1")); q.PublicStart || q.PublicEnabled {
		t.Errorf("off: %+v", q)
	}
	cw := speedSend(loc, "POST", "/api/speed/config", `{"public": true}`, speedPOSTHeaders())
	if cw.Code != 200 || !strings.Contains(cw.Body.String(), `"public":true`) || !strings.Contains(cw.Body.String(), `"time_hour_max_s":600`) {
		t.Fatalf("toggle: %d %s", cw.Code, cw.Body.String())
	}
	for i, ip := range []string{"1.1.1.1", "2.2.2.2"} {
		if w := do(pub, "POST", speedPub+"/api/speed/run?id=us-east", speedVisitor(ip)); w.Code != 202 {
			t.Fatalf("start %d: %d %s", i, w.Code, w.Body.String())
		}
		waitUntil(t, func() bool {
			return queue(loc, "/api/speed?part=queue", speedHost).Budget.UsedBytes >= int64(i+1)*180e6
		})
	}
	q := queue(pub, speedPub+"/api/speed?part=queue", speedVisitor("3.3.3.3"))
	// 140 MB left: less than a whole test (210 MB), so no more start.
	if q.PublicStart || !q.PublicEnabled || q.Budget.LimitBytes != 500e6 || q.Budget.UsedBytes != 360e6 || q.Budget.RemainingBytes != 140e6 ||
		q.Budget.TestMaxBytes != 210e6 || q.Budget.FitsTests != 0 || q.Budget.ResetsAt == nil || q.PublicOffReason != nil {
		t.Errorf("spent: %+v", q)
	}
	w = do(pub, "POST", speedPub+"/api/speed/run?kind=sweep", speedVisitor("3.3.3.3"))
	var e struct {
		Code       string
		ResetsAt   int64 `json:"resets_at"`
		RetryAfter int   `json:"retry_after"`
	}
	json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != 429 || e.Code != "budget_exhausted" || e.ResetsAt != *q.Budget.ResetsAt || e.RetryAfter <= 0 || w.Header().Get("Retry-After") == "" {
		t.Errorf("budget: %d %s", w.Code, w.Body.String())
	}
	ts, _ := g.st.SpeedTests(context.Background(), speedNow.Add(-time.Minute), speedNow.Add(time.Minute))
	if n := len(ts); n == 0 || ts[n-1].Trigger != "public" || ts[n-1].Origin() != "public" {
		t.Errorf("stored %+v", ts)
	}
}

// TestSpeedPublicBudgetTooSmall: a budget under one test (150 MB < 210 MB)
// turns the share link's tests off: 403 disabled with reason
// budget_too_small, and part=queue says why.
func TestSpeedPublicBudgetTooSmall(t *testing.T) {
	g := newSpeedRigWith(t, 5*time.Hour, rigOpts{budget: 150e6})
	loc, pub, _ := speedHandlers(t, g, LocalOptions{})
	if cw := speedSend(loc, "POST", "/api/speed/config", `{"public": true}`, speedPOSTHeaders()); cw.Code != 200 {
		t.Fatalf("toggle: %d %s", cw.Code, cw.Body.String())
	}
	w := do(pub, "POST", speedPub+"/api/speed/run?id=us-east", speedVisitor("1.1.1.1"))
	var e struct{ Code, Reason string }
	if json.Unmarshal(w.Body.Bytes(), &e); w.Code != 403 || e.Code != "budget_too_small" || e.Reason != "budget_too_small" {
		t.Errorf("run: %d %s", w.Code, w.Body.String())
	}
	for _, v := range []struct {
		h    http.Handler
		path string
		hdr  map[string]string
	}{{loc, "/api/speed?part=queue", speedHost}, {pub, speedPub + "/api/speed?part=queue", speedVisitor("1.1.1.1")}} {
		var q speedQueueJSON
		json.Unmarshal(do(v.h, "GET", v.path, v.hdr).Body.Bytes(), &q)
		if q.PublicStart || !q.PublicEnabled || q.PublicOffReason == nil || *q.PublicOffReason != "budget_too_small" ||
			q.Budget.LimitBytes != 150e6 || q.Budget.TestMaxBytes != 210e6 || q.Budget.FitsTests != 0 {
			t.Errorf("%s: %+v", v.path, q)
		}
	}
}

// newOwnServerRig: mode custom on one own server (10.77.66.55:5333) with a
// week of scheduled tests on it, so the summary's rows, hours, server_now
// and next_row all name it.
func newOwnServerRig(t *testing.T) *speedRig {
	t.Helper()
	ctx := context.Background()
	cat := speedCatalog(t)
	custom, err := speedtest.Custom(ownServer, cat)
	if err != nil {
		t.Fatal(err)
	}
	g := &speedRig{st: &speedtest.MemStore{}, release: make(chan struct{}), tokens: make(chan struct{}, 100)}
	g.st.SetSetting(ctx, speedtest.SettingKey, "custom")
	f := func(v float64) *float64 { return &v }
	i := 0
	for at := speedNow.Add(-7 * 24 * time.Hour); at.Before(speedNow); at = at.Add(3 * time.Hour) {
		i++
		x := model.SpeedTest{Start: at, End: at.Add(12 * time.Second), Trigger: "schedule", Outcome: model.SpeedOK, Server: ownServer,
			Port: 5333, Attempts: 1, FallbackLevel: speedtest.FallbackNone, DownSecs: 4, UpSecs: 3, Bytes: 150e6}
		switch {
		case i%9 == 4:
			x.Outcome, x.Bytes = model.SpeedBusy, 0
		case i%11 == 6:
			x.Outcome, x.Trigger, x.Bytes = model.SpeedCancelled, "manual", 40e6
		default:
			sc := 1.0
			if h := at.Hour(); h >= 19 && h <= 22 {
				sc = 0.6
			}
			down, up := 500e6*sc, 50e6*sc
			x.DownBps, x.UpBps, x.ScoreDown, x.ScoreUp = &down, &up, f(sc), f(sc)
		}
		g.st.SaveSpeedTest(ctx, &x)
	}
	on := true
	g.s = speedtest.New(speedtest.Options{Catalog: cat, Custom: custom, Store: g.st, Now: func() time.Time { return speedNow }, Public: &on})
	if _, err := g.s.SetMode(ctx, speedtest.ModeCustom, nil); err != nil {
		t.Fatal(err)
	}
	return g
}

const ownServer = "10.77.66.55:5333"

// TestSpeedPublicOwnServer: the share link never names the owner's own
// server (no host, no port) in any of its responses: it is custom-1 in the
// rows, their hours, server_now, next_row, the tests, the queue and the
// windows. Locally it keeps its address. The public summary is pinned in
// testdata/speed/public-own.json (-update).
func TestSpeedPublicOwnServer(t *testing.T) {
	g := newOwnServerRig(t)
	loc, pub, _ := speedHandlers(t, g, LocalOptions{})
	// Locally the fields name it, so the share link's checks below mean something.
	l := do(loc, "GET", "/api/speed?days=7&tz=120", speedHost).Body.String()
	for _, want := range []string{`"next_row":"` + ownServer + `"`, `"server_now":"` + ownServer + `"`, `"` + ownServer + `":`} {
		if !strings.Contains(l, want) {
			t.Errorf("local summary lacks %s", want)
		}
	}
	if w := do(pub, "POST", speedPub+"/api/speed/run?id=custom-1", speedVisitor("1.1.1.1")); w.Code != 202 {
		t.Fatalf("public run: %d %s", w.Code, w.Body.String())
	}
	leak := regexp.MustCompile(`10\.77\.66\.55|5333`)
	for _, path := range []string{
		"/api/speed?days=7&tz=120", "/api/speed?days=30", "/api/speed?server=custom-1&days=7", "/api/speed?server=custom-1&days=30&tz=120",
		"/api/speed?part=windows&from=now-7d&to=now", "/api/speed?part=queue", "/api/speed?part=sweep", "/api/speed?part=running",
		"/api/speed?part=entry&id=1", "/api/home?part=live&voice=public", "/api/home?part=history&span=w&voice=public", fmt.Sprintf("/api/home?part=at&at=%d&voice=public", speedNow.Add(-time.Hour).UnixMilli()),
	} {
		w := do(pub, "GET", speedPub+path, speedVisitor("1.1.1.1"))
		if w.Code != 200 {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
			continue
		}
		if m := leak.FindStringIndex(w.Body.String()); m != nil {
			b := w.Body.String()
			t.Errorf("%s names the own server: …%s…", path, b[max(0, m[0]-80):min(len(b), m[1]+20)])
		}
	}
	p := do(pub, "GET", speedPub+"/api/speed?days=7&tz=120", nil).Body.String()
	for _, want := range []string{`"next_row":"custom-1"`, `"server_now":"custom-1"`, `"custom-1":`} {
		if !strings.Contains(p, want) {
			t.Errorf("public summary lacks %s", want)
		}
	}
	var v any
	json.Unmarshal([]byte(p), &v)
	got, _ := json.MarshalIndent(v, "", " ")
	got = append(got, '\n')
	path := filepath.Join("testdata", "speed", "public-own.json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from the golden file; run go test ./internal/web -run TestSpeedPublicOwnServer -update and review the diff", path)
	}
}

// TestSweepResults: a running sweep lists each server with its status and
// the results so far.
func TestSweepResults(t *testing.T) {
	g := newSpeedRig(t, 5*time.Hour)
	for range 4 { // two servers, both directions
		g.tokens <- struct{}{}
	}
	loc, _, _ := speedHandlers(t, g, LocalOptions{})
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
	loc, _, _ := speedHandlers(t, g, LocalOptions{})
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

// TestQueueRefusals: every share-link refusal's code and fields.
func TestQueueRefusals(t *testing.T) {
	now := speedNow
	for _, tc := range []struct {
		err    error
		status int
		want   []string
	}{
		{speedtest.ErrPublicDisabled, 403, []string{`"code":"disabled"`}},
		{&speedtest.QueueFullError{Reason: "the queue is full"}, 429, []string{`"code":"queue_full"`}},
		{&speedtest.BudgetError{ResetsAt: now.Add(time.Hour)}, 429, []string{`"code":"budget_exhausted"`, `"resets_at":`, `"retry_after":3600`}},
		{&speedtest.BudgetError{ResetsAt: now.Add(time.Minute), Time: true}, 429, []string{`"code":"time_cap"`, `"resets_at":`, `"retry_after":60`}},
		{&speedtest.SweepWaitError{Until: now.Add(30 * time.Minute)}, 429, []string{`"code":"cooldown"`, `"resets_at":`, `"retry_after":1800`}},
	} {
		w := httptest.NewRecorder()
		if !queueRefused(w, httptest.NewRequest("POST", "/", nil), tc.err, now) || w.Code != tc.status {
			t.Errorf("%v: %d", tc.err, w.Code)
		}
		for _, x := range tc.want {
			if !strings.Contains(w.Body.String(), x) {
				t.Errorf("%v: %s lacks %s", tc.err, w.Body.String(), x)
			}
		}
	}
	if queueRefused(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil), nil, now) {
		t.Error("nil refused")
	}
}

func TestSpeedConfig(t *testing.T) {
	g := newSpeedRig(t, 5*time.Hour)
	loc, _, _ := speedHandlers(t, g, LocalOptions{})
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
	if i := slices.IndexFunc(s.Rows, func(r speedRow) bool { return r.Why == "added" }); len(s.Rows) != 3 || i < 0 || s.Rows[i].ID != "us-mid-atlantic" || len(s.Extra) != 1 {
		t.Errorf("added region: %d rows, extra %v", len(s.Rows), s.Extra)
	}
	if v, _, _ := g.st.Setting(context.Background(), speedtest.SettingKey); v != "regions" {
		t.Errorf("saved %q", v)
	}
	if w := cfg(`{"mode": "nearby", "public": true}`); w.Code != 200 || g.s.State().Mode != "nearby" || !g.s.Queue().PublicEnabled() {
		t.Errorf("nearby: %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{``, `{}`, `{"mode": true}`, `{"mode": "fast"}`, `{"enabled": true}`, `{"mode": "off", "x": 1}`,
		`{"mode": "regions", "extra": ["mars"]}`, `{"extra": ["canada"]}`, `{"mode": "nearest"}`} {
		if w := cfg(body); w.Code != 400 {
			t.Errorf("%q: %d", body, w.Code)
		}
	}
}

// TestHomeSpeedLine: Home's line from the finding; none while off.
func TestHomeSpeedLine(t *testing.T) {
	loc, _, _ := speedHandlers(t, newSpeedRig(t, 15*time.Minute), LocalOptions{})
	var l HomeLive
	json.Unmarshal(do(loc, "GET", "/api/home?part=live", speedHost).Body.Bytes(), &l)
	if l.Speed == nil || l.Speed.Kind != "pattern" || l.Speed.Impact != "ok" || !strings.HasPrefix(l.Speed.Text, "Speed: evenings are about 40% slower than daytime") {
		t.Errorf("speed line %+v", l.Speed)
	}
	g := newSpeedRig(t, 15*time.Minute)
	g.s.SetMode(context.Background(), speedtest.ModeOff, nil)
	loc, _, _ = speedHandlers(t, g, LocalOptions{})
	l = HomeLive{}
	json.Unmarshal(do(loc, "GET", "/api/home?part=live", speedHost).Body.Bytes(), &l)
	if l.Speed != nil {
		t.Errorf("speed line while off: %+v", l.Speed)
	}
}

// TestSpeedEdgeHours: api/speed's edge_hours folds the ISP edge's stored
// RTT and loss into local hours of day (the access network's control).
func TestSpeedEdgeHours(t *testing.T) {
	loc, pub, d := speedHandlers(t, newSpeedRig(t, 5*time.Hour), LocalOptions{})
	f := store.NewFake()
	p := testProfile()
	p.Targets = append(p.Targets, model.Target{Name: "ISP edge", Host: model.HostEdge, Group: p.Targets[0].Group, Layer: model.LayerEdge,
		Kinds: []model.ProbeKind{model.KindICMP}, Interval: 3 * time.Second})
	edge := model.SeriesKey{Target: "ISP edge", Kind: model.KindICMP}
	for ts := speedNow.Add(-3 * 24 * time.Hour).Truncate(time.Hour); ts.Before(speedNow); ts = ts.Add(time.Minute) {
		s := model.Sample{Key: edge, Slot: ts, RTT: 8 * time.Millisecond}
		if h := ts.UTC().Hour(); h == 19 {
			s.RTT = 30 * time.Millisecond // the evening: the access network is busy
			if ts.Minute()%10 == 0 {
				s.Lost, s.Reason, s.RTT = true, model.ReasonTimeout, 0
			}
		}
		f.Observe(s)
	}
	d.Store, d.Profile = f, func() *model.Profile { return p }
	loc, pub = Local(d, LocalOptions{Addr: "127.0.0.1:3000"}).(*localHandler), Public(d, "0123456789abcdef0123456789abcdef").(*publicHandler)
	loc.now, pub.now = func() time.Time { return speedNow }, func() time.Time { return speedNow }
	for name, raw := range map[string][]byte{
		"local":  do(loc, "GET", "/api/speed?tz=120", speedHost).Body.Bytes(),
		"public": do(pub, "GET", speedPub+"/api/speed?tz=120", nil).Body.Bytes(),
	} {
		var s speedJSON
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		if len(s.EdgeHours) != 24 {
			t.Fatalf("%s: edge_hours %+v", name, s.EdgeHours)
		}
		ev, day := s.EdgeHours[21], s.EdgeHours[10] // 19 UTC is 21 at UTC+2
		if ev.N != 3 || ev.RTTms == nil || *ev.RTTms != 30 || ev.Loss == nil || *ev.Loss != 0.1 {
			t.Errorf("%s: evening %+v (rtt %v loss %v), want 3 days, 30 ms, 10%% loss", name, ev, ev.RTTms, ev.Loss)
		}
		if day.N < 2 || day.RTTms == nil || *day.RTTms != 8 || *day.Loss != 0 {
			t.Errorf("%s: daytime %+v", name, day)
		}
	}
	// Without an ISP edge target: null.
	var s map[string]json.RawMessage
	_, _, d2 := speedHandlers(t, newSpeedRig(t, 5*time.Hour), LocalOptions{})
	l2 := Local(d2, LocalOptions{Addr: "127.0.0.1:3000"}).(*localHandler)
	l2.now = func() time.Time { return speedNow }
	if err := json.Unmarshal(do(l2, "GET", "/api/speed", speedHost).Body.Bytes(), &s); err != nil || string(s["edge_hours"]) != "null" {
		t.Errorf("edge_hours without an edge target: %s %v", s["edge_hours"], err)
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
// masked minutes explained: its window is in part=windows, and Home's
// history shows no "fyisp wasn't running" there.
func TestCancelledWindow(t *testing.T) {
	offAt := func(withRow bool) (off bool, windows []speedWindow) {
		g := newSpeedRigWith(t, 5*time.Hour, rigOpts{noRun: true})
		if withRow {
			// The fixture's probes are "not measured" 30-35 minutes ago.
			g.st.SaveSpeedTest(context.Background(), &model.SpeedTest{Start: speedNow.Add(-36 * time.Minute), End: speedNow.Add(-29 * time.Minute),
				Trigger: "manual", Outcome: model.SpeedCancelled, Server: "Leaseweb-London"})
		}
		loc, _, _ := speedHandlers(t, g, LocalOptions{})
		var h HomeHistory
		json.Unmarshal(do(loc, "GET", "/api/home?part=history&span=d", speedHost).Body.Bytes(), &h)
		from, to := speedNow.Add(-35*time.Minute).UnixMilli(), speedNow.Add(-30*time.Minute).UnixMilli()
		for _, d := range h.Days {
			for _, sp := range d.Off {
				if sp.From < to && sp.To > from {
					off = true
				}
			}
		}
		var w struct{ Windows []speedWindow }
		json.Unmarshal(do(loc, "GET", "/api/speed?part=windows&from=now-1h&to=now", speedHost).Body.Bytes(), &w)
		return off, w.Windows
	}
	if off, _ := offAt(false); !off {
		t.Fatal("the fixture's unmeasured minutes don't show as not running: the test proves nothing")
	}
	off, ws := offAt(true)
	if off {
		t.Error("a cancelled test's window shows as \"fyisp wasn't running\"")
	}
	if !slices.ContainsFunc(ws, func(w speedWindow) bool { return w.Start == speedNow.Add(-36*time.Minute).UnixMilli() }) {
		t.Errorf("windows %+v lack the cancelled test's", ws)
	}
}

// TestWithVia: the exchange a handoff crossed is added locally only, and
// only to tests whose handoff is the server's latest.
func TestWithVia(t *testing.T) {
	st := speedtest.State{Vias: map[string]model.Handoff{"A": {ASN: 60781, ISPASN: 3320, Via: "DE-CIX"}}}
	ts := []model.SpeedTest{
		{Server: "A", Handoff: &model.Handoff{ASN: 60781, ISPASN: 3320}},
		{Server: "A", Handoff: &model.Handoff{ASN: 3356, ISPASN: 3320}}, // an older route
		{Server: "B", Handoff: &model.Handoff{ASN: 60781, ISPASN: 3320}},
		{Server: "A"},
	}
	got := withVia(ts, st, false)
	if got[0].Handoff.Via != "DE-CIX" || got[1].Handoff.Via != "" || got[2].Handoff.Via != "" || got[3].Handoff != nil || ts[0].Handoff.Via != "" {
		t.Errorf("local: %+v %+v %+v", got[0].Handoff, got[1].Handoff, got[2].Handoff)
	}
	if got := withVia(ts, st, true); got[0].Handoff.Via != "" {
		t.Error("the share link got the exchange")
	}
}

// TestSpeedWindowsOrigin: the share link's windows never say whose test
// it was; the owner's do.
func TestSpeedWindowsOrigin(t *testing.T) {
	at := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	ts := []model.SpeedTest{{Start: at, End: at.Add(10 * time.Second), Trigger: "public"}, {Start: at.Add(time.Hour), End: at.Add(time.Hour + 9*time.Second), Trigger: "schedule"}}
	if w := speedWindows(ts, at.Add(-time.Hour), false); len(w) != 2 || w[0].Origin != "public" || w[1].Origin != "scheduled" {
		t.Errorf("local %+v", w)
	}
	for _, w := range speedWindows(ts, at.Add(-time.Hour), true) {
		if w.Origin != "test" {
			t.Errorf("public %+v", w)
		}
	}
}

// TestSpeedOwnerZone: a local page's IANA tz becomes the owner's zone, and
// the share link's summary is in it whatever tz the visitor sends.
func TestSpeedOwnerZone(t *testing.T) {
	loc, pub, _ := speedHandlers(t, newSpeedRig(t, 5*time.Hour), LocalOptions{})
	do(loc, "GET", "/api/speed?days=7&tz=America/New_York", speedHost)
	var v struct {
		OwnerTZ   string `json:"owner_tz"`
		TZName    string `json:"tz_name"`
		ZoneLabel string `json:"zone_label"`
	}
	if err := json.Unmarshal(do(pub, "GET", speedPub+"/api/speed?days=7&tz=Asia/Tokyo", nil).Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.OwnerTZ != "America/New_York" || v.TZName != "America/New_York" || !strings.HasPrefix(v.ZoneLabel, "ED") {
		t.Errorf("share link: %+v", v)
	}
}

// TestSweepBytes: "Test every server" is estimated from each server's
// recent median data, else the line's rate both ways for MaxMeasure;
// skipped servers and the share link's capped tests are left out.
func TestSweepBytes(t *testing.T) {
	st := speedtest.State{Servers: []speedtest.ServerState{
		{Server: speedtest.Server{ID: "A"}}, {Server: speedtest.Server{ID: "B"}}, {Server: speedtest.Server{ID: "C"}, Skipped: true}}}
	d, u := 80e6, 40e6
	ts := []model.SpeedTest{
		{Server: "A", Outcome: model.SpeedOK, Bytes: 100e6, DownBps: &d, UpBps: &u, Trigger: "schedule"},
		{Server: "A", Outcome: model.SpeedOK, Bytes: 300e6, DownBps: &d, UpBps: &u, Trigger: "manual"},
		{Server: "B", Outcome: model.SpeedOK, Bytes: 200e6, DownBps: &d, UpBps: &u, Trigger: "public"},
		{Server: "B", Outcome: model.SpeedBusy, Bytes: 1e3, Trigger: "schedule"},
	}
	if b, n := sweepBytes(st, ts); n != 2 || b != 200e6+150e6 {
		t.Errorf("%d bytes over %d servers", b, n)
	}
	if b, _ := sweepBytes(st, nil); b != 2*defaultTestBytes {
		t.Errorf("no tests: %d", b)
	}
}
