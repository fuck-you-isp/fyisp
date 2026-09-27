//go:build netns && linux

// Package netns is fyisp's network-fault harness. Every scenario builds its
// own throwaway topology of network namespaces
//
//	fyt-R-N-cli (fyisp) <-> fyt-R-N-gw <-> fyt-R-N-isp <-> fyt-R-N-net (targets)
//
// (R: a per-run id, N: a per-topology counter), runs the real fyisp binary
// in the client namespace as an unprivileged user, injects faults on the
// path (nft drop/reject, netem loss/delay, a withdrawn route, an
// unresolvable name, a restart) and checks what fyisp reports through
// /api/panel, /api/status and /metrics. TestNetworkPath (path_test.go)
// covers gateway/ISP edge discovery, TestVerdict (verdict_test.go) the
// verdict and the outage log.
//
// The topologies share nothing (namespaces, responders, resolver, CA, data
// directories, fyisp processes), so the scenarios run in parallel: a full
// run takes about as long as the longest scenario. FYISP_NETNS_PARALLEL=0
// (or -test.parallel=1) runs them one after the other.
//
// It needs root (namespaces, nft, tc) and Linux. Run it with
// test/netns/run.sh (builds everything in Docker), or with a Go toolchain:
//
//	sudo -E go test -tags netns -v -timeout 20m ./test/netns
//
// FYISP_BIN selects a prebuilt fyisp binary (default: go build ./cmd/fyisp).
// FYISP_NETNS_OUT, if set, receives the raw API responses and fyisp's logs,
// one subdirectory per scenario. FYISP_NETNS_RUNID (4 hex digits, set by
// run.sh) fixes the run id in the namespace names, so that a crashed run
// can be cleaned up by name.
package netns

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Topology. Namespace names are global, so each topology gets its own
// prefix fyt-<run id>-<counter>- (see newHarness); interface names live in
// the namespaces and are the same in every topology, and so are the
// addresses.
const (
	nsPrefix  = "fyt-"
	ispUplink = "i1" // the ISP namespace's interface towards the internet one: netem goes here

	runUID      = 65532 // fyisp runs as this uid/gid (like the container image)
	listenAddr  = "127.0.0.1:3000"
	probeIv     = 6 * time.Second // HTTPS/TCP interval; ICMP runs at probeIv/3
	settle      = probeIv + time.Second
	httpTimeout = 10 * time.Second

	parallelEnv = "FYISP_NETNS_PARALLEL" // "0": run the scenarios one at a time
	runIDEnv    = "FYISP_NETNS_RUNID"    // 4 hex digits; random if unset
)

// Target addresses, all on the internet namespace's loopback (TEST-NET-3).
var (
	addrAlpha     = netip.MustParseAddr("203.0.113.10") // control
	addrBravo     = netip.MustParseAddr("203.0.113.11") // nft drop
	addrCharlie   = netip.MustParseAddr("203.0.113.12") // nft reject with tcp reset
	badTLSAddr    = netip.MustParseAddr("203.0.113.13") // untrusted certificate
	dnsTargetAddr = netip.MustParseAddr("203.0.113.14") // what svc.fyisp.test resolves to
	addrGolf      = netip.MustParseAddr("203.0.113.15") // route withdrawn
	dnsAddr       = netip.MustParseAddr("203.0.113.53") // the test resolver
)

// The anycast resolvers of the built-in network path group, also put on
// the internet namespace's loopback so that TestVerdict can keep the path group enabled.
var anycastAddrs = []netip.Addr{
	netip.MustParseAddr("1.1.1.1"),
	netip.MustParseAddr("8.8.8.8"),
	netip.MustParseAddr("9.9.9.9"),
}

// The built-in network path group is left out here: it is covered by
// TestNetworkPath (path_test.go).
// Targets list their kinds explicitly: without kinds a target gets only
// TCP (model.DefaultKinds), and these scenarios exercise all three probes.
const profileYAML = `name: netns-lab
version: "1"
path: false
groups:
  - {id: lab, title: Lab}
targets:
  - {name: alpha,   host: 203.0.113.10,   group: lab, interval: 6s, kinds: [https, tcp, icmp]}
  - {name: bravo,   host: 203.0.113.11,   group: lab, interval: 6s, kinds: [https, tcp, icmp]}
  - {name: charlie, host: 203.0.113.12,   group: lab, interval: 6s, kinds: [https, tcp, icmp]}
  - {name: delta,   host: svc.fyisp.test, group: lab, interval: 6s, kinds: [https, tcp, icmp]}
  - {name: echo,    host: x.invalid,      group: lab, interval: 6s, kinds: [tcp, icmp]}
  - {name: foxtrot, host: 203.0.113.13,   group: lab, interval: 6s, kinds: [https]}
  - {name: golf,    host: 203.0.113.15,   group: lab, interval: 6s, kinds: [tcp, https]}
`

// healthy are the targets that answer unless a fault is injected.
var (
	healthy  = []string{"alpha", "bravo", "charlie", "delta", "golf"}
	allKinds = []string{"icmp", "tcp", "https"}
)

