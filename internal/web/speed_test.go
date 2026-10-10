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
// stands still, so nothing is scheduled; queued tests run until release
// is closed (or they are cancelled).
type speedRig struct {
	s       *speedtest.Scheduler
	st      *speedtest.MemStore
	release chan struct{}
}

func newSpeedRig(t *testing.T, step time.Duration) *speedRig {
	t.Helper()
	ctx := context.Background()
	cat := speedCatalog(t)
	custom, err := speedtest.Custom("192.168.1.20:5201", cat)
	if err != nil {
		t.Fatal(err)
	}
	g := &speedRig{st: &speedtest.MemStore{}, release: make(chan struct{})}
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
		Fake: &speedtest.Fake{Seed: 1,
			Run: func(ctx context.Context, c speedtest.Config) (speedtest.Result, error) {
				select {
				case <-ctx.Done():
					return speedtest.Result{}, ctx.Err()
				case <-g.release:
				}
				return speedtest.Result{Direction: c.Direction, Bytes: 200e6, Elapsed: 4 * time.Second, Streams: 8, TotalBytes: 240e6}, nil
			},
			RTT: func(_ context.Context, host string, _ int) (time.Duration, error) {
				if strings.Contains(host, "lon") {
					return 9 * time.Millisecond, nil
				}
				return 80 * time.Millisecond, nil
			}}})
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
		!strings.Contains(s.UnderLoad.Text[1], "rises from 8 ms to 48 ms while uploading (+40 ms): noticeable on calls") {
		t.Errorf("under load %+v", s.UnderLoad)
	}
	if len(s.PerServer) != 5 || s.Latest == nil || s.Servers[0].Host == "" || s.Servers[0].Ports != "5200-5209" || s.Servers[0].Capacity == nil ||
		*s.Servers[0].Capacity != 10000 || s.Servers[0].Display != "UK · Clouvider, London" {
		t.Errorf("servers: %d per server, latest %v, %+v", len(s.PerServer), s.Latest, s.Servers[0])
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
	if one.Kind != "server" || one.Row.ID != "custom-1" || len(one.Tests) == 0 || one.Tests[0].Server != "custom-1" || one.Tests[0].Port != 0 || !one.Tests[0].Manual {
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
	if r := do(loc, "GET", "/api/speed?part=sweep", speedHost); r.Code != 200 || !strings.Contains(r.Body.String(), `"sweep":null`) {
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

func decodeEntry(t *testing.T, w *httptest.ResponseRecorder) speedPostJSON {
	t.Helper()
	var p speedPostJSON
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	return p
}

// TestSpeedQueue: Test now and sweeps on both listeners share one queue;
// equivalent requests return the waiting entry; a visitor may have one
// waiting entry; the owner cancels and clears.
func TestSpeedQueue(t *testing.T) {
	g := newSpeedRig(t, 5*time.Hour)
	loc, pub, _ := speedHandlers(t, g, LocalOptions{})
	good := speedPOSTHeaders()
	// The owner's test runs at once (the fake network holds it).
	w := speedSend(loc, "POST", "/api/speed/run", `{"server": "us-east"}`, good)
	first := decodeEntry(t, w)
	if w.Code != 202 || first.Entry.Who != "you" || first.Entry.Kind != "test" || first.Entry.Label != "US East" || first.Entry.Target != "us-east" {
		t.Fatalf("run: %d %s", w.Code, w.Body.String())
	}
	waitUntil(t, func() bool { return g.s.Queue().Running != nil })
	var run struct{ Running *speedRunning }
	json.Unmarshal(do(loc, "GET", "/api/speed?part=running", speedHost).Body.Bytes(), &run)
	if run.Running == nil || run.Running.Entry != first.Entry.ID || run.Running.Kind != "test" || run.Running.ExpectedEnd <= run.Running.Started {
		t.Errorf("running %+v", run.Running)
	}
	// Waiting, then the same again: the waiting entry.
	w = speedSend(loc, "POST", "/api/speed/run", `{"server": "Leaseweb-London"}`, good)
	second := decodeEntry(t, w)
	if w.Code != 202 || second.Entry.Position != 1 || second.Entry.EstStart == nil || second.Entry.State != speedtest.StateWaiting {
		t.Errorf("second: %d %s", w.Code, w.Body.String())
	}
	w = speedSend(loc, "POST", "/api/speed/run", `{"server": "Leaseweb-London"}`, good)
	if p := decodeEntry(t, w); w.Code != 200 || !p.Duplicate || p.Entry.ID != second.Entry.ID {
		t.Errorf("duplicate: %d %s", w.Code, w.Body.String())
	}
	// Visitors on the share link: same origin and the page's token.
	vis := map[string]string{"Host": "share.example", "Origin": "https://share.example", "Sec-Fetch-Site": "same-origin", HeaderCSRF: pub.token,
		"Cf-Connecting-Ip": "2001:db8:1:2:3::9"}
	for name, hdr := range map[string]map[string]string{
		"no token":     {"Host": "share.example", "Origin": "https://share.example", "Cf-Connecting-Ip": "198.51.100.1"},
		"cross-origin": {"Host": "share.example", "Origin": "https://evil.example", HeaderCSRF: pub.token, "Cf-Connecting-Ip": "198.51.100.1"},
		"wrong token":  {"Host": "share.example", "Origin": "https://share.example", HeaderCSRF: "tok123", "Cf-Connecting-Ip": "198.51.100.1"},
	} {
		if w := speedSend(pub, "POST", speedPub+"/api/speed/sweep", "", hdr); w.Code != 403 {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	w = speedSend(pub, "POST", speedPub+"/api/speed/sweep", "", vis)
	sweep := decodeEntry(t, w)
	if w.Code != 202 || sweep.Entry.Who != "you" || sweep.Entry.Kind != "sweep" || sweep.Entry.Label != "All servers" || sweep.Entry.Position != 2 {
		t.Errorf("public sweep: %d %s", w.Code, w.Body.String())
	}
	// The same /64: one waiting entry per visitor.
	vis2 := map[string]string{}
	for k, v := range vis {
		vis2[k] = v
	}
	vis2["Cf-Connecting-Ip"] = "2001:db8:1:2:ffff::1"
	w = speedSend(pub, "POST", speedPub+"/api/speed/run", `{"server": "custom-1"}`, vis2)
	var full struct {
		Code       string
		RetryAfter int `json:"retry_after"`
		Queue      speedQueueJSON
	}
	json.Unmarshal(w.Body.Bytes(), &full)
	if w.Code != 429 || full.Code != "queue_full" || full.RetryAfter == 0 || len(full.Queue.Waiting) != 2 || full.Queue.Waiting[1].Who != "you" ||
		full.Queue.Waiting[0].Who != "someone" || strings.Contains(w.Body.String(), "2001:db8") {
		t.Errorf("queue full: %d %s", w.Code, w.Body.String())
	}
	// Another visitor's own-server test, by its public id.
	vis2["Cf-Connecting-Ip"] = "203.0.113.5"
	w = speedSend(pub, "POST", speedPub+"/api/speed/run", `{"server": "custom-1"}`, vis2)
	if p := decodeEntry(t, w); w.Code != 202 || p.Entry.Target != "custom-1" || p.Entry.Label != "Your server" {
		t.Errorf("public custom: %d %s", w.Code, w.Body.String())
	}
	// The share link's rate: 3 a client at once.
	for range 3 {
		w = speedSend(pub, "POST", speedPub+"/api/speed/run", `{"server": "uk"}`, vis2)
	}
	if w.Code != 429 || !strings.Contains(w.Body.String(), `"code":"rate_limited"`) {
		t.Errorf("rate: %d %s", w.Code, w.Body.String())
	}
	if w := speedSend(pub, "DELETE", speedPub+"/api/speed/queue/1", "", vis); w.Code != 404 {
		t.Errorf("public cancel: %d", w.Code)
	}
	// The queue as the owner sees it: no client keys.
	var q speedQueueJSON
	b := do(loc, "GET", "/api/speed?part=queue", speedHost).Body.String()
	json.Unmarshal([]byte(b), &q)
	if q.Running == nil || q.Running.Progress == nil || len(q.Waiting) != 3 || q.Waiting[0].Who != "you" || q.Waiting[1].Who != "someone" ||
		strings.Contains(b, "2001:db8") || strings.Contains(b, "203.0.113") {
		t.Errorf("queue %s", b)
	}
	// Clear the visitors', cancel the owner's waiting one, then the running one.
	if w := speedSend(loc, "POST", "/api/speed/queue/clear?who=public", "", good); w.Code != 200 || !strings.Contains(w.Body.String(), `"cancelled":2`) {
		t.Errorf("clear: %d %s", w.Code, w.Body.String())
	}
	if w := speedSend(loc, "DELETE", fmt.Sprintf("/api/speed/queue/%d", second.Entry.ID), "", good); w.Code != 200 {
		t.Errorf("cancel waiting: %d %s", w.Code, w.Body.String())
	}
	if w := speedSend(loc, "DELETE", fmt.Sprintf("/api/speed/queue/%d", first.Entry.ID), "", good); w.Code != 200 {
		t.Errorf("cancel running: %d %s", w.Code, w.Body.String())
	}
	waitUntil(t, func() bool { e, _ := g.s.QueueEntry(first.Entry.ID); return e.State == speedtest.StateCancelled })
	if w := speedSend(loc, "DELETE", "/api/speed/queue/999", "", good); w.Code != 404 {
		t.Errorf("cancel unknown: %d", w.Code)
	}
	var e struct{ Entry speedEntry }
	json.Unmarshal(do(loc, "GET", fmt.Sprintf("/api/speed?part=entry&id=%d", sweep.Entry.ID), speedHost).Body.Bytes(), &e)
	if e.Entry.State != speedtest.StateCancelled {
		t.Errorf("cleared sweep %+v", e.Entry)
	}
	// A sweep runs every server once, then reads back as the last sweep.
	close(g.release)
	w = speedSend(loc, "POST", "/api/speed/sweep", "", good)
	sw := decodeEntry(t, w)
	waitUntil(t, func() bool { x, _ := g.s.QueueEntry(sw.Entry.ID); return x.State == speedtest.StateDone })
	var last struct{ Sweep *speedEntry }
	json.Unmarshal(do(pub, "GET", speedPub+"/api/speed?part=sweep", nil).Body.Bytes(), &last)
	if last.Sweep == nil || last.Sweep.ID != sw.Entry.ID || len(last.Sweep.Results) != 5 || last.Sweep.Who != "someone" || last.Sweep.Results[0].DownBps == nil {
		t.Fatalf("last sweep %+v", last.Sweep)
	}
	// Bad requests.
	for _, c := range []struct {
		method, path, body string
		code               int
	}{
		{"POST", "/api/speed/run", `{"server": "Nope"}`, 404},
		{"POST", "/api/speed/run", `{"id": "uk"}`, 400},
		{"POST", "/api/speed/run?id=uk", ``, 400},
		{"POST", "/api/speed/sweep", `{"server": "uk"}`, 400},
		{"GET", "/api/speed/run", ``, 405},
		{"POST", "/api/speed/queue/1", ``, 405},
		{"POST", "/api/speed/queue/clear?who=me", ``, 400},
		{"POST", "/api/speed/nope", ``, 404},
	} {
		if w := speedSend(loc, c.method, c.path, c.body, good); w.Code != c.code {
			t.Errorf("%s %s %s: %d %s", c.method, c.path, c.body, w.Code, w.Body.String())
		}
	}
	bad := map[string]string{"Host": "127.0.0.1:3000", "Origin": "http://evil.com", HeaderCSRF: "tok123"}
	if w := speedSend(loc, "POST", "/api/speed/run", "", bad); w.Code != 403 {
		t.Errorf("cross-origin: %d", w.Code)
	}
}

func TestSpeedConfig(t *testing.T) {
	g := newSpeedRig(t, 5*time.Hour)
	loc, _, d := speedHandlers(t, g, LocalOptions{})
	good := speedPOSTHeaders()
	cfg := func(body string) *httptest.ResponseRecorder {
		return speedSend(loc, "POST", "/api/speed/config", body, good)
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
	if w := cfg(`{"mode": "nearby"}`); w.Code != 200 || g.s.State().Mode != "nearby" {
		t.Errorf("nearby: %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{``, `{}`, `{"mode": true}`, `{"mode": "fast"}`, `{"enabled": true}`, `{"mode": "off", "x": 1}`, `{"mode": "regions", "extra": ["mars"]}`} {
		if w := cfg(body); w.Code != 400 {
			t.Errorf("%q: %d", body, w.Code)
		}
	}
	// An admin token is required when set.
	adm := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123", AdminToken: "sekrit"})
	if w := speedSend(adm, "POST", "/api/speed/run", "", good); w.Code != 403 {
		t.Errorf("without admin token: %d", w.Code)
	}
	json.Unmarshal(do(adm, "GET", "/api/speed", speedHost).Body.Bytes(), &s)
	if s.Configurable {
		t.Error("configurable without the admin token")
	}
	withAdm := map[string]string{"X-FYISP-Admin": "sekrit"}
	for k, v := range good {
		withAdm[k] = v
	}
	if w := speedSend(adm, "POST", "/api/speed/run", "", withAdm); w.Code != 202 {
		t.Errorf("with admin token: %d %s", w.Code, w.Body.String())
	}
}

// TestHomeSpeedLine: Home's line from the finding; none while off.
func TestHomeSpeedLine(t *testing.T) {
	loc, _, _ := speedHandlers(t, newSpeedRig(t, 15*time.Minute), LocalOptions{})
	var l HomeLive
	json.Unmarshal(do(loc, "GET", "/api/home?part=live", speedHost).Body.Bytes(), &l)
	if l.Speed == nil || l.Speed.Kind != "pattern" || l.Speed.Impact != "ok" || !strings.HasPrefix(l.Speed.Text, "Evenings are slower: about 40% below your daytime speed") {
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
