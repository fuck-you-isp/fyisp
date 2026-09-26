package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// fakeVerdict is a VerdictSource with fixed answers.
type fakeVerdict struct {
	v     model.Verdict
	list  []model.Incident
	err   error
	calls atomic.Int32
	mu    sync.Mutex
	from  time.Time
	to    time.Time
}

func (f *fakeVerdict) Current() model.Verdict { return f.v }
func (f *fakeVerdict) Incidents(_ context.Context, from, to time.Time) ([]model.Incident, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.from, f.to = from, to
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.list, nil
}

// cleanVerdict is what a well-behaved engine reports: IP-free summaries,
// profile target names and identifier evidence keys.
func cleanVerdict(now time.Time) *fakeVerdict {
	return &fakeVerdict{
		v: model.Verdict{Kind: model.VerdictService, Since: now.Add(-7 * time.Minute),
			Summary: "Your connection is fine; Alpha is having trouble.", Targets: []string{"Alpha"},
			Evidence: map[string]float64{"gateway_loss": 0, "edge_loss": 0.01, "services_loss": 0.4}},
		list: []model.Incident{
			{ID: 2, Start: now.Add(-7 * time.Minute), Kind: model.VerdictService, Summary: "Alpha is having trouble.", Targets: []string{"Alpha"}, PeakLoss: 0.5},
			{ID: 1, Start: now.Add(-3 * time.Hour), End: now.Add(-170 * time.Minute), Kind: model.VerdictISP, Summary: "Your ISP dropped packets.", PeakLoss: 0.9},
		},
	}
}

func localVerdictHandler(t *testing.T, vs VerdictSource) (*localHandler, map[string]string) {
	t.Helper()
	_, d, _ := fixture(t, time.Now())
	d.Verdict = vs
	return Local(d, LocalOptions{Addr: "127.0.0.1:3000"}).(*localHandler), map[string]string{"Host": "127.0.0.1:3000"}
}

func TestVerdictNilSource(t *testing.T) {
	_, d, _ := fixture(t, time.Now())
	d.Verdict = nil
	for _, tc := range []struct {
		name, prefix string
		hdr          map[string]string
		h            http.Handler
	}{
		{"local", "/", map[string]string{"Host": "127.0.0.1:3000"}, Local(d, LocalOptions{Addr: "127.0.0.1:3000"})},
		{"public", "/s/" + testSecret + "/", map[string]string{"Cf-Connecting-Ip": "198.51.100.30"}, Public(d, testSecret)},
	} {
		w := do(tc.h, "GET", tc.prefix+"api/verdict", tc.hdr)
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"kind":"unknown"}` {
			t.Errorf("%s verdict = %d %s", tc.name, w.Code, w.Body.String())
		}
		w = do(tc.h, "GET", tc.prefix+"api/incidents?from=now-24h", tc.hdr)
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `[]` {
			t.Errorf("%s incidents = %d %s", tc.name, w.Code, w.Body.String())
		}
	}
}

func TestVerdictShape(t *testing.T) {
	now := time.Now()
	fv := cleanVerdict(now)
	fv.v.Evidence["nan"] = math.NaN()
	h, host := localVerdictHandler(t, fv)
	w := do(h, "GET", "/api/verdict", host)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body.String())
	}
	var v struct {
		Kind     string             `json:"kind"`
		Since    time.Time          `json:"since"`
		Summary  string             `json:"summary"`
		Targets  []string           `json:"targets"`
		Evidence map[string]float64 `json:"evidence"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Kind != "service" || !v.Since.Equal(fv.v.Since.Truncate(time.Nanosecond)) || v.Summary != fv.v.Summary ||
		len(v.Targets) != 1 || v.Evidence["services_loss"] != 0.4 {
		t.Errorf("verdict = %+v", v)
	}
	if _, ok := v.Evidence["nan"]; ok {
		t.Error("NaN evidence served")
	}
	// A zero verdict (engine not started) has no since.
	fv.v = model.Verdict{Kind: model.VerdictWarmingUp}
	if b := do(h, "GET", "/api/verdict", host).Body.String(); strings.TrimSpace(b) != `{"kind":"warming_up"}` {
		t.Errorf("warming up = %s", b)
	}
}