// kindsFor lists a healthy target's kinds. golf has no ICMP: the kernel
// rate-limits ICMP errors per source host (about 1/s), and golf's own echo
// requests would use up the "host unreachable" budget that its TCP SYNs need
// in scenario h.
func kindsFor(name string) []string {
	if name == "golf" {
		return []string{"tcp", "https"}
	}
	return allKinds
}

func TestMain(m *testing.M) {
	if dir := os.Getenv(responderEnv); dir != "" {
		runResponder(dir) // never returns
	}
	if os.Getenv(pathProbeEnv) != "" {
		runPathProbe() // never returns
	}
	if os.Geteuid() == 0 {
		bins.err = bins.prepare()
	}
	code := m.Run()
	if bins.dir != "" {
		_ = os.RemoveAll(bins.dir)
	}
	// Every topology deletes itself; anything left under this run's prefix
	// is a harness bug.
	if left := leftNamespaces(); len(left) > 0 {
		fmt.Fprintf(os.Stderr, "netns: namespaces left behind: %s\n", strings.Join(left, " "))
		code = 1
	}
	os.Exit(code)
}

// bins are the executables the scenarios run as uid 65532, prepared once
// before any scenario starts: writing an executable while other goroutines
// fork lets a child inherit the write descriptor, and exec then fails with
// "text file busy".
type binaries struct {
	dir   string // 0755, readable by uid 65532
	fyisp string // FYISP_BIN, or go build ./cmd/fyisp
	test  string // a copy of this test binary (its own directory may be private)
	err   error
}

var bins binaries

func (b *binaries) prepare() error {
	dir, err := os.MkdirTemp("", "fyisp-netns-bin-")
	if err != nil {
		return err
	}
	b.dir = dir
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	b.fyisp = filepath.Join(dir, "fyisp")
	if src := os.Getenv("FYISP_BIN"); src != "" {
		if err := copyExe(src, b.fyisp); err != nil {
			return err
		}
	} else {
		cmd := exec.Command("go", "build", "-o", b.fyisp, "./cmd/fyisp")
		cmd.Dir = "../.."
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go build: %v\n%s", err, out)
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	b.test = filepath.Join(dir, "netns.test")
	return copyExe(self, b.test)
}

func copyExe(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o755)
}

// parallel marks a scenario as parallel unless FYISP_NETNS_PARALLEL=0.
func parallel(t *testing.T) {
	if os.Getenv(parallelEnv) != "0" {
		t.Parallel()
	}
}

// ---------------------------------------------------------------------------
// The test. Each scenario runs on its own topology and fyisp.

// startLab starts fyisp with the lab profile on a fresh topology and waits
// until every target has been probed once.
func startLab(t *testing.T) (*harness, *fyisp) {
	h := newHarness(t)
	fy := h.startFyisp(t, "--ephemeral")
	h.waitReady(t, fy)
	h.checkStatus(t, fy)
	h.stopAtEnd(t, fy)
	return h, fy
}

// stopAtEnd stops fy with SIGTERM once the scenario is done and expects a
// clean exit (unless the scenario already failed: then it is just killed).
func (h *harness) stopAtEnd(t *testing.T, fy *fyisp) {
	t.Cleanup(func() {
		if !t.Failed() {
			h.stopFyisp(t, fy, 0)
		}
	})
}

