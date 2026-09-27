package web

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// mapBaselines is a BaselineSource over a map of TCP medians (p95 is 1.5×
// the median).
type mapBaselines map[string]float64

func (m mapBaselines) Get(k model.SeriesKey, at time.Time) (model.Baseline, bool) {
	med, ok := m[k.Target]
	if !ok || k.Kind != model.KindTCP {
		return model.Baseline{}, false
	}
	return model.Baseline{Key: k, MedianMs: med, P95Ms: med * 1.5, Samples: 1000, HourOfDay: k.Target == "OK"}, true
}

// overviewFixture has one target per row state over the last 30 minutes of
// 15s TCP samples (the kind shipped profiles use), plus targets the TCP
// overview must leave out: a path target and an HTTPS-only target (a
// --config file may still ask for HTTPS).
func overviewFixture(t *testing.T, now time.Time) (*countingStore, Deps) {
	t.Helper()
	acme := func(name, geo string) model.Target {
		return model.Target{Name: name, Host: strings.ToLower(name) + ".example.com", Group: "clouds", Port: 8443,
			Provider: "acme", ProviderTitle: "Acme Cloud", ProviderKind: "cloud", City: "Frankfurt", Country: "DE", Geo: geo}
	}
	tcp := []model.ProbeKind{model.KindTCP}
	p := &model.Profile{Name: "ov", Groups: []model.Group{{ID: "path", Title: "Network path"}, {ID: "clouds", Title: "Clouds"}, {ID: "home", Title: "Home"}}}
	for _, tg := range []model.Target{
		{Name: "Gateway", Host: "192.168.1.1", Group: "path", Layer: model.LayerGateway, Kinds: []model.ProbeKind{model.KindICMP}},
		acme("Fail", "eu"), acme("Lossy", "eu"), acme("VerySlow", "na"), acme("Slow", "as"), acme("OK", "eu"),
		{Name: "Few", Host: "10.1.2.3", Group: "home", Kinds: tcp},
		{Name: "New", Host: "10.1.2.4", Group: "home", Kinds: tcp},
		{Name: "NoBase", Host: "10.1.2.5", Group: "home"},
		{Name: "HTTPSOnly", Host: "10.1.2.6", Group: "home", Kinds: []model.ProbeKind{model.KindHTTPS}},
	} {
		p.Targets = append(p.Targets, tg)
	}
	f := store.NewFake()
	rtt := map[string]time.Duration{"Fail": 10, "Lossy": 10, "VerySlow": 40, "Slow": 20, "OK": 11, "NoBase": 50, "HTTPSOnly": 5, "Gateway": 1}
	i := 0
	for ts := now.Add(-30 * time.Minute).Truncate(15 * time.Second); ts.Before(now); ts = ts.Add(15 * time.Second) {
		i++
		for name, ms := range rtt {
			kind := model.KindTCP
			switch name {
			case "HTTPSOnly":
				kind = model.KindHTTPS
			case "Gateway":
				kind = model.KindICMP
			}
			s := model.Sample{Key: model.SeriesKey{Target: name, Kind: kind}, Slot: ts, RTT: ms * time.Millisecond}
			switch {
			case name == "Fail" && i%2 == 0: // 50% loss, mostly refused
				s.Lost, s.RTT, s.Reason = true, 0, model.ReasonRefused
				if i%8 == 0 {
					s.Reason = model.ReasonTimeout
				}
			case name == "Lossy" && i%20 == 0: // 5% loss
				s.Lost, s.RTT, s.Reason = true, 0, model.ReasonTimeout
			}
			f.Observe(s)
		}
	}
	for j := range 3 { // too few samples to judge
		f.Observe(model.Sample{Key: model.SeriesKey{Target: "Few", Kind: model.KindTCP}, Slot: now.Add(-time.Duration(j+1) * 15 * time.Second), RTT: 9 * time.Millisecond})
	}
	cs := &countingStore{Reader: f}
	d := Deps{
		Profile:   func() *model.Profile { return p },
		Store:     cs,
		Status:    func() Status { return Status{} },
		Baselines: mapBaselines{"Fail": 10, "Lossy": 10, "VerySlow": 10, "Slow": 10, "OK": 10, "Few": 10, "New": 10},
	}
	return cs, d
}

