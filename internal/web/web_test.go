package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/probe"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func testProfile() *model.Profile {
	return &model.Profile{
		Name: "test",
		Groups: []model.Group{
			{ID: "common", Title: "Common Services", Order: 1},
			{ID: "lan", Title: "LAN", Order: 2},
		},
		Targets: []model.Target{
			{Name: "Alpha", Host: "alpha.example.com", Group: "common", Path: "/secret-path"},
			{Name: "Beta", Host: "beta.example.com", Group: "common", Kinds: []model.ProbeKind{model.KindHTTPS}},
			{Name: "Router", Host: "192.168.1.1", Group: "lan", Port: 8443},
			{Name: "NAS", Host: "10.1.2.3", Group: "lan", HostOverrides: map[model.ProbeKind]string{model.KindICMP: "172.16.0.9"}},
		},
	}
}

// countingStore wraps a store.Reader, counting and recording Panel calls.
type countingStore struct {
	store.Reader
	calls  atomic.Int32
	mu     sync.Mutex
	last   store.PanelQuery
	err    error
	delay  time.Duration
	oldest time.Time
}

func (c *countingStore) Panel(ctx context.Context, q store.PanelQuery) (*store.PanelResult, error) {
	c.calls.Add(1)
	c.mu.Lock()
	c.last = q
	c.mu.Unlock()
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.Reader.Panel(ctx, q)
}

func (c *countingStore) Stats(ctx context.Context) (store.Stats, error) {
	return store.Stats{Dir: "/home/venkat/.local/state/fyisp", FileBytes: 1234, Oldest: c.oldest,
		LastFlushErr: "open /var/lib/fyisp/fyisp.db: disk full on 192.168.1.20"}, nil
}

type fakeShare struct {
	mu    sync.Mutex
	st    ShareState
	start int
}

func (f *fakeShare) State() ShareState { f.mu.Lock(); defer f.mu.Unlock(); return f.st }
func (f *fakeShare) Start(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.start++
	f.st = ShareState{Phase: ShareConnected, URL: "https://x.trycloudflare.com/s/" + testSecret + "/", Protocol: "quic", LastErr: "dial 10.0.0.1:7844 failed"}
	return nil
}
func (f *fakeShare) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st = ShareState{Phase: ShareOff}
	return nil
}

// fixture fills a fake store: 15s HTTPS/TCP and 5s ICMP samples for the last
// 40 minutes, with a timeout outage for Alpha HTTPS (20-22 min ago) and a
// not-measured hole for everyone (30-35 min ago).
func fixture(t *testing.T, now time.Time) (*countingStore, Deps, *fakeShare) {
	t.Helper()
	f := store.NewFake()
	p := testProfile()
	for _, tg := range p.Targets {
		for _, k := range kindsOf(tg) {
			iv := intervalOf(tg, k)
			for ts := now.Add(-40 * time.Minute).Truncate(iv); ts.Before(now); ts = ts.Add(iv) {
				age := now.Sub(ts)
				if age > 30*time.Minute && age <= 35*time.Minute {
					continue // not measured
				}
				s := model.Sample{Key: model.SeriesKey{Target: tg.Name, Kind: k}, Slot: ts, RTT: 20 * time.Millisecond}
				if tg.Name == "Alpha" && k == model.KindHTTPS && age > 20*time.Minute && age <= 22*time.Minute {
					s.Lost, s.Reason, s.RTT = true, model.ReasonTimeout, 0
				}
				f.Observe(s)
			}
		}
	}
	cs := &countingStore{Reader: f}
	sh := &fakeShare{st: ShareState{Phase: ShareOff}}
	d := Deps{
		Profile: func() *model.Profile { return p },
		Store:   cs,
		Status: func() Status {
			return Status{
				Version: "v9.9.9-build-/home/venkat/src", Started: now.Add(-time.Hour),
				Caps:    probe.Caps{ICMP: "unavailable", TCP: true, HTTPS: true, ICMPHint: "set /proc/sys/net/ipv4/ping_group_range"},
				Targets: 4, Ready: 3,
			}
		},
		Metrics: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ping_rtt_seconds 1\n") }),
		Share:   sh,
	}
	return cs, d, sh
}

