package web

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// fakeTrace is a TraceSource over a fixed route with a route change.
type fakeTrace struct {
	mu       sync.Mutex
	now      time.Time
	started  []string
	sessions []*fakeInv
	err      error
}

// fakeInv is an on-demand session whose rounds the test advances.
type fakeInv struct {
	mu      sync.Mutex
	target  string
	max     int
	rounds  int
	ended   time.Time
	stopped bool
}

func (i *fakeInv) Stop() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.stopped = true
	if i.ended.IsZero() {
		i.ended = time.Now()
	}
}

func (i *fakeInv) Progress() (int, time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.rounds, i.ended
}

// advance runs n rounds, ending the session at its max, as the tracer does.
func (i *fakeInv) advance(n int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for ; n > 0 && i.ended.IsZero(); n-- {
		i.rounds++
		if i.rounds >= i.max {
			i.ended = time.Now()
		}
	}
}

var (
	ip = netip.MustParseAddr
	// Alpha's route: gateway, CGNAT, ISP private, ISP edge, ISP core, no
	// reply, IX, cloud, destination.
	routeA = []netip.Addr{ip("192.168.1.1"), ip("100.72.0.1"), ip("10.20.0.1"), ip("203.0.113.9"), ip("198.51.100.20"),
		{}, ip("192.0.2.30"), ip("45.0.0.5"), ip("142.250.1.1")}
	// The old route went through a different edge address and transit.
	routeOld = []netip.Addr{ip("192.168.1.1"), ip("100.72.0.1"), ip("10.20.0.1"), ip("203.0.113.77"), ip("198.51.100.20"),
		ip("4.69.0.1"), ip("4.69.0.2"), ip("45.0.0.5"), ip("142.250.1.1")}
	hopInfo = map[netip.Addr]model.HopInfo{
		ip("192.168.1.1"):   {RDNS: "router.home.lan"},
		ip("10.20.0.1"):     {RDNS: "cmts1.springfield.isp.test"},
		ip("203.0.113.9"):   {RDNS: "edge1.springfield.isp.test", ASN: 64500, Owner: "ISP-NET"},
		ip("203.0.113.77"):  {RDNS: "edge2.springfield.isp.test", ASN: 64500, Owner: "ISP-NET"},
		ip("198.51.100.20"): {RDNS: "core2.springfield.isp.test", ASN: 64500, Owner: "ISP-NET"},
		ip("192.0.2.30"):    {RDNS: "ix-peer.shelbyville.test", ASN: 64501, Owner: "IX-NET"},
		ip("45.0.0.5"):      {RDNS: "hop8.cloud.test", ASN: 15169, Owner: "GOOGLE"},
		ip("142.250.1.1"):   {RDNS: "dest.cloud.test", ASN: 15169, Owner: "GOOGLE"},
		ip("4.69.0.1"):      {RDNS: "ae1.transit.test", ASN: 3356, Owner: "LEVEL3"},
		ip("4.69.0.2"):      {RDNS: "ae2.transit.test", ASN: 3356, Owner: "LEVEL3"},
	}
)

