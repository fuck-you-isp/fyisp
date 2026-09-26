//go:build netns && linux

package netns

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// verdictProfileYAML keeps the built-in network path group (gateway, ISP
// edge, the three anycast resolvers, which live on fyt-inet's loopback in
// the lab). The internet targets: three IP literals and two names served by
// the test resolver, in two groups.
const verdictProfileYAML = `name: netns-verdict
version: "1"
groups:
  - {id: core, title: Core}
  - {id: web, title: Web}
targets:
  - {name: alpha,   host: 203.0.113.10,   group: core, interval: 6s}
  - {name: bravo,   host: 203.0.113.11,   group: core, interval: 6s}
  - {name: charlie, host: 203.0.113.12,   group: web,  interval: 6s}
  - {name: delta,   host: svc.fyisp.test, group: web,  interval: 6s}
  - {name: kilo,    host: www.fyisp.test, group: web,  interval: 6s}
`

// Hysteresis of the verdict engine (internal/verdict): 60s warm-up, a new
// problem kind must hold for two 5s evaluations over a 60s window (so a 40%
// loss needs ~30s to push the window past 20%), and a return to ok needs 60s
// of ok evaluations once the faulty samples have left the window.
const (
	enterTimeout   = 150 * time.Second
	recoverTimeout = 240 * time.Second
)

// Summaries are shown on the public link: no addresses, no lab host names.
var (
	ipPattern = regexp.MustCompile(`\d+\.\d+\.\d+\.\d+`)
	labNames  = []string{"fyisp.test", dnsName, dnsName2, "fyt-", nsClient, nsGW, nsISP, nsInet}
)

type verdictAPI struct {
	Kind     string             `json:"kind"`
	Since    *time.Time         `json:"since"`
	Summary  string             `json:"summary"`
	Targets  []string           `json:"targets"`
	Evidence map[string]float64 `json:"evidence"`
}

type incidentAPI struct {
	ID       int64      `json:"id"`
	Start    time.Time  `json:"start"`
	End      *time.Time `json:"end"`
	Kind     string     `json:"kind"`
	Summary  string     `json:"summary"`
	Targets  []string   `json:"targets"`
	PeakLoss float64    `json:"peak_loss"`
}

func (in incidentAPI) String() string {
	end := "open"
	if in.End != nil {
		end = in.End.Format("15:04:05")
	}
	return fmt.Sprintf("#%d %-8s %s-%s peak_loss=%.3f targets=%v %q",
		in.ID, in.Kind, in.Start.Format("15:04:05"), end, in.PeakLoss, in.Targets, in.Summary)
}

func (h *harness) verdict(t *testing.T) (verdictAPI, []byte) {
	t.Helper()
	b := h.get(t, "/api/verdict")
	var v verdictAPI
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("/api/verdict: %v: %s", err, b)
	}
	return v, b
}

func (h *harness) incidents(t *testing.T, from time.Time) ([]incidentAPI, []byte) {
	t.Helper()
	b := h.get(t, fmt.Sprintf("/api/incidents?from=%d&to=now", from.UnixMilli()))
	var list []incidentAPI
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("/api/incidents: %v: %s", err, b)
	}
	return list, b
}

