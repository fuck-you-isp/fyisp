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
// edge, the three anycast resolvers, which live on the internet namespace's loopback in
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
	// A resolver outage while running shows after the next periodic lookup
	// (every 60s) and a retry 10s later both fail, plus the hold above.
	dnsEnterTimeout = 180 * time.Second
)

// Summaries are shown on the public link: no addresses, no lab host names.
var (
	ipPattern = regexp.MustCompile(`\d+\.\d+\.\d+\.\d+`)
	labNames  = []string{"fyisp.test", dnsName, dnsName2, nsPrefix} // every namespace name starts with nsPrefix
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
// change of kind on the way. While entering a problem (from ok or
// warming_up), any other problem kind on the way is an error: the verdict
// must name the faulty layer first.
func (h *harness) waitVerdict(t *testing.T, want string, timeout time.Duration) (verdictAPI, []byte) {
	t.Helper()
	t0 := time.Now()
	last := ""
	for {
		v, raw := h.verdict(t)
		if v.Kind != last {
			t.Logf("  +%3.0fs verdict %s: %q", time.Since(t0).Seconds(), v.Kind, v.Summary)
			last = v.Kind
			if want != "ok" && v.Kind != want && v.Kind != "ok" && v.Kind != "warming_up" {
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
//	e) resolver unreachable while running        -> dns [delta kilo]
//	e2) fyisp starting while it is unreachable   -> dns [delta kilo]
//
// Each scenario runs on its own topology and fyisp. Apart from e2, fyisp
// first warms up to ok; then the fault must make its kind the first
// problem shown and the first incident logged, and that incident must be
// closed once the verdict is back to ok. In e2 the resolver is unreachable
// before fyisp starts. In e, the names resolved before the outage: fyisp
// re-resolves every 60s and keeps the last good address for one failed
// lookup, so the probes turn into DNS losses after the second failed
// lookup.
func TestVerdict(t *testing.T) {
	parallel(t)

	t.Run("a-baseline", func(t *testing.T) {
		parallel(t)
		h, _ := startVerdictLab(t)
		start := time.Now()
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

	// A fault: apply and revert run on the scenario's own topology.
	type fault func(h *harness, t *testing.T)
	scenario := func(name, kind string, targets []string, enter time.Duration, atStart bool, apply, revert fault) {
		t.Run(name, func(t *testing.T) {
			parallel(t)
			var h *harness
			var applied time.Time
			if atStart {
				// The fault is on before fyisp starts; it counts from
				// the moment fyisp is ready.
				h = newVerdictHarness(t)
				apply(h, t)
				fy := h.startFyisp(t, "--ephemeral")
				h.waitReady(t, fy)
				h.stopAtEnd(t, fy)
				applied = time.Now()
			} else {
				h, _ = startVerdictLab(t)
				h.waitVerdict(t, "ok", 120*time.Second)
				applied = time.Now()
				apply(h, t)
			}
			v, raw := h.waitVerdict(t, kind, enter)
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

			revert(h, t)
			reverted := time.Now()
			_, raw = h.waitVerdict(t, "ok", recoverTimeout)
			t.Logf("back to ok %s after the fault was removed", time.Since(reverted).Round(time.Second))

			list, rawList := h.incidents(t, applied.Add(-time.Minute))
			h.save(t, "incidents-"+name+".json", rawList)
			// The first incident that started under the fault (the list
			// is newest first) must be of the expected kind.
			var found *incidentAPI
			for i, in := range list {
				t.Logf("incident %s", in)
				checkSummary(t, "incident", in.Summary)
				if !in.Start.Before(applied.Add(-time.Second)) {
					found = &list[i]
				}
				if in.End == nil {
					t.Errorf("incident still open after recovery: %s", in)
				}
			}
			switch {
			case found == nil:
				t.Errorf("no %s incident recorded: %s", kind, rawList)
			case found.Kind != kind:
				t.Errorf("first incident under the fault is %s, want %s", found, kind)
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

	nft := func(rule string) fault { return func(h *harness, t *testing.T) { h.nft(t, rule) } }
	flush := nft("flush chain inet fyt fault")
	scenario("b-lan", "lan", nil, enterTimeout, false,
		func(h *harness, t *testing.T) {
			h.nsRun(t, h.gw, "tc", "qdisc", "add", "dev", "g0", "root", "netem", "loss", "40%")
		},
		func(h *harness, t *testing.T) { h.nsRun(t, h.gw, "tc", "qdisc", "del", "dev", "g0", "root") })
	scenario("c-isp", "isp", nil, enterTimeout, false,
		func(h *harness, t *testing.T) {
			h.nsRun(t, h.isp, "tc", "qdisc", "add", "dev", "i0", "root", "netem", "loss", "40%")
		},
		func(h *harness, t *testing.T) { h.nsRun(t, h.isp, "tc", "qdisc", "del", "dev", "i0", "root") })
	scenario("d-upstream", "upstream", nil, enterTimeout, false,
		nft("add rule inet fyt fault ip daddr { 1.1.1.1, 8.8.8.8, 9.9.9.9, 203.0.113.10, 203.0.113.11, 203.0.113.12 } drop"),
		flush)
	scenario("f-service", "service", []string{"bravo"}, enterTimeout, false,
		nft("add rule inet fyt fault ip daddr 203.0.113.11 drop"), flush)
	scenario("e-dns-runtime", "dns", []string{"delta", "kilo"}, dnsEnterTimeout, false,
		nft("add rule inet fyt fault ip daddr 203.0.113.53 drop"), flush)
	// e2) DNS: a fresh fyisp starts while the resolver is unreachable.
	scenario("e2-dns-at-start", "dns", []string{"delta", "kilo"}, enterTimeout, true,
		nft("add rule inet fyt fault ip daddr 203.0.113.53 drop"), flush)
}

// newVerdictHarness: a fresh topology with the verdict profile.
func newVerdictHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.profile = filepath.Join(h.dir, "verdict.yml")
	mustWrite(t, h.profile, verdictProfileYAML)
	return h
}

// startVerdictLab starts fyisp with the verdict profile on a fresh
// topology and waits until every target has been probed once.
func startVerdictLab(t *testing.T) (*harness, *fyisp) {
	h := newVerdictHarness(t)
	fy := h.startFyisp(t, "--ephemeral")
	h.waitReady(t, fy)
	_, raw, err := h.status(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("/api/status: %s", strings.TrimSpace(string(raw)))
	checkUnprivileged(t, fy.cmd.Process.Pid)
	h.stopAtEnd(t, fy)
	return h, fy
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