type incResp []struct {
	ID       int64      `json:"id"`
	Start    time.Time  `json:"start"`
	End      *time.Time `json:"end"`
	Kind     string     `json:"kind"`
	Summary  string     `json:"summary"`
	Targets  []string   `json:"targets"`
	PeakLoss float64    `json:"peak_loss"`
}

func TestIncidentsShapeAndClamps(t *testing.T) {
	now := time.Now()
	fv := cleanVerdict(now)
	h, host := localVerdictHandler(t, fv)
	w := do(h, "GET", "/api/incidents?from=now-24h&to=now", host)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var r incResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if len(r) != 2 || r[0].ID != 2 || r[0].End != nil || r[1].End == nil || r[1].Kind != "isp" || r[0].PeakLoss != 0.5 {
		t.Errorf("incidents = %+v", r)
	}
	if strings.Contains(w.Body.String(), "0001-01-01") {
		t.Error("zero end time served")
	}
	if got := now.Sub(fv.from); got < 24*time.Hour-time.Minute || got > 24*time.Hour+time.Minute {
		t.Errorf("from = %v ago", got)
	}

	// Range clamps: at most 90d, never past now.
	do(h, "GET", fmt.Sprintf("/api/incidents?from=now-400d&to=%d", now.Add(48*time.Hour).UnixMilli()), host)
	if fv.to.After(time.Now()) || fv.to.Sub(fv.from) != MaxRange {
		t.Errorf("clamped range = %v .. %v", fv.from, fv.to)
	}
	from := now.Add(-2 * time.Hour).UnixMilli()
	do(h, "GET", fmt.Sprintf("/api/incidents?from=%d&to=%d", from, now.Add(-time.Hour).UnixMilli()), host)
	if fv.from.UnixMilli() != from {
		t.Errorf("absolute from = %v", fv.from)
	}
	// Defaults: the last 30 minutes.
	do(h, "GET", "/api/incidents", host)
	if got := fv.to.Sub(fv.from); got != 30*time.Minute {
		t.Errorf("default range = %v", got)
	}
	for _, bad := range []string{"from=now-", "from=yesterday", "from=now&to=now-1h", "to=x", "group=common", "from=now-1h&from=now-2h", "from=now-5y"} {
		if w := do(h, "GET", "/api/incidents?"+bad, host); w.Code != 400 {
			t.Errorf("%s = %d, want 400", bad, w.Code)
		}
	}
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		if w := do(h, m, "/api/incidents", host); w.Code != 405 {
			t.Errorf("%s /api/incidents = %d", m, w.Code)
		}
		if w := do(h, m, "/api/verdict", host); w.Code != 405 {
			t.Errorf("%s /api/verdict = %d", m, w.Code)
		}
	}

	// At most MaxIncidents items, newest (first) kept.
	fv.list = nil
	for i := range MaxIncidents + 100 {
		fv.list = append(fv.list, model.Incident{ID: int64(10000 - i), Start: now.Add(-time.Duration(i) * time.Minute), End: now.Add(-time.Duration(i)*time.Minute + 30*time.Second), Kind: model.VerdictLAN, Summary: "Wi-Fi trouble.", PeakLoss: 2})
	}
	r = nil
	_ = json.Unmarshal(do(h, "GET", "/api/incidents?from=now-90d", host).Body.Bytes(), &r)
	if len(r) != MaxIncidents || r[0].ID != 10000 || r[0].PeakLoss != 1 {
		t.Errorf("capped: %d items, first %+v", len(r), r[0])
	}

	// Engine errors: detail locally, nothing publicly.
	fv.err = errors.New("sqlite: /home/venkat/.local/state/fyisp/fyisp.db locked")
	if w := do(h, "GET", "/api/incidents?from=now-1h", host); w.Code != 500 || !strings.Contains(w.Body.String(), "locked") {
		t.Errorf("local error = %d %s", w.Code, w.Body.String())
	}
}

