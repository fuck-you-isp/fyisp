package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/probe"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

var allKinds = []model.ProbeKind{model.KindHTTPS, model.KindTCP, model.KindICMP}

func testProfile() *model.Profile {
	return &model.Profile{
		Name: "test",
		Groups: []model.Group{
			{ID: "common", Title: "Common Services", Order: 1},
			{ID: "lan", Title: "LAN", Order: 2},
		},
		// Targets of a --config file that asks for every kind (shipped
		// profiles use TCP only; see TestProfileTCPOnly).
		Targets: []model.Target{
			{Name: "Alpha", Host: "alpha.example.com", Group: "common", Path: "/secret-path", Kinds: allKinds},
			{Name: "Beta", Host: "beta.example.com", Group: "common", Kinds: []model.ProbeKind{model.KindHTTPS}},
			{Name: "Router", Host: "192.168.1.1", Group: "lan", Port: 8443, Kinds: allKinds},
			{Name: "NAS", Host: "10.1.2.3", Group: "lan", HostOverrides: map[model.ProbeKind]string{model.KindICMP: "172.16.0.9"}, Kinds: allKinds},
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
	return store.Stats{FileBytes: 1234, Oldest: c.oldest,
		LastFlushErr: "open /var/lib/fyisp/fyisp.db: disk full on 192.168.1.20"}, nil
}

// fakeControl is the speed tests (a stopped speedRig's Scheduler) with a
// counter: doControl's POST /api/speed/config {"skip": []} counts the
// writes that got through controlAllowed.
type fakeControl struct {
	SpeedSource
	mu sync.Mutex
	n  int
}

func (f *fakeControl) SetSkip(context.Context, []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return nil
}

// doControl uses a control (the speed tests' skipped servers) with hdr.
func doControl(h http.Handler, hdr map[string]string) *httptest.ResponseRecorder {
	return doBody(h, "POST", "/api/speed/config", `{"skip": []}`, hdr)
}

// fixture fills a fake store: 15s HTTPS/TCP and 5s ICMP samples for the last
// 40 minutes, with a timeout outage for Alpha HTTPS (20-22 min ago) and a
// not-measured hole for everyone (30-35 min ago).
func fixture(t *testing.T, now time.Time) (*countingStore, Deps) {
	t.Helper()
	f := store.NewFake()
	p := testProfile()
	for _, tg := range p.Targets {
		for _, k := range tg.ProbeKinds() {
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
	d := Deps{
		Profile: func() *model.Profile { return p },
		Store:   cs,
		Status: func() Status {
			return Status{
				Version: "v9.9.9-build-/home/venkat/src", Started: now.Add(-time.Hour),
				Caps:    probe.Caps{ICMP: "unavailable", ICMPHint: "set /proc/sys/net/ipv4/ping_group_range"},
				Targets: 4, Ready: 3,
			}
		},
		Metrics: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ping_rtt_seconds 1\n") }),
		Verdict: cleanVerdict(now),
	}
	return cs, d
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

// portLeak matches the planted port 8443 as a number of its own (":8443",
// "port":8443, "8443"), not the same digits inside a millisecond timestamp
// or a decimal RTT.
const portLeak = `(^|[^\d.])8443($|[^\d.])`

var portRE = regexp.MustCompile(portLeak)

// leakRE matches private details planted in the fixtures.
var leakRE = regexp.MustCompile(`\b(10\.\d+\.\d+\.\d+|192\.168\.\d+\.\d+|172\.(1[6-9]|2\d|3[01])\.\d+\.\d+|127\.\d+\.\d+\.\d+|169\.254\.\d+\.\d+)\b|\[?::1\]?|localhost|/home/|/var/|/proc/|/etc/|/Users/|[A-Z]:\\|v9\.9\.9|secret-path|example\.com|` + portLeak)

// ---------- local ----------

func localHandlerFor(t *testing.T, o LocalOptions) (http.Handler, *fakeControl) {
	_, d := fixture(t, time.Now())
	fc := &fakeControl{SpeedSource: newSpeedRigWith(t, 24*time.Hour, rigOpts{noRun: true}).s}
	d.Speed = fc
	if o.Addr == "" {
		o.Addr = "127.0.0.1:3000"
	}
	o.CSRFToken = "tok123"
	return Local(d, o), fc
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
		"/static/index.html": 404, "/api/verdict": 200, "/api/incidents": 200,
		"/api/verdict?from=now": 400, "/api/incidents?from=now&to=now-1h": 400,
	} {
		if w := do(h, "GET", p, host); w.Code != code {
			t.Errorf("GET %s = %d, want %d", p, w.Code, code)
		}
	}
	w := do(h, "GET", "/", host)
	if !strings.Contains(w.Body.String(), `<meta name="fyisp-token" content="tok123">`) ||
		!strings.Contains(w.Body.String(), `<title>fyisp</title>`) {
		t.Error("index lacks token meta or title")
	}
	if strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), "style=") {
		t.Error("index has inline script or style")
	}
	var st Status
	_ = json.Unmarshal(do(h, "GET", "/api/status", host).Body.Bytes(), &st)
	if st.Version == "" || st.Store.FileBytes != 1234 {
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
		if w := doControl(h, c.hdr); w.Code != c.code {
			t.Errorf("%s: %d, want %d (%s)", c.name, w.Code, c.code, w.Body.String())
		}
	}
	if sh.n != 2 {
		t.Errorf("control used %d times, want 2", sh.n)
	}
	if w := do(h, "POST", "/api/status", good); w.Code != 405 {
		t.Errorf("POST /api/status = %d", w.Code)
	}

	// Every speed control goes through the same checks.
	for _, p := range []string{"/api/speed/run?kind=sweep", "/api/speed/cancel?id=x"} {
		if w := do(h, "POST", p, with(nil, HeaderCSRF)); w.Code != 403 {
			t.Errorf("POST %s without CSRF = %d, want 403", p, w.Code)
		}
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
		Target string              `json:"target"`
		Kind   string              `json:"kind"`
		Mean   []*float64          `json:"mean"`
		Min    []*float64          `json:"min"`
		Max    []*float64          `json:"max"`
		N      []uint32            `json:"n"`
		Lost   []uint32            `json:"lost"`
		Gap    []uint32            `json:"gap"`
		LostBy map[string][]uint32 `json:"lost_by"`
	} `json:"series"`
}

func TestPanelJSON(t *testing.T) {
	now := time.Now()
	cs, d := fixture(t, now)
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

func TestPanelClamp(t *testing.T) {
	now := time.Now()
	cs, d := fixture(t, now)
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
	_, d := fixture(t, time.Now())
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
	_, d := fixture(t, time.Now())
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000"})
	b := do(h, "GET", "/api/profile", map[string]string{"Host": "127.0.0.1:3000"}).Body.String()
	for _, s := range []string{"example.com", "192.168", "10.1.2.3", "172.16", "secret-path"} {
		if strings.Contains(b, s) {
			t.Errorf("profile leaks %q: %s", s, b)
		}
	}
	if m := portRE.FindString(b); m != "" {
		t.Errorf("profile leaks %q: %s", m, b)
	}
	var p profileJSON
	if err := json.Unmarshal([]byte(b), &p); err != nil || len(p.Groups) != 2 || len(p.Targets) != 4 || len(p.Targets[1].Kinds) != 1 {
		t.Errorf("%v %+v", err, p)
	}
}

// TestProfileTCPOnly: with the default profile, /api/profile lists TCP as
// the only kind of every service target but the STUN targets (UDP, with
// their tags), and the network path keeps ICMP.
func TestProfileTCPOnly(t *testing.T) {
	def, err := profile.Default()
	if err != nil {
		t.Fatal(err)
	}
	_, d := fixture(t, time.Now())
	d.Profile = func() *model.Profile { return def }
	b := do(Local(d, LocalOptions{Addr: "127.0.0.1:3000"}), "GET", "/api/profile", map[string]string{"Host": "127.0.0.1:3000"}).Body.String()
	var p profileJSON
	if err := json.Unmarshal([]byte(b), &p); err != nil {
		t.Fatal(err)
	}
	ivs := map[string]int64{}
	for _, tg := range def.Targets {
		ivs[tg.Name] = intervalOf(tg, model.KindTCP).Milliseconds()
	}
	var path, services, udp int
	for _, tg := range p.Targets {
		iv := ivs[tg.Name]
		switch {
		case slices.Equal(tg.Kinds, []string{"udp"}):
			udp++
			if !slices.Contains(tg.Tags, "udp-path") || (iv != 5000 && iv != 15000) {
				t.Errorf("%s: tags %v interval %d", tg.Name, tg.Tags, iv)
			}
		case tg.Layer == "":
			services++
			if !slices.Equal(tg.Kinds, []string{"tcp"}) || iv != 15000 {
				t.Errorf("%s: kinds %v interval %d, want [tcp] every 15s", tg.Name, tg.Kinds, iv)
			}
		case !slices.Contains(tg.Kinds, "icmp"):
			t.Errorf("path target %s without ICMP: %v", tg.Name, tg.Kinds)
		default:
			path++
		}
	}
	if services != 23 || path != 4 || udp != 5 {
		t.Errorf("%d services, %d path targets, %d UDP targets", services, path, udp)
	}
	if !strings.Contains(b, `"name":"Microsoft-Teams","group":"call-apps","kinds":["tcp"],"provider":"services-communication","provider_title":"Communication Services","provider_kind":"service","geo":"global","tags":["voice","video","signaling","app:teams","tile:teams"]`) {
		t.Errorf("tags missing from /api/profile: %s", b)
	}
}

func TestGzipAndETag(t *testing.T) {
	_, d := fixture(t, time.Now())
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