func TestNetworkFaults(t *testing.T) {
	parallel(t)

	// a) baseline, f) DNS
	t.Run("a-baseline", func(t *testing.T) {
		parallel(t)
		h, _ := startLab(t)
		base := h.phase(t, "a-baseline", 36*time.Second, nil, nil)
		for _, n := range healthy {
			for _, k := range kindsFor(n) {
				expectOK(t, base, n+"/"+k)
			}
		}
		expectLost(t, base, "foxtrot/https", "tls")
		t.Run("f-dns", func(t *testing.T) {
			expectLost(t, base, "echo/tcp", "dns")
			expectLost(t, base, "echo/icmp", "dns")
			for _, k := range allKinds {
				expectOK(t, base, "delta/"+k) // resolved through the test resolver
			}
		})
	})

	// b) nft drop
	t.Run("b-nft-drop", func(t *testing.T) {
		parallel(t)
		h, _ := startLab(t)
		var mt metricsText
		var restored time.Time
		p := h.phase(t, "b-nft-drop", 36*time.Second,
			func() { h.nft(t, "add rule inet fyt fault ip daddr 203.0.113.11 drop") },
			func() { mt = h.metrics(t); h.nft(t, "flush chain inet fyt fault"); restored = time.Now() })
		for _, k := range allKinds {
			expectLost(t, p, "bravo/"+k, "timeout")
			expectAllBucketsEmpty(t, p, "bravo/"+k)
		}
		for _, n := range []string{"alpha", "charlie", "delta", "golf"} {
			for _, k := range kindsFor(n) {
				expectOK(t, p, n+"/"+k)
			}
		}
		expectMetric(t, mt, `ping_loss_percent{name="bravo",target="203.0.113.11",target_ip=""}`, 1)
		expectMetric(t, mt, `ping_status{name="bravo",target="203.0.113.11",target_ip=""}`, 0)
		expectMetric(t, mt, `ping_loss_percent{name="alpha",target="203.0.113.10",target_ip=""}`, 0)
		if v := mt[`fyisp_probe_lost_total{kind="icmp",name="bravo",reason="timeout"}`]; v < 5 {
			t.Errorf("fyisp_probe_lost_total icmp/bravo/timeout = %v, want >= 5", v)
		}

		// Recovery: once the path is back, every kind recovers at once,
		// apart from a probe in flight at the switch. HTTPS drops its
		// connection after a failure, so it does not keep using one whose
		// TCP retransmission timer backed off during the outage.
		time.Sleep(30 * time.Second)
		r := h.panel(t, "b-recovery", restored, time.Now().Add(-settle))
		for _, k := range allKinds {
			a := r.get("bravo/" + k)
			t.Logf("bravo/%s in the %s after the drop was removed: %s",
				k, time.Since(restored).Round(time.Second)-settle, a)
			if a.Lost > 1 {
				t.Errorf("bravo/%s: %d probes lost after the path was restored, want <= 1", k, a.Lost)
			}
		}
	})

	// c) nft reject with tcp reset
	t.Run("c-nft-reject", func(t *testing.T) {
		parallel(t)
		h, _ := startLab(t)
		p := h.phase(t, "c-nft-reject", 36*time.Second,
			func() { h.nft(t, "add rule inet fyt fault ip daddr 203.0.113.12 tcp dport 443 reject with tcp reset") },
			func() { h.nft(t, "flush chain inet fyt fault") })
		expectLost(t, p, "charlie/tcp", "refused")
		expectLost(t, p, "charlie/https", "refused", "reset")
		expectOK(t, p, "charlie/icmp") // only TCP/443 is rejected
		for _, k := range allKinds {
			expectOK(t, p, "alpha/"+k)
		}
	})

	// h) route withdrawn: the ISP router answers "host unreachable"
	t.Run("h-route-unreachable", func(t *testing.T) {
		parallel(t)
		h, _ := startLab(t)
		p := h.phase(t, "h-route-unreachable", 36*time.Second,
			func() { h.nsRun(t, h.isp, "ip", "route", "add", "unreachable", "203.0.113.15/32") },
			func() { h.nsRun(t, h.isp, "ip", "route", "del", "unreachable", "203.0.113.15/32") })
		// A probe in flight when the route is withdrawn may time out.
		expectLost(t, p, "golf/tcp", "unreachable", "timeout")
		// The kept-alive connection first times out, new ones are unreachable.
		expectLost(t, p, "golf/https", "unreachable", "timeout")
		for _, k := range allKinds {
			expectOK(t, p, "alpha/"+k)
		}
	})

	// d) netem loss 20%
	t.Run("d-netem-loss", func(t *testing.T) {
		parallel(t)
		h, _ := startLab(t)
		p := h.phase(t, "d-netem-loss", 150*time.Second,
			func() { h.nsRun(t, h.isp, "tc", "qdisc", "add", "dev", ispUplink, "root", "netem", "loss", "20%") },
			func() { h.nsRun(t, h.isp, "tc", "qdisc", "del", "dev", ispUplink, "root") })
		for _, k := range allKinds {
			var n, lost int
			for _, name := range healthy {
				a := p.get(name + "/" + k)
				n += a.N
				lost += a.Lost
				if a.Gap != 0 {
					t.Errorf("%s/%s: %d not-measured slots under netem loss, want 0", name, k, a.Gap)
				}
				for r := range a.By {
					if r != "timeout" && r != "reset" {
						t.Errorf("%s/%s: loss reason %q under netem loss", name, k, r)
					}
				}
			}
			ratio := float64(lost) / float64(n+lost)
			t.Logf("netem loss 20%%: %s lost %d of %d = %.1f%%", k, lost, n+lost, 100*ratio)
			switch k {
			case "icmp": // one echo per probe: loss maps 1:1
				if ratio < 0.12 || ratio > 0.30 {
					t.Errorf("icmp loss ratio %.3f, want 0.20 +- 0.08", ratio)
				}
			case "tcp": // a lost SYN is retransmitted after 1s, within the 3s
				// timeout, so only back-to-back losses (~4%) count as loss;
				// the rest shows up as ~1s extra latency.
				if ratio > 0.12 {
					t.Errorf("tcp loss ratio %.3f, want well below the packet loss (SYN retransmit)", ratio)
				}
			case "https": // kept-alive connection: TCP retransmits within the 5s timeout
				if ratio > 0.20 {
					t.Errorf("https loss ratio %.3f, want well below the packet loss", ratio)
				}
			}
		}
	})

	// e) netem delay 80ms, against a baseline taken on the same topology
	// just before (so under the same load).
	t.Run("e-netem-delay", func(t *testing.T) {
		parallel(t)
		h, _ := startLab(t)
		base := h.phase(t, "e-baseline", 36*time.Second, nil, nil)
		p := h.phase(t, "e-netem-delay", 36*time.Second,
			func() { h.nsRun(t, h.isp, "tc", "qdisc", "add", "dev", ispUplink, "root", "netem", "delay", "80ms") },
			func() { h.nsRun(t, h.isp, "tc", "qdisc", "del", "dev", ispUplink, "root") })
		for _, name := range healthy {
			for _, k := range kindsFor(name) {
				key := name + "/" + k
				expectOK(t, base, key)
				expectOK(t, p, key)
				d := p.get(key).Mean() - base.get(key).Mean()
				t.Logf("%-14s baseline %7.3f ms  delayed %7.3f ms  (+%.1f ms)", key, base.get(key).Mean(), p.get(key).Mean(), d)
				if d < 70 || d > 120 {
					t.Errorf("%s: RTT rose by %.1f ms, want ~80", key, d)
				}
			}
		}
	})

	// g) restart with a persistent data directory
	t.Run("g-restart", func(t *testing.T) {
		parallel(t)
		newHarness(t).restart(t)
	})
}

