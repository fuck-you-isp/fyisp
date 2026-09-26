//go:build netns && linux

package netns

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/trace"
)

// TestTrace adds an alternative router to its topology (namespace
// fyt-R-N-alt):
//
//	isp (i2 10.99.4.1) <-> (a0 10.99.4.2) alt (a1 10.99.5.1) <-> (n2 10.99.5.2) net
//
// The traced target (hotel, on the internet namespace's loopback) is
// normally reached through the ISP's direct uplink: hops [gw, isp, hotel].
// A /32 route in the ISP namespace moves it behind alt: [gw, isp, alt, hotel].
const (
	altAddr       = "10.99.4.2"
	traceIv       = 2 * time.Second // below the kernel's ICMP error rate limit (1/s per router)
	traceProbeEnv = "FYISP_NETNS_TRACEPROBE"
)

var addrHotel = netip.MustParseAddr("203.0.113.20")

// The test binary runs runTraceProbe when started with traceProbeEnv set
// (from init, so that the shared TestMain stays untouched).
func init() {
	if os.Getenv(traceProbeEnv) != "" {
		runTraceProbe()
	}
}

type traceLine struct {
	Sample *pathSample        `json:"sample,omitempty"`
	Hop    int                `json:"hop,omitempty"`
	Route  *model.Route       `json:"route,omitempty"`
	Change *model.RouteChange `json:"change,omitempty"`
	Info   *model.HopInfo     `json:"info,omitempty"`
}

type lineSink struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (s *lineSink) emit(l traceLine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.enc.Encode(l)
}

func (s *lineSink) Observe(x model.Sample) {
	ps := &pathSample{Target: x.Key.Target, Kind: x.Key.Kind.String(), Slot: x.Slot, RTTms: float64(x.RTT) / 1e6, Lost: x.Lost}
	if x.Lost {
		ps.Reason = x.Reason.String()
	}
	s.emit(traceLine{Sample: ps, Hop: int(x.Key.Hop)})
}
func (s *lineSink) ObserveHop(h model.HopInfo)             { s.emit(traceLine{Info: &h}) }
func (s *lineSink) ObserveRoute(r model.Route)             { s.emit(traceLine{Route: &r}) }
func (s *lineSink) ObserveRouteChange(c model.RouteChange) { s.emit(traceLine{Change: &c}) }

// runTraceProbe traces hotel every traceIv with package trace, as uid 65532
// in the client namespace, printing JSON lines on stdout.
func runTraceProbe() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tr := trace.New(trace.Options{Interval: traceIv, MaxHops: 8, Log: log,
		ReverseDNS: func(context.Context, netip.Addr) string { return "" }})
	p := &model.Profile{Name: "trace", Groups: []model.Group{{ID: "lab"}},
		Targets: []model.Target{{Name: "hotel", Host: addrHotel.String(), Group: "lab", Trace: true}}}
	err := tr.Run(context.Background(), p, &lineSink{enc: json.NewEncoder(os.Stdout)})
	log.Error("trace returned", "err", err)
	os.Exit(1)
}

type traceProc struct {
	cmd     *exec.Cmd
	mu      sync.Mutex
	samples []traceLine
	routes  []model.Route
	changes []model.RouteChange
}

type hopStat struct{ ok, lost int }

func (s hopStat) String() string {
	return fmt.Sprintf("ok=%d lost=%d (%.0f%%)", s.ok, s.lost, 100*float64(s.lost)/float64(max(s.ok+s.lost, 1)))
}

// stats returns per-hop counts for slots in [from, to].
func (tp *traceProc) stats(from, to time.Time) map[int]hopStat {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	out := map[int]hopStat{}
	for _, l := range tp.samples {
		if l.Sample.Slot.Before(from) || l.Sample.Slot.After(to) {
			continue
		}
		s := out[l.Hop]
		if l.Sample.Lost {
			s.lost++
			if l.Sample.Reason != "timeout" {
				s.ok = -1000 // flags an unexpected reason
			}
		} else {
			s.ok++
		}
		out[l.Hop] = s
	}
	return out
}

func (tp *traceProc) lastRoute() model.Route {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	if len(tp.routes) == 0 {
		return model.Route{}
	}
	return tp.routes[len(tp.routes)-1]
}

func (tp *traceProc) routeChanges() []model.RouteChange {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return slices.Clone(tp.changes)
}