func getOverview(t *testing.T, h http.Handler, path string, hdr map[string]string) (overviewJSON, string) {
	t.Helper()
	w := do(h, "GET", path, hdr)
	if w.Code != 200 {
		t.Fatalf("GET %s = %d %s", path, w.Code, w.Body.String())
	}
	var o overviewJSON
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	return o, w.Body.String()
}

func TestOverviewStates(t *testing.T) {
	now := time.Now()
	cs, d := overviewFixture(t, now)
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000"})
	o, body := getOverview(t, h, "/api/overview?from=now-30m&to=now", map[string]string{"Host": "127.0.0.1:3000"})
	if n := cs.calls.Load(); n != 1 {
		t.Errorf("%d store calls, want 1", n)
	}
	if q := cs.last; len(q.Keys) != 8 || q.MaxPoints != 1 {
		t.Errorf("store query %+v", q)
	}
	if o.Kind != "tcp" || o.To-o.From != (30*time.Minute).Milliseconds() || o.Capped {
		t.Errorf("header %+v", o)
	}
	want := map[string]string{"Fail": "failing", "Lossy": "lossy", "VerySlow": "very_slow", "Slow": "slow", "OK": "ok",
		"Few": "unmeasured", "New": "unmeasured", "NoBase": "ok"}
	rows := map[string]overviewRow{}
	for _, r := range o.Rows {
		rows[r.Target] = r
		if want[r.Target] != r.State {
			t.Errorf("%s: state %s, want %s (%+v)", r.Target, r.State, want[r.Target], r)
		}
	}
	if len(o.Rows) != len(want) {
		t.Errorf("rows %s", body) // Gateway (path) and HTTPSOnly (no TCP) are left out
	}
	if r := rows["Fail"]; r.Reason != "refused" || math.Abs(r.Loss-0.5) > 0.02 || r.Lost == 0 || r.N == 0 {
		t.Errorf("Fail %+v", r)
	}
	if r := rows["Lossy"]; r.Reason != "timeout" || r.Loss < 0.04 || r.Loss > 0.06 {
		t.Errorf("Lossy %+v", r)
	}
	if r := rows["VerySlow"]; r.Ratio == nil || *r.Ratio != 4 || *r.NowMs != 40 || *r.NormalMs != 10 || *r.NormalP95Ms != 15 || r.Reason != "" {
		t.Errorf("VerySlow %+v", r)
	}
	if r := rows["OK"]; r.Ratio == nil || *r.Ratio != 1.1 || !r.HourOfDay {
		t.Errorf("OK %+v", r)
	}
	if r := rows["Few"]; r.N != 3 || r.NowMs == nil || r.NormalMs == nil {
		t.Errorf("Few %+v", r)
	}
	// Omitted fields: no now_ms without samples, no normal or ratio without
	// a baseline, no reason without losses.
	var raw struct{ Rows []map[string]any }
	_ = json.Unmarshal([]byte(body), &raw)
	for _, r := range raw.Rows {
		name := r["target"]
		_, hasNow := r["now_ms"]
		_, hasNormal := r["normal_ms"]
		_, hasRatio := r["ratio"]
		_, hasReason := r["reason"]
		switch name {
		case "New":
			if hasNow || hasRatio || !hasNormal {
				t.Errorf("New %v", r)
			}
		case "NoBase":
			if !hasNow || hasNormal || hasRatio || r["normal_p95_ms"] != nil || r["hour_of_day"] != nil {
				t.Errorf("NoBase %v", r)
			}
		}
		if hasReason != (r["lost"].(float64) > 0) {
			t.Errorf("%s: reason %v with lost %v", name, r["reason"], r["lost"])
		}
	}
	// Providers: acme (Fail, Lossy, VerySlow, Slow, OK) and the "home" group
	// (Few, New, NoBase); geos eu, na, as among acme's targets only.
	wantSum := overviewSummary{Targets: 8, Measured: 6, WithNormal: 7, Slow: 2, Lossy: 1, Failing: 1,
		Providers: 2, ProvidersAffected: 1, Geos: 3, GeosAffected: 3}
	if o.Summary != wantSum {
		t.Errorf("summary %+v, want %+v", o.Summary, wantSum)
	}
	// HTTPS: only the target that asks for it; ICMP: none (the path is
	// left out).
	o, _ = getOverview(t, h, "/api/overview?kind=https", map[string]string{"Host": "127.0.0.1:3000"})
	if len(o.Rows) != 1 || o.Kind != "https" || o.Rows[0].Target != "HTTPSOnly" || o.Rows[0].State != "ok" || o.Rows[0].NormalMs != nil {
		t.Errorf("https rows %+v", o.Rows)
	}
	o, _ = getOverview(t, h, "/api/overview?kind=icmp", map[string]string{"Host": "127.0.0.1:3000"})
	if len(o.Rows) != 0 || o.Kind != "icmp" {
		t.Errorf("icmp rows %+v", o.Rows)
	}
}

