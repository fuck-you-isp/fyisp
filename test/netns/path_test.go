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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/probe"
	"github.com/fuck-you-isp/fyisp/internal/profile"
)

// Addresses netinfo must find from the client namespace.
var (
	gwAddr   = netip.MustParseAddr("10.99.1.1")    // the gateway namespace on the client LAN
	edgeAddr = netip.MustParseAddr("198.51.100.2") // the ISP namespace towards the gateway (public-looking)
)

// pathProbeEnv makes the test binary run runPathProbe: netinfo and the
// probe runner with the built-in path targets, as uid 65532 in the client namespace,
// printing JSON lines ({"path":...} and {"sample":...}) on stdout.
const pathProbeEnv = "FYISP_NETNS_PATHPROBE"

type pathLine struct {
	Path   *netinfo.Path `json:"path,omitempty"`
	Sample *pathSample   `json:"sample,omitempty"`
}

type pathSample struct {
	Target string    `json:"target"`
	Kind   string    `json:"kind"`
	Slot   time.Time `json:"slot"`
	RTTms  float64   `json:"rtt_ms"`
	Lost   bool      `json:"lost"`
	Reason string    `json:"reason,omitempty"`
}

func runPathProbe() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	var mu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	emit := func(l pathLine) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(l)
	}
	w := netinfo.New(netinfo.Options{Probe: addrAlpha, PollGateway: time.Second, Log: log})
	ch, _ := w.Subscribe()
	go func() {
		for p := range ch {
			emit(pathLine{Path: &p})
		}
	}()
	ctx := context.Background()
	go func() {
		if err := w.Run(ctx); err != nil {
			log.Error("netinfo", "err", err)
			os.Exit(1)
		}
	}()
	// The built-in gateway and ISP edge targets, exactly as the profile
	// package defines them (the anycast ones are not reachable in the lab).
	def, err := profile.Default()
	if err != nil {
		log.Error("profile", "err", err)
		os.Exit(1)
	}
	p := &model.Profile{Name: "path", Groups: def.Groups[:1]}
	for _, t := range def.Targets {
		if t.Layer == model.LayerGateway || t.Layer == model.LayerEdge {
			p.Targets = append(p.Targets, t)
		}
	}
	r := probe.New(probe.Options{Log: log, Path: w.Current})
	if c := r.Caps(); c.ICMP != probe.ICMPUDP {
		log.Error("want unprivileged ICMP", "caps", c)
		os.Exit(1)
	}
	err = r.Run(ctx, p, model.SinkFunc(func(s model.Sample) {
		ps := &pathSample{Target: s.Key.Target, Kind: s.Key.Kind.String(), Slot: s.Slot,
			RTTms: float64(s.RTT) / 1e6, Lost: s.Lost}
		if s.Lost {
			ps.Reason = s.Reason.String()
		}
		emit(pathLine{Sample: ps})
	}))
	log.Error("probe runner returned", "err", err)
	os.Exit(1)
}

// pathProc is a running runPathProbe.
type pathProc struct {
	cmd *exec.Cmd
	mu  sync.Mutex
	pth []netinfo.Path
	smp []pathSample
}

func (pp *pathProc) paths() []netinfo.Path {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	return append([]netinfo.Path(nil), pp.pth...)
}

// samples returns the samples of target with slots in [from, to].
func (pp *pathProc) samples(target string, from, to time.Time) (ok, lost int, reasons map[string]int, meanMs float64) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	reasons = map[string]int{}
	var sum float64
	for _, s := range pp.smp {
		if s.Target != target || s.Slot.Before(from) || s.Slot.After(to) {
			continue
		}
		if s.Lost {
			lost++
			reasons[s.Reason]++
		} else {
			ok++
			sum += s.RTTms
		}
	}
	if ok > 0 {
		meanMs = sum / float64(ok)
	}
	return ok, lost, reasons, meanMs
}

func (h *harness) startPathProbe(t *testing.T) *pathProc {
	t.Helper()
	// The test binary may live in a directory uid 65532 cannot read: run
	// the copy made in TestMain.
	bin := bins.test
	logf, err := os.Create(filepath.Join(h.dir, "pathprobe.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ip", "netns", "exec", h.cli,
		"setpriv", "--reuid="+strconv.Itoa(runUID), "--regid="+strconv.Itoa(runUID), "--clear-groups",
		"--inh-caps=-all", "--no-new-privs", "--pdeathsig=KILL",
		bin, "-test.run=^$")
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", pathProbeEnv + "=1",
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
	pp := &pathProc{cmd: cmd}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		logf.Close()
		if t.Failed() {
			if b, err := os.ReadFile(logf.Name()); err == nil {
				t.Logf("---- pathprobe.log (tail) ----\n%s", tail(string(b), 40))
			}
		}
	})
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var l pathLine
			if json.Unmarshal(sc.Bytes(), &l) != nil {
				continue
			}
			pp.mu.Lock()
			if l.Path != nil {
				pp.pth = append(pp.pth, *l.Path)
			}
			if l.Sample != nil {
				pp.smp = append(pp.smp, *l.Sample)
			}
			pp.mu.Unlock()
		}
	}()
	return pp
}

