package metrics

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

// ---- a tiny Prometheus text-format parser (enough for the golden test) ----

type family struct {
	help, typ string
	labelSets map[string]bool // sorted label names joined by ","
	samples   []promSample
}

type promSample struct {
	name   string
	labels map[string]string
	value  float64
}

func parseExposition(t *testing.T, r io.Reader) map[string]*family {
	t.Helper()
	fams := map[string]*family{}
	get := func(n string) *family {
		f := fams[n]
		if f == nil {
			f = &family{labelSets: map[string]bool{}}
			fams[n] = f
		}
		return f
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "# HELP "):
			name, help, _ := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			get(name).help = help
		case strings.HasPrefix(line, "# TYPE "):
			name, typ, _ := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			get(name).typ = typ
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			s := parseSample(t, line)
			f := get(s.name)
			names := make([]string, 0, len(s.labels))
			for k := range s.labels {
				names = append(names, k)
			}
			sort.Strings(names)
			f.labelSets[strings.Join(names, ",")] = true
			f.samples = append(f.samples, s)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return fams
}

func parseSample(t *testing.T, line string) promSample {
	t.Helper()
	s := promSample{labels: map[string]string{}}
	rest := line
	if i := strings.IndexByte(line, '{'); i >= 0 {
		s.name = line[:i]
		j := strings.LastIndexByte(line, '}')
		for _, kv := range splitLabels(line[i+1 : j]) {
			k, v, _ := strings.Cut(kv, "=")
			uq, err := strconv.Unquote(v)
			if err != nil {
				t.Fatalf("bad label %q in %q", kv, line)
			}
			s.labels[k] = uq
		}
		rest = strings.TrimSpace(line[j+1:])
	} else {
		s.name, rest, _ = strings.Cut(line, " ")
	}
	v, err := strconv.ParseFloat(strings.Fields(rest)[0], 64)
	if err != nil {
		t.Fatalf("bad value in %q", line)
	}
	s.value = v
	return s
}