func do(h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
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

// ---------- public ----------

func TestPublicRoutes(t *testing.T) {
	_, d, _ := fixture(t, time.Now())
	h := Public(d, testSecret)
	base := "/s/" + testSecret + "/"
	cases := []struct {
		method, path string
		code         int
	}{
		{"GET", "/", 404},
		{"GET", "/metrics", 404},
		{"GET", "/debug/pprof/", 404},
		{"GET", "/debug/pprof/heap", 404},
		{"GET", "/api/status", 404},
		{"GET", "/static/app.js", 404},
		{"GET", "/s/", 404},
		{"GET", "/s/wrong-secret-wrong-secret-xx/", 404},
		{"GET", "/s/" + testSecret[:31] + "/", 404},
		{"GET", base + "metrics", 404},
		{"GET", base + "debug/pprof/", 404},
		{"GET", base + "api/share/start", 404},
		{"POST", base + "api/share/start", 404},
		{"GET", base + "nope", 404},
		{"GET", base + "static/../api/status", 404},
		{"GET", "/s/" + testSecret, 301},
		{"GET", base, 200},
		{"HEAD", base, 200},
		{"GET", base + "static/app.js", 200},
		{"GET", base + "static/vendor/uPlot.iife.min.js", 200},
		{"GET", base + "api/status", 200},
		{"GET", base + "api/profile", 200},
		{"GET", base + "api/panel?group=common", 200},
		{"GET", base + "api/panel.csv?group=common", 200},
		{"POST", base, 405},
		{"POST", base + "api/panel?group=common", 405},
		{"PUT", base + "api/status", 405},
		{"DELETE", base + "api/profile", 405},
		{"OPTIONS", base + "api/panel", 405},
		{"GET", base + "api/status?x=1", 400},
		{"GET", base + "api/panel?group=common&debug=1", 400},
		{"GET", base + "api/panel?group=common&group=lan", 400},
		{"GET", base + "api/panel?group=nope", 404},
	}
	for i, c := range cases {
		w := do(h, c.method, c.path, map[string]string{"Cf-Connecting-Ip": fmt.Sprintf("203.0.113.%d", i)})
		if w.Code != c.code {
			t.Errorf("%s %s = %d, want %d (%s)", c.method, c.path, w.Code, c.code, w.Body.String())
		}
		hd := w.Header()
		if !strings.Contains(hd.Get("Content-Security-Policy"), "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'") ||
			hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s %s: missing security headers: %v", c.method, c.path, hd)
		}
	}
}

var leakRE = regexp.MustCompile(`\b(10\.\d+\.\d+\.\d+|192\.168\.\d+\.\d+|172\.(1[6-9]|2\d|3[01])\.\d+\.\d+|127\.\d+\.\d+\.\d+|169\.254\.\d+\.\d+)\b|\[?::1\]?|localhost|/home/|/var/|/proc/|/etc/|/Users/|[A-Z]:\\|v9\.9\.9|trycloudflare|secret-path|example\.com|8443`)

func TestPublicRedaction(t *testing.T) {
	cs, d, sh := fixture(t, time.Now())
	_ = sh.Start(context.Background()) // share state carries a LAN error string
	h := Public(d, testSecret)
	base := "/s/" + testSecret + "/"
	paths := []string{"", "api/status", "api/profile", "api/panel?group=common", "api/panel?group=lan&from=now-90d",
		"api/panel.csv?group=lan", "static/app.js", "static/style.css", "nope", "api/panel?group=bad", "api/panel?from=x"}
	check := func(p string) {
		w := do(h, "GET", base+p, map[string]string{"Cf-Connecting-Ip": "198.51.100.7"})
		body := w.Body.String()
		for k, v := range w.Header() {
			body += "\n" + k + ": " + strings.Join(v, ",")
		}
		if m := leakRE.FindString(body); m != "" {
			t.Errorf("public %q leaks %q", p, m)
		}
	}
	for _, p := range paths {
		check(p)
	}
	// Store errors (which may contain paths) are not passed through.
	cs.err = errors.New("sqlite: open /home/venkat/.local/state/fyisp/fyisp.db: 192.168.1.20")
	check("api/panel?group=lan&points=77")
	check("api/panel.csv?group=lan&points=77")

	var st map[string]any
	w := do(h, "GET", base+"api/status", map[string]string{"Cf-Connecting-Ip": "198.51.100.8"})
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if _, ok := st["version"]; ok {
		t.Error("public status exposes version")
	}
	if st["mode"] != "public" {
		t.Errorf("mode = %v", st["mode"])
	}
}