// TestVerdictPublicRedaction: the public link drops target names that are not
// in the profile and evidence keys that are not plain identifiers. Summaries
// are served as the engine wrote them: the engine guarantees they are
// IP-free (see model.Verdict), and this handler never adds anything to them.
func TestVerdictPublicRedaction(t *testing.T) {
	now := time.Now()
	_, d, _ := fixture(t, now)
	planted := "Your router at 192.168.1.1 is dropping packets."
	fv := &fakeVerdict{
		v: model.Verdict{Kind: model.VerdictLAN, Since: now.Add(-time.Minute), Summary: planted,
			Targets:  []string{"Alpha", "192.168.1.1", "Ghost", "router.lan"},
			Evidence: map[string]float64{"gateway_loss": 0.5, "10.0.0.1_loss": 1, "loss:router.lan": 1, "Gateway-Loss": 1, "edge_rtt_ms": 12}},
		list: []model.Incident{{ID: 1, Start: now.Add(-time.Minute), Kind: model.VerdictLAN, Summary: planted, Targets: []string{"NAS", "10.1.2.3"}, PeakLoss: 0.5}},
	}
	d.Verdict = fv
	h := Public(d, testSecret)
	base := "/s/" + testSecret + "/"
	hdr := map[string]string{"Cf-Connecting-Ip": "198.51.100.40"}

	var v struct {
		Summary  string             `json:"summary"`
		Targets  []string           `json:"targets"`
		Evidence map[string]float64 `json:"evidence"`
	}
	if err := json.Unmarshal(do(h, "GET", base+"api/verdict", hdr).Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if strings.Join(v.Targets, ",") != "Alpha" {
		t.Errorf("public targets = %v, want [Alpha]", v.Targets)
	}
	if len(v.Evidence) != 2 || v.Evidence["gateway_loss"] != 0.5 || v.Evidence["edge_rtt_ms"] != 12 {
		t.Errorf("public evidence = %v", v.Evidence)
	}
	if v.Summary != planted {
		t.Errorf("summary rewritten: %q (the engine owns summaries; we pass them through)", v.Summary)
	}
	var r incResp
	_ = json.Unmarshal(do(h, "GET", base+"api/incidents?from=now-1h", hdr).Body.Bytes(), &r)
	if len(r) != 1 || strings.Join(r[0].Targets, ",") != "NAS" {
		t.Errorf("public incidents = %+v", r)
	}
	// Locally, everything is shown as is.
	lh := Local(d, LocalOptions{Addr: "127.0.0.1:3000"})
	b := do(lh, "GET", "/api/verdict", map[string]string{"Host": "127.0.0.1:3000"}).Body.String()
	if !strings.Contains(b, "10.0.0.1_loss") || !strings.Contains(b, "Ghost") {
		t.Errorf("local verdict filtered: %s", b)
	}

	// With planted values removed, nothing we add leaks: the only addresses a
	// response could carry are the ones the engine put in its summary.
	fv.v.Summary, fv.list[0].Summary = "Your Wi-Fi or router is dropping packets.", "Wi-Fi trouble."
	for _, p := range []string{"api/verdict", "api/incidents?from=now-2h&to=now", "api/profile"} {
		w := do(h, "GET", base+p, map[string]string{"Cf-Connecting-Ip": "198.51.100.41"})
		if m := leakRE.FindString(w.Body.String()); m != "" {
			t.Errorf("public %s leaks %q: %s", p, m, w.Body.String())
		}
	}
	// Public errors carry no detail.
	fv.err = errors.New("sqlite: /home/venkat/x.db: 192.168.1.20")
	w := do(h, "GET", base+"api/incidents?from=now-3h", map[string]string{"Cf-Connecting-Ip": "198.51.100.42"})
	if w.Code != 500 || leakRE.MatchString(w.Body.String()) {
		t.Errorf("public error = %d %s", w.Code, w.Body.String())
	}
}

func TestIncidentsPublicCache(t *testing.T) {
	now := time.Now()
	_, d, _ := fixture(t, now)
	fv := cleanVerdict(now)
	d.Verdict = fv
	h := Public(d, testSecret)
	for i := range 3 {
		w := do(h, "GET", "/s/"+testSecret+"/api/incidents?from=now-24h&to=now", map[string]string{"Cf-Connecting-Ip": fmt.Sprintf("203.0.113.%d", i)})
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if n := fv.calls.Load(); n != 1 {
		t.Errorf("3 identical public incident queries made %d engine calls, want 1", n)
	}
}

func TestProfileLayers(t *testing.T) {
	_, d, _ := fixture(t, time.Now())
	p := testProfile()
	p.Groups = append(p.Groups, model.Group{ID: "path", Title: "Network path"})
	p.Targets = append(p.Targets,
		model.Target{Name: "Gateway", Host: model.HostGateway, Group: "path", Layer: model.LayerGateway, Kinds: []model.ProbeKind{model.KindICMP}},
		model.Target{Name: "ISP-edge", Host: model.HostEdge, Group: "path", Layer: model.LayerEdge, Kinds: []model.ProbeKind{model.KindICMP}},
		model.Target{Name: "Anycast-A", Host: "192.0.2.53", Group: "path", Layer: model.LayerAnycast, Kinds: []model.ProbeKind{model.KindICMP, model.KindTCP}},
		model.Target{Name: "Odd", Host: "x", Group: "path", Layer: "../etc/passwd"},
	)
	d.Profile = func() *model.Profile { return p }
	for _, tc := range []struct {
		name, base string
		hdr        map[string]string
		h          http.Handler
	}{
		{"local", "/", map[string]string{"Host": "127.0.0.1:3000"}, Local(d, LocalOptions{Addr: "127.0.0.1:3000"})},
		{"public", "/s/" + testSecret + "/", map[string]string{"Cf-Connecting-Ip": "198.51.100.50"}, Public(d, testSecret)},
	} {
		b := do(tc.h, "GET", tc.base+"api/profile", tc.hdr).Body.String()
		for _, s := range []string{"@gateway", "@isp-edge", "192.0.2.53", "passwd"} {
			if strings.Contains(b, s) {
				t.Errorf("%s profile leaks %q: %s", tc.name, s, b)
			}
		}
		var pj profileJSON
		if err := json.Unmarshal([]byte(b), &pj); err != nil {
			t.Fatal(err)
		}
		layers := map[string]string{}
		for _, x := range pj.Targets {
			layers[x.Name] = x.Layer
		}
		if layers["Gateway"] != "gateway" || layers["ISP-edge"] != "isp-edge" || layers["Anycast-A"] != "anycast" || layers["Odd"] != "" || layers["Alpha"] != "" {
			t.Errorf("%s layers = %v", tc.name, layers)
		}
		if strings.Count(b, `"layer"`) != 3 {
			t.Errorf("%s: layer on non-path targets: %s", tc.name, b)
		}
	}
}

func TestVerdictPublicRoutes(t *testing.T) {
	_, d, _ := fixture(t, time.Now())
	h := Public(d, testSecret)
	base := "/s/" + testSecret + "/"
	for i, c := range []struct {
		method, path string
		code         int
	}{
		{"GET", base + "api/verdict", 200},
		{"HEAD", base + "api/verdict", 200},
		{"GET", base + "api/incidents", 200},
		{"GET", base + "api/incidents?from=now-7d&to=now", 200},
		{"POST", base + "api/verdict", 405},
		{"POST", base + "api/incidents", 405},
		{"GET", base + "api/verdict?x=1", 400},
		{"GET", base + "api/incidents?group=common", 400},
		{"GET", base + "api/incidents?from=now&to=now-1h", 400},
		{"GET", "/api/verdict", 404},
		{"GET", "/api/incidents", 404},
		{"GET", "/s/wrong-secret-wrong-secret-xx/api/verdict", 404},
	} {
		w := do(h, c.method, c.path, map[string]string{"Cf-Connecting-Ip": fmt.Sprintf("203.0.113.%d", i)})
		if w.Code != c.code {
			t.Errorf("%s %s = %d, want %d (%s)", c.method, c.path, w.Code, c.code, w.Body.String())
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") {
			t.Errorf("%s %s: no CSP", c.method, c.path)
		}
	}
}