func TestOverviewNoBaselines(t *testing.T) {
	_, d := overviewFixture(t, time.Now())
	d.Baselines = nil
	o, _ := getOverview(t, Local(d, LocalOptions{Addr: "127.0.0.1:3000"}), "/api/overview", map[string]string{"Host": "127.0.0.1:3000"})
	if o.Summary.WithNormal != 0 || o.Summary.Slow != 0 || o.Summary.Failing != 1 || o.Summary.Lossy != 1 || o.Summary.Measured != 6 {
		t.Errorf("summary %+v", o.Summary)
	}
	for _, r := range o.Rows {
		if r.NormalMs != nil || r.Ratio != nil || r.HourOfDay {
			t.Errorf("row with a normal: %+v", r)
		}
		if (r.Target == "VerySlow" || r.Target == "Slow") && r.State != "ok" {
			t.Errorf("%s without a normal: %s", r.Target, r.State)
		}
	}
	// An empty store: every row unmeasured.
	d.Store = &countingStore{Reader: store.NewFake()}
	o, _ = getOverview(t, Local(d, LocalOptions{Addr: "127.0.0.1:3000"}), "/api/overview", map[string]string{"Host": "127.0.0.1:3000"})
	if o.Summary.Measured != 0 || o.Summary.Targets != 8 || o.Summary.ProvidersAffected != 0 {
		t.Errorf("empty store summary %+v", o.Summary)
	}
}

func TestOverviewParams(t *testing.T) {
	_, d := overviewFixture(t, time.Now())
	now := time.Now()
	for _, tc := range []struct {
		name, prefix string
		h            http.Handler
	}{
		{"local", "/", Local(d, LocalOptions{Addr: "127.0.0.1:3000"})},
		{"public", "/s/" + testSecret + "/", Public(d, testSecret)},
	} {
		for i, c := range []struct {
			q    string
			code int
		}{
			{"", 200}, {"?kind=icmp", 200}, {"?kind=tcp&from=now-1h", 200},
			{"?kind=HTTPS", 400}, {"?kind=https,tcp", 400}, {"?kind=trace", 400}, {"?kind=https&kind=tcp", 400},
			{"?x=1", 400}, {"?group=clouds", 400}, {"?points=1", 400}, {"?from=x", 400}, {"?to=soon", 400},
			{"?from=now-1h&to=now-2h", 400}, {"?from=now&to=now", 400},
			{fmt.Sprintf("?from=%d&to=%d", now.Add(-time.Hour).UnixMilli(), now.Add(-30*time.Minute).UnixMilli()), 200},
		} {
			w := do(tc.h, "GET", tc.prefix+"api/overview"+c.q, map[string]string{"Host": "127.0.0.1:3000", "Cf-Connecting-Ip": fmt.Sprintf("198.51.100.%d", 10+i)})
			if w.Code != c.code {
				t.Errorf("%s %q = %d, want %d (%s)", tc.name, c.q, w.Code, c.code, w.Body.String())
			}
		}
		if w := do(tc.h, "POST", tc.prefix+"api/overview", map[string]string{"Host": "127.0.0.1:3000", "Cf-Connecting-Ip": "198.51.100.40"}); w.Code != 405 {
			t.Errorf("%s POST = %d", tc.name, w.Code)
		}
		// Long windows are capped.
		o, _ := getOverview(t, tc.h, tc.prefix+"api/overview?from=now-90d", map[string]string{"Host": "127.0.0.1:3000", "Cf-Connecting-Ip": "198.51.100.41"})
		if !o.Capped || o.To-o.From != OverviewMaxRange.Milliseconds() {
			t.Errorf("%s 90d: capped %v, %v", tc.name, o.Capped, time.Duration(o.To-o.From)*time.Millisecond)
		}
		o, _ = getOverview(t, tc.h, tc.prefix+"api/overview?from=now-30d", map[string]string{"Host": "127.0.0.1:3000", "Cf-Connecting-Ip": "198.51.100.42"})
		if o.Capped {
			t.Errorf("%s 30d capped", tc.name)
		}
	}
}

