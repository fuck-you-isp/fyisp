//go:build netns && linux

package netns

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// udpProfileYAML is the verdict profile (the built-in network path group
// and five TCP targets) plus a calls group of STUN targets on the responder
// (stunAddr). fyisp tells STUN providers apart by the registrable part of
// the host name, so the targets use names from two domains served by the
// test resolver: stun-a is provider A (provider-a.test), stun-b and stun-53
// are provider B (provider-b.test), stun-53 on UDP port 53 and tagged
// udp-53 (it tells "STUN ports blocked" from "UDP blocked"). Custom UDP
// targets need an interval of at least 6s and a port; the --config file is
// a local file, so its addresses are never refused as private (and the lab
// addresses are not private anyway).
const udpProfileYAML = `name: netns-udp
version: "1"
groups:
  - {id: core, title: Core}
  - {id: web, title: Web}
  - {id: calls, title: Calls}
targets:
  - {name: alpha,   host: 203.0.113.10,   group: core, interval: 6s}
  - {name: bravo,   host: 203.0.113.11,   group: core, interval: 6s}
  - {name: charlie, host: 203.0.113.12,   group: web,  interval: 6s}
  - {name: delta,   host: svc.fyisp.test, group: web,  interval: 6s}
  - {name: kilo,    host: www.fyisp.test, group: web,  interval: 6s}
  - {name: stun-a,  host: stun.provider-a.test, port: 3478, group: calls, kinds: [udp], interval: 6s,  tags: [udp-path, app:meet]}
  - {name: stun-b,  host: stun.provider-b.test, port: 3479, group: calls, kinds: [udp], interval: 6s,  tags: [udp-path, app:teams]}
  - {name: stun-53, host: stun.provider-b.test, port: 53,   group: calls, kinds: [udp], interval: 15s, tags: [udp-path, udp-53]}
`

// The TCP targets of udpProfileYAML by group.
var udpTCPTargets = map[string][]string{"core": {"alpha", "bravo"}, "web": {"charlie", "delta", "kilo"}}

// UDP verdict timing (internal/verdict/udp.go): a target counts as blocked
// after 10 timed-out bursts in a row (60s at 6s), the verdict waits for the
// UDP warm-up (the first answered burst, or 2 minutes after start), a new
// kind holds for two 5s evaluations, and the return to ok takes 60s of ok
// evaluations. udp_degraded needs ≥ 2 poor providers for 3 minutes, judged
// over a 1-minute window (breaks under a minute do not restart the 3
// minutes).
const (
	udpEnterTimeout    = 120 * time.Second
	udpWarmUp          = 2 * time.Minute
	udpRecoverTimeout  = 150 * time.Second
	udpDegradedTimeout = 300 * time.Second
)