// splitLabels splits `a="x",b="y,z"` on commas outside quotes.
func splitLabels(s string) []string {
	var out []string
	inQ, start := false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			inQ = !inQ
		case ',':
			if !inQ {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func (f *family) find(labels map[string]string) (float64, bool) {
next:
	for _, s := range f.samples {
		for k, v := range labels {
			if s.labels[k] != v {
				continue next
			}
		}
		return s.value, true
	}
	return 0, false
}

// ---- fixtures ----

func testProfile() *model.Profile {
	return &model.Profile{
		Name: "test",
		Targets: []model.Target{
			{Name: "Google-Meet", Host: "meet.google.com", HostOverrides: map[model.ProbeKind]string{model.KindICMP: "lens.l.google.com"}},
			{Name: "AWS-me-south-1", Host: "ec2.me-south-1.amazonaws.com"},
			{Name: "TCP-only", Host: "tcp.example", Kinds: []model.ProbeKind{model.KindTCP}},
		},
	}
}

func scrape(t *testing.T, c *Collector) map[string]*family {
	t.Helper()
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d: %s", rec.Code, rec.Body)
	}
	return parseExposition(t, rec.Body)
}

func sample(target string, k model.ProbeKind, rtt time.Duration) model.Sample {
	return model.Sample{Key: model.SeriesKey{Target: target, Kind: k}, Slot: time.Now(), RTT: rtt}
}

func lost(target string, k model.ProbeKind, r model.Reason) model.Sample {
	return model.Sample{Key: model.SeriesKey{Target: target, Kind: k}, Slot: time.Now(), Lost: true, Reason: r}
}

// ---- tests ----

// TestNetworkExporterGolden checks that every ping_* family network_exporter
// exposes (captured from the real image, see testdata) exists with the same
// HELP, TYPE and label names, and the same rtt "type" values.
func TestNetworkExporterGolden(t *testing.T) {
	f, err := os.Open("testdata/network_exporter.golden")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	want := parseExposition(t, f)
	if len(want) < 8 {
		t.Fatalf("golden has only %d families", len(want))
	}

	c := New(testProfile)
	c.Observe(sample("Google-Meet", model.KindICMP, 1735*time.Microsecond))
	c.Observe(lost("AWS-me-south-1", model.KindICMP, model.ReasonTimeout))
	got := scrape(t, c)

	for name, w := range want {
		g := got[name]
		if g == nil {
			t.Errorf("missing family %s", name)
			continue
		}
		if g.help != w.help || g.typ != w.typ {
			t.Errorf("%s: HELP/TYPE = %q/%q, want %q/%q", name, g.help, g.typ, w.help, w.typ)
		}
		if !mapsEqual(g.labelSets, w.labelSets) {
			t.Errorf("%s: label sets = %v, want %v", name, keys(g.labelSets), keys(w.labelSets))
		}
	}
	typeValues := func(f *family) []string {
		var v []string
		for _, s := range f.samples {
			if s.labels["name"] == "Google-Meet" {
				v = append(v, s.labels["type"])
			}
		}
		sort.Strings(v)
		return v
	}
	if g, w := typeValues(got["ping_rtt_seconds"]), typeValues(want["ping_rtt_seconds"]); !slices.Equal(g, w) {
		t.Errorf("ping_rtt_seconds types = %v, want %v", g, w)
	}
	// Nothing from other exporters' families leaks in under the ping_ prefix.
	for name := range got {
		if strings.HasPrefix(name, "ping_") && want[name] == nil {
			t.Errorf("unexpected ping_ family %s", name)
		}
	}
}

func TestICMPValues(t *testing.T) {
	c := New(testProfile)
	for i := 0; i < 3; i++ {
		c.Observe(sample("Google-Meet", model.KindICMP, 20*time.Millisecond))
	}
	c.Observe(lost("Google-Meet", model.KindICMP, model.ReasonGap)) // not measured: ignored
	c.Observe(lost("AWS-me-south-1", model.KindICMP, model.ReasonTimeout))
	c.Observe(sample("AWS-me-south-1", model.KindICMP, 80*time.Millisecond))
	c.Observe(lost("AWS-me-south-1", model.KindICMP, model.ReasonUnreachable))
	m := scrape(t, c)

	meet := map[string]string{"name": "Google-Meet", "target": "lens.l.google.com", "target_ip": ""}
	aws := map[string]string{"name": "AWS-me-south-1", "target": "ec2.me-south-1.amazonaws.com", "target_ip": ""}
	with := func(l map[string]string, k, v string) map[string]string {
		o := map[string]string{k: v}
		for a, b := range l {
			o[a] = b
		}
		return o
	}
	check := func(fam string, labels map[string]string, want float64) {
		t.Helper()
		f := m[fam]
		if f == nil {
			t.Fatalf("no family %s", fam)
		}
		got, ok := f.find(labels)
		if !ok {
			t.Errorf("%s%v missing", fam, labels)
			return
		}
		if diff := got - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s%v = %v, want %v", fam, labels, got, want)
		}
	}
	check("ping_up", nil, 1)
	check("ping_targets", nil, 2) // TCP-only has no ICMP
	check("ping_status", meet, 1)
	check("ping_loss_percent", meet, 0)
	for _, typ := range []string{"best", "worst", "mean", "sum"} {
		check("ping_rtt_seconds", with(meet, "type", typ), 0.020)
		check("ping_rtt_seconds", with(aws, "type", typ), 0)
	}
	for _, typ := range []string{"sd", "usd", "csd", "range"} {
		check("ping_rtt_seconds", with(meet, "type", typ), 0)
	}
	check("ping_rtt_snt_count", meet, 3)
	check("ping_rtt_snt_fail_count", meet, 0)
	check("ping_rtt_snt_seconds", meet, 0.060)

	check("ping_status", aws, 0)
	check("ping_loss_percent", aws, 1) // a fraction, as upstream
	check("ping_rtt_snt_count", aws, 3)
	check("ping_rtt_snt_fail_count", aws, 2)
	check("ping_rtt_snt_seconds", aws, 0.080)

	check("fyisp_probe_samples_total", map[string]string{"name": "AWS-me-south-1", "kind": "icmp"}, 3)
	check("fyisp_probe_lost_total", map[string]string{"name": "AWS-me-south-1", "kind": "icmp", "reason": "timeout"}, 1)
	check("fyisp_probe_lost_total", map[string]string{"name": "AWS-me-south-1", "kind": "icmp", "reason": "unreachable"}, 1)
	if _, ok := m["fyisp_probe_lost_total"].find(map[string]string{"name": "Google-Meet"}); ok {
		t.Error("gap sample counted as loss")
	}
}

func TestHTTPSAndTCP(t *testing.T) {
	c := New(testProfile)
	c.Observe(sample("Google-Meet", model.KindHTTPS, 35*time.Millisecond))
	c.Observe(sample("TCP-only", model.KindTCP, 12*time.Millisecond))
	c.Observe(sample("AWS-me-south-1", model.KindTCP, 90*time.Millisecond))
	c.Observe(lost("AWS-me-south-1", model.KindTCP, model.ReasonNoNetwork))
	m := scrape(t, c)

	if f := m["fyisp_https_rtt_seconds"]; f == nil || f.typ != "gauge" {
		t.Fatalf("fyisp_https_rtt_seconds missing or wrong type: %+v", f)
	}
	if v, ok := m["fyisp_https_rtt_seconds"].find(map[string]string{"name": "Google-Meet", "target": "meet.google.com"}); !ok || v != 0.035 {
		t.Errorf("https rtt = %v %v", v, ok)
	}
	if v, ok := m["fyisp_tcp_rtt_seconds"].find(map[string]string{"name": "TCP-only", "target": "tcp.example"}); !ok || v != 0.012 {
		t.Errorf("tcp rtt = %v %v", v, ok)
	}
	if _, ok := m["fyisp_tcp_rtt_seconds"].find(map[string]string{"name": "AWS-me-south-1"}); ok {
		t.Error("tcp rtt exported although the latest probe was lost")
	}
	if v, ok := m["fyisp_probe_lost_total"].find(map[string]string{"name": "AWS-me-south-1", "kind": "tcp", "reason": "no_network"}); !ok || v != 1 {
		t.Errorf("lost_total = %v %v", v, ok)
	}
	if f := m["fyisp_probe_lost_total"]; f.typ != "counter" {
		t.Errorf("fyisp_probe_lost_total type %q", f.typ)
	}
	if _, ok := m["ping_status"]; ok && len(m["ping_status"].samples) > 0 {
		t.Error("ping_status exported without ICMP samples")
	}
}