func (h *harness) startTraceProbe(t *testing.T) *traceProc {
	t.Helper()
	bin := bins.test // readable by uid 65532 (see TestMain)
	logf, err := os.Create(filepath.Join(h.dir, "traceprobe.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ip", "netns", "exec", h.cli,
		"setpriv", "--reuid="+strconv.Itoa(runUID), "--regid="+strconv.Itoa(runUID), "--clear-groups",
		"--inh-caps=-all", "--no-new-privs", "--pdeathsig=KILL",
		bin, "-test.run=^$")
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", traceProbeEnv + "=1",
		"HOME=" + filepath.Join(h.dir, "home"), "TMPDIR=" + filepath.Join(h.dir, "tmp")}
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tp := &traceProc{cmd: cmd}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		logf.Close()
		if t.Failed() {
			if b, err := os.ReadFile(logf.Name()); err == nil {
				t.Logf("---- traceprobe.log (tail) ----\n%s", tail(string(b), 40))
			}
		}
	})
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var l traceLine
			if json.Unmarshal(sc.Bytes(), &l) != nil {
				continue
			}
			tp.mu.Lock()
			switch {
			case l.Sample != nil:
				tp.samples = append(tp.samples, l)
			case l.Route != nil:
				tp.routes = append(tp.routes, *l.Route)
			case l.Change != nil:
				tp.changes = append(tp.changes, *l.Change)
			}
			tp.mu.Unlock()
		}
	}()
	return tp
}

