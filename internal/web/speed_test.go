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
)

// fakeSpeed is a SpeedSource over a fixed list of tests.
type fakeSpeed struct {
	mu      sync.Mutex
	st      speedtest.State
	tests   []model.SpeedTest
	started int
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

func (f *fakeSpeed) TestNow() (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st.Running != nil {
		return time.Time{}, speedtest.ErrRunning
	}
	f.started++
	f.st.Running = &speedtest.RunState{Started: speedNow, Direction: "picking"}
	return speedNow, nil
}

func (f *fakeSpeed) SetEnabled(_ context.Context, on bool) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.Enabled = on
	f.st.Next = time.Time{}
	if on {
		f.st.Next = speedNow.Add(2 * time.Minute)
	}
	return f.st.Next, nil
}

var speedNow = time.Date(2026, 10, 10, 18, 0, 0, 0, time.UTC)

// newFakeSpeed has 30 days of tests every 15 minutes, 40% slower from
// 19:00 to 23:00 UTC, with a busy test every 50th and an unreachable one
// every 97th.
func newFakeSpeed() *fakeSpeed { return newFakeSpeedEvery(15 * time.Minute) }

func newFakeSpeedEvery(step time.Duration) *fakeSpeed {
	f := &fakeSpeed{}
	ms := func(v float64) *float64 { return &v }
	london, ams := 12.4, 18.9
	f.st = speedtest.State{Enabled: true, Interval: 15 * time.Minute, Next: speedNow.Add(7 * time.Minute),
		Servers: []speedtest.ServerState{
			{Name: "Clouvider-London", City: "London", Country: "GB", RTTms: &london, Picked: true},
			{Name: "Eranium-Amsterdam", City: "Amsterdam", Country: "NL", RTTms: &ams, Picked: true},
			{Name: "Edgoo-SaoPaulo", City: "Sao Paulo", Country: "BR"},
		}}
	start := speedNow.Add(-30 * 24 * time.Hour)
	for i := range int(30 * 24 * time.Hour / step) {
		at := start.Add(time.Duration(i) * step)
		t := model.SpeedTest{ID: int64(i + 1), Start: at, End: at.Add(23 * time.Second), Trigger: "schedule", Outcome: model.SpeedOK,
			Server: []string{"Clouvider-London", "Eranium-Amsterdam"}[i%2], Port: 5200 + i%10, Attempts: 2}
		switch {
		case i%50 == 7:
			t.Outcome, t.Attempts, t.End = model.SpeedBusy, 3, at.Add(6*time.Second)
		case i%97 == 5:
			t.Outcome, t.Attempts = model.SpeedUnreachable, 1
		default:
			down := 400e6 * (1 + 0.03*float64(i%7-3))
			if h := at.Hour(); h >= 19 && h <= 22 {
				down *= 0.6
			}
			up := down / 10
			re := int64(i % 40)
			t.DownBps, t.UpBps, t.DownRetrans = &down, &up, &re
			t.IdleGatewayMs, t.LoadedGatewayMs, t.IdleCallMs, t.LoadedCallMs = ms(1.1), ms(9.8), ms(14.2), ms(61)
		}
		f.tests = append(f.tests, t)
	}
	return f
}

func speedDeps(t *testing.T) (Deps, *fakeSpeed) {
	_, d, _ := fixture(t, speedNow)
	f := newFakeSpeed()
	d.Speed = f
	return d, f
}