// TestUDP runs fyisp with STUN targets and injects faults on the UDP path
// to the STUN responder only (nft rules and a prio qdisc whose tc filter
// sends that UDP, and nothing else, through netem; DNS to the resolver and
// TCP are never touched):
//
//	i)  all UDP to the responder dropped, port 53 too -> udp_blocked, then ok
//	i2) the STUN ports dropped, port 53 answers        -> stun_blocked
//	j)  netem delay 70ms ±65ms                         -> path band fair or worse by jitter, udp_degraded
//	k)  netem loss 10%                                 -> udp_loss 5-16%, band below good
//	l)  one provider's server dropped                  -> no verdict, server_not_answering
//	m)  UDP dropped from the start                     -> udp_blocked only after the 2-minute warm-up
//
// In every scenario the verdict never names the network path or a TCP
// service, every TCP probe succeeds, and the address the STUN responder
// reports (mappedAddr) never appears in fyisp's API, metrics or log.
func TestUDP(t *testing.T) {
	parallel(t)

	// i) UDP blocked, including port 53.
	t.Run("i-udp-block", func(t *testing.T) {
		parallel(t)
		h, _ := startUDPLab(t)
		h.waitVerdict(t, "ok", 120*time.Second)
		h.expectAnswered(t)
		h.checkResponderMapped(t)
		applied := time.Now()
		h.nft(t, fmt.Sprintf("add rule inet fyt fault ip daddr %s meta l4proto udp drop", stunAddr))
		v, raw := h.waitVerdict(t, "udp_blocked", udpEnterTimeout)
		t.Logf("entered udp_blocked after %s: %s", time.Since(applied).Round(time.Second), strings.TrimSpace(string(raw)))
		h.save(t, "verdict-i-udp-block.json", raw)
		expectHealthyEvidence(t, v)
		if !slices.Equal(v.Targets, []string{"stun-a", "stun-b"}) {
			t.Errorf("verdict targets = %v, want [stun-a stun-b] (stun-53 is not counted)", v.Targets)
		}
		if v.Evidence["udp53_ok"] != 0 || v.Evidence["udp_blocked_providers"] != 2 {
			t.Errorf("evidence %v, want udp53_ok 0 and udp_blocked_providers 2", v.Evidence)
		}
		c, craw := h.calls(t)
		h.save(t, "calls-i-udp-block.json", craw)
		if c.State != "blocked" || c.Cause == nil || c.Cause.Kind != "udp_blocked" {
			t.Errorf("/api/calls state %q cause %+v, want blocked by udp_blocked: %s", c.State, c.Cause, craw)
		}
		if c.Path == nil || c.Path.State != "blocked" {
			t.Errorf("/api/calls path %s, want blocked", c.Path)
		}
		h.expectTCPOK(t, "i-tcp", applied, time.Now())
		h.checkHome(t, "i-udp-block", "udp_blocked")

		h.nft(t, "flush chain inet fyt fault")
		reverted := time.Now()
		h.waitVerdict(t, "ok", udpRecoverTimeout)
		t.Logf("back to ok %s after the fault was removed", time.Since(reverted).Round(time.Second))
		if c, raw := h.calls(t); c.State == "blocked" {
			t.Errorf("/api/calls still blocked after recovery: %s", raw)
		}
		h.checkHome(t, "i-recovered", "ok")
		h.checkNoMappedAddr(t)
	})

	// i2) STUN ports blocked, UDP 53 still answers.
	t.Run("i2-stun-ports-block", func(t *testing.T) {
		parallel(t)
		h, _ := startUDPLab(t)
		h.waitVerdict(t, "ok", 120*time.Second)
		h.expectAnswered(t)
		applied := time.Now()
		h.nft(t, fmt.Sprintf("add rule inet fyt fault ip daddr %s udp dport { %d, %d } drop", stunAddr, stunPortA, stunPortB))
		v, raw := h.waitVerdict(t, "stun_blocked", udpEnterTimeout)
		t.Logf("entered stun_blocked after %s: %s", time.Since(applied).Round(time.Second), strings.TrimSpace(string(raw)))
		h.save(t, "verdict-i2-stun-ports-block.json", raw)
		expectHealthyEvidence(t, v)
		if v.Evidence["udp53_ok"] != 1 {
			t.Errorf("evidence udp53_ok = %v, want 1", v.Evidence["udp53_ok"])
		}
		c, craw := h.calls(t)
		h.save(t, "calls-i2-stun-ports-block.json", craw)
		if c.State != "blocked" || c.Cause == nil || c.Cause.Kind != "stun_blocked" {
			t.Errorf("/api/calls state %q cause %+v, want blocked by stun_blocked: %s", c.State, c.Cause, craw)
		}
		h.expectTCPOK(t, "i2-tcp", applied, time.Now())
		h.checkHome(t, "i2-stun-ports-block", "stun_blocked")
		h.checkNoMappedAddr(t)
	})

	// j) Jitter: delay 70ms ±65ms (uniform) on the requests gives an
	// in-burst jitter (mean |ΔRTT| of consecutive replies) of about 43ms: a
	// choppy call. The E-model alone still scores that Good (MOS ≈ 4.35:
	// jitter counts only as delay); the jitter cap (callq.JitterFairMs)
	// makes the path Fair (Poor in a minute that reaches 50ms), and the
	// median burst jitter of each provider stays above udp_degraded's 30ms.
	// Milder cases, measured: ±30ms gives about 20ms in-burst (Good); ±50ms
	// about 32ms (Fair by the cap, but a provider's 1-minute median keeps
	// dipping under 30ms, restarting udp_degraded's 3 minutes).
	t.Run("j-udp-jitter", func(t *testing.T) {
		parallel(t)
		h := newUDPHarness(t)
		h.shapeSTUN(t, "delay", "70ms", "65ms")
		start := h.startUDP(t)
		v, raw := h.waitVerdictVia(t, "udp_degraded", udpDegradedTimeout)
		t.Logf("entered udp_degraded %s after start: %s", time.Since(start).Round(time.Second), strings.TrimSpace(string(raw)))
		h.save(t, "verdict-j-udp-jitter.json", raw)
		expectHealthyEvidence(t, v)
		if j := v.Evidence["udp_jitter_ms"]; j < 30 {
			t.Errorf("evidence udp_jitter_ms = %v, want >= 30", j)
		}
		c, craw := h.calls(t)
		h.save(t, "calls-j-udp-jitter.json", craw)
		c.log(t)
		switch {
		case c.Path == nil:
			t.Errorf("/api/calls has no path: %s", craw)
		case !slices.Contains([]string{"fair", "poor", "unusable"}, c.Path.State):
			t.Errorf("path state %q, want fair, poor or unusable", c.Path.State)
		case !(val(c.Path.Jitter) >= 30):
			t.Errorf("path jitter %v ms, want >= 30", val(c.Path.Jitter))
		case c.Path.Factor != "jitter":
			t.Errorf("path factor %q, want jitter (the jitter cap sets the band)", c.Path.Factor)
		}
		if c.State == "good" {
			t.Errorf("/api/calls top state good under 43ms of jitter")
		}
		h.logShaping(t)
		h.expectTCPOK(t, "j-tcp", start, time.Now())
		h.checkNoMappedAddr(t)
	})

	// k) 10% loss on the requests: each burst loses 0-5 of its 5 requests.
	// About 100 bursts (stun-a and stun-b every 6s, stun-53 every 15s, for
	// 4 minutes): the mean burst loss lands within 5-16%.
	t.Run("k-udp-loss", func(t *testing.T) {
		parallel(t)
		h := newUDPHarness(t)
		h.shapeSTUN(t, "loss", "10%")
		start := h.startUDP(t)
		h.watchVerdict(t, 4*time.Minute, "udp_degraded")
		c, craw := h.calls(t)
		h.save(t, "calls-k-udp-loss.json", craw)
		c.log(t)
		if c.Path == nil {
			t.Fatalf("/api/calls has no path: %s", craw)
		}
		// The mean udp_loss over every burst (the path's own loss is a mean
		// of per-minute medians across targets, a noisier estimate).
		bursts, sum := 0, 0.0
		for _, tg := range c.Path.Targets {
			bursts += tg.Samples
			sum += val(tg.Loss) * float64(tg.Samples)
		}
		loss := sum / float64(max(bursts, 1))
		t.Logf("udp_loss: mean %.3f over %d bursts (path %.3f)", loss, bursts, val(c.Path.Loss))
		if !(loss >= 0.05 && loss <= 0.16) || bursts < 80 {
			t.Errorf("udp_loss mean %.3f over %d bursts, want 0.05-0.16 over about 100", loss, bursts)
		}
		if !(val(c.Path.Loss) > 0) {
			t.Errorf("path loss %v, want > 0", val(c.Path.Loss))
		}
		if c.Path.State == "good" || c.State == "good" {
			t.Errorf("state %s, path state %s, want below good under 10%% loss", c.State, c.Path.State)
		}
		h.logShaping(t)
		h.expectTCPOK(t, "k-tcp", start, time.Now())
		h.checkNoMappedAddr(t)
	})

	// l) One STUN server is down (provider A's only target), from the start:
	// a dead server is not the network's fault, so no verdict, and the
	// Calls view says the server is not answering.
	t.Run("l-one-dead-server", func(t *testing.T) {
		parallel(t)
		h := newUDPHarness(t)
		h.nft(t, fmt.Sprintf("add rule inet fyt fault ip daddr %s udp dport %d drop", stunAddr, stunPortA))
		start := h.startUDP(t)
		// Past the UDP warm-up and 10 timed-out bursts.
		h.watchVerdict(t, udpWarmUp+45*time.Second)
		c, craw := h.calls(t)
		h.save(t, "calls-l-one-dead-server.json", craw)
		c.log(t)
		meet := c.app("meet")
		if meet == nil || meet.State != "server_not_answering" {
			t.Errorf("meet (stun-a only) = %+v, want server_not_answering: %s", meet, craw)
		}
		if teams := c.app("teams"); teams == nil || teams.State != "good" {
			t.Errorf("teams (stun-b) = %+v, want good", teams)
		}
		if c.Path == nil || c.Path.State != "good" {
			t.Errorf("path %s, want good (stun-a left out)", c.Path)
		} else {
			for _, tg := range c.Path.Targets {
				if dead := tg.Name == "stun-a"; tg.NotAnswering != dead {
					t.Errorf("path target %s not_answering = %v, want %v", tg.Name, tg.NotAnswering, dead)
				}
			}
		}
		if c.State != "good" {
			t.Errorf("/api/calls top state %s, want good", c.State)
		}
		h.expectTCPOK(t, "l-tcp", start, time.Now())
		h.checkNoMappedAddr(t)
	})

	// m) UDP blocked before fyisp starts: nothing ever answers, so the
	// blocked verdict waits for the 2-minute warm-up, then comes.
	t.Run("m-warmup", func(t *testing.T) {
		parallel(t)
		h := newUDPHarness(t)
		h.nft(t, fmt.Sprintf("add rule inet fyt fault ip daddr %s meta l4proto udp drop", stunAddr))
		// Count from before the process starts: its engine starts later.
		start := time.Now()
		fy := h.startFyisp(t, "--ephemeral")
		h.waitReady(t, fy)
		h.stopAtEnd(t, fy)
		v, raw := h.waitVerdict(t, "udp_blocked", udpWarmUp+udpEnterTimeout)
		after := time.Since(start)
		t.Logf("entered udp_blocked %s after start: %s", after.Round(time.Second), strings.TrimSpace(string(raw)))
		h.save(t, "verdict-m-warmup.json", raw)
		if after < udpWarmUp {
			t.Errorf("udp_blocked %s after start, want not before the %s warm-up", after.Round(time.Second), udpWarmUp)
		}
		if after > udpWarmUp+45*time.Second {
			t.Errorf("udp_blocked %s after start, want soon after the %s warm-up", after.Round(time.Second), udpWarmUp)
		}
		expectHealthyEvidence(t, v)
		if c, craw := h.calls(t); c.State != "blocked" {
			t.Errorf("/api/calls state %q, want blocked: %s", c.State, craw)
		}
		h.expectTCPOK(t, "m-tcp", start, time.Now())
		h.checkNoMappedAddr(t)
	})
}