func TestProfileReloadDropsTargets(t *testing.T) {
	var mu sync.Mutex
	p := testProfile()
	c := New(func() *model.Profile { mu.Lock(); defer mu.Unlock(); return p })
	c.Observe(sample("AWS-me-south-1", model.KindICMP, time.Millisecond))
	c.Observe(sample("Unknown", model.KindICMP, time.Millisecond))
	m := scrape(t, c)
	if n := len(m["ping_status"].samples); n != 1 {
		t.Fatalf("want 1 ping_status series, got %d", n)
	}
	mu.Lock()
	p = &model.Profile{Targets: []model.Target{{Name: "Other", Host: "o.example"}}}
	mu.Unlock()
	m = scrape(t, c)
	if f := m["ping_status"]; f != nil && len(f.samples) != 0 {
		t.Errorf("removed target still exported: %+v", f.samples)
	}
	// nil profile must not panic
	c2 := New(func() *model.Profile { return nil })
	c2.Observe(sample("x", model.KindICMP, time.Millisecond))
	scrape(t, c2)
}

// The handler must serve a private registry: cloudflared (and anything else)
// registering into the default registry must not show up.
func TestPrivateRegistry(t *testing.T) {
	canary := prometheus.NewCounter(prometheus.CounterOpts{Name: "fyisp_test_default_registry_canary", Help: "x"})
	if err := prometheus.DefaultRegisterer.Register(canary); err == nil {
		defer prometheus.DefaultRegisterer.Unregister(canary)
	}
	m := scrape(t, New(testProfile))
	if _, ok := m["fyisp_test_default_registry_canary"]; ok {
		t.Fatal("default registry leaked into /metrics")
	}
	if _, ok := m["go_goroutines"]; !ok {
		t.Error("go collector missing")
	}
	// Two collectors in one process don't conflict.
	scrape(t, New(testProfile))
}

func TestConcurrentObserveAndScrape(t *testing.T) {
	c := New(testProfile)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				k := model.ProbeKind(j%3 + 1)
				if j%5 == 0 {
					c.Observe(lost("Google-Meet", k, model.ReasonTimeout))
				} else {
					c.Observe(sample("Google-Meet", k, time.Duration(j)*time.Microsecond))
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		scrape(t, c)
	}
	close(stop)
	wg.Wait()
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func keys(m map[string]bool) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

func TestVerdictGauges(t *testing.T) {
	c := New(testProfile)
	if m := scrape(t, c); m["fyisp_verdict"] != nil || m["fyisp_layer_healthy"] != nil {
		t.Fatal("verdict series without a source")
	}
	yes, no := true, false
	c.SetVerdictSource(func() (model.Verdict, map[string]*bool) {
		return model.Verdict{Kind: model.VerdictISP}, map[string]*bool{
			model.LayerGateway: &yes, model.LayerEdge: &no, model.LayerAnycast: nil,
		}
	})
	m := scrape(t, c)
	f := m["fyisp_verdict"]
	if f == nil || f.typ != "gauge" || len(f.samples) != 8 {
		t.Fatalf("fyisp_verdict: %+v", f)
	}
	for _, s := range f.samples {
		want := 0.0
		if s.labels["kind"] == "isp" {
			want = 1
		}
		if s.value != want {
			t.Errorf("fyisp_verdict%v = %v", s.labels, s.value)
		}
	}
	l := m["fyisp_layer_healthy"]
	if l == nil || len(l.samples) != 2 {
		t.Fatalf("fyisp_layer_healthy: %+v", l)
	}
	if v, ok := l.find(map[string]string{"layer": "gateway"}); !ok || v != 1 {
		t.Errorf("gateway %v %v", v, ok)
	}
	if v, ok := l.find(map[string]string{"layer": "isp-edge"}); !ok || v != 0 {
		t.Errorf("isp-edge %v %v", v, ok)
	}
}