// restart runs fyisp with --data-dir, stops it for a while and starts it
// again: the downtime must show as "not measured", never as loss.
func (h *harness) restart(t *testing.T) {
	data := filepath.Join(h.dir, "data")
	h.mkdirOwned(t, data, 0o700)
	fy := h.startFyisp(t, "--data-dir", data)
	h.waitReady(t, fy)
	h.checkStatus(t, fy)
	t0 := time.Now()
	time.Sleep(30 * time.Second)

	// A second instance on the same directory must refuse to start (exit 3).
	second := h.startFyispOn(t, "127.0.0.1:3001", "--data-dir", data)
	select {
	case <-second.done:
		if second.code != 3 {
			t.Errorf("second instance exited with %d, want 3", second.code)
		} else {
			t.Logf("second instance on the same --data-dir exited with code 3")
		}
	case <-time.After(15 * time.Second):
		t.Errorf("second instance on the same --data-dir is still running")
		h.stopFyisp(t, second, -1)
	}

	tStop := time.Now()
	h.stopFyisp(t, fy, 0)
	time.Sleep(30 * time.Second)
	tStart := time.Now()
	fy2 := h.startFyisp(t, "--data-dir", data)
	h.waitReady(t, fy2)
	time.Sleep(30 * time.Second)
	t1 := time.Now().Add(-settle)

	before := h.panel(t, "g-before", t0.Add(time.Second), tStop.Add(-settle))
	// Slots are labelled with their scheduled start, which precedes the
	// first probe by up to one interval (the per-target phase): end the
	// "stopped" window one interval before the restart.
	down := h.panel(t, "g-down", tStop.Add(settle), tStart.Add(-probeIv))
	after := h.panel(t, "g-after", tStart.Add(settle), t1)
	whole := h.panel(t, "g-whole", t0, t1)
	for _, name := range healthy {
		for _, k := range kindsFor(name) {
			key := name + "/" + k
			expectOK(t, before, key) // the first run's data survived the restart
			expectOK(t, after, key)
			d := down.get(key)
			if d.N != 0 || d.Lost != 0 {
				t.Errorf("%s while stopped: n=%d lost=%d, want 0/0", key, d.N, d.Lost)
			}
			if d.Gap == 0 {
				t.Errorf("%s while stopped: no not-measured slots reported", key)
			}
			if w := whole.get(key); w.Lost != 0 || w.Gap == 0 {
				t.Errorf("%s over the whole run: lost=%d gap=%d, want lost 0 and gap > 0", key, w.Lost, w.Gap)
			}
		}
	}
	h.stopFyisp(t, fy2, 0)
}

// ---------------------------------------------------------------------------
// Harness: topology, processes.

type harness struct {
	dir     string // 0755 work dir: binary, profile, certificates, resolv.conf
	bin     string
	profile string // the --config file fyisp runs with (default profile.yml)
	out     string // evidence directory, may be ""
	client  *http.Client
	started time.Time
	runs    int // fyisp processes started (names their logs)

	// This topology's namespaces: client (fyisp) <-> gw <-> isp <-> inet.
	cli, gw, isp, inet string
}

func (h *harness) namespaces() []string { return []string{h.cli, h.gw, h.isp, h.inet} }

// Namespace names: fyt-<run id>-<n>-<role>, at most 15 characters.
var (
	runID    = sync.OnceValue(newRunID)
	topoMu   sync.Mutex // guards topoN; also serialises `ip netns add/del`
	topoN    int
	runIDHex = regexp.MustCompile(`^[0-9a-f]{4}$`)
)

func newRunID() string {
	if id := os.Getenv(runIDEnv); runIDHex.MatchString(id) {
		return id
	}
	var b [2]byte
	_, _ = crand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func runPrefix() string { return nsPrefix + runID() + "-" }

// leftNamespaces lists this run's namespaces that still exist.
func leftNamespaces() []string {
	ents, _ := os.ReadDir("/run/netns")
	var left []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), runPrefix()) {
			left = append(left, e.Name())
		}
	}
	return left
}

