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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

var speedNow = time.Date(2026, 10, 10, 18, 0, 0, 0, time.UTC)

// fakeSpeed is a SpeedSource over a fixed list of tests.
type fakeSpeed struct {
	mu      sync.Mutex
	st      speedtest.State
	tests   []model.SpeedTest
	started []string
	q       *speedtest.Queue // not run: entries stay waiting
	public  bool             // the share link's opt-in at start
	budget  int64
	bytes   int64 // > 0: tests are instant and transfer this much (for a running queue)
}

func (f *fakeSpeed) State() speedtest.State { f.mu.Lock(); defer f.mu.Unlock(); return f.st }

func (f *fakeSpeed) SpeedTests(_ context.Context, from, to time.Time) ([]model.SpeedTest, error) {
	var out []model.SpeedTest
	for _, t := range f.tests {
		if !t.Start.Before(from) && !t.Start.After(to) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeSpeed) Queue() *speedtest.Queue {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.q == nil {
		f.q = speedtest.NewQueue(speedtest.QueueOptions{Now: func() time.Time { return speedNow },
			Resolve: func(kind speedtest.Kind, id string) ([]speedtest.Job, error) {
				if kind == speedtest.KindSweep {
					var out []speedtest.Job
					for _, sv := range speedServers {
						out = append(out, speedtest.Job{Chain: []speedtest.Server{sv}})
					}
					return out, nil
				}
				switch id {
				case "", "us-east", "Leaseweb-NewYork", "192.168.1.20:5201":
					f.mu.Lock()
					f.started = append(f.started, id)
					f.mu.Unlock()
					return []speedtest.Job{{Region: id}}, nil
				}
				return nil, speedtest.ErrUnknownID
			},
			Exec: func(ctx context.Context, j speedtest.Job, trigger string) (int64, bool, int64) {
				if f.bytes > 0 { // a run queue: each test is instant and uses f.bytes
					return 0, false, f.bytes
				}
				<-ctx.Done()
				return 0, false, 0
			},
			Public: &f.public, PublicBudget: f.budget})
	}
	return f.q
}

func (f *fakeSpeed) SetMode(_ context.Context, m speedtest.Mode) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m != speedtest.ModeOff && m != speedtest.ModeRegions && m != speedtest.ModeNearest {
		return time.Time{}, speedtest.ErrMode
	}
	f.st.Mode, f.st.Next = m, time.Time{}
	if m != speedtest.ModeOff {
		f.st.Next = speedNow.Add(2 * time.Minute)
	}
	return f.st.Next, nil
}

func (f *fakeSpeed) MaskedShare(time.Duration) float64 { return 0.0712345 }

var speedServers = []speedtest.Server{
	{ID: "Clouvider-London", Label: "Clouvider London", Provider: "Clouvider", Host: "lon.speedtest.clouvider.net", PortMin: 5200, PortMax: 5209, City: "London", Country: "GB", Geo: "eu"},
	{ID: "Leaseweb-London", Label: "Leaseweb London", Provider: "Leaseweb", Host: "speedtest.lon12.uk.leaseweb.net", PortMin: 5201, PortMax: 5210, City: "London", Country: "GB", Geo: "eu"},
	{ID: "Leaseweb-NewYork", Label: "Leaseweb New York", Provider: "Leaseweb", Host: "speedtest.nyc1.us.leaseweb.net", PortMin: 5201, PortMax: 5210, City: "New York", Country: "US", Geo: "na"},
	{ID: "Clouvider-NewYork", Label: "Clouvider New York", Provider: "Clouvider", Host: "nyc.speedtest.clouvider.net", PortMin: 5200, PortMax: 5209, City: "New York", Country: "US", Geo: "na"},
	{ID: "192.168.1.20:5201", Label: "Your server", Provider: "Your server", Host: "192.168.1.20", PortMin: 5201, PortMax: 5201, Custom: true},
}