// newUDPHarness: a fresh topology with the UDP profile (fyisp not started).
func newUDPHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.profile = filepath.Join(h.dir, "udp.yml")
	mustWrite(t, h.profile, udpProfileYAML)
	return h
}

// startUDP starts fyisp on h and waits until every target has been probed
// once. It returns when fyisp was started.
func (h *harness) startUDP(t *testing.T) time.Time {
	start := time.Now()
	fy := h.startFyisp(t, "--ephemeral")
	h.waitReady(t, fy)
	checkUnprivileged(t, fy.cmd.Process.Pid)
	h.stopAtEnd(t, fy)
	return start
}

// startUDPLab starts fyisp with the UDP profile on a fresh topology.
func startUDPLab(t *testing.T) (*harness, time.Time) {
	h := newUDPHarness(t)
	return h, h.startUDP(t)
}

// shapeSTUN sends the UDP datagrams to the STUN responder, and nothing
// else, through netem on the ISP router's uplink: a prio qdisc whose bands
// all default to 1:2 (priomap of 1s, so no TOS value lands in the netem
// band), and a u32 filter on destination stunAddr and protocol UDP to
// 1:3, where netem runs. DNS goes to another address and TCP is another
// protocol: neither is shaped.
func (h *harness) shapeSTUN(t *testing.T, netem ...string) {
	t.Helper()
	dev := []string{"dev", ispUplink}
	tc := func(args ...string) { h.nsRun(t, h.isp, append([]string{"tc"}, args...)...) }
	tc(slices.Concat([]string{"qdisc", "add"}, dev, []string{"root", "handle", "1:", "prio", "bands", "3", "priomap"},
		slices.Repeat([]string{"1"}, 16))...)
	tc(slices.Concat([]string{"qdisc", "add"}, dev, []string{"parent", "1:3", "handle", "30:", "netem"}, netem)...)
	tc(slices.Concat([]string{"filter", "add"}, dev, []string{"parent", "1:", "protocol", "ip", "prio", "1", "u32",
		"match", "ip", "dst", stunAddr.String() + "/32", "match", "ip", "protocol", "17", "0xff", "flowid", "1:3"})...)
}

