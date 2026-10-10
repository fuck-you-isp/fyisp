package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

var speedNow = time.Date(2026, 10, 10, 18, 0, 0, 0, time.UTC)

// fakeSpeed is a SpeedSource over a fixed list of tests.
type fakeSpeed struct {
	mu      sync.Mutex
	st      speedtest.State
	tests   []model.SpeedTest
	started []string
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

func (f *fakeSpeed) TestNow(id string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st.Running != nil {
		return time.Time{}, speedtest.ErrRunning
	}
	if id != "" && id != "us-east" && id != "Leaseweb-NewYork" && id != "192.168.1.20:5201" {
		return time.Time{}, context.Canceled // any error: unknown
	}
	f.started = append(f.started, id)
	f.st.Running = &speedtest.RunState{Started: speedNow, ExpectedEnd: speedNow.Add(20 * time.Second), Direction: "picking"}
	return speedNow, nil
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
	f := &fakeSpeed{}
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
				t.IdleGatewayMs, t.LoadedGatewayMs, t.Conn = ms(1.1), ms(9.8), model.ConnWired
				t.Latency = &model.SpeedLatency{
					Edge:    model.LoadPhases{Idle: &model.LoadStats{N: 40, P50Ms: 8.2, P95Ms: 9.9}, Down: &model.LoadStats{N: 60, P50Ms: 24, P95Ms: 61.5}, Up: &model.LoadStats{N: 58, Lost: 3, Loss: 0.052, P50Ms: 140, P95Ms: 1000}},
					Gateway: model.LoadPhases{Idle: &model.LoadStats{N: 40, P50Ms: 1.1, P95Ms: 1.9}, Down: &model.LoadStats{N: 60, P50Ms: 9.8, P95Ms: 14}}}
				t.Handoff = &model.Handoff{ASN: 3356, Name: "LEVEL3", ISPASN: 3320, At: t.Start}
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
	if len(s.PerServer) != 4 || s.Latest == nil || s.Servers[0].Host == "" || s.Servers[0].Ports != "5200-5209" {
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
	if w := do(loc, "POST", "/api/speed/run?id=us-east", good); w.Code != 202 || f.started[0] != "us-east" {
		t.Fatalf("run: %d %s", w.Code, w.Body.String())
	}
	w := do(loc, "POST", "/api/speed/run", good)
	var e struct{ Code string }
	json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != 409 || e.Code != "running" {
		t.Errorf("second run: %d %s", w.Code, w.Body.String())
	}
	f.st.Running = nil
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
	// Never on the share link.
	pub := Public(d, "0123456789abcdef0123456789abcdef")
	if w := do(pub, "POST", speedPub+"/api/speed/run", nil); w.Code != 404 && w.Code != 405 {
		t.Errorf("share link run: %d", w.Code)
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

// TestSpeedEdgeHours: api/speed's edge_hours folds the ISP edge's stored
// RTT and loss into local hours of day (the access network's control).
func TestSpeedEdgeHours(t *testing.T) {
	loc, pub, d := speedHandlers(t, newFakeSpeedEvery(5*time.Hour), LocalOptions{})
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
	_, _, d2 := speedHandlers(t, newFakeSpeedEvery(5*time.Hour), LocalOptions{})
	l2 := Local(d2, LocalOptions{Addr: "127.0.0.1:3000"}).(*localHandler)
	l2.now = func() time.Time { return speedNow }
	if err := json.Unmarshal(do(l2, "GET", "/api/speed", speedHost).Body.Bytes(), &s); err != nil || string(s["edge_hours"]) != "null" {
		t.Errorf("edge_hours without an edge target: %s %v", s["edge_hours"], err)
	}
}