// waitVerdict polls /api/verdict until its kind is want, logging every
// change of kind on the way (brief other kinds are possible while a
// window fills or drains).
func (h *harness) waitVerdict(t *testing.T, want string, timeout time.Duration) (verdictAPI, []byte) {
	t.Helper()
	t0 := time.Now()
	last := ""
	for {
		v, raw := h.verdict(t)
		if v.Kind != last {
			t.Logf("  +%3.0fs verdict %s: %q", time.Since(t0).Seconds(), v.Kind, v.Summary)
			last = v.Kind
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

func checkSummary(t *testing.T, what, s string) {
	t.Helper()
	if s == "" {
		t.Errorf("%s: empty summary", what)
	}
	if m := ipPattern.FindString(s); m != "" {
		t.Errorf("%s summary %q contains an address (%s)", what, s, m)
	}
	ls := strings.ToLower(s)
	for _, n := range labNames {
		if strings.Contains(ls, strings.ToLower(n)) {
			t.Errorf("%s summary %q contains the lab name %q", what, s, n)
		}
	}
}

// TestVerdict runs fyisp with the network path group enabled and checks
// the verdict and the outage log for faults at each layer:
//
//	a) baseline                                  -> ok
//	b) netem loss 40% between client and gateway -> lan
//	c) netem loss 40% between gateway and ISP    -> isp
//	d) anycast and most targets dropped          -> upstream
//	f) one target dropped                        -> service [bravo]
//	e) resolver unreachable                      -> dns [delta kilo]
//
// Each fault must open an incident of its kind that is closed once the
// verdict is back to ok. e runs last, in a second fyisp that starts while
// the resolver is unreachable: fyisp keeps probing a host's last good
// address when a later lookup fails (and re-resolves every 15 minutes), so
// a resolver outage only shows for names that have not resolved yet.
func TestVerdict(t *testing.T) {
	h := newHarness(t)
	h.profile = filepath.Join(h.dir, "verdict.yml")
	mustWrite(t, h.profile, verdictProfileYAML)

	fy := h.startFyisp(t, "--ephemeral")
	h.waitReady(t, fy)
	_, raw, err := h.status(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("/api/status: %s", strings.TrimSpace(string(raw)))
	checkUnprivileged(t, fy.cmd.Process.Pid)
	start := time.Now()

	t.Run("a-baseline", func(t *testing.T) {
		v, raw := h.waitVerdict(t, "ok", 120*time.Second)
		t.Logf("verdict: %s", strings.TrimSpace(string(raw)))
		h.save(t, "verdict-a-baseline.json", raw)
		for _, k := range []string{"gateway_loss", "edge_loss", "anycast_loss", "services_loss"} {
			if x, ok := v.Evidence[k]; !ok || x != 0 {
				t.Errorf("evidence %s = %v (present %v), want 0: the layer must be known and healthy", k, x, ok)
			}
		}
		if n := v.Evidence["internet_targets"]; n != 5 {
			t.Errorf("evidence internet_targets = %v, want 5", n)
		}
		if list, raw := h.incidents(t, start.Add(-time.Hour)); len(list) != 0 {
			t.Errorf("incidents after a healthy start: %s", raw)
		}
	})
	if t.Failed() {
		t.FailNow()
	}

	scenario := func(name, kind string, targets []string, apply, revert func()) {
		t.Run(name, func(t *testing.T) {
			applied := time.Now()
			apply()
			v, raw := h.waitVerdict(t, kind, enterTimeout)
			t.Logf("entered %s after %s: %s", kind, time.Since(applied).Round(time.Second), strings.TrimSpace(string(raw)))
			h.save(t, "verdict-"+name+".json", raw)
			if targets != nil && !slices.Equal(v.Targets, targets) {
				t.Errorf("verdict targets = %v, want %v", v.Targets, targets)
			}
			if targets == nil && len(v.Targets) != 0 {
				t.Errorf("verdict targets = %v, want none for a path-level kind", v.Targets)
			}
			// Let the fault run a little longer, then look at the open incident.
			time.Sleep(10 * time.Second)
			open := h.findIncident(t, applied, kind)
			if open == nil {
				t.Errorf("no %s incident while the fault is on", kind)
			} else if open.End != nil {
				t.Errorf("%s incident already closed while the fault is on: %s", kind, open)
			}

			revert()
			reverted := time.Now()
			_, raw = h.waitVerdict(t, "ok", recoverTimeout)
			t.Logf("back to ok %s after the fault was removed", time.Since(reverted).Round(time.Second))

			list, rawList := h.incidents(t, applied.Add(-time.Minute))
			h.save(t, "incidents-"+name+".json", rawList)
			var found *incidentAPI
			for i, in := range list {
				t.Logf("incident %s", in)
				checkSummary(t, "incident", in.Summary)
				if in.Kind == kind && found == nil {
					found = &list[i]
				}
				if in.End == nil {
					t.Errorf("incident still open after recovery: %s", in)
				}
			}
			switch {
			case found == nil:
				t.Errorf("no %s incident recorded: %s", kind, rawList)
			case found.End == nil:
				t.Errorf("%s incident not closed after recovery", kind)
			case found.Start.Before(applied.Add(-time.Second)) || found.End.Before(reverted):
				t.Errorf("%s incident %s-%s, want it to start after the fault (%s) and end after its removal (%s)",
					kind, found.Start.Format(time.TimeOnly), found.End.Format(time.TimeOnly), applied.Format(time.TimeOnly), reverted.Format(time.TimeOnly))
			}
			if found != nil && targets != nil {
				for _, n := range targets {
					if !slices.Contains(found.Targets, n) {
						t.Errorf("%s incident targets %v lack %s", kind, found.Targets, n)
					}
				}
			}
			if found != nil && found.PeakLoss <= 0 {
				t.Errorf("%s incident peak_loss %v, want > 0", kind, found.PeakLoss)
			}
		})
	}

	scenario("b-lan", "lan", nil,
		func() { h.nsRun(t, nsGW, "tc", "qdisc", "add", "dev", "g0", "root", "netem", "loss", "40%") },
		func() { h.nsRun(t, nsGW, "tc", "qdisc", "del", "dev", "g0", "root") })
	scenario("c-isp", "isp", nil,
		func() { h.nsRun(t, nsISP, "tc", "qdisc", "add", "dev", "i0", "root", "netem", "loss", "40%") },
		func() { h.nsRun(t, nsISP, "tc", "qdisc", "del", "dev", "i0", "root") })
	scenario("d-upstream", "upstream", nil,
		func() {
			h.nft(t, "add rule inet fyt fault ip daddr { 1.1.1.1, 8.8.8.8, 9.9.9.9, 203.0.113.10, 203.0.113.11, 203.0.113.12 } drop")
		},
		func() { h.nft(t, "flush chain inet fyt fault") })
	scenario("f-service", "service", []string{"bravo"},
		func() { h.nft(t, "add rule inet fyt fault ip daddr 203.0.113.11 drop") },
		func() { h.nft(t, "flush chain inet fyt fault") })

	h.stopFyisp(t, fy, 0)

	// e) DNS: a fresh fyisp starts while the resolver is unreachable.
	h.nft(t, "add rule inet fyt fault ip daddr 203.0.113.53 drop")
	fy2 := h.startFyisp(t, "--ephemeral")
	h.waitReady(t, fy2)
	scenario("e-dns", "dns", []string{"delta", "kilo"},
		func() {},
		func() { h.nft(t, "flush chain inet fyt fault") })
	h.stopFyisp(t, fy2, 0)
}

// findIncident returns the newest incident of kind that started after from.
func (h *harness) findIncident(t *testing.T, from time.Time, kind string) *incidentAPI {
	t.Helper()
	list, _ := h.incidents(t, from.Add(-time.Minute))
	for i, in := range list {
		if in.Kind == kind && !in.Start.Before(from.Add(-time.Second)) {
			return &list[i]
		}
	}
	return nil
}
