//go:build netns && linux

package netns

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Seven traced targets behind the lab's gateway and ISP router (hops
// [gw, isp, target]): package trace probes the shared hops once per round
// for all of them (see internal/trace/share.go), so the gateway gets well
// under 1 TTL-limited probe per second instead of 7 per 5s round, and never
// runs into the kernel's ICMP error rate limit (1/s per destination host,
// burst 6): hop 1 shows no loss.
var sharedTargets = func() []netip.Addr {
	var out []netip.Addr
	for i := range 7 {
		out = append(out, netip.AddrFrom4([4]byte{203, 0, 113, byte(40 + i)}))
	}
	return out
}()

func sharedProfile() string {
	var b strings.Builder
	b.WriteString("name: netns-hopshare\nversion: \"1\"\npath: false\ngroups:\n  - {id: lab, title: Lab}\ntargets:\n")
	for i, a := range sharedTargets {
		fmt.Fprintf(&b, "  - {name: trace%d, host: %s, group: lab, interval: 6s, kinds: [icmp], trace: true}\n", i, a)
	}
	return b.String()
}

var nftPackets = regexp.MustCompile(`packets (\d+)`)

// counters reads the packet counters of the rules in chain (table inet fytc)
// in namespace ns, in rule order.
func (h *harness) counters(t *testing.T, ns, chain string) []int {
	t.Helper()
	var out []int
	for _, m := range nftPackets.FindAllStringSubmatch(h.nsRun(t, ns, "nft", "list", "chain", "inet", "fytc", chain), -1) {
		n, _ := strconv.Atoi(m[1])
		out = append(out, n)
	}
	return out
}

// addCounters counts, in router namespace ns, the client's probes that
// expire there (TTL 1 on arrival) and the time-exceeded messages it sends.
func (h *harness) addCounters(t *testing.T, ns string) {
	t.Helper()
	h.nsRun(t, ns, "nft", "add table inet fytc")
	h.nsRun(t, ns, "nft", "add chain inet fytc pre { type filter hook prerouting priority -300; policy accept; }")
	h.nsRun(t, ns, "nft", "add rule inet fytc pre ip saddr 10.99.1.2 ip ttl 1 icmp type echo-request counter")
	h.nsRun(t, ns, "nft", "add chain inet fytc out { type filter hook output priority 0; policy accept; }")
	h.nsRun(t, ns, "nft", "add rule inet fytc out ip daddr 10.99.1.2 icmp type time-exceeded counter")
}

type traceHopsJSON struct {
	Hops []struct {
		Hop  int    `json:"hop"`
		IP   string `json:"ip"`
		N    int    `json:"n"`
		Lost int    `json:"lost"`
	} `json:"hops"`
}

func TestTraceSharedHops(t *testing.T) {
	parallel(t)
	h := newHarness(t)
	for _, a := range sharedTargets {
		h.run(t, "ip", "-n", h.inet, "addr", "add", a.String()+"/32", "dev", "lo")
	}
	mustWrite(t, h.profile, sharedProfile())
	h.addCounters(t, h.gw)
	h.addCounters(t, h.isp)
	fy := h.startFyisp(t, "--ephemeral")
	h.waitReady(t, fy)
	h.stopAtEnd(t, fy)

	time.Sleep(30 * time.Second) // routes learnt, shared hops settled
	gw0, isp0 := append(h.counters(t, h.gw, "pre"), h.counters(t, h.gw, "out")...), append(h.counters(t, h.isp, "pre"), h.counters(t, h.isp, "out")...)
	t0 := time.Now()
	const d = 60 * time.Second
	time.Sleep(d)
	t1 := time.Now()
	gw1, isp1 := append(h.counters(t, h.gw, "pre"), h.counters(t, h.gw, "out")...), append(h.counters(t, h.isp, "pre"), h.counters(t, h.isp, "out")...)
	secs := t1.Sub(t0).Seconds()
	for _, r := range []struct {
		name   string
		c0, c1 []int
	}{{"gateway (hop 1)", gw0, gw1}, {"isp (hop 2)", isp0, isp1}} {
		if len(r.c0) != 2 || len(r.c1) != 2 {
			t.Fatalf("%s: counters %v %v", r.name, r.c0, r.c1)
		}
		probes, answers := r.c1[0]-r.c0[0], r.c1[1]-r.c0[1]
		t.Logf("%s: %d expiring probes (%.2f/s), %d time-exceeded sent (%.2f/s) in %.0fs, %d traced targets every 5s",
			r.name, probes, float64(probes)/secs, answers, float64(answers)/secs, secs, len(sharedTargets))
		if float64(probes) > secs+2 {
			t.Errorf("%s: %d probes in %.0fs, want at most 1/s", r.name, probes, secs)
		}
		if probes == 0 {
			t.Errorf("%s: never probed", r.name)
		}
	}

	// Every trace has samples of hop 1 (the shared measurement), none lost.
	// The trace store's slots end up to one round after t1.
	time.Sleep(6 * time.Second)
	for i := range sharedTargets {
		name := fmt.Sprintf("trace%d", i)
		b := h.get(t, fmt.Sprintf("/api/trace?target=%s&from=%d&to=%d", name, t0.UnixMilli(), t1.UnixMilli()))
		h.save(t, "trace-"+name+".json", b)
		var tj traceHopsJSON
		if err := json.Unmarshal(b, &tj); err != nil {
			t.Fatal(err)
		}
		var line []string
		n1, lost1 := 0, 0
		for _, hs := range tj.Hops {
			line = append(line, fmt.Sprintf("hop %d %s n=%d lost=%d", hs.Hop, hs.IP, hs.N, hs.Lost))
			if hs.Hop == 1 {
				n1 += hs.N
				lost1 += hs.Lost
			}
		}
		t.Logf("%s: %s", name, strings.Join(line, "; "))
		if lost1 != 0 {
			t.Errorf("%s: hop 1 lost %d of %d, want no loss", name, lost1, n1)
		}
		if n1 < int(secs/5)*2/3 {
			t.Errorf("%s: %d samples of hop 1 in %.0fs, want about one per 5s", name, n1, secs)
		}
	}
}