// logShaping logs the qdisc counters on the ISP uplink: netem must have
// seen the STUN requests, the default band everything else.
func (h *harness) logShaping(t *testing.T) {
	t.Helper()
	t.Logf("tc -s qdisc on %s:\n%s", ispUplink, h.nsRun(t, h.isp, "tc", "-s", "qdisc", "show", "dev", ispUplink))
}

// waitVerdictVia is waitVerdict that also accepts the UDP kinds on the
// way that a fault on the call path may show first.
func (h *harness) waitVerdictVia(t *testing.T, want string, timeout time.Duration) (verdictAPI, []byte) {
	t.Helper()
	t0 := time.Now()
	last := ""
	for {
		v, raw := h.verdict(t)
		if v.Kind != last {
			t.Logf("  +%3.0fs verdict %s: %q", time.Since(t0).Seconds(), v.Kind, v.Summary)
			last = v.Kind
			if !slices.Contains([]string{want, "ok", "warming_up"}, v.Kind) {
				t.Errorf("verdict %s before %s: %s", v.Kind, want, raw)
			}
		}
		checkSummary(t, "verdict", v.Summary)
		if v.Kind == want {
			return v, raw
		}
		if time.Since(t0) > timeout {
			t.Fatalf("verdict still %q after %s, want %q: %s", v.Kind, timeout, want, raw)
		}
		time.Sleep(2 * time.Second)
	}
}

