//go:build netns && linux

package netns

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSpeed runs fyisp's speed tests against the lab's iperf3 servers
// (iperf_test.go) with the line shaped on the ISP router and the gateway,
// as a real access line is: the download queue on the ISP's side of the
// line (i0, towards the gateway), the upload queue on the gateway's uplink
// (g1, towards the ISP). The ISP edge (the ISP router's address on that
// line) is behind both queues; the gateway's LAN address is in front of
// them. Tests are started with "Test now" (POST api/speed/run).
//
//	bufferbloat       1 s tail-drop queues: loaded ISP-edge RTT ~1 s, gateway flat, stored
//	aqm               the same rates with fq_codel: loaded ISP-edge RTT stays low
//	access            the download rate drops for everyone: every server dips
//	one-path          only the path to one server is congested: only it dips
//	busy-failed       busy and failing servers: busy/failed, never a speed
//	fast-evening-cut  2 Gbit/s: capped tests' rate method and lower bounds; a 30% cut; the finding says isp
//
// Every scenario also checks the masking: the verdict stays ok during the
// tests, no incident is logged and Home's "not running" spans stay empty.
func TestSpeed(t *testing.T) {
	parallel(t)

	t.Run("bufferbloat", func(t *testing.T) {
		parallel(t)
		h := newSpeedLab(t, func(h *harness) {
			h.nsRun(t, h.isp, "tc", "qdisc", "add", "dev", "i0", "root", "tbf", "rate", "50mbit", "burst", "32kb", "limit", "6250000") // 1 s
			h.nsRun(t, h.gw, "tc", "qdisc", "add", "dev", "g1", "root", "tbf", "rate", "20mbit", "burst", "32kb", "limit", "2500000")  // 1 s
		})
		h.waitIdle(t)
		r := h.speedTest(t, iperfA)
		l := r.latency(t)
		e, g := l.Edge, l.Gateway
		// A short test does not fill the 1 s queues every time (the
		// senders' windows, a busy machine): at least 200 ms more at the
		// median, and the upload's queue well past that at p95.
		for _, x := range []struct {
			name string
			s    *speedStats
		}{{"edge down", e.Down}, {"edge up", e.Up}} {
			if x.s.P50-e.Idle.P50 < 200 {
				t.Errorf("%s %+v vs idle %+v: want the 1 s queue to show (p50 at least 200 ms more)", x.name, *x.s, *e.Idle)
			}
		}
		if e.Up.P95 < 400 {
			t.Errorf("edge up %+v: want the 1 s upload queue at p95 (at least 400 ms)", *e.Up)
		}
		for _, x := range []*speedStats{g.Down, g.Up} {
			if x.P50 > 20 {
				t.Errorf("gateway loaded %+v: want flat, the queue is past the router", *x)
			}
		}
		if r.DownBps == nil || *r.DownBps < 30e6 || *r.DownBps > 55e6 || r.UpBps == nil || *r.UpBps < 12e6 || *r.UpBps > 22e6 {
			t.Errorf("down %v up %v bit/s, want about 50 and 20 Mbit/s", r.DownBps, r.UpBps)
		}
		// The store has it, and so has /metrics.
		stored := h.speedTests(t, r.Server)
		if i := slices.IndexFunc(stored, func(x speedTestAPI) bool { return x.ID == r.ID }); i < 0 || stored[i].Latency == nil || stored[i].Latency.Edge.Up == nil ||
			stored[i].Latency.Edge.Up.P50 != e.Up.P50 {
			t.Errorf("stored tests %+v lack test %d's latency", stored, r.ID)
		}
		mt := h.metrics(t)
		key := fmt.Sprintf(`fyisp_speed_loaded_rtt_seconds{layer="edge",load="upload",region="",server=%q}`, r.Server)
		if v, ok := mt[key]; !ok || v < 0.3 {
			t.Errorf("/metrics %s = %v (present %v), want the loaded RTT", key, v, ok)
		}
		h.checkMasking(t)
	})

	t.Run("aqm", func(t *testing.T) {
		parallel(t)
		h := newSpeedLab(t, func(h *harness) {
			for _, x := range []struct{ ns, dev, rate string }{{h.isp, "i0", "50mbit"}, {h.gw, "g1", "20mbit"}} {
				h.nsRun(t, x.ns, "tc", "qdisc", "add", "dev", x.dev, "root", "handle", "1:", "htb", "default", "10")
				h.nsRun(t, x.ns, "tc", "class", "add", "dev", x.dev, "parent", "1:", "classid", "1:10", "htb", "rate", x.rate)
				h.nsRun(t, x.ns, "tc", "qdisc", "add", "dev", x.dev, "parent", "1:10", "fq_codel")
			}
		})
		h.waitIdle(t)
		r := h.speedTest(t, iperfA)
		e := r.latency(t).Edge
		for _, x := range []struct {
			name string
			s    *speedStats
		}{{"edge down", e.Down}, {"edge up", e.Up}} {
			if x.s.P50-e.Idle.P50 > 30 || x.s.Loss > 0.05 {
				t.Errorf("%s %+v vs idle %+v: want under 30 ms more and no loss with fq_codel", x.name, *x.s, *e.Idle)
			}
		}
		h.checkMasking(t)
	})

	// access: the line's download rate drops (the access network is
	// congested): both servers, behind different paths, dip together.
	t.Run("access", func(t *testing.T) {
		parallel(t)
		h := newSpeedLab(t, func(h *harness) { h.lineRate(t, "100mbit") })
		before := h.speedPair(t)
		h.nsRun(t, h.isp, "tc", "class", "change", "dev", "i0", "parent", "1:", "classid", "1:10", "htb", "rate", "30mbit")
		after := h.speedPair(t)
		for i, a := range []netip.Addr{iperfA, iperfB} {
			if r := after[i] / before[i]; r > 0.5 {
				t.Errorf("%s: %.0f -> %.0f Mbit/s (x%.2f), want a dip under congestion of the access line", a, before[i]/1e6, after[i]/1e6, r)
			}
		}
		h.checkMasking(t)
	})

	// one-path: only traffic from server B is congested (its path past the
	// ISP): B dips, A does not.
	t.Run("one-path", func(t *testing.T) {
		parallel(t)
		h := newSpeedLab(t, func(h *harness) { h.lineRate(t, "100mbit") })
		before := h.speedPair(t)
		h.nsRun(t, h.isp, "tc", "class", "add", "dev", "i0", "parent", "1:", "classid", "1:20", "htb", "rate", "30mbit")
		h.nsRun(t, h.isp, "tc", "filter", "add", "dev", "i0", "parent", "1:", "protocol", "ip", "prio", "1", "u32",
			"match", "ip", "src", iperfB.String()+"/32", "flowid", "1:20")
		after := h.speedPair(t)
		if r := after[1] / before[1]; r > 0.5 {
			t.Errorf("B: %.0f -> %.0f Mbit/s (x%.2f), want a dip on its path", before[1]/1e6, after[1]/1e6, r)
		}
		if r := after[0] / before[0]; r < 0.8 {
			t.Errorf("A: %.0f -> %.0f Mbit/s (x%.2f), want no dip: its path is fine", before[0]/1e6, after[0]/1e6, r)
		}
		h.checkMasking(t)
	})

	// fast-evening-cut: a 2 Gbit/s line (speedfast_test.go).
	t.Run("fast-evening-cut", func(t *testing.T) {
		parallel(t)
		fastEveningCut(t)
	})

	t.Run("busy-failed", func(t *testing.T) {
		parallel(t)
		h := newSpeedLab(t, func(h *harness) { h.lineRate(t, "100mbit") })
		for _, c := range []struct {
			srv  netip.Addr
			want string
		}{{iperfBusy, "busy"}, {iperfFail, "failed"}} {
			r := h.speedTest(t, c.srv)
			if r.Outcome != c.want || r.DownBps != nil || r.UpBps != nil {
				t.Errorf("%s: outcome %s down %v up %v, want %s and no speed", c.srv, r.Outcome, r.DownBps, r.UpBps, c.want)
			}
			if d := time.Duration(r.End-r.Start) * time.Millisecond; d > 45*time.Second {
				t.Errorf("%s: the slot took %s, over the 45 s cap", c.srv, d)
			}
		}
		// A healthy server still measures afterwards.
		if r := h.speedTest(t, iperfA); r.Outcome != "ok" || r.DownBps == nil {
			t.Errorf("after busy and failed: %+v", r)
		}
		h.checkMasking(t)
	})
}