func (f *fakeTrace) Hops(ctx context.Context, target string, from, to time.Time) ([]HopStat, error) {
	if f.err != nil {
		return nil, f.err
	}
	if target != "Alpha" {
		return nil, nil
	}
	var out []HopStat
	for i, a := range routeA {
		h := HopStat{Hop: i + 1, IP: a, N: 100, Lost: 0, Min: float64(i) + 1, Mean: float64(i) + 2, Max: float64(i) + 5, P95: float64(i) + 4, Jitter: 0.5}
		if a.IsValid() {
			h.Info = hopInfo[a]
			h.Info.IP = a
		} else {
			h.N, h.Lost, h.Loss = 0, 100, 1
		}
		if i == 4 {
			h.Lost, h.Loss = 30, 0.3 // rate limiting
		}
		out = append(out, h)
	}
	// ECMP at hop 3: a link-local address.
	out = append(out, HopStat{Hop: 3, IP: ip("169.254.10.1"), N: 3, Info: model.HopInfo{RDNS: "ll.home.lan"}})
	// The old route's hops.
	for i, a := range routeOld {
		if a == routeA[i] {
			continue
		}
		h := HopStat{Hop: i + 1, IP: a, N: 20, Mean: 9, Min: 8, Max: 10, P95: 10, Loss: 0.1, Lost: 2, LossContinues: true}
		h.Info = hopInfo[a]
		out = append(out, h)
	}
	out = append(out, HopStat{Hop: 10, IP: ip("142.250.1.1"), N: 1, Mean: math.NaN(), Loss: math.Inf(1)}) // served as null / clamped
	return out, nil
}

func (f *fakeTrace) Route(ctx context.Context, target string) (model.Route, bool) {
	if target != "Alpha" {
		return model.Route{}, false
	}
	return model.Route{Target: target, Hops: routeA, Since: f.now.Add(-10 * time.Minute)}, true
}

func (f *fakeTrace) RouteChanges(ctx context.Context, from, to time.Time) ([]model.RouteChange, error) {
	return []model.RouteChange{
		{ID: 1, Target: "Alpha", At: f.now.Add(-10 * time.Minute), From: routeOld, To: routeA, FirstDiff: 4},
		{ID: 2, Target: "Secret-Target", At: f.now.Add(-5 * time.Minute), From: routeA, To: routeOld, FirstDiff: 4},
	}, nil
}

func (f *fakeTrace) Investigate(target string, rounds int) (Investigation, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, target)
	inv := &fakeInv{target: target, max: rounds}
	f.sessions = append(f.sessions, inv)
	return inv, nil
}

func (f *fakeTrace) Traced() []string { return []string{"Alpha"} }

func traceFixture(t *testing.T) (Deps, *fakeTrace, *countingStore) {
	now := time.Now()
	cs, d := fixture(t, now)
	ft := &fakeTrace{now: now}
	d.Trace = ft
	f := cs.Reader.(interface{ Observe(model.Sample) })
	for hop := 1; hop <= len(routeA); hop++ {
		for ts := now.Add(-20 * time.Minute).Truncate(10 * time.Second); ts.Before(now); ts = ts.Add(10 * time.Second) {
			s := model.Sample{Key: model.SeriesKey{Target: "Alpha", Kind: model.KindTrace, Hop: uint8(hop)}, Slot: ts, RTT: time.Duration(hop) * time.Millisecond}
			if hop == 6 {
				s.Lost, s.Reason, s.RTT = true, model.ReasonTimeout, 0
			}
			f.Observe(s)
		}
	}
	return d, ft, cs
}

func TestTraceNil(t *testing.T) {
	_, d := fixture(t, time.Now())
	lh := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"})
	host := map[string]string{"Host": "127.0.0.1:3000"}
	for _, p := range []string{"api/trace?target=Alpha", "api/trace/panel?target=Alpha", "api/routes/changes"} {
		if w := do(lh, "GET", "/"+p, host); w.Code != 404 {
			t.Errorf("local nil %s = %d", p, w.Code)
		}
	}
	good := map[string]string{"Host": "127.0.0.1:3000", "Origin": "http://127.0.0.1:3000", HeaderCSRF: "tok123"}
	if w := do(lh, "POST", "/api/investigate?target=Alpha", good); w.Code != 404 {
		t.Errorf("investigate nil = %d", w.Code)
	}
	var prof struct {
		Features struct{ Trace bool } `json:"features"`
	}
	_ = json.Unmarshal(do(lh, "GET", "/api/profile", host).Body.Bytes(), &prof)
	if prof.Features.Trace {
		t.Error("features.trace with nil source")
	}
}