// newFakeSpeedEvery has 30 days of tests: two regions every step (the UK
// near and fast, US East far and slower), both 40% slower from 19:00 to
// 23:00 UTC; the UK uses its fallback a lot in the last day; a manual test
// against the own server now and then; a busy test every 50th.
func newFakeSpeedEvery(step time.Duration) *fakeSpeed {
	f := &fakeSpeed{public: true}
	ms := func(v float64) *float64 { return &v }
	rtt := 12.4
	f.st = speedtest.State{Mode: speedtest.ModeRegions, Modes: []speedtest.Mode{"off", "regions", "nearest", "custom"},
		Interval: 15 * time.Minute, Spacing: 450 * time.Second, PlannedShare: 2 * 15.0 / 900, Next: speedNow.Add(7 * time.Minute),
		NextRegion: "uk", NextServer: "Clouvider-London", Custom: []string{"192.168.1.20:5201"},
		Regions: []speedtest.Region{{ID: "uk", Title: "UK", Chain: []string{"Clouvider-London", "Leaseweb-London"}},
			{ID: "us-east", Title: "US East", Chain: []string{"Leaseweb-NewYork", "Clouvider-NewYork"}}}}
	for _, sv := range speedServers {
		ss := speedtest.ServerState{Server: sv}
		if sv.ID == "Clouvider-London" {
			ss.RTTms, ss.Picked = &rtt, true
		}
		f.st.Servers = append(f.st.Servers, ss)
	}
	start := speedNow.Add(-30 * 24 * time.Hour)
	i := 0
	for at := start; at.Before(speedNow); at = at.Add(step) {
		for _, reg := range []string{"uk", "us-east"} {
			i++
			t := model.SpeedTest{ID: int64(i), Start: at, End: at.Add(18 * time.Second), Trigger: "schedule", Outcome: model.SpeedOK,
				Region: reg, Server: "Clouvider-London", Port: 5200 + i%10, Attempts: 2}
			base := 400e6
			if reg == "us-east" {
				t.Server, base = "Leaseweb-NewYork", 150e6
				t.Start = at.Add(450 * time.Second)
				t.End = t.Start.Add(18 * time.Second)
			}
			if reg == "uk" && speedNow.Sub(at) < 24*time.Hour && i%3 != 0 {
				t.Server, t.Fallback = "Leaseweb-London", true
			}
			switch {
			case i%50 == 7:
				t.Outcome, t.Attempts = model.SpeedBusy, 3
			case i%97 == 5:
				t.Outcome, t.Attempts = model.SpeedUnreachable, 1
			default:
				down := base * (1 + 0.03*float64(i%7-3))
				if h := t.Start.Hour(); h >= 19 && h <= 22 {
					down *= 0.6
				}
				up := down / 10
				re := int64(i % 40)
				t.DownBps, t.UpBps, t.DownRetrans = &down, &up, &re
				t.IdleGatewayMs, t.LoadedGatewayMs, t.IdleCallMs, t.LoadedCallMs = ms(1.1), ms(9.8), ms(14.2), ms(61)
			}
			f.tests = append(f.tests, t)
		}
		if i%20 == 0 {
			down, up := 900e6, 900e6
			f.tests = append(f.tests, model.SpeedTest{ID: int64(10000 + i), Start: at.Add(60 * time.Second), End: at.Add(75 * time.Second),
				Trigger: "manual", Outcome: model.SpeedOK, Server: "192.168.1.20:5201", Port: 5201, Attempts: 2, DownBps: &down, UpBps: &up})
		}
	}
	return f
}