// watchVerdict polls /api/verdict for d, logging every change of kind; any
// kind other than warming_up, ok and allowed is an error (a UDP fault must
// never look like a network path or TCP service problem).
func (h *harness) watchVerdict(t *testing.T, d time.Duration, allowed ...string) {
	t.Helper()
	t0 := time.Now()
	last := ""
	for time.Since(t0) < d {
		v, raw := h.verdict(t)
		if v.Kind != last {
			t.Logf("  +%3.0fs verdict %s: %q", time.Since(t0).Seconds(), v.Kind, v.Summary)
			last = v.Kind
			if v.Kind != "ok" && v.Kind != "warming_up" && !slices.Contains(allowed, v.Kind) {
				t.Errorf("verdict %s, want ok%s: %s", v.Kind, strings.Join(append([]string{""}, allowed...), " or "), raw)
			}
		}
		checkSummary(t, "verdict", v.Summary)
		time.Sleep(2 * time.Second)
	}
}

// expectHealthyEvidence: a UDP verdict comes with a healthy network path
// and healthy TCP services.
func expectHealthyEvidence(t *testing.T, v verdictAPI) {
	t.Helper()
	for _, k := range []string{"gateway_loss", "edge_loss", "anycast_loss", "services_loss"} {
		if x, ok := v.Evidence[k]; !ok || x != 0 {
			t.Errorf("%s evidence %s = %v (present %v), want 0", v.Kind, k, x, ok)
		}
	}
}

// expectTCPOK: every TCP probe in [from, to] succeeded.
func (h *harness) expectTCPOK(t *testing.T, name string, from, to time.Time) {
	t.Helper()
	for g, names := range udpTCPTargets {
		w := h.panelGroup(t, g, name+"-"+g, from.Add(settle), to.Add(-settle))
		for _, n := range names {
			expectOK(t, w, n+"/tcp")
		}
	}
}

// expectAnswered: every STUN target answered in the calls window.
func (h *harness) expectAnswered(t *testing.T) {
	t.Helper()
	c, raw := h.calls(t)
	if c.Path == nil || c.Path.State != "good" {
		t.Errorf("/api/calls path %s, want good before the fault: %s", c.Path, raw)
		return
	}
	for _, tg := range c.Path.Targets {
		// A target's loss is null until it has a scored minute (3 bursts:
		// stun-53 probes every 15s).
		if tg.Samples == 0 || tg.NotAnswering || val(tg.Loss) > 0 {
			t.Errorf("path target %+v, want answered without loss before the fault", tg)
		}
	}
}

// checkNoMappedAddr: the public address the STUN responder reports appears
// nowhere fyisp shows it (API, metrics, log).
func (h *harness) checkNoMappedAddr(t *testing.T) {
	t.Helper()
	needle := mappedAddr.String()
	for _, p := range []string{"/api/calls", "/api/status", "/metrics", "/api/verdict", "/api/profile",
		fmt.Sprintf("/api/panel?group=calls&from=%d&to=now", h.started.UnixMilli()),
		fmt.Sprintf("/api/incidents?from=%d&to=now", h.started.UnixMilli())} {
		if b := h.get(t, p); strings.Contains(string(b), needle) {
			t.Errorf("GET %s shows the mapped address %s", p, needle)
		}
	}
	logs, _ := filepath.Glob(filepath.Join(h.dir, "fyisp-*.log"))
	for _, l := range logs {
		if b, err := os.ReadFile(l); err == nil && strings.Contains(string(b), needle) {
			t.Errorf("%s shows the mapped address %s", filepath.Base(l), needle)
		}
	}
}