func newHarness(t *testing.T) *harness {
	if os.Geteuid() != 0 {
		t.Skip("needs root: run test/netns/run.sh, or sudo -E go test -tags netns ./test/netns")
	}
	for _, tool := range []string{"ip", "nft", "tc", "setpriv", "sh", "mount"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s not found in PATH", tool)
		}
	}
	dir, err := os.MkdirTemp("", "fyisp-netns-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	h := &harness{dir: dir, started: time.Now()}
	topoMu.Lock()
	topoN++
	pre := fmt.Sprintf("%s%02x-", runPrefix(), topoN)
	topoMu.Unlock()
	h.cli, h.gw, h.isp, h.inet = pre+"cli", pre+"gw", pre+"isp", pre+"net"
	t.Logf("topology %s{cli,gw,isp,net}, work dir %s", pre, dir)
	if out := os.Getenv("FYISP_NETNS_OUT"); out != "" {
		h.out = filepath.Join(out, strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	}
	t.Cleanup(func() {
		if t.Failed() {
			for _, f := range []string{"fyisp-1.log", "fyisp-2.log", "responder.log"} {
				if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
					t.Logf("---- %s (tail) ----\n%s", f, tail(string(b), 40))
				}
			}
		}
		if h.out != "" {
			logs, _ := filepath.Glob(filepath.Join(dir, "*.log"))
			for _, l := range logs {
				if b, err := os.ReadFile(l); err == nil {
					dst := filepath.Join(h.out, filepath.Base(l))
					if os.WriteFile(dst, b, 0o644) == nil {
						giveBack(dst)
					}
				}
			}
		}
		_ = os.RemoveAll(dir)
	})
	if h.out != "" {
		if err := os.MkdirAll(h.out, 0o755); err != nil {
			t.Fatal(err)
		}
		giveBack(h.out)
	}

	if bins.err != nil {
		t.Fatal(bins.err)
	}
	h.bin = bins.fyisp
	if out, err := exec.Command(h.bin, "--version").CombinedOutput(); err != nil {
		t.Fatalf("fyisp --version: %v\n%s", err, out)
	} else {
		t.Logf("binary: %s", strings.TrimSpace(string(out)))
	}

	goodIPs := []netip.Addr{addrAlpha, addrBravo, addrCharlie, dnsTargetAddr, addrGolf}
	if err := writeCerts(dir, goodIPs, badTLSAddr); err != nil {
		t.Fatal(err)
	}
	h.profile = filepath.Join(dir, "profile.yml")
	mustWrite(t, h.profile, profileYAML)
	// Three 1s attempts: under netem loss a lookup rarely fails, and two
	// failed lookups in a row would turn delta's probes into DNS losses.
	mustWrite(t, filepath.Join(dir, "resolv.conf"), "nameserver "+dnsAddr.String()+"\noptions timeout:1 attempts:3\n")
	h.mkdirOwned(t, filepath.Join(dir, "home"), 0o700)
	h.mkdirOwned(t, filepath.Join(dir, "tmp"), 0o700)

	t.Cleanup(func() { h.teardown(t) })
	h.setupTopology(t)
	h.startResponder(t)

	h.client = &http.Client{
		Timeout:   httpTimeout,
		Transport: &http.Transport{DialContext: dialIn(h.cli), DisableKeepAlives: true},
	}
	return h
}

func (h *harness) setupTopology(t *testing.T) {
	for _, ns := range h.namespaces() {
		// Concurrent `ip netns add` can race on setting up /run/netns.
		topoMu.Lock()
		out, err := exec.Command("ip", "netns", "add", ns).CombinedOutput()
		topoMu.Unlock()
		if err != nil {
			t.Fatalf("ip netns add %s: %v\n%s", ns, err, out)
		}
		h.nsRun(t, ns, "ip", "link", "set", "lo", "up")
	}
	// veth pairs are created inside the namespaces: the host's own network
	// namespace is never touched.
	pair := func(a, ifa, b, ifb, addrA, addrB string) {
		h.run(t, "ip", "-n", a, "link", "add", "name", ifa, "type", "veth", "peer", "name", ifb, "netns", b)
		h.run(t, "ip", "-n", a, "addr", "add", addrA, "dev", ifa)
		h.run(t, "ip", "-n", b, "addr", "add", addrB, "dev", ifb)
		h.run(t, "ip", "-n", a, "link", "set", ifa, "up")
		h.run(t, "ip", "-n", b, "link", "set", ifb, "up")
	}
	// The gw <-> isp link is numbered from TEST-NET-2, a public-looking
	// range: like a real ISP's access router, the ISP namespace answers the client's
	// TTL-limited probes from a public address, which netinfo takes as the
	// ISP edge (hop 2; hop 1 is the gateway's LAN address).
	pair(h.cli, "c0", h.gw, "g0", "10.99.1.2/24", gwAddr.String()+"/24")
	pair(h.gw, "g1", h.isp, "i0", "198.51.100.1/24", edgeAddr.String()+"/24")
	pair(h.isp, ispUplink, h.inet, "n0", "10.99.3.1/24", "10.99.3.2/24")

	h.run(t, "ip", "-n", h.cli, "route", "add", "default", "via", gwAddr.String())
	h.run(t, "ip", "-n", h.gw, "route", "add", "default", "via", edgeAddr.String())
	h.run(t, "ip", "-n", h.isp, "route", "add", "10.99.1.0/24", "via", "198.51.100.1")
	h.run(t, "ip", "-n", h.isp, "route", "add", "default", "via", "10.99.3.2")
	h.run(t, "ip", "-n", h.inet, "route", "add", "default", "via", "10.99.3.1")
	for _, ns := range []string{h.gw, h.isp} {
		h.nsRun(t, ns, "sysctl", "-qw", "net.ipv4.ip_forward=1")
	}
	// Targets live on the internet namespace's loopback; every namespace's default route
	// leads there through the chain.
	for _, a := range append([]netip.Addr{addrAlpha, addrBravo, addrCharlie, badTLSAddr, dnsTargetAddr, addrGolf, dnsAddr}, anycastAddrs...) {
		h.run(t, "ip", "-n", h.inet, "addr", "add", a.String()+"/32", "dev", "lo")
	}
	// Unprivileged ICMP (ping sockets) is off in a new namespace
	// (ping_group_range "1 0"); allow every group, as most distributions do.
	h.nsRun(t, h.cli, "sysctl", "-qw", "net.ipv4.ping_group_range=0 2147483647")
	// The fault-injection table on the ISP router.
	h.nsRun(t, h.isp, "nft", "add table inet fyt")
	h.nsRun(t, h.isp, "nft", "add chain inet fyt fault { type filter hook forward priority 0; policy accept; }")

	h.nsRun(t, h.cli, "ping", "-c1", "-W2", addrAlpha.String())
}