// speedLab is a harness running fyisp with the verdict profile (the path
// group gives the gateway and the ISP edge) and every lab iperf3 server as
// the user's own; speed tests start only with Test now.
type speedLab struct {
	*harness
	started time.Time
	csrf    string
	dataDir string // "" with --ephemeral

	mu    sync.Mutex
	kinds []string // verdict kinds seen while watching
	stop  chan struct{}
	done  chan struct{}
}

func newSpeedLab(t *testing.T, shape func(h *harness)) *speedLab {
	return newSpeedLabData(t, shape, nil)
}

// newSpeedLabData is newSpeedLab with a data directory that data (if not
// nil) fills before fyisp starts; nil runs --ephemeral.
func newSpeedLabData(t *testing.T, shape func(h *harness), data func(dir string)) *speedLab {
	h := newVerdictHarness(t)
	shape(h)
	var servers []string
	for _, a := range iperfAddrs {
		servers = append(servers, net4(a, iperfPorts[0]))
	}
	store, dataDir := []string{"--ephemeral"}, ""
	if data != nil {
		dir := filepath.Join(h.dir, "data")
		h.mkdirOwned(t, dir, 0o700)
		data(dir)
		h.run(t, "chown", "-R", fmt.Sprintf("%d:%d", runUID, runUID), dir)
		store = []string{"--data-dir", dir}
		dataDir = dir
	}
	start := time.Now()
	fy := h.startFyisp(t, append(store, "--speedtest-server", strings.Join(servers, ","))...)
	h.waitReady(t, fy)
	h.stopAtEnd(t, fy)
	l := &speedLab{harness: h, started: start, dataDir: dataDir, stop: make(chan struct{}), done: make(chan struct{})}
	l.waitVerdict(t, "ok", 120*time.Second)
	m := regexp.MustCompile(`<meta name="fyisp-token" content="([^"]+)">`).FindSubmatch(h.get(t, "/"))
	if m == nil {
		t.Fatal("no CSRF token in the page")
	}
	l.csrf = string(m[1])
	// Watch the verdict for the rest of the scenario.
	go func() {
		defer close(l.done)
		for {
			select {
			case <-l.stop:
				return
			case <-time.After(2 * time.Second):
			}
			resp, err := h.client.Get("http://" + listenAddr + "/api/verdict")
			if err != nil {
				continue
			}
			var v verdictAPI
			_ = json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			l.mu.Lock()
			if !slices.Contains(l.kinds, v.Kind) {
				l.kinds = append(l.kinds, v.Kind)
			}
			l.mu.Unlock()
		}
	}()
	t.Cleanup(func() { close(l.stop); <-l.done })
	return l
}