// TestSpeedGolden pins api/speed's JSON on both listeners (7 days, a
// 2-hour time zone; a test every 5 hours keeps the files small): go test
// ./internal/web -run TestSpeedGolden -update.
func TestSpeedGolden(t *testing.T) {
	d, _ := speedDeps(t)
	d.Speed = newFakeSpeedEvery(5 * time.Hour)
	loc := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"}).(*localHandler)
	loc.now = func() time.Time { return speedNow }
	pub := Public(d, "0123456789abcdef0123456789abcdef").(*publicHandler)
	pub.now = func() time.Time { return speedNow }
	for name, get := range map[string]func() string{
		"local": func() string {
			return do(loc, "GET", "/api/speed?days=7&tz=120", map[string]string{"Host": "127.0.0.1:3000"}).Body.String()
		},
		"public": func() string {
			return do(pub, "GET", "/s/0123456789abcdef0123456789abcdef/api/speed?days=7&tz=120", nil).Body.String()
		},
	} {
		raw := get()
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
	d, _ := speedDeps(t)
	loc := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"}).(*localHandler)
	loc.now = func() time.Time { return speedNow }
	pub := Public(d, "0123456789abcdef0123456789abcdef").(*publicHandler)
	pub.now = func() time.Time { return speedNow }
	host := map[string]string{"Host": "127.0.0.1:3000"}
	var s speedJSON
	if err := json.Unmarshal(do(loc, "GET", "/api/speed?days=30", host).Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Tests) != 30*96 || s.Latest == nil || s.Latest.ID != 30*96 || !s.Configurable || !s.Enabled || s.IntervalS != 900 || len(s.Servers) != 3 {
		t.Errorf("tests %d latest %+v configurable %v", len(s.Tests), s.Latest, s.Configurable)
	}
	if s.Pattern == nil || s.Pattern.Text != "Evenings are slower: about 40% below your daytime speed." || s.Pattern.DipFrom != 19 || s.Pattern.DipTo != 23 {
		t.Errorf("pattern %+v", s.Pattern)
	}
	if h := s.ByHour.Hours[20]; h.N < 100 || *h.DownMedian > 250e6 || len(s.ByHour.DipHours) != 4 {
		t.Errorf("hour 20 %+v dips %v", h, s.ByHour.DipHours)
	}
	// Busy tests are gaps, never slow results: no speed, outcome busy.
	for _, x := range s.Tests {
		if x.Outcome != model.SpeedOK && (x.DownBps != nil || x.UpBps != nil) {
			t.Fatalf("%+v", x)
		}
	}
	// tz moves the hours and the dip with them.
	json.Unmarshal(do(loc, "GET", "/api/speed?tz=-300", host).Body.Bytes(), &s)
	if s.Pattern == nil || s.Pattern.DipFrom != 14 || s.Pattern.Text != "Afternoons are slower: about 40% below your usual speed." || s.TZ != -300 {
		t.Errorf("tz pattern %+v", s.Pattern)
	}
	// The share link: no servers, cities or ports, never configurable.
	b := do(pub, "GET", "/s/0123456789abcdef0123456789abcdef/api/speed", nil).Body.String()
	for _, leak := range []string{"Clouvider", "London", "Amsterdam", `"port"`, "rtt_ms", `"configurable":true`} {
		if strings.Contains(b, leak) {
			t.Errorf("share link leaks %s", leak)
		}
	}
	// Windows for a chart range.
	var w struct{ Windows []speedWindow }
	json.Unmarshal(do(loc, "GET", "/api/speed?part=windows&from=now-1h&to=now", host).Body.Bytes(), &w)
	if len(w.Windows) != 4 || w.Windows[0].End-w.Windows[0].Start < 1000 {
		t.Errorf("windows %+v", w.Windows)
	}
	for q, code := range map[string]int{
		"days=14": 400, "tz=900": 400, "part=x": 400, "part=windows&days=7": 400, "from=now-1h": 400,
"bogus=1": 400, "part=windows&from=now&to=now-1h": 400,
	} {
		if w := do(loc, "GET", "/api/speed?"+q, host); w.Code != code {
			t.Errorf("%s: %d %s", q, w.Code, w.Body.String())
		}
	}
	// Without a source every speed route is 404.
	d.Speed = nil
	loc2 := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"})
	if w := do(loc2, "GET", "/api/speed", host); w.Code != 404 {
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
	d, f := speedDeps(t)
	loc := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"})
	good := speedPOSTHeaders()
	if w := do(loc, "POST", "/api/speed/run", good); w.Code != 202 || f.started != 1 {
		t.Fatalf("run: %d %s", w.Code, w.Body.String())
	}
	if w := do(loc, "POST", "/api/speed/run", good); w.Code != 409 {
		t.Errorf("second run: %d", w.Code)
	}
	bad := map[string]string{"Host": "127.0.0.1:3000", "Origin": "http://evil.com", HeaderCSRF: "tok123"}
	if w := do(loc, "POST", "/api/speed/run", bad); w.Code != 403 {
		t.Errorf("cross-origin: %d", w.Code)
	}
	if w := do(loc, "GET", "/api/speed/run", good); w.Code != 405 {
		t.Errorf("GET run: %d", w.Code)
	}
	post := func(body string, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/speed/config", strings.NewReader(body))
		for k, v := range hdr {
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
	if w := post(`{"enabled": false}`, good); w.Code != 200 || f.st.Enabled || !strings.Contains(w.Body.String(), `"next_at":null`) {
		t.Errorf("disable: %d %s", w.Code, w.Body.String())
	}
	if w := post(`{"enabled": true}`, good); w.Code != 200 || !f.st.Enabled || !strings.Contains(w.Body.String(), `"next_at":1791655320000`) {
		t.Errorf("enable: %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{``, `{}`, `{"enabled": "yes"}`, `{"enabled": true, "x": 1}`} {
		if w := post(body, good); w.Code != 400 {
			t.Errorf("%q: %d", body, w.Code)
		}
	}
	// An admin token is required when set.
	adm := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123", AdminToken: "sekrit"})
	f.st.Running = nil
	if w := do(adm, "POST", "/api/speed/run", good); w.Code != 403 {
		t.Errorf("without admin token: %d", w.Code)
	}
	var s speedJSON
	json.Unmarshal(do(adm, "GET", "/api/speed", map[string]string{"Host": "127.0.0.1:3000"}).Body.Bytes(), &s)
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
	if w := do(pub, "POST", "/s/0123456789abcdef0123456789abcdef/api/speed/run", nil); w.Code != 404 && w.Code != 405 {
		t.Errorf("share link run: %d", w.Code)
	}
}

func TestHomeSpeedLine(t *testing.T) {
	d, f := speedDeps(t)
	loc := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"}).(*localHandler)
	loc.now = func() time.Time { return speedNow }
	host := map[string]string{"Host": "127.0.0.1:3000"}
	var l HomeLive
	json.Unmarshal(do(loc, "GET", "/api/home?part=live&tz=120", host).Body.Bytes(), &l)
	if l.Speed == nil || l.Speed.Text != "Evenings are slower: about 40% below your daytime speed." || l.Speed.Level != "info" {
		t.Errorf("speed line %+v", l.Speed)
	}
	f.tests = f.tests[:40] // ten hours: no clear pattern yet
	loc = Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"}).(*localHandler)
	loc.now = func() time.Time { return speedNow }
	l = HomeLive{}
	json.Unmarshal(do(loc, "GET", "/api/home?part=live", host).Body.Bytes(), &l)
	if l.Speed != nil {
		t.Errorf("speed line without a pattern: %+v", l.Speed)
	}
}