func (h *harness) startResponder(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logf, err := os.Create(filepath.Join(h.dir, "responder.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ip", "netns", "exec", h.inet, self, "-test.run=^$")
	cmd.Env = append(os.Environ(), responderEnv+"="+h.dir)
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); logf.Close() })
	ready := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if sc.Text() == "responder ready" {
				ready <- true
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("responder exited; see responder.log")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("responder did not start")
	}
}

// teardown kills whatever runs in the topology's namespaces and deletes
// them (and with them their veths, routes, qdiscs and nft rules).
func (h *harness) teardown(t *testing.T) {
	for _, ns := range h.namespaces() {
		if _, err := os.Stat("/run/netns/" + ns); err != nil {
			continue
		}
		if out, err := exec.Command("ip", "netns", "pids", ns).Output(); err == nil {
			for _, f := range strings.Fields(string(out)) {
				if pid, err := strconv.Atoi(f); err == nil {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
		var err error
		for range 20 { // processes may take a moment to go away
			topoMu.Lock()
			err = exec.Command("ip", "netns", "del", ns).Run()
			topoMu.Unlock()
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Errorf("deleting namespace %s: %v", ns, err)
		}
	}
}

func (h *harness) run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (h *harness) nsRun(t *testing.T, ns string, args ...string) string {
	t.Helper()
	return h.run(t, "ip", append([]string{"netns", "exec", ns}, args...)...)
}

func (h *harness) nft(t *testing.T, rule string) {
	t.Helper()
	h.nsRun(t, h.isp, "nft", rule)
}

func (h *harness) mkdirOwned(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(dir, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, runUID, runUID); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
}

// fyisp is one fyisp process.
type fyisp struct {
	cmd  *exec.Cmd
	addr string
	done chan struct{}
	code int
}

func (h *harness) startFyisp(t *testing.T, args ...string) *fyisp {
	return h.startFyispOn(t, listenAddr, args...)
}

// startFyispOn runs fyisp in the client namespace as uid/gid 65532 with no
// capabilities. It gets its own mount namespace from `ip netns exec`, where
// the test resolv.conf is bind-mounted (nothing changes on the host), and
// trusts the test CA through SSL_CERT_FILE.
func (h *harness) startFyispOn(t *testing.T, addr string, args ...string) *fyisp {
	t.Helper()
	h.runs++
	logf, err := os.Create(filepath.Join(h.dir, fmt.Sprintf("fyisp-%d.log", h.runs)))
	if err != nil {
		t.Fatal(err)
	}
	argv := []string{"netns", "exec", h.cli,
		"sh", "-c", `mount --bind "$0" /etc/resolv.conf && exec "$@"`, filepath.Join(h.dir, "resolv.conf"),
		"setpriv", "--reuid=" + strconv.Itoa(runUID), "--regid=" + strconv.Itoa(runUID), "--clear-groups",
		"--inh-caps=-all", "--no-new-privs", "--pdeathsig=KILL",
		h.bin, "--listen", addr, "--config", h.profile,
		"--log-format", "json", "--open-browser=false"}
	argv = append(argv, args...)
	cmd := exec.Command("ip", argv...)
	cmd.Env = []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + filepath.Join(h.dir, "home"),
		"TMPDIR=" + filepath.Join(h.dir, "tmp"),
		"SSL_CERT_FILE=" + filepath.Join(h.dir, "ca.pem"),
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	fy := &fyisp{cmd: cmd, addr: addr, done: make(chan struct{}), code: -1}
	go func() {
		_ = cmd.Wait()
		fy.code = cmd.ProcessState.ExitCode()
		logf.Close()
		close(fy.done)
	}()
	t.Cleanup(func() {
		select {
		case <-fy.done:
		default:
			_ = cmd.Process.Kill()
			<-fy.done
		}
	})
	t.Logf("started fyisp #%d (pid %d): %s", h.runs, cmd.Process.Pid, strings.Join(args, " "))
	return fy
}

// stopFyisp sends SIGTERM and waits; wantCode < 0 skips the exit code check.
func (h *harness) stopFyisp(t *testing.T, fy *fyisp, wantCode int) {
	t.Helper()
	_ = fy.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-fy.done:
	case <-time.After(20 * time.Second):
		_ = fy.cmd.Process.Kill()
		<-fy.done
		t.Errorf("fyisp did not stop within 20s of SIGTERM")
		return
	}
	if wantCode >= 0 && fy.code != wantCode {
		t.Errorf("fyisp exited with %d, want %d", fy.code, wantCode)
	}
}

// dialIn dials from inside a network namespace: the socket is created on a
// thread switched into ns and keeps that namespace for its lifetime.
func dialIn(ns string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		runtime.LockOSThread()
		orig, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			runtime.UnlockOSThread()
			return nil, err
		}
		defer orig.Close()
		target, err := os.Open("/run/netns/" + ns)
		if err != nil {
			runtime.UnlockOSThread()
			return nil, err
		}
		defer target.Close()
		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			return nil, err
		}
		var d net.Dialer
		c, err := d.DialContext(ctx, network, addr)
		if unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET) == nil {
			runtime.UnlockOSThread() // else the thread dies with this goroutine
		}
		return c, err
	}
}