// waitIdle waits until a test has idle latency to compare with: the
// first 2 minutes after fyisp starts are never idle (path discovery and
// the first traceroutes), and the idle stats need 20 samples (the ISP edge
// and the gateway are pinged every second).
func (l *speedLab) waitIdle(t *testing.T) {
	if d := time.Until(l.started.Add(2*time.Minute + 30*time.Second)); d > 0 {
		t.Logf("waiting %s for idle latency samples", d.Round(time.Second))
		time.Sleep(d)
	}
}

func net4(a netip.Addr, port int) string { return a.String() + ":" + strconv.Itoa(port) }

// lineRate shapes the line's download (the ISP's side, i0) to rate with an
// htb class 1:10 that scenarios change, and its upload (g1) to 40 Mbit/s
// (unshaped, a veth runs at tens of Gbit/s and burns the CPU).
func (h *harness) lineRate(t *testing.T, rate string) {
	for _, x := range []struct{ ns, dev, rate string }{{h.isp, "i0", rate}, {h.gw, "g1", "40mbit"}} {
		h.nsRun(t, x.ns, "tc", "qdisc", "add", "dev", x.dev, "root", "handle", "1:", "htb", "default", "10")
		h.nsRun(t, x.ns, "tc", "class", "add", "dev", x.dev, "parent", "1:", "classid", "1:10", "htb", "rate", x.rate)
		h.nsRun(t, x.ns, "tc", "qdisc", "add", "dev", x.dev, "parent", "1:10", "fq_codel")
	}
}

type speedStats struct {
	N    int     `json:"n"`
	Lost int     `json:"lost"`
	Loss float64 `json:"loss"`
	P50  float64 `json:"p50_ms"`
	P95  float64 `json:"p95_ms"`
}

type speedPhases struct {
	Idle *speedStats `json:"idle"`
	Down *speedStats `json:"down"`
	Up   *speedStats `json:"up"`
}

type speedTestAPI struct {
	ID      int64    `json:"id"`
	Start   int64    `json:"start"`
	End     int64    `json:"end"`
	Outcome string   `json:"outcome"`
	Server  string   `json:"server"`
	DownBps *float64 `json:"down_bps"`
	UpBps   *float64 `json:"up_bps"`
	// Still speeding up at the 10 s time cap (lower bounds).
	DownLowerBound bool `json:"down_lower_bound"`
	UpLowerBound   bool `json:"up_lower_bound"`
	// Seconds measured.
	DownS   float64 `json:"down_s"`
	UpS     float64 `json:"up_s"`
	Conn    string  `json:"conn"`
	Latency *struct {
		Edge    speedPhases `json:"edge"`
		Gateway speedPhases `json:"gateway"`
	} `json:"latency"`
}

// latency checks that every phase of both layers has stats and returns them.
func (r speedTestAPI) latency(t *testing.T) struct{ Edge, Gateway speedPhases } {
	t.Helper()
	l := r.Latency
	if l == nil || l.Edge.Idle == nil || l.Edge.Down == nil || l.Edge.Up == nil || l.Gateway.Idle == nil || l.Gateway.Down == nil || l.Gateway.Up == nil {
		b, _ := json.Marshal(r)
		t.Fatalf("test without full latency under load: %s", b)
	}
	return struct{ Edge, Gateway speedPhases }{l.Edge, l.Gateway}
}