func TestPublicRateLimit(t *testing.T) {
	_, d, _ := fixture(t, time.Now())
	h := Public(d, testSecret).(*publicHandler)
	now := time.Now()
	h.now = func() time.Time { return now } // frozen clock: no refill
	url := "/s/" + testSecret + "/api/status"

	// Per client: burst of PublicClientBurst, then 429.
	got429 := 0
	for range PublicClientBurst + 5 {
		if w := do(h, "GET", url, map[string]string{"Cf-Connecting-Ip": "198.51.100.1"}); w.Code == 429 {
			got429++
			if w.Header().Get("Retry-After") == "" {
				t.Error("429 without Retry-After")
			}
		}
	}
	if got429 != 5 {
		t.Errorf("per-client: %d x 429, want 5", got429)
	}
	// Global: many clients drain the global burst of 40.
	h2 := Public(d, testSecret).(*publicHandler)
	h2.now = h.now
	ok, limited := 0, 0
	for i := range 100 {
		w := do(h2, "GET", url, map[string]string{"Cf-Connecting-Ip": fmt.Sprintf("203.0.113.%d", i)})
		switch w.Code {
		case 200:
			ok++
		case 429:
			limited++
		}
	}
	if ok != PublicBurst || limited != 100-PublicBurst {
		t.Errorf("global: %d ok, %d limited; want %d/%d", ok, limited, PublicBurst, 100-PublicBurst)
	}
	// Refill: after one second, 5 more for a client.
	now = now.Add(time.Second)
	if w := do(h, "GET", url, map[string]string{"Cf-Connecting-Ip": "198.51.100.1"}); w.Code != 200 {
		t.Errorf("after refill: %d", w.Code)
	}
}

func TestPublicCacheAndConcurrency(t *testing.T) {
	cs, d, _ := fixture(t, time.Now())
	h := Public(d, testSecret)
	url := "/s/" + testSecret + "/api/panel?group=common&from=now-30m&to=now&points=500"
	for i := range 3 {
		if w := do(h, "GET", url, map[string]string{"Cf-Connecting-Ip": fmt.Sprintf("203.0.113.%d", i)}); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if n := cs.calls.Load(); n != 1 {
		t.Errorf("3 identical queries made %d store calls, want 1 (cached)", n)
	}
	// Concurrent distinct queries never exceed PublicMaxQueries in flight.
	cs.delay = 20 * time.Millisecond
	var inflight, peak atomic.Int32
	d.Store = &peakStore{Reader: cs, inflight: &inflight, peak: &peak}
	h = Public(d, testSecret)
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Go(func() {
			u := fmt.Sprintf("/s/%s/api/panel?group=common&points=%d", testSecret, 100+i)
			do(h, "GET", u, map[string]string{"Cf-Connecting-Ip": fmt.Sprintf("203.0.113.%d", i)})
		})
	}
	wg.Wait()
	if p := peak.Load(); p > PublicMaxQueries || p == 0 {
		t.Errorf("peak concurrent queries = %d, want 1..%d", p, PublicMaxQueries)
	}
}

type peakStore struct {
	store.Reader
	inflight, peak *atomic.Int32
}