// ---------------------------------------------------------------------------
// API access.

func (h *harness) get(t *testing.T, path string) []byte {
	t.Helper()
	resp, err := h.client.Get("http://" + listenAddr + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s: %s", path, resp.Status, b)
	}
	return b
}

type statusJSON struct {
	Version string `json:"version"`
	Caps    struct {
		ICMP  string `json:"icmp"`
		TCP   bool   `json:"tcp"`
		HTTPS bool   `json:"https"`
	} `json:"caps"`
	Targets int `json:"targets"`
	Ready   int `json:"ready"`
}

func (h *harness) status(t *testing.T) (statusJSON, []byte, error) {
	var s statusJSON
	resp, err := h.client.Get("http://" + listenAddr + "/api/status")
	if err != nil {
		return s, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return s, nil, err
	}
	return s, b, json.Unmarshal(b, &s)
}

func (h *harness) waitReady(t *testing.T, fy *fyisp) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		select {
		case <-fy.done:
			t.Fatalf("fyisp exited early with code %d", fy.code)
		default:
		}
		s, _, err := h.status(t)
		if err == nil && s.Targets > 0 && s.Ready == s.Targets {
			return
		}
		last = err
		if err == nil {
			last = fmt.Errorf("ready %d of %d", s.Ready, s.Targets)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("fyisp not ready after 40s: %v", last)
}

// checkStatus: unprivileged ICMP works, the profile loaded, fyisp runs as
// runUID.
func (h *harness) checkStatus(t *testing.T, fy *fyisp) {
	s, raw, err := h.status(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("/api/status: %s", strings.TrimSpace(string(raw)))
	h.save(t, "status.json", raw)
	if s.Caps.ICMP != "udp" || !s.Caps.TCP || !s.Caps.HTTPS {
		t.Errorf("caps = %+v, want icmp=udp (unprivileged ping socket), tcp, https", s.Caps)
	}
	if s.Targets != 7 {
		t.Errorf("targets = %d, want 7", s.Targets)
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", fy.cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "Name:") || strings.HasPrefix(l, "Uid:") || strings.HasPrefix(l, "Gid:") || strings.HasPrefix(l, "CapEff:") {
			t.Logf("fyisp %s", strings.Join(strings.Fields(l), " "))
		}
		if f := strings.Fields(l); len(f) == 5 && f[0] == "Uid:" && f[2] != strconv.Itoa(runUID) {
			t.Errorf("fyisp runs with euid %s, want %d", f[2], runUID)
		}
		if f := strings.Fields(l); len(f) == 2 && f[0] == "CapEff:" && f[1] != "0000000000000000" {
			t.Errorf("fyisp has effective capabilities %s", f[1])
		}
	}
}

type metricsText map[string]float64

func (h *harness) metrics(t *testing.T) metricsText {
	b := h.get(t, "/metrics")
	h.save(t, fmt.Sprintf("metrics-%d.txt", time.Now().Unix()), b)
	m := metricsText{}
	for _, l := range strings.Split(string(b), "\n") {
		if l == "" || l[0] == '#' {
			continue
		}
		i := strings.LastIndexByte(l, ' ')
		if i < 0 {
			continue
		}
		if v, err := strconv.ParseFloat(l[i+1:], 64); err == nil {
			m[l[:i]] = v
		}
	}
	return m
}

func expectMetric(t *testing.T, m metricsText, series string, want float64) {
	t.Helper()
	v, ok := m[series]
	switch {
	case !ok:
		t.Errorf("/metrics: %s missing", series)
	case v != want:
		t.Errorf("/metrics: %s = %v, want %v", series, v, want)
	default:
		t.Logf("/metrics: %s %v", series, v)
	}
}

// panelJSON is the part of /api/panel the harness reads.
type panelJSON struct {
	Start  int64 `json:"start"`
	Step   int64 `json:"step"`
	Len    int   `json:"len"`
	Series []struct {
		Target string           `json:"target"`
		Kind   string           `json:"kind"`
		Mean   []*float64       `json:"mean"`
		N      []int            `json:"n"`
		Lost   []int            `json:"lost"`
		Gap    []int            `json:"gap"`
		LostBy map[string][]int `json:"lost_by"`
	} `json:"series"`
}

// agg sums one series over a window.
type agg struct {
	N, Lost, Gap  int
	By            map[string]int
	sumMs         float64
	Buckets       int
	FilledBuckets int // buckets with a mean (at least one success)
}