// setupAlt adds the alt namespace and hotel to h's topology. It is deleted
// when the test ends, before the rest of the topology.
func (h *harness) setupAlt(t *testing.T) {
	alt := strings.TrimSuffix(h.cli, "cli") + "alt"
	topoMu.Lock()
	out, err := exec.Command("ip", "netns", "add", alt).CombinedOutput()
	topoMu.Unlock()
	if err != nil {
		t.Fatalf("ip netns add %s: %v\n%s", alt, err, out)
	}
	t.Cleanup(func() {
		var err error
		for range 20 {
			topoMu.Lock()
			err = exec.Command("ip", "netns", "del", alt).Run()
			topoMu.Unlock()
			if err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Errorf("deleting namespace %s: %v", alt, err)
	})
	h.nsRun(t, alt, "ip", "link", "set", "lo", "up")
	pair := func(a, ifa, b, ifb, addrA, addrB string) {
		h.run(t, "ip", "-n", a, "link", "add", "name", ifa, "type", "veth", "peer", "name", ifb, "netns", b)
		h.run(t, "ip", "-n", a, "addr", "add", addrA, "dev", ifa)
		h.run(t, "ip", "-n", b, "addr", "add", addrB, "dev", ifb)
		h.run(t, "ip", "-n", a, "link", "set", ifa, "up")
		h.run(t, "ip", "-n", b, "link", "set", ifb, "up")
	}
	pair(h.isp, "i2", alt, "a0", "10.99.4.1/24", altAddr+"/24")
	pair(alt, "a1", h.inet, "n2", "10.99.5.1/24", "10.99.5.2/24")
	h.nsRun(t, alt, "sysctl", "-qw", "net.ipv4.ip_forward=1")
	h.run(t, "ip", "-n", alt, "route", "add", "default", "via", "10.99.4.1")
	h.run(t, "ip", "-n", alt, "route", "add", addrHotel.String()+"/32", "via", "10.99.5.2")
	// Via alt, the internet namespace receives the client's packets on n2
	// but answers through its default route (n0): no reverse path filter.
	h.nsRun(t, h.inet, "sysctl", "-qw", "net.ipv4.conf.all.rp_filter=0", "net.ipv4.conf.n2.rp_filter=0")
	h.run(t, "ip", "-n", h.inet, "addr", "add", addrHotel.String()+"/32", "dev", "lo")
	h.nsRun(t, h.cli, "ping", "-c1", "-W2", addrHotel.String())
}

// TestTrace: package trace, unprivileged in the client namespace, traces
// hotel every 2s. It must find the hops [gw, isp, hotel]; netem loss on the
// gw -> isp link shows as loss on hop 2 and every later hop ("loss that
// continues"); the ISP router dropping only its own ICMP time-exceeded
// messages shows as loss on hop 2 alone (like an ICMP rate limit: not real
// loss); moving hotel behind alt gives exactly one route change, at TTL 3.
func TestTrace(t *testing.T) {
	parallel(t)
	h := newHarness(t)
	h.setupAlt(t)
	tp := h.startTraceProbe(t)
	gw, isp, alt := gwAddr, edgeAddr, netip.MustParseAddr(altAddr)
	direct := []netip.Addr{gw, isp, addrHotel}

	// The route.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !slices.Equal(tp.lastRoute().Hops, direct) {
		time.Sleep(200 * time.Millisecond)
	}
	r := tp.lastRoute()
	t.Logf("route: %v", r.Hops)
	if !slices.Equal(r.Hops, direct) {
		t.Fatalf("route %v, want %v", r.Hops, direct)
	}
	checkUnprivileged(t, tp.cmd.Process.Pid)

	window := func(name string, d time.Duration, apply, revert func()) map[int]hopStat {
		t.Helper()
		if apply != nil {
			apply()
		}
		t0 := time.Now()
		time.Sleep(d)
		t1 := time.Now()
		if revert != nil {
			revert()
		}
		// A slot's round starts within the slot and ends within 1s.
		st := tp.stats(t0.Add(time.Second), t1.Add(-traceIv-time.Second))
		for hop := 1; hop <= 4; hop++ {
			if s, ok := st[hop]; ok {
				t.Logf("%s: hop %d %s", name, hop, s)
			}
		}
		b, _ := json.Marshal(st)
		h.save(t, "trace-"+name+".json", b)
		time.Sleep(traceIv + time.Second) // let the fault's rounds finish
		return st
	}
	lossOf := func(s hopStat) float64 { return float64(s.lost) / float64(max(s.ok+s.lost, 1)) }
	expectHops := func(name string, st map[int]hopStat, n int) {
		t.Helper()
		for hop := range st {
			if hop < 1 || hop > n {
				t.Errorf("%s: samples for hop %d, want hops 1..%d", name, hop, n)
			}
		}
		for hop := 1; hop <= n; hop++ {
			if s := st[hop]; s.ok < 0 {
				t.Errorf("%s: hop %d lost for a reason other than timeout", name, hop)
			} else if s.ok+s.lost < 8 {
				t.Errorf("%s: hop %d has %d samples, want one per %s", name, hop, s.ok+s.lost, traceIv)
			}
		}
	}

	// Baseline.
	st := window("baseline", 20*time.Second, nil, nil)
	expectHops("baseline", st, 3)
	for hop := 1; hop <= 3; hop++ {
		if st[hop].lost != 0 {
			t.Errorf("baseline: hop %d %s, want no loss", hop, st[hop])
		}
	}

	// Loss that continues: netem on the gw -> isp link (the gateway's g1).
	st = window("netem", 60*time.Second,
		func() { h.nsRun(t, h.gw, "tc", "qdisc", "add", "dev", "g1", "root", "netem", "loss", "40%") },
		func() { h.nsRun(t, h.gw, "tc", "qdisc", "del", "dev", "g1", "root") })
	expectHops("netem", st, 3)
	if st[1].lost != 0 {
		t.Errorf("netem after hop 1: hop 1 %s, want no loss", st[1])
	}
	for hop := 2; hop <= 3; hop++ {
		if l := lossOf(st[hop]); l < 0.15 || l > 0.7 {
			t.Errorf("netem 40%% after hop 1: hop %d %s, want about 40%% loss", hop, st[hop])
		}
	}

	// Not real loss: the ISP router drops only the time-exceeded messages it sends.
	st = window("no-time-exceeded", 30*time.Second,
		func() {
			h.nsRun(t, h.isp, "nft", "add chain inet fyt traceout { type filter hook output priority 0; policy accept; }")
			h.nsRun(t, h.isp, "nft", "add rule inet fyt traceout icmp type time-exceeded drop")
		},
		func() { h.nsRun(t, h.isp, "nft", "delete chain inet fyt traceout") })
	expectHops("no-time-exceeded", st, 3)
	if st[2].ok != 0 {
		t.Errorf("isp drops time-exceeded: hop 2 %s, want every probe lost", st[2])
	}
	for _, hop := range []int{1, 3} {
		if st[hop].lost != 0 {
			t.Errorf("isp drops time-exceeded: hop %d %s, want no loss", hop, st[hop])
		}
	}
	if c := tp.routeChanges(); len(c) != 0 {
		t.Fatalf("route changes before the route was moved: %+v", c)
	}

	// Route change: the ISP router sends hotel through alt.
	h.run(t, "ip", "-n", h.isp, "route", "add", addrHotel.String()+"/32", "via", altAddr)
	moved := time.Now()
	via := []netip.Addr{gw, isp, alt, addrHotel}
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && len(tp.routeChanges()) == 0 {
		time.Sleep(200 * time.Millisecond)
	}
	took := time.Since(moved)
	time.Sleep(10 * traceIv) // no second change; rounds on the new route
	cs := tp.routeChanges()
	b, _ := json.Marshal(cs)
	t.Logf("route changes (%s after the route was moved): %s", took.Round(100*time.Millisecond), b)
	h.save(t, "trace-route-changes.json", b)
	if len(cs) != 1 {
		t.Fatalf("%d route changes, want exactly 1", len(cs))
	}
	c := cs[0]
	if c.Target != "hotel" || c.FirstDiff != 3 || !slices.Equal(c.From, direct) || !slices.Equal(c.To, via) {
		t.Errorf("route change %+v, want %v -> %v at TTL 3", c, direct, via)
	}
	if took < traceIv {
		t.Errorf("route change after %s: want it only after 2 rounds", took)
	}
	if r := tp.lastRoute(); !slices.Equal(r.Hops, via) {
		t.Errorf("route %v, want %v", r.Hops, via)
	}
	st = tp.stats(moved.Add(3*traceIv), time.Now().Add(-traceIv-time.Second))
	for hop := 1; hop <= 4; hop++ {
		t.Logf("via alt: hop %d %s", hop, st[hop])
		if st[hop].ok < 3 || st[hop].lost != 0 {
			t.Errorf("via alt: hop %d %s, want answers and no loss", hop, st[hop])
		}
	}
}