func (p *peakStore) Panel(ctx context.Context, q store.PanelQuery) (*store.PanelResult, error) {
	n := p.inflight.Add(1)
	defer p.inflight.Add(-1)
	for {
		old := p.peak.Load()
		if n <= old || p.peak.CompareAndSwap(old, n) {
			break
		}
	}
	return p.Reader.Panel(ctx, q)
}

func TestPublicSecretValidation(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("short secret accepted")
		}
	}()
	_, d, _ := fixture(t, time.Now())
	Public(d, "short")
}

// ---------- local ----------

func localHandlerFor(t *testing.T, o LocalOptions) (http.Handler, *fakeShare) {
	_, d, sh := fixture(t, time.Now())
	if o.Addr == "" {
		o.Addr = "127.0.0.1:3000"
	}
	o.CSRFToken = "tok123"
	return Local(d, o), sh
}

func TestLocalHost(t *testing.T) {
	h, _ := localHandlerFor(t, LocalOptions{ExtraHosts: []string{"fyisp.lan:3000"}})
	for host, code := range map[string]int{
		"127.0.0.1:3000": 200, "localhost:3000": 200, "LOCALHOST:3000": 200, "[::1]:3000": 200, "fyisp.lan:3000": 200,
		"evil.com": 421, "evil.com:3000": 421, "127.0.0.1:3001": 421, "localhost.evil.com:3000": 421,
		"192.168.1.5:3000": 421, "": 421, "fyisp.lan:3001": 421,
	} {
		if w := do(h, "GET", "/api/status", map[string]string{"Host": host}); w.Code != code {
			t.Errorf("Host %q = %d, want %d", host, w.Code, code)
		}
	}
	// 0.0.0.0: IP literals allowed (no DNS rebinding), hostnames still refused.
	hb, _ := localHandlerFor(t, LocalOptions{Addr: "0.0.0.0:3000"})
	for host, code := range map[string]int{"192.168.1.5:3000": 200, "localhost:8080": 200, "evil.com:3000": 421, "rebind.evil.com": 421} {
		if w := do(hb, "GET", "/api/status", map[string]string{"Host": host}); w.Code != code {
			t.Errorf("0.0.0.0 Host %q = %d, want %d", host, w.Code, code)
		}
	}
}

func TestLocalRoutes(t *testing.T) {
	h, _ := localHandlerFor(t, LocalOptions{})
	host := map[string]string{"Host": "127.0.0.1:3000"}
	for p, code := range map[string]int{
		"/": 200, "/metrics": 200, "/api/status": 200, "/api/profile": 200, "/api/panel?group=lan": 200,
		"/api/panel.csv?group=lan": 200, "/static/app.js": 200, "/debug/pprof/": 404, "/nope": 404,
		"/static/index.html": 404, "/api/share/start": 405,
	} {
		if w := do(h, "GET", p, host); w.Code != code {
			t.Errorf("GET %s = %d, want %d", p, w.Code, code)
		}
	}
	w := do(h, "GET", "/", host)
	if !strings.Contains(w.Body.String(), `<meta name="fyisp-token" content="tok123">`) ||
		!strings.Contains(w.Body.String(), `<title>F*** you ISP!</title>`) {
		t.Error("index lacks token meta or title")
	}
	if strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), "style=") {
		t.Error("index has inline script or style")
	}
	var st localStatus
	_ = json.Unmarshal(do(h, "GET", "/api/status", host).Body.Bytes(), &st)
	if st.Mode != "local" || st.Controls != "enabled" || st.Version == "" || st.Store.FileBytes != 1234 {
		t.Errorf("local status = %+v", st)
	}
}