func (a *agg) Mean() float64 {
	if a.N == 0 {
		return math.NaN()
	}
	return a.sumMs / float64(a.N)
}

func (a *agg) String() string {
	var by []string
	for r, n := range a.By {
		by = append(by, fmt.Sprintf("%s:%d", r, n))
	}
	sort.Strings(by)
	return fmt.Sprintf("n=%-3d lost=%-3d gap=%-3d lost_by={%s} mean=%.3fms buckets=%d/%d",
		a.N, a.Lost, a.Gap, strings.Join(by, ","), a.Mean(), a.FilledBuckets, a.Buckets)
}

type window struct {
	name string
	m    map[string]*agg
}

func (w *window) get(key string) *agg {
	if a := w.m[key]; a != nil {
		return a
	}
	return &agg{By: map[string]int{}}
}

// panel queries /api/panel for [from, to] and sums every series.
func (h *harness) panel(t *testing.T, name string, from, to time.Time) *window {
	t.Helper()
	q := fmt.Sprintf("/api/panel?group=lab&from=%d&to=%d&points=10", from.UnixMilli(), to.UnixMilli())
	b := h.get(t, q)
	h.save(t, "panel-"+name+".json", b)
	var p panelJSON
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	w := &window{name: name, m: map[string]*agg{}}
	for _, s := range p.Series {
		a := &agg{By: map[string]int{}, Buckets: p.Len}
		for i := range p.Len {
			n := at(s.N, i)
			a.N += n
			a.Lost += at(s.Lost, i)
			a.Gap += at(s.Gap, i)
			if i < len(s.Mean) && s.Mean[i] != nil {
				a.sumMs += *s.Mean[i] * float64(n)
				a.FilledBuckets++
			}
		}
		for r, v := range s.LostBy {
			for _, x := range v {
				a.By[r] += x
			}
		}
		w.m[s.Target+"/"+s.Kind] = a
	}
	keys := make([]string, 0, len(w.m))
	for k := range w.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: GET %s (step %dms, %d buckets)\n", name, q, p.Step, p.Len)
	for _, k := range keys {
		fmt.Fprintf(&sb, "    %-14s %s\n", k, w.m[k])
	}
	t.Log(sb.String())
	return w
}

// phase applies a fault, waits d, captures anything needed (revert runs
// while the fault is still active, then removes it), lets in-flight probes
// finish, and returns the panel for the part of the window in which every
// scheduled probe ran under the fault.
func (h *harness) phase(t *testing.T, name string, d time.Duration, apply, revert func()) *window {
	t.Helper()
	t0 := time.Now()
	if apply != nil {
		apply()
	}
	time.Sleep(d)
	t1 := time.Now()
	if revert != nil {
		revert()
	}
	time.Sleep(settle) // in-flight probes (HTTPS timeout 5s) land
	// A slot starting at s fires in [s, s+iv) and takes up to 5s: slots in
	// [t0+1s, t1-iv-5s] ran entirely under the fault.
	return h.panel(t, name, t0.Add(time.Second), t1.Add(-probeIv-5*time.Second))
}

func (h *harness) save(t *testing.T, name string, b []byte) {
	if h.out == "" {
		return
	}
	dst := filepath.Join(h.out, name)
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Logf("saving %s: %v", name, err)
	}
	giveBack(dst)
}

// giveBack hands an evidence file to the user who ran sudo.
func giveBack(path string) {
	uid, err1 := strconv.Atoi(os.Getenv("SUDO_UID"))
	gid, err2 := strconv.Atoi(os.Getenv("SUDO_GID"))
	if err1 == nil && err2 == nil {
		_ = os.Chown(path, uid, gid)
	}
}

// ---------------------------------------------------------------------------
// Expectations.

func expectOK(t *testing.T, w *window, key string) {
	t.Helper()
	a := w.get(key)
	if a.N == 0 || a.Lost != 0 || a.Gap != 0 {
		t.Errorf("%s %s: want all samples OK and no gaps, got %s", w.name, key, a)
	}
}

// expectLost: every sample lost, only for the given reasons, no
// "not measured" slots.
func expectLost(t *testing.T, w *window, key string, reasons ...string) {
	t.Helper()
	a := w.get(key)
	if a.N != 0 || a.Lost == 0 || a.Gap != 0 {
		t.Errorf("%s %s: want every sample lost (no gaps), got %s", w.name, key, a)
	}
	for r, n := range a.By {
		ok := false
		for _, x := range reasons {
			ok = ok || r == x
		}
		if !ok && n > 0 {
			t.Errorf("%s %s: %d lost with reason %q, want %v", w.name, key, n, r, reasons)
		}
	}
}

// expectAllBucketsEmpty: the RTT line has a hole (null mean) in every bucket.
func expectAllBucketsEmpty(t *testing.T, w *window, key string) {
	t.Helper()
	if a := w.get(key); a.FilledBuckets != 0 {
		t.Errorf("%s %s: %d of %d buckets have an RTT, want none", w.name, key, a.FilledBuckets, a.Buckets)
	}
}

// ---------------------------------------------------------------------------
// Small helpers.

func at(s []int, i int) int {
	if i < len(s) {
		return s[i]
	}
	return 0
}

func mustWrite(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