// TestNetworkPath: netinfo, running unprivileged in the client namespace,
// finds the gateway (its LAN address) and the ISP edge (the ISP router's
// public-looking address, hop 2); the gateway and edge targets of the built-in path group
// are probed over ICMP every second; netem loss between the client and the
// gateway shows up as loss on the Gateway series.
func TestNetworkPath(t *testing.T) {
	parallel(t)
	h := newHarness(t)
	pp := h.startPathProbe(t)

	// Discovery.
	var p netinfo.Path
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ps := pp.paths(); len(ps) > 0 && ps[len(ps)-1].Edge.IsValid() {
			p = ps[len(ps)-1]
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	b, _ := json.Marshal(p)
	t.Logf("netinfo path: %s", b)
	h.save(t, "path.json", b)
	if p.Gateway != gwAddr || p.Iface != "c0" || p.Edge != edgeAddr || p.EdgeHop != 2 {
		t.Fatalf("path = %+v, want gateway %s on c0, edge %s at hop 2", p, gwAddr, edgeAddr)
	}
	checkUnprivileged(t, pp.cmd.Process.Pid)

	// Baseline.
	t0 := time.Now()
	time.Sleep(15 * time.Second)
	t1 := time.Now()
	for _, name := range []string{"Gateway", "ISP edge"} {
		ok, lost, by, mean := pp.samples(name, t0.Add(time.Second), t1.Add(-3*time.Second))
		t.Logf("baseline %-8s ok=%d lost=%d %v mean=%.3fms", name, ok, lost, by, mean)
		if ok < 10 || lost != 0 {
			t.Errorf("baseline %s: ok=%d lost=%d, want >= 10 samples (ICMP every 1s) and no loss", name, ok, lost)
		}
	}

	// netem loss on the gateway's LAN side (its replies to the client).
	h.nsRun(t, h.gw, "tc", "qdisc", "add", "dev", "g0", "root", "netem", "loss", "30%")
	t0 = time.Now()
	time.Sleep(60 * time.Second)
	t1 = time.Now()
	h.nsRun(t, h.gw, "tc", "qdisc", "del", "dev", "g0", "root")
	time.Sleep(4 * time.Second)
	for _, name := range []string{"Gateway", "ISP edge"} {
		ok, lost, by, mean := pp.samples(name, t0.Add(time.Second), t1.Add(-3*time.Second))
		ratio := float64(lost) / float64(max(ok+lost, 1))
		t.Logf("netem 30%% loss on client<->gw: %-8s ok=%d lost=%d (%.1f%%) %v mean=%.3fms", name, ok, lost, 100*ratio, by, mean)
		if ok+lost < 45 {
			t.Errorf("%s: only %d samples in 56s, want one per second", name, ok+lost)
		}
		if ratio < 0.15 || ratio > 0.45 {
			t.Errorf("%s: loss %.3f under 30%% netem loss", name, ratio)
		}
		for r := range by {
			if r != "timeout" {
				t.Errorf("%s: loss reason %q, want timeout", name, r)
			}
		}
	}

	// Recovery.
	t0 = time.Now()
	time.Sleep(10 * time.Second)
	ok, lost, _, _ := pp.samples("Gateway", t0, time.Now().Add(-3*time.Second))
	t.Logf("after netem removed: Gateway ok=%d lost=%d", ok, lost)
	if lost > 0 || ok < 5 {
		t.Errorf("Gateway after recovery: ok=%d lost=%d", ok, lost)
	}
	if ps := pp.paths(); ps[len(ps)-1].Gateway != gwAddr || ps[len(ps)-1].Edge != edgeAddr {
		t.Errorf("path changed: %+v", ps[len(ps)-1])
	}
}

// checkUnprivileged: the process runs as runUID without capabilities.
func checkUnprivileged(t *testing.T, pid int) {
	t.Helper()
	// setpriv execs the test binary in place; allow a moment.
	time.Sleep(100 * time.Millisecond)
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		switch {
		case len(f) == 5 && f[0] == "Uid:":
			seen = append(seen, l)
			if f[2] != strconv.Itoa(runUID) {
				t.Errorf("path probe runs with euid %s, want %d", f[2], runUID)
			}
		case len(f) == 2 && f[0] == "CapEff:":
			seen = append(seen, l)
			if f[1] != "0000000000000000" {
				t.Errorf("path probe has capabilities %s", f[1])
			}
		case len(f) >= 2 && f[0] == "Name:":
			seen = append(seen, l)
		}
	}
	t.Logf("path probe: %s", strings.Join(seen, " | "))
}