func TestOverviewPublicCache(t *testing.T) {
	now := time.Now()
	cs, d := overviewFixture(t, now)
	h := Public(d, testSecret)
	base := "/s/" + testSecret + "/api/overview"
	for i := range 3 {
		getOverview(t, h, base+"?from=now-30m&kind=tcp", pubHdr(i))
	}
	if n := cs.calls.Load(); n != 1 {
		t.Errorf("3 identical requests made %d store calls, want 1", n)
	}
	// Absolute bounds within the same minute share one entry; the key
	// includes the kind.
	m := now.Add(-time.Hour).Truncate(time.Minute)
	for i, off := range []time.Duration{time.Second, 17 * time.Second, 59 * time.Second} {
		q := fmt.Sprintf("?from=%d&to=%d", m.Add(off).UnixMilli(), m.Add(30*time.Minute+off).UnixMilli())
		o, _ := getOverview(t, h, base+q, pubHdr(10+i))
		if o.From != m.UnixMilli() || o.To != m.Add(31*time.Minute).UnixMilli() {
			t.Errorf("bounds not rounded to the minute: %d..%d", o.From, o.To)
		}
	}
	if n := cs.calls.Load(); n != 2 {
		t.Errorf("absolute ranges in one minute made %d store calls, want 2 in total", n)
	}
	getOverview(t, h, base+"?from=now-30m&kind=https", pubHdr(20))
	if n := cs.calls.Load(); n != 3 {
		t.Errorf("another kind: %d store calls, want 3", n)
	}
	// Concurrent misses coalesce into one fill.
	cs.delay = 50 * time.Millisecond
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Go(func() {
			if w := do(h, "GET", base+"?from=now-2h", pubHdr(30+i)); w.Code != 200 {
				t.Errorf("concurrent request = %d", w.Code)
			}
		})
	}
	wg.Wait()
	if n := cs.calls.Load(); n != 4 {
		t.Errorf("10 concurrent identical requests: %d store calls in total, want 4", n)
	}
	// The local listener is not cached.
	l := Local(d, LocalOptions{Addr: "127.0.0.1:3000"})
	cs.delay = 0
	getOverview(t, l, "/api/overview", map[string]string{"Host": "127.0.0.1:3000"})
	getOverview(t, l, "/api/overview", map[string]string{"Host": "127.0.0.1:3000"})
	if n := cs.calls.Load(); n != 6 {
		t.Errorf("local: %d store calls in total, want 6", n)
	}
}

func TestOverviewPublicRedaction(t *testing.T) {
	cs, d := overviewFixture(t, time.Now())
	h := Public(d, testSecret)
	base := "/s/" + testSecret + "/"
	check := func(i int, p string) {
		w := do(h, "GET", base+p, pubHdr(60+i))
		body := w.Body.String()
		for k, v := range w.Header() {
			body += "\n" + k + ": " + strings.Join(v, ",")
		}
		if m := leakRE.FindString(body); m != "" {
			t.Errorf("public %q leaks %q", p, m)
		}
	}
	for i, p := range []string{"api/overview", "api/overview?kind=tcp", "api/overview?kind=icmp&from=now-90d", "api/overview?kind=x", "api/profile"} {
		check(i, p)
	}
	// Store errors are not passed through.
	cs.err = fmt.Errorf("sqlite: open /home/venkat/.local/state/fyisp/fyisp.db: 192.168.1.20")
	check(10, "api/overview?from=now-1h")
}