// speedTest runs Test now against srv's first port and returns the result
// (from api/speed's latest test).
func (l *speedLab) speedTest(t *testing.T, srv netip.Addr) speedTestAPI {
	t.Helper()
	var s struct {
		Running json.RawMessage `json:"running"`
		Latest  *speedTestAPI   `json:"latest"`
	}
	read := func() {
		// A fresh value each time: Unmarshal into the old Latest would keep
		// fields the new JSON omits (omitempty flags).
		s.Running, s.Latest = nil, nil
		if err := json.Unmarshal(l.get(t, "/api/speed"), &s); err != nil {
			t.Fatal(err)
		}
	}
	read()
	prev := int64(0)
	if s.Latest != nil {
		prev = s.Latest.ID
	}
	id := net4(srv, iperfPorts[0])
	req, _ := http.NewRequest("POST", "http://"+listenAddr+"/api/speed/run?id="+id, nil)
	req.Header.Set("X-FYISP-Token", l.csrf)
	req.Header.Set("Origin", "http://"+listenAddr)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := l.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST api/speed/run?id=%s: %s: %s", id, resp.Status, b)
	}
	t0 := time.Now()
	for {
		time.Sleep(time.Second)
		read()
		if s.Latest != nil && s.Latest.ID != prev && string(s.Running) == "null" {
			break
		}
		if time.Since(t0) > 90*time.Second {
			t.Fatalf("speed test against %s did not finish in 90 s", id)
		}
	}
	r := *s.Latest
	b, _ = json.Marshal(r)
	t.Logf("speed test %s (%s): %s", id, time.Duration(r.End-r.Start)*time.Millisecond, b)
	l.save(t, fmt.Sprintf("speedtest-%d.json", r.ID), b)
	// The lab's c0 is a veth: virtual (no /sys/class/net/c0/device), so
	// neither wired nor Wi-Fi.
	if r.Conn != "unknown" {
		t.Errorf("conn %q, want unknown (a veth)", r.Conn)
	}
	return r
}

// speedPair tests A then B and returns their download rates.
func (l *speedLab) speedPair(t *testing.T) [2]float64 {
	t.Helper()
	var out [2]float64
	for i, a := range []netip.Addr{iperfA, iperfB} {
		r := l.speedTest(t, a)
		if r.Outcome != "ok" || r.DownBps == nil {
			t.Fatalf("%s: %+v", a, r)
		}
		out[i] = *r.DownBps
	}
	return out
}

// speedTests returns the stored tests of a server (api/speed?server=).
func (l *speedLab) speedTests(t *testing.T, server string) []speedTestAPI {
	t.Helper()
	var s struct {
		Tests []speedTestAPI `json:"tests"`
	}
	if err := json.Unmarshal(l.get(t, "/api/speed?server="+server), &s); err != nil {
		t.Fatal(err)
	}
	return s.Tests
}

// checkMasking: tests are not judged. The verdict stayed ok (or warming
// up), no incident was logged, and Home shows no problem and no drop:
// masked probes are not measured, not a loss.
func (l *speedLab) checkMasking(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	kinds := slices.Clone(l.kinds)
	l.mu.Unlock()
	for _, k := range kinds {
		if k != "ok" && k != "warming_up" {
			t.Errorf("verdict %s during the speed tests, want ok (seen %v)", k, kinds)
		}
	}
	if list, raw := l.incidents(t, l.started.Add(-time.Minute)); len(list) != 0 {
		t.Errorf("incidents during the speed tests: %s", raw)
	}
	// Home: no problem, no drop and no "working again" from the tests.
	var home struct {
		Answer struct {
			Key string `json:"key"`
		} `json:"answer"`
		Path []struct {
			ID  string `json:"id"`
			Sub string `json:"sub"`
		} `json:"path"`
		Problems struct {
			Items []struct {
				When string `json:"when"`
				What string `json:"what"`
			} `json:"items"`
		} `json:"problems"`
	}
	b := l.get(t, "/api/home?part=now")
	l.save(t, "home-now.json", b)
	if err := json.Unmarshal(b, &home); err != nil {
		t.Fatal(err)
	}
	if k := home.Answer.Key; strings.HasPrefix(k, "drop:") || strings.HasPrefix(k, "working_again:") {
		t.Errorf("Home's answer %s after the speed tests: %s", k, b)
	}
	for _, n := range home.Path {
		if strings.HasPrefix(n.Sub, "Dropped") {
			t.Errorf("Home's %s: %q after the speed tests", n.ID, n.Sub)
		}
	}
	for _, p := range home.Problems.Items {
		t.Errorf("Home shows a problem during the speed tests: %s: %s", p.When, p.What)
	}
}