func TestLocalPOST(t *testing.T) {
	h, sh := localHandlerFor(t, LocalOptions{})
	good := map[string]string{"Host": "127.0.0.1:3000", "Origin": "http://127.0.0.1:3000", "Sec-Fetch-Site": "same-origin", HeaderCSRF: "tok123"}
	with := func(over map[string]string, drop ...string) map[string]string {
		m := map[string]string{}
		for k, v := range good {
			m[k] = v
		}
		for k, v := range over {
			m[k] = v
		}
		for _, k := range drop {
			delete(m, k)
		}
		return m
	}
	cases := []struct {
		name string
		hdr  map[string]string
		code int
	}{
		{"cross-origin Origin", with(map[string]string{"Origin": "http://evil.com"}, "Sec-Fetch-Site"), 403},
		{"cross-site fetch", with(map[string]string{"Sec-Fetch-Site": "cross-site"}), 403},
		{"same-site fetch", with(map[string]string{"Sec-Fetch-Site": "same-site"}), 403},
		{"no origin info", with(nil, "Origin", "Sec-Fetch-Site"), 403},
		{"missing token", with(nil, HeaderCSRF), 403},
		{"bad token", with(map[string]string{HeaderCSRF: "tok124"}), 403},
		{"foreign host", with(map[string]string{"Host": "evil.com", "Origin": "http://evil.com"}), 421},
		{"ok via Origin only", with(nil, "Sec-Fetch-Site"), 200},
		{"ok via Sec-Fetch-Site only", with(nil, "Origin"), 200},
	}
	for _, c := range cases {
		if w := do(h, "POST", "/api/share/start", c.hdr); w.Code != c.code {
			t.Errorf("%s: %d, want %d (%s)", c.name, w.Code, c.code, w.Body.String())
		}
	}
	if sh.start != 2 {
		t.Errorf("share started %d times, want 2", sh.start)
	}
	w := do(h, "POST", "/api/share/stop", good)
	if w.Code != 200 || sh.State().Phase != ShareOff {
		t.Errorf("stop: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", "/api/status", good); w.Code != 405 {
		t.Errorf("POST /api/status = %d", w.Code)
	}

	// Listening on all interfaces without an admin token: POST disabled.
	hb, shb := localHandlerFor(t, LocalOptions{Addr: "0.0.0.0:3000"})
	if w := do(hb, "POST", "/api/share/start", good); w.Code != 403 || shb.start != 0 {
		t.Errorf("PublicBind POST = %d (starts %d), want 403", w.Code, shb.start)
	}
	var st localStatus
	_ = json.Unmarshal(do(hb, "GET", "/api/status", good).Body.Bytes(), &st)
	if st.Controls != "disabled" {
		t.Errorf("controls = %q", st.Controls)
	}
	// With an admin token, the header is required.
	ha, sha := localHandlerFor(t, LocalOptions{PublicBind: true, AdminToken: "adm"})
	if w := do(ha, "POST", "/api/share/start", good); w.Code != 403 {
		t.Errorf("admin missing = %d", w.Code)
	}
	if w := do(ha, "POST", "/api/share/start", with(map[string]string{HeaderAdmin: "adm"})); w.Code != 200 || sha.start != 1 {
		t.Errorf("admin ok = %d", w.Code)
	}
}

// ---------- API ----------

type panelResp struct {
	Group  struct{ ID, Title string } `json:"group"`
	Tier   string                     `json:"tier"`
	From   int64                      `json:"from"`
	To     int64                      `json:"to"`
	Now    int64                      `json:"now"`
	Start  int64                      `json:"start"`
	Step   int64                      `json:"step"`
	Len    int                        `json:"len"`
	Series []struct {
		Target   string              `json:"target"`
		Kind     string              `json:"kind"`
		Interval int64               `json:"interval"`
		Mean     []*float64          `json:"mean"`
		Min      []*float64          `json:"min"`
		Max      []*float64          `json:"max"`
		N        []uint32            `json:"n"`
		Lost     []uint32            `json:"lost"`
		Gap      []uint32            `json:"gap"`
		LostBy   map[string][]uint32 `json:"lost_by"`
	} `json:"series"`
}

func TestPanelJSON(t *testing.T) {
	now := time.Now()
	cs, d, _ := fixture(t, now)
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000"})
	host := map[string]string{"Host": "127.0.0.1:3000"}
	w := do(h, "GET", "/api/panel?group=common&from=now-40m&to=now&points=1000", host)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if cs.calls.Load() != 1 {
		t.Errorf("store calls = %d, want 1", cs.calls.Load())
	}
	q := cs.last
	if len(q.Keys) != 4 { // Alpha x3 + Beta x1
		t.Errorf("keys = %v", q.Keys)
	}
	// 40m at a 15s probe interval: at most 160 buckets, never finer than the interval.
	if q.MaxPoints > 160 {
		t.Errorf("MaxPoints = %d, want <= 160", q.MaxPoints)
	}
	var r panelResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Group.ID != "common" || r.Tier != "raw" || r.Step != 15000 || r.Len == 0 || len(r.Series) != 4 {
		t.Fatalf("bad header: %+v", r)
	}
	if r.Series[0].Target != "Alpha" || r.Series[0].Kind != "https" || r.Series[3].Target != "Beta" {
		t.Errorf("series order: %s/%s ... %s", r.Series[0].Target, r.Series[0].Kind, r.Series[3].Target)
	}
	a := r.Series[0]
	var nulls, timeouts, gaps, vals int
	for i := range r.Len {
		if len(a.Mean) != r.Len || len(a.N) != r.Len || len(a.Gap) != r.Len {
			t.Fatal("column length mismatch")
		}
		if a.Mean[i] == nil {
			nulls++
			if a.Min[i] != nil || a.Max[i] != nil {
				t.Error("min/max not null with mean null")
			}
		} else {
			vals++
			if *a.Mean[i] != 20 {
				t.Errorf("mean = %v", *a.Mean[i])
			}
		}
		timeouts += int(a.LostBy["timeout"][i])
		gaps += int(a.Gap[i])
	}
	if timeouts != 8 || a.Lost == nil {
		t.Errorf("timeouts = %d, want 8 (2 min at 15s)", timeouts)
	}
	if gaps < 18 || gaps > 20 {
		t.Errorf("not measured = %d, want ~20 (5 min at 15s)", gaps)
	}
	if nulls < 28 || vals < 100 {
		t.Errorf("nulls=%d vals=%d", nulls, vals)
	}
	if strings.Contains(w.Body.String(), "NaN") {
		t.Error("NaN in JSON")
	}
	if _, ok := r.Series[1].LostBy["timeout"]; ok {
		t.Error("TCP series has timeouts")
	}
}

func TestPanelGapStartsAtOldestData(t *testing.T) {
	now := time.Now()
	cs, d, _ := fixture(t, now)
	cs.oldest = now.Add(-32 * time.Minute) // "installed" 32 minutes ago
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000"})
	w := do(h, "GET", "/api/panel?group=common&from=now-40m", map[string]string{"Host": "127.0.0.1:3000"})
	var r panelResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	gaps := 0
	for _, g := range r.Series[0].Gap {
		gaps += int(g)
	}
	if gaps < 6 || gaps > 8 { // 32m..30m ago at 15s
		t.Errorf("not measured = %d, want ~8", gaps)
	}
}

func TestPanelClamp(t *testing.T) {
	now := time.Now()
	cs, d, _ := fixture(t, now)
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000"})
	host := map[string]string{"Host": "127.0.0.1:3000"}
	w := do(h, "GET", "/api/panel?group=lan&from=now-400d&points=99999", host)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := cs.last.To.Sub(cs.last.From); got != MaxRange {
		t.Errorf("range = %v, want 90d", got)
	}
	if cs.last.MaxPoints > MaxPoints {
		t.Errorf("points = %d", cs.last.MaxPoints)
	}
	from := now.Add(-2 * time.Hour).UnixMilli()
	w = do(h, "GET", fmt.Sprintf("/api/panel?group=lan&from=%d&to=%d", from, now.Add(24*time.Hour).UnixMilli()), host)
	if w.Code != 200 || cs.last.To.After(time.Now()) || cs.last.From.UnixMilli() != from {
		t.Errorf("absolute: %d to=%v", w.Code, cs.last.To)
	}
	for _, bad := range []string{"group=lan&from=now-", "group=lan&from=yesterday", "group=lan&from=now&to=now-1h", "group=lan&points=x", "from=now-1h", "group=lan&from=now-5y"} {
		if w := do(h, "GET", "/api/panel?"+bad, host); w.Code != 400 {
			t.Errorf("%s = %d, want 400", bad, w.Code)
		}
	}
	// Long ranges use the hourly tier.
	var r panelResp
	w = do(h, "GET", "/api/panel?group=lan&from=now-7d", host)
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	if r.Tier != "1h" || r.Step < 3600000 {
		t.Errorf("7d: tier %s step %d", r.Tier, r.Step)
	}
}

func TestPanelCSV(t *testing.T) {
	_, d, _ := fixture(t, time.Now())
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000"})
	w := do(h, "GET", "/api/panel.csv?group=common&from=now-40m", map[string]string{"Host": "127.0.0.1:3000"})
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") ||
		!strings.Contains(w.Header().Get("Content-Disposition"), `filename="fyisp-common-`) {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	rows, err := csv.NewReader(w.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := "bucket_start,target,kind,mean_ms,min_ms,max_ms,samples,lost,not_measured,lost_timeout,lost_refused,lost_reset,lost_unreachable,lost_dns,lost_tls,lost_http,lost_no_network,lost_other"
	if strings.Join(rows[0], ",") != want {
		t.Errorf("header = %v", rows[0])
	}
	var timeouts, empty int
	for _, r := range rows[1:] {
		if r[1] == "Alpha" && r[2] == "https" && r[9] != "0" {
			timeouts++
			if r[3] != "" {
				t.Errorf("lost bucket has mean %q", r[3])
			}
		}
		if r[3] == "" {
			empty++
		}
		if _, err := time.Parse(time.RFC3339, r[0]); err != nil {
			t.Error(err)
		}
	}
	if timeouts == 0 || empty == 0 || (len(rows)-1)%4 != 0 {
		t.Errorf("timeouts=%d empty=%d rows=%d", timeouts, empty, len(rows))
	}
}

func TestProfileNoHosts(t *testing.T) {
	_, d, _ := fixture(t, time.Now())
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000"})
	b := do(h, "GET", "/api/profile", map[string]string{"Host": "127.0.0.1:3000"}).Body.String()
	for _, s := range []string{"example.com", "192.168", "10.1.2.3", "172.16", "secret-path", "8443"} {
		if strings.Contains(b, s) {
			t.Errorf("profile leaks %q: %s", s, b)
		}
	}
	var p profileJSON
	if err := json.Unmarshal([]byte(b), &p); err != nil || len(p.Groups) != 2 || len(p.Targets) != 4 || len(p.Targets[1].Kinds) != 1 {
		t.Errorf("%v %+v", err, p)
	}
}

func TestGzipAndETag(t *testing.T) {
	_, d, _ := fixture(t, time.Now())
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000"})
	w := do(h, "GET", "/static/app.js", map[string]string{"Host": "127.0.0.1:3000", "Accept-Encoding": "gzip"})
	if w.Header().Get("Content-Encoding") != "gzip" || w.Header().Get("ETag") == "" {
		t.Fatalf("%v", w.Header())
	}
	w2 := do(h, "GET", "/static/app.js", map[string]string{"Host": "127.0.0.1:3000", "If-None-Match": w.Header().Get("ETag")})
	if w2.Code != 304 {
		t.Errorf("If-None-Match = %d", w2.Code)
	}
}

func TestWarmup(t *testing.T) {
	var w Warmup
	w.Observe(model.Sample{Key: model.SeriesKey{Target: "a", Kind: model.KindTCP}})
	w.Observe(model.Sample{Key: model.SeriesKey{Target: "a", Kind: model.KindICMP}})
	w.Observe(model.Sample{Key: model.SeriesKey{Target: "b"}, Lost: true, Reason: model.ReasonTimeout})
	w.Observe(model.Sample{Key: model.SeriesKey{Target: "c"}, Lost: true, Reason: model.ReasonGap})
	if w.Ready() != 2 {
		t.Errorf("ready = %d", w.Ready())
	}
}