func speedHandlers(t *testing.T, f *fakeSpeed, o LocalOptions) (*localHandler, *publicHandler, Deps) {
	_, d, _ := fixture(t, speedNow)
	d.Speed = f
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
	loc, pub, _ := speedHandlers(t, newFakeSpeedEvery(5*time.Hour), LocalOptions{})
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
	loc, pub, d := speedHandlers(t, newFakeSpeedEvery(15*time.Minute), LocalOptions{})
	var s speedJSON
	if err := json.Unmarshal(do(loc, "GET", "/api/speed?days=30", speedHost).Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Mode != "regions" || !s.Enabled || !s.Configurable || s.SpacingS != 450 || s.MaskedShare24h != 0.071 || s.Warning != nil ||
		len(s.Servers) != 5 || len(s.Regions) != 2 || len(s.PerRegion) != 2 || s.Next == nil || *s.Next.Region != "uk" {
		t.Fatalf("summary %+v", s)
	}
	uk, us := s.PerRegion[0], s.PerRegion[1]
	if uk.ID != "uk" || uk.Label != "UK" || uk.N < 2500 || uk.Busy == 0 || uk.Fallback == 0 || len(uk.Hours) != 24 || uk.Hours[20].DownNorm == nil || *uk.Hours[20].DownNorm > 0.7 {
		t.Errorf("uk row %+v", uk)
	}
	if uk.FallbackShare24h == nil || *uk.FallbackShare24h < 0.5 || uk.FallbackNote == nil || us.FallbackNote != nil || *us.FallbackShare24h != 0 {
		t.Errorf("fallback: uk %v %v, us %v", uk.FallbackShare24h, uk.FallbackNote, us.FallbackNote)
	}
	if uk.Dip == nil || us.Dip == nil || *us.DownMedian > 160e6 || *uk.BestDown < 400e6 {
		t.Errorf("rows: uk dip %v us dip %v", uk.Dip, us.Dip)
	}
	// The pooled pattern: both regions dip together, whatever their speed.
	if s.Pattern == nil || s.Pattern.Text != "Evenings are slower: about 40% below your daytime speed." || len(s.Pattern.Hours) != 4 || s.Pattern.Hours[0] != 19 {
		t.Errorf("pattern %+v", s.Pattern)
	}
	// The own server has manual tests only: on demand, so no row of its own.
	if len(s.PerServer) != 3 || s.Latest == nil || !s.Latest.OnDemand || s.Servers[0].Host == "" || s.Servers[0].Ports != "5200-5209" {
		t.Errorf("servers: %d per server, latest %v, %+v", len(s.PerServer), s.Latest, s.Servers[0])
	}
	// Fallback tests left out of the region rows on request.
	var x speedJSON
	json.Unmarshal(do(loc, "GET", "/api/speed?days=30&exclude_fallback=1", speedHost).Body.Bytes(), &x)
	if x.PerRegion[0].Fallback != 0 || x.PerRegion[0].N >= uk.N || !x.ExcludeFallback {
		t.Errorf("exclude_fallback: %+v", x.PerRegion[0].Stats.N)
	}
	// The share link: labels and catalog ids, never hosts or ports; the own
	// server is custom-1.
	b := do(pub, "GET", speedPub+"/api/speed?days=30", nil).Body.String()
	for _, leak := range []string{"192.168.1.20", "clouvider.net", "leaseweb.net", `"port"`, `"host"`, `"ports"`, `"configurable":true`} {
		if strings.Contains(b, leak) {
			t.Errorf("share link leaks %s", leak)
		}
	}
	for _, want := range []string{`"id":"custom-1"`, `"label":"Your server"`, `"label":"Leaseweb New York"`, `"Clouvider-London"`} {
		if !strings.Contains(b, want) {
			t.Errorf("share link lacks %s", want)
		}
	}
	// One region, one server (local id, and the public custom id).
	var one speedOneJSON
	json.Unmarshal(do(loc, "GET", "/api/speed?region=us-east&days=7", speedHost).Body.Bytes(), &one)
	if one.Kind != "region" || one.Row.Label != "US East" || len(one.Tests) != 7*96 || len(one.Windows) != 7*96 || one.Tests[0].Region == nil {
		t.Errorf("region: %s %s %d tests", one.Kind, one.Row.Label, len(one.Tests))
	}
	one = speedOneJSON{}
	json.Unmarshal(do(pub, "GET", speedPub+"/api/speed?server=custom-1&days=30", nil).Body.Bytes(), &one)
	if one.Kind != "server" || one.Row.ID != "custom-1" || len(one.Tests) == 0 || one.Tests[0].Server != "custom-1" || one.Tests[0].Port != 0 {
		t.Errorf("public custom server: %+v", one.Row)
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
	} {
		w := do(loc, "GET", path, speedHost)
		var e struct{ Code string }
		json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != code || e.Code != errCode(code) {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	// Windows and the light running part.
	var w struct {
		Windows []speedWindow
		Running *speedRunning
	}
	json.Unmarshal(do(loc, "GET", "/api/speed?part=windows&from=now-1h&to=now", speedHost).Body.Bytes(), &w)
	if len(w.Windows) < 8 || w.Windows[0].End-w.Windows[0].Start != 18000 {
		t.Errorf("windows %+v", w.Windows)
	}
	f := d.Speed.(*fakeSpeed)
	f.st.Running = &speedtest.RunState{Started: speedNow, ExpectedEnd: speedNow.Add(20 * time.Second), Direction: "download", Server: "192.168.1.20:5201"}
	json.Unmarshal(do(pub, "GET", speedPub+"/api/speed?part=running", nil).Body.Bytes(), &w)
	if w.Running == nil || w.Running.Server.ID != "custom-1" || w.Running.ExpectedEnd-w.Running.Started != 20000 {
		t.Errorf("running %+v", w.Running)
	}
	// Without a source every speed route is 404.
	d.Speed = nil
	loc2 := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"})
	if w := do(loc2, "GET", "/api/speed", speedHost); w.Code != 404 {
		t.Errorf("no source: %d", w.Code)
	}
	if w := do(loc2, "POST", "/api/speed/run", speedPOSTHeaders()); w.Code != 404 {
		t.Errorf("no source run: %d", w.Code)
	}
}

func speedPOSTHeaders() map[string]string {
	return map[string]string{"Host": "127.0.0.1:3000", "Origin": "http://127.0.0.1:3000", "Sec-Fetch-Site": "same-origin", HeaderCSRF: "tok123"}
}

func TestSpeedWrites(t *testing.T) {
	f := newFakeSpeedEvery(5 * time.Hour)
	loc, _, d := speedHandlers(t, f, LocalOptions{})
	good := speedPOSTHeaders()
	type runResp struct {
		Entry     speedEntryJSON
		Duplicate bool
		Code      string
	}
	run := func(path string) (int, runResp) {
		w := do(loc, "POST", path, good)
		var r runResp
		json.Unmarshal(w.Body.Bytes(), &r)
		return w.Code, r
	}
	if c, r := run("/api/speed/run?id=us-east"); c != 202 || r.Duplicate || r.Entry.Kind != "single" || r.Entry.Target == nil ||
		r.Entry.Target.ID != "us-east" || r.Entry.Who != "you" || r.Entry.State != "waiting" || r.Entry.Position != 1 {
		t.Fatalf("run: %d %+v", c, r)
	}
	if c, r := run("/api/speed/run?kind=single&id=us-east"); c != 202 || !r.Duplicate || r.Entry.ID != 1 {
		t.Errorf("duplicate: %d %+v", c, r)
	}
	if c, r := run("/api/speed/run"); c != 202 || r.Duplicate || r.Entry.Target != nil || r.Entry.Position != 2 {
		t.Errorf("nearest: %d %+v", c, r)
	}
	if c, r := run("/api/speed/run?kind=sweep"); c != 202 || r.Entry.Kind != "sweep" || r.Entry.Steps != len(speedServers) {
		t.Errorf("sweep: %d %+v", c, r)
	}
	for _, p := range []string{"/api/speed/run?kind=sweep&id=us-east", "/api/speed/run?kind=fast", "/api/speed/run?id=a&id=b"} {
		if c, _ := run(p); c != 400 {
			t.Errorf("%s: %d", p, c)
		}
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
	if w := do(loc, "POST", "/api/speed/cancel?id=1", map[string]string{"Host": "127.0.0.1:3000", "Origin": "http://evil.com", HeaderCSRF: "tok123"}); w.Code != 403 {
		t.Errorf("cancel cross-origin: %d", w.Code)
	}
	if w := do(loc, "POST", "/api/speed/run?id=Nope", good); w.Code != 404 {
		t.Errorf("unknown id: %d", w.Code)
	}
	if w := do(loc, "POST", "/api/speed/run?server=x", good); w.Code != 400 {
		t.Errorf("unknown param: %d", w.Code)
	}
	bad := map[string]string{"Host": "127.0.0.1:3000", "Origin": "http://evil.com", HeaderCSRF: "tok123"}
	if w := do(loc, "POST", "/api/speed/run", bad); w.Code != 403 {
		t.Errorf("cross-origin: %d", w.Code)
	}
	if w := do(loc, "GET", "/api/speed/run", good); w.Code != 405 {
		t.Errorf("GET run: %d", w.Code)
	}
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/speed/config", strings.NewReader(body))
		for k, v := range good {
			if k == "Host" {
				r.Host = v
			} else {
				r.Header.Set(k, v)
			}
		}
		w := httptest.NewRecorder()
		loc.ServeHTTP(w, r)
		return w
	}
	if w := post(`{"mode": "off"}`); w.Code != 200 || f.st.Mode != "off" || !strings.Contains(w.Body.String(), `"next_at":null`) {
		t.Errorf("off: %d %s", w.Code, w.Body.String())
	}
	if w := post(`{"mode": "nearest"}`); w.Code != 200 || f.st.Mode != "nearest" || !strings.Contains(w.Body.String(), `"next_at":1791655320000`) {
		t.Errorf("nearest: %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{``, `{}`, `{"mode": true}`, `{"mode": "fast"}`, `{"enabled": true}`, `{"mode": "off", "x": 1}`} {
		if w := post(body); w.Code != 400 {
			t.Errorf("%q: %d", body, w.Code)
		}
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
	if w := do(adm, "POST", "/api/speed/run", withAdm); w.Code != 202 {
		t.Errorf("with admin token: %d %s", w.Code, w.Body.String())
	}
	// The share link has its own route (TestSpeedPublicQueue); never config or cancel.
	pub := Public(d, "0123456789abcdef0123456789abcdef")
	for _, p := range []string{"/api/speed/config", "/api/speed/cancel?id=1"} {
		if w := do(pub, "POST", speedPub+p, nil); w.Code != 404 && w.Code != 405 {
			t.Errorf("share link %s: %d", p, w.Code)
		}
	}
}

func TestHomeSpeedLine(t *testing.T) {
	f := newFakeSpeedEvery(15 * time.Minute)
	loc, _, _ := speedHandlers(t, f, LocalOptions{})
	var l HomeLive
	json.Unmarshal(do(loc, "GET", "/api/home?part=live&tz=120", speedHost).Body.Bytes(), &l)
	if l.Speed == nil || l.Speed.Text != "Evenings are slower: about 40% below your daytime speed." || l.Speed.Level != "info" {
		t.Errorf("speed line %+v", l.Speed)
	}
	f.tests = f.tests[:40] // half a day: no clear pattern yet
	loc, _, _ = speedHandlers(t, f, LocalOptions{})
	l = HomeLive{}
	json.Unmarshal(do(loc, "GET", "/api/home?part=live", speedHost).Body.Bytes(), &l)
	if l.Speed != nil {
		t.Errorf("speed line without a pattern: %+v", l.Speed)
	}
}

// TestSpeedPublicQueue: the share link queues tests and sweeps (same
// origin, no body, per-visitor caps and start limits, never cached), sees
// "you" for its own entries and "someone" for everyone else's, and never
// an address, a host or another visitor's key.
func TestSpeedPublicQueue(t *testing.T) {
	f := newFakeSpeedEvery(5 * time.Hour)
	loc, pub, _ := speedHandlers(t, f, LocalOptions{})
	now := speedNow
	pub.now = func() time.Time { return now }
	vis := func(ip string) map[string]string {
		return map[string]string{"Host": "x.trycloudflare.com", "Origin": "https://x.trycloudflare.com", "Sec-Fetch-Site": "same-origin", "Cf-Connecting-Ip": ip}
	}
	type resp struct {
		Entry     speedEntryJSON
		Duplicate bool
		Code      string
	}
	post := func(path string, hdr map[string]string) (*httptest.ResponseRecorder, resp) {
		w := do(pub, "POST", speedPub+path, hdr)
		var r resp
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
		"bad kind":     {"/api/speed/run?kind=all", vis("1.1.1.1"), 400, "bad_request"},
		"sweep and id": {"/api/speed/run?kind=sweep&id=us-east", vis("1.1.1.1"), 400, "bad_request"},
		"other param":  {"/api/speed/run?host=8.8.8.8", vis("1.1.1.1"), 400, "bad_request"},
		"unknown id":   {"/api/speed/run?id=8.8.8.8", vis("1.1.1.1"), 404, "not_found"},
		"local id":     {"/api/speed/run?id=192.168.1.20:5201", vis("1.1.1.1"), 404, "not_found"},
	} {
		if w, r := post(c.path, c.hdr); w.Code != c.code || r.Code != c.ec {
			t.Errorf("%s: %d %q (%s)", name, w.Code, r.Code, w.Body.String())
		}
	}
	if w := do(pub, "GET", speedPub+"/api/speed/run", vis("1.1.1.1")); w.Code != 405 {
		t.Errorf("GET run: %d", w.Code)
	}
	r := httptest.NewRequest("POST", speedPub+"/api/speed/run", strings.NewReader("kind=sweep"))
	for k, v := range vis("1.1.1.1") {
		if k == "Host" {
			r.Host = v
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	pub.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Errorf("body: %d", w.Code)
	}
	// A sweep from visitor 1, a single (own server, by its public id) from 2.
	if w, r := post("/api/speed/run?kind=sweep", vis("1.1.1.1")); w.Code != 202 || r.Duplicate || r.Entry.Who != "you" || r.Entry.Kind != "sweep" {
		t.Fatalf("sweep: %d %+v", w.Code, r)
	}
	if w, r := post("/api/speed/run?id=custom-1", vis("2.2.2.2")); w.Code != 202 || r.Entry.Target == nil || r.Entry.Target.ID != "custom-1" {
		t.Fatalf("own server: %d %s", w.Code, w.Body.String())
	}
	// Asking again: the same entry, not a new start.
	if w, r := post("/api/speed/run?kind=sweep", vis("1.1.1.1")); w.Code != 202 || !r.Duplicate {
		t.Errorf("again: %d %+v", w.Code, r)
	}
	// One waiting entry per visitor; IPv6 visitors per /64.
	if w, r := post("/api/speed/run?id=us-east", vis("1.1.1.1")); w.Code != 429 || r.Code != "queue_full" {
		t.Errorf("second entry: %d %+v", w.Code, r)
	}
	if w, _ := post("/api/speed/run?id=us-east", vis("2001:db8:1:2::1")); w.Code != 202 {
		t.Errorf("v6: %d", w.Code)
	}
	if w, r := post("/api/speed/run?id=Leaseweb-NewYork", vis("2001:db8:1:2::99")); w.Code != 429 || r.Code != "queue_full" {
		t.Errorf("same /64: %d %+v", w.Code, r)
	}
	// The owner goes first.
	lw := do(loc, "POST", "/api/speed/run?id=Leaseweb-NewYork", speedPOSTHeaders())
	var lr resp
	json.Unmarshal(lw.Body.Bytes(), &lr)
	if lw.Code != 202 || lr.Entry.Position != 1 || lr.Entry.Who != "you" {
		t.Errorf("owner: %d %+v", lw.Code, lr)
	}
	// The start limit: after the owner clears the share link's entries, a
	// visitor can't start again within 30 s.
	if w := do(loc, "POST", "/api/speed/cancel?public=1", speedPOSTHeaders()); w.Code != 200 || !strings.Contains(w.Body.String(), `"cancelled":3`) {
		t.Errorf("clear: %d %s", w.Code, w.Body.String())
	}
	if w, r := post("/api/speed/run?id=us-east", vis("1.1.1.1")); w.Code != 429 || r.Code != "rate_limited" || w.Header().Get("Retry-After") == "" {
		t.Errorf("start limit: %d %+v", w.Code, r)
	}
	now = now.Add(31 * time.Second)
	if w, _ := post("/api/speed/run?id=us-east", vis("1.1.1.1")); w.Code != 202 {
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
	body, q1 := get(pub, speedPub+"/api/speed?part=queue", vis("1.1.1.1"))
	if len(q1.Waiting) != 2 || q1.Waiting[0].Who != "someone" || q1.Waiting[1].Who != "you" || q1.Mine == nil || *q1.Mine != q1.Waiting[1].ID ||
		q1.MaxWaiting != 12 || q1.Full {
		t.Errorf("visitor 1: %s", body)
	}
	_, q3 := get(pub, speedPub+"/api/speed?part=queue", vis("3.3.3.3"))
	if q3.Mine != nil || q3.Waiting[1].Who != "someone" {
		t.Errorf("visitor 3: %+v", q3)
	}
	for _, leak := range []string{"1.1.1.1", "2001:db8", "192.168.1.20", "leaseweb.net", "clouvider.net", "visitor"} {
		if strings.Contains(body, leak) {
			t.Errorf("public queue shows %q: %s", leak, body)
		}
	}
	lbody, ql := get(loc, "/api/speed?part=queue", speedHost)
	if ql.Waiting[0].Who != "you" || ql.Waiting[1].Who != "someone" || strings.Contains(lbody, "1.1.1.1") {
		t.Errorf("owner's view: %s", lbody)
	}
	// One entry by id; the last sweep (none finished yet).
	if w := do(pub, "GET", fmt.Sprintf("%s/api/speed?part=entry&id=%d", speedPub, q1.Waiting[1].ID), vis("1.1.1.1")); w.Code != 200 || !strings.Contains(w.Body.String(), `"who":"you"`) {
		t.Errorf("entry: %d %s", w.Code, w.Body.String())
	}
	if w := do(pub, "GET", speedPub+"/api/speed?part=entry&id=999", vis("1.1.1.1")); w.Code != 404 {
		t.Errorf("unknown entry: %d", w.Code)
	}
	if w := do(pub, "GET", speedPub+"/api/speed?part=sweep", vis("1.1.1.1")); w.Code != 200 || !strings.Contains(w.Body.String(), `"entry":null`) {
		t.Errorf("sweep: %d %s", w.Code, w.Body.String())
	}
	if w := do(pub, "GET", speedPub+"/api/speed?part=queue&id=1", vis("1.1.1.1")); w.Code != 400 {
		t.Errorf("id outside part=entry: %d", w.Code)
	}
}

// TestSpeedPublicOptInBudget: the share link's tests are an owner opt-in
// (403 disabled; the Details toggle turns them on), and spend a 24-hour
// data budget (429 budget_exhausted with resets_at); part=queue says so,
// and labels each entry's origin for the owner only.
func TestSpeedPublicOptInBudget(t *testing.T) {
	f := newFakeSpeedEvery(5 * time.Hour)
	f.public, f.budget, f.bytes = false, 5e9, 3e9
	loc, pub, _ := speedHandlers(t, f, LocalOptions{})
	vis := func(ip string) map[string]string {
		return map[string]string{"Host": "x.trycloudflare.com", "Origin": "https://x.trycloudflare.com", "Sec-Fetch-Site": "same-origin", "Cf-Connecting-Ip": ip}
	}
	queue := func(h http.Handler, path string, hdr map[string]string) speedQueueJSON {
		var q speedQueueJSON
		json.Unmarshal(do(h, "GET", path, hdr).Body.Bytes(), &q)
		return q
	}
	w := do(pub, "POST", speedPub+"/api/speed/run?id=us-east", vis("1.1.1.1"))
	if w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"disabled"`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("off: %d %s", w.Code, w.Body.String())
	}
	if q := queue(pub, speedPub+"/api/speed?part=queue", vis("1.1.1.1")); q.PublicStart || q.PublicEnabled {
		t.Errorf("off: %+v", q)
	}
	// The owner turns it on (Details toggle).
	r := httptest.NewRequest("POST", "/api/speed/config", strings.NewReader(`{"public": true}`))
	for k, v := range speedPOSTHeaders() {
		if k == "Host" {
			r.Host = v
		} else {
			r.Header.Set(k, v)
		}
	}
	cw := httptest.NewRecorder()
	loc.ServeHTTP(cw, r)
	if cw.Code != 200 || !strings.Contains(cw.Body.String(), `"public":true`) {
		t.Fatalf("toggle: %d %s", cw.Code, cw.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.Queue().Run(ctx)
	for i, ip := range []string{"1.1.1.1", "2.2.2.2"} {
		if w := do(pub, "POST", speedPub+"/api/speed/run?id=us-east", vis(ip)); w.Code != 202 {
			t.Fatalf("start %d: %d %s", i, w.Code, w.Body.String())
		}
		for queue(loc, "/api/speed?part=queue", speedHost).Budget.UsedBytes < int64(i+1)*3e9 {
			time.Sleep(2 * time.Millisecond)
		}
	}
	q := queue(pub, speedPub+"/api/speed?part=queue", vis("3.3.3.3"))
	if q.PublicStart || !q.PublicEnabled || q.Budget.LimitBytes != 5e9 || q.Budget.UsedBytes != 6e9 || q.Budget.RemainingBytes != 0 || q.Budget.ResetsAt == nil {
		t.Errorf("spent: %+v", q)
	}
	w = do(pub, "POST", speedPub+"/api/speed/run?kind=sweep", vis("3.3.3.3"))
	var e struct {
		Code       string
		ResetsAt   int64 `json:"resets_at"`
		RetryAfter int   `json:"retry_after"`
	}
	json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != 429 || e.Code != "budget_exhausted" || e.ResetsAt != *q.Budget.ResetsAt || e.RetryAfter <= 0 || w.Header().Get("Retry-After") == "" {
		t.Errorf("budget: %d %s", w.Code, w.Body.String())
	}
	// The owner is not limited; origins are labelled for the owner only.
	if w := do(loc, "POST", "/api/speed/run?kind=sweep", speedPOSTHeaders()); w.Code != 202 || !strings.Contains(w.Body.String(), `"origin":"owner"`) {
		t.Errorf("owner: %d %s", w.Code, w.Body.String())
	}
	if body := do(pub, "GET", speedPub+"/api/speed?part=queue", vis("3.3.3.3")).Body.String(); strings.Contains(body, `"origin"`) {
		t.Errorf("origin on the share link: %s", body)
	}
}