// checkResponderMapped asks the STUN responder from the client namespace
// and checks that it reports mappedAddr:mappedPort, so that
// checkNoMappedAddr looks for an address fyisp was actually given.
func (h *harness) checkResponderMapped(t *testing.T) {
	t.Helper()
	const cookie = 0x2112A442
	c, err := dialIn(h.cli)(context.Background(), "udp4", netip.AddrPortFrom(stunAddr, stunPortA).String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:2], 0x0001)
	binary.BigEndian.PutUint32(req[4:8], cookie)
	copy(req[8:], "fyisp-netns!")
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 548)
	n, err := c.Read(b)
	if err != nil {
		t.Fatalf("STUN responder: %v", err)
	}
	b = b[:n]
	if n != 32 || binary.BigEndian.Uint16(b[0:2]) != 0x0101 || string(b[8:20]) != "fyisp-netns!" || binary.BigEndian.Uint16(b[20:22]) != 0x0020 {
		t.Fatalf("STUN responder answered % x, want a Binding Success with XOR-MAPPED-ADDRESS", b)
	}
	var a [4]byte
	binary.BigEndian.PutUint32(a[:], binary.BigEndian.Uint32(b[28:32])^cookie)
	got := netip.AddrPortFrom(netip.AddrFrom4(a), binary.BigEndian.Uint16(b[26:28])^(cookie>>16))
	if want := netip.AddrPortFrom(mappedAddr, mappedPort); got != want {
		t.Fatalf("STUN responder maps to %s, want %s", got, want)
	}
	t.Logf("STUN responder: XOR-MAPPED-ADDRESS %s", got)
}

// callsGroupAPI is the part of a /api/calls group the harness reads.
type callsGroupAPI struct {
	State   string   `json:"state"`
	MOS     *float64 `json:"mos"`
	RTT     *float64 `json:"rtt_ms"`
	Jitter  *float64 `json:"jitter_ms"`
	Loss    *float64 `json:"loss"`
	Samples int      `json:"samples"`
	Factor  string   `json:"factor"`
	Targets []struct {
		Name         string   `json:"name"`
		NotAnswering bool     `json:"not_answering"`
		Samples      int      `json:"samples"`
		Loss         *float64 `json:"loss"`
	} `json:"targets"`
}

func (g *callsGroupAPI) String() string {
	if g == nil {
		return "<none>"
	}
	var ts []string
	for _, x := range g.Targets {
		dead := ""
		if x.NotAnswering {
			dead = " not_answering"
		}
		ts = append(ts, fmt.Sprintf("%s(n=%d loss=%.3f%s)", x.Name, x.Samples, val(x.Loss), dead))
	}
	return fmt.Sprintf("%s mos=%.2f rtt=%.1fms jitter=%.1fms loss=%.3f factor=%s bursts=%d [%s]",
		g.State, val(g.MOS), val(g.RTT), val(g.Jitter), val(g.Loss), g.Factor, g.Samples, strings.Join(ts, " "))
}

type callsAppAPI struct {
	App string `json:"app"`
	UDP string `json:"udp"`
	callsGroupAPI
}

type callsAPI struct {
	State string `json:"state"`
	Cause *struct {
		Source string `json:"source"`
		Kind   string `json:"kind"`
		App    string `json:"app"`
		Factor string `json:"factor"`
	} `json:"cause"`
	Path *callsGroupAPI `json:"path"`
	Apps []callsAppAPI  `json:"apps"`
}

// log logs the top state, the path and the measured apps.
func (c *callsAPI) log(t *testing.T) {
	t.Helper()
	var sb strings.Builder
	fmt.Fprintf(&sb, "/api/calls: state %s", c.State)
	if c.Cause != nil {
		fmt.Fprintf(&sb, " (cause %+v)", *c.Cause)
	}
	fmt.Fprintf(&sb, "\n    path  %s", c.Path)
	for _, a := range c.Apps {
		if a.UDP == "measured" {
			fmt.Fprintf(&sb, "\n    %-5s %s", a.App, &a.callsGroupAPI)
		}
	}
	t.Log(sb.String())
}

func (c *callsAPI) app(id string) *callsAppAPI {
	for i := range c.Apps {
		if c.Apps[i].App == id {
			return &c.Apps[i]
		}
	}
	return nil
}

// calls reads /api/calls (the last 5 minutes).
func (h *harness) calls(t *testing.T) (callsAPI, []byte) {
	t.Helper()
	b := h.get(t, "/api/calls")
	var c callsAPI
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("/api/calls: %v: %s", err, b)
	}
	return c, b
}

// val is *p, NaN for nil (a JSON null).
func val(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}