// TestProfileMetadata checks /api/profile's provider and location fields on
// a real catalog profile, and that neither listener serves a host.
func TestProfileMetadata(t *testing.T) {
	reg, err := profile.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	p, err := reg.Resolve([]string{"linode", "dns", "default"}, profile.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_, d, _ := fixture(t, now)
	d.Profile = func() *model.Profile { return p }
	for _, tc := range []struct {
		name, path string
		h          http.Handler
	}{
		{"local", "/api/profile", Local(d, LocalOptions{Addr: "127.0.0.1:3000"})},
		{"public", "/s/" + testSecret + "/api/profile", Public(d, testSecret)},
	} {
		w := do(tc.h, "GET", tc.path, map[string]string{"Host": "127.0.0.1:3000", "Cf-Connecting-Ip": "198.51.100.90"})
		body := w.Body.String()
		for _, tg := range p.Targets {
			hosts := []string{tg.Host}
			for _, x := range tg.HostOverrides {
				hosts = append(hosts, x)
			}
			for _, x := range hosts {
				if strings.Contains(body, `"`+x+`"`) {
					t.Errorf("%s profile leaks host %q of %s", tc.name, x, tg.Name)
				}
			}
		}
		var pj profileJSON
		if err := json.Unmarshal(w.Body.Bytes(), &pj); err != nil {
			t.Fatal(err)
		}
		byName := map[string]targetSummary{}
		for _, ts := range pj.Targets {
			byName[ts.Name] = ts
		}
		if v := byName["Linode-us-east"]; v.Provider != "linode" || v.ProviderTitle != "Akamai Cloud (Linode)" || v.ProviderKind != "cloud" ||
			v.City != "Newark" || v.Country != "US" || v.Geo != "na" {
			t.Errorf("%s Linode-us-east %+v", tc.name, v)
		}
		// A default-profile target: its group is its provider, no geo.
		if v := byName["DevTunnel-UkSouth"]; v.Provider != "devtunnels" || v.ProviderTitle != "DevTunnels" || v.Geo != "" || v.ProviderKind != "" {
			t.Errorf("%s DevTunnel-UkSouth %+v", tc.name, v)
		}
		for _, ts := range pj.Targets {
			if ts.Layer != "" && (ts.Provider != "" || ts.Geo != "") {
				t.Errorf("%s path target with metadata %+v", tc.name, ts)
			}
			if ts.Layer == "" && ts.Provider == "" {
				t.Errorf("%s target without a provider %+v", tc.name, ts)
			}
			if ts.Geo != "" && !strings.Contains(" na sa eu me af as oc global ", " "+ts.Geo+" ") {
				t.Errorf("%s geo %q", tc.name, ts.Geo)
			}
		}
	}
}

func TestOverviewRowState(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	row := func(n, lost int64, ratio *float64) overviewRow {
		r := overviewRow{N: n, Lost: lost, Ratio: ratio}
		if n+lost > 0 {
			r.Loss = float64(lost) / float64(n+lost)
		}
		return r
	}
	for _, c := range []struct {
		name string
		r    overviewRow
		want string
	}{
		{"nothing", row(0, 0, nil), stateUnmeasured},
		{"4 samples", row(2, 2, nil), stateUnmeasured},
		{"all lost, too few", row(0, 4, nil), stateUnmeasured},
		{"1 lost of 6", row(5, 1, nil), stateOK},
		{"1 lost of 100", row(99, 1, nil), stateOK},
		{"2 lost of 9", row(7, 2, nil), stateUnmeasured},
		{"all lost, 9", row(0, 9, nil), stateUnmeasured},
		{"1 lost of 9, slow", row(8, 1, f(2)), stateSlow},
		{"2 lost of 10", row(8, 2, nil), stateFailing},
		{"all lost, 10", row(0, 10, nil), stateFailing},
		{"2 lost of 11", row(9, 2, nil), stateLossy},
		{"2 lost of 200", row(198, 2, f(4)), stateLossy},
		{"2 lost of 201", row(199, 2, f(4)), stateVerySlow}, // under 1%
		{"ratio 1.49", row(100, 0, f(1.49)), stateOK},
		{"ratio 1.5", row(100, 0, f(1.5)), stateSlow},
		{"ratio 3", row(100, 0, f(3)), stateVerySlow},
	} {
		if got := rowState(c.r); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}