type traceResp struct {
	Target  string `json:"target"`
	Traced  bool   `json:"traced"`
	From    int64  `json:"from"`
	To      int64  `json:"to"`
	ISPASN  uint32 `json:"isp_asn"`
	Route   *struct{ Hops []hopJSON }
	Hops    []map[string]any `json:"hops"`
	Changes []struct {
		ID        int64     `json:"id"`
		FirstDiff int       `json:"first_diff"`
		From      []hopJSON `json:"from"`
		To        []hopJSON `json:"to"`
	} `json:"changes"`
	Investigation *investigationJSON `json:"investigation"`
}

func TestTraceShapeAndClamps(t *testing.T) {
	d, _, cs := traceFixture(t)
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"})
	host := map[string]string{"Host": "127.0.0.1:3000"}
	w := do(h, "GET", "/api/trace?target=Alpha&from=now-200d", host)
	if w.Code != 200 {
		t.Fatalf("trace = %d %s", w.Code, w.Body.String())
	}
	var tr traceResp
	if err := json.Unmarshal(w.Body.Bytes(), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Target != "Alpha" || !tr.Traced || tr.ISPASN != 64500 || tr.Route == nil || len(tr.Route.Hops) != len(routeA) {
		t.Errorf("trace = %+v", tr)
	}
	if span := time.Duration(tr.To-tr.From) * time.Millisecond; span > MaxRange {
		t.Errorf("span %v > MaxRange", span)
	}
	if len(tr.Changes) != 1 || tr.Changes[0].FirstDiff != 4 {
		t.Errorf("changes = %+v (only Alpha's)", tr.Changes)
	}
	// Sorted by hop; local shows everything.
	last := 0
	for _, hp := range tr.Hops {
		n := int(hp["hop"].(float64))
		if n < last {
			t.Errorf("hops not sorted: %v", tr.Hops)
		}
		last = n
	}
	body := w.Body.String()
	for _, want := range []string{`"192.168.1.1"`, `"203.0.113.9"`, "edge1.springfield.isp.test", `"loss_continues":true`, `"no_reply":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("local trace lacks %s", want)
		}
	}
	for p, code := range map[string]int{
		"/api/trace":                                 400,
		"/api/trace?target=Nope":                     404,
		"/api/trace?target=Alpha&x=1":                400,
		"/api/trace?target=Alpha&from=now&to=now-1h": 400,
		"/api/trace?target=Alpha&target=Beta":        400,
		"/api/trace/panel?target=Alpha&hops=0":       400,
		"/api/trace/panel?target=Alpha&hops=41":      400,
		"/api/trace/panel?target=Alpha&hops=a":       400,
		"/api/trace/panel?target=Nope&hops=1":        404,
		"/api/routes/changes?from=x":                 400,
		"/api/routes/changes":                        200,
		"/api/trace/panel?target=Beta":               200,
	} {
		if w := do(h, "GET", p, host); w.Code != code {
			t.Errorf("GET %s = %d, want %d (%s)", p, w.Code, code, w.Body.String())
		}
	}
	for _, p := range []string{"/api/trace?target=Alpha", "/api/trace/panel?target=Alpha", "/api/routes/changes"} {
		if w := do(h, "POST", p, host); w.Code != 405 {
			t.Errorf("POST %s = %d", p, w.Code)
		}
	}
	if w := do(h, "GET", "/api/investigate?target=Alpha", host); w.Code != 405 {
		t.Errorf("GET investigate = %d", w.Code)
	}

	// Timelines: one store query, one series per requested hop.
	before := cs.calls.Load()
	w = do(h, "GET", "/api/trace/panel?target=Alpha&hops=9,1,6,1&from=now-10m&points=5000", host)
	if w.Code != 200 {
		t.Fatalf("trace panel = %d %s", w.Code, w.Body.String())
	}
	if n := cs.calls.Load() - before; n != 1 {
		t.Errorf("trace panel made %d store calls", n)
	}
	var pr struct {
		Group  struct{ Title string } `json:"group"`
		Len    int                    `json:"len"`
		Series []struct {
			Target string     `json:"target"`
			Kind   string     `json:"kind"`
			Hop    int        `json:"hop"`
			Mean   []*float64 `json:"mean"`
			Lost   []uint32   `json:"lost"`
		} `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pr); err != nil {
		t.Fatal(err)
	}
	if len(pr.Series) != 3 || pr.Series[0].Hop != 1 || pr.Series[1].Hop != 6 || pr.Series[2].Hop != 9 || pr.Series[0].Kind != "trace" {
		t.Fatalf("series = %+v", pr.Series)
	}
	if pr.Len > int(10*time.Minute/DefaultTraceInterval)+1 {
		t.Errorf("len %d: buckets finer than the trace interval", pr.Len)
	}
	lost := 0
	for _, x := range pr.Series[1].Lost {
		lost += int(x)
	}
	if lost == 0 {
		t.Error("hop 6 shows no loss")
	}
	// Default hops: the current route.
	w = do(h, "GET", "/api/trace/panel?target=Alpha", host)
	_ = json.Unmarshal(w.Body.Bytes(), &pr)
	if len(pr.Series) != len(routeA) {
		t.Errorf("default hops = %d series", len(pr.Series))
	}
	// Profile marks traced targets.
	var prof struct {
		Features struct{ Trace bool } `json:"features"`
		Targets  []struct {
			Name  string
			Trace bool
		}
	}
	_ = json.Unmarshal(do(h, "GET", "/api/profile", host).Body.Bytes(), &prof)
	if !prof.Features.Trace || !prof.Targets[0].Trace || prof.Targets[1].Trace {
		t.Errorf("profile = %+v", prof)
	}
}

func TestInvestigate(t *testing.T) {
	d, ft, _ := traceFixture(t)
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"})
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
	for name, c := range map[string]struct {
		hdr  map[string]string
		code int
	}{
		"cross-origin": {with(map[string]string{"Origin": "http://evil.com"}, "Sec-Fetch-Site"), 403},
		"cross-site":   {with(map[string]string{"Sec-Fetch-Site": "cross-site"}), 403},
		"no origin":    {with(nil, "Origin", "Sec-Fetch-Site"), 403},
		"no token":     {with(nil, HeaderCSRF), 403},
		"bad token":    {with(map[string]string{HeaderCSRF: "nope"}), 403},
		"foreign host": {with(map[string]string{"Host": "evil.com", "Origin": "http://evil.com"}), 421},
	} {
		if w := do(h, "POST", "/api/investigate?target=Beta", c.hdr); w.Code != c.code {
			t.Errorf("%s: %d, want %d", name, w.Code, c.code)
		}
	}
	if len(ft.started) != 0 {
		t.Fatalf("refused requests started traces: %v", ft.started)
	}
	for p, code := range map[string]int{"/api/investigate": 400, "/api/investigate?target=Nope": 404, "/api/investigate?target=Beta&x=1": 400} {
		if w := do(h, "POST", p, good); w.Code != code {
			t.Errorf("POST %s = %d, want %d", p, w.Code, code)
		}
	}
	w := do(h, "POST", "/api/investigate?target=Beta", good)
	var r investigationJSON
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	if w.Code != 200 || r.Target != "Beta" || r.State != "running" || r.RoundsMax != InvestigateRounds || r.RoundsDone != 0 ||
		r.IntervalMs != 2500 || r.StartedAt == 0 || r.FinishedAt != 0 {
		t.Fatalf("investigate = %d %s", w.Code, w.Body.String())
	}
	// A POST while it runs returns its state: no new session, no reset.
	ft.sessions[0].advance(40)
	w = do(h, "POST", "/api/investigate?target=Beta", good)
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	if len(ft.started) != 1 || r.RoundsDone != 40 || r.State != "running" {
		t.Errorf("re-post: started %v, %s", ft.started, w.Body.String())
	}
	// The trace view reports it.
	var tr traceResp
	_ = json.Unmarshal(do(h, "GET", "/api/trace?target=Beta", good).Body.Bytes(), &tr)
	if tr.Investigation == nil || tr.Investigation.RoundsDone != 40 || tr.Traced {
		t.Errorf("trace Beta = %+v", tr)
	}
	// It stops at 250 rounds by itself, and stays visible as finished.
	ft.sessions[0].advance(1000)
	_ = json.Unmarshal(do(h, "GET", "/api/trace?target=Beta", good).Body.Bytes(), &tr)
	if inv := tr.Investigation; inv == nil || inv.State != "finished" || inv.RoundsDone != InvestigateRounds || inv.FinishedAt == 0 {
		t.Errorf("finished: %+v", tr.Investigation)
	}
	// Starting again after it finished is a fresh 250.
	w = do(h, "POST", "/api/investigate?target=Beta", good)
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	if len(ft.started) != 2 || r.State != "running" || r.RoundsDone != 0 {
		t.Errorf("restart: started %v, %s", ft.started, w.Body.String())
	}
	// Bounded: at most MaxInvestigations distinct targets running.
	p := d.Profile()
	for i := range MaxInvestigations + 2 {
		p.Targets = append(p.Targets, model.Target{Name: fmt.Sprintf("T%d", i), Group: "common"})
	}
	codes := map[int]int{}
	for i := range MaxInvestigations + 2 {
		codes[do(h, "POST", fmt.Sprintf("/api/investigate?target=T%d", i), good).Code]++
	}
	if codes[200] != MaxInvestigations-1 || codes[http.StatusTooManyRequests] != 3 {
		t.Errorf("limit: %v", codes)
	}
	// No admin token needed where there is none, even where the other
	// controls are disabled (LAN bind without one); where there is one
	// (a LAN bind, a tunnel via --allow-host), it is needed.
	lan := with(map[string]string{"Host": "192.168.1.5:3000", "Origin": "http://192.168.1.5:3000"})
	for _, c := range []struct {
		o    LocalOptions
		hdr  map[string]string
		code int
	}{
		{LocalOptions{Addr: "0.0.0.0:3000", CSRFToken: "tok123"}, lan, 200},
		{LocalOptions{Addr: "0.0.0.0:3000", CSRFToken: "tok123", AdminToken: testAdmin}, lan, 403},
		{LocalOptions{Addr: "0.0.0.0:3000", CSRFToken: "tok123", AdminToken: testAdmin}, with(map[string]string{"Host": "192.168.1.5:3000", "Origin": "http://192.168.1.5:3000", HeaderAdmin: testAdmin}), 200},
		// --allow-host with --admin-token=off: off, proxy headers or not.
		{LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123", ExtraHosts: []string{"fyisp.example.com"}}, good, 403},
		// Loopback without a token, through a proxy that rewrote Host.
		{LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"}, with(map[string]string{"X-Forwarded-For": "203.0.113.9"}), 403},
	} {
		d2, _, _ := traceFixture(t)
		hb := Local(d2, c.o)
		if w := do(hb, "POST", "/api/investigate?target=Alpha", c.hdr); w.Code != c.code {
			t.Errorf("%+v: %d %s, want %d", c.o, w.Code, w.Body.String(), c.code)
		}
	}
	// Tracer errors surface locally.
	d3, ft3, _ := traceFixture(t)
	ft3.err = fmt.Errorf("no ping socket")
	h2 := Local(d3, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"})
	if w := do(h2, "POST", "/api/investigate?target=Alpha", good); w.Code != 500 || !strings.Contains(w.Body.String(), "no ping socket") {
		t.Errorf("tracer error = %d %s", w.Code, w.Body.String())
	}
}
