package verdict

import (
	"cmp"
	"slices"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// stunHarness is newHarness plus default.yml's STUN targets: four
// providers on STUN ports (Google, Cloudflare, Twilio, Microsoft's Teams
// relay) and Cloudflare's STUN on UDP 53.
func stunHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.p.Groups = append(h.p.Groups, model.Group{ID: "calls", Title: "Call path (UDP)"})
	for _, s := range []struct {
		name, host string
		port       int
		iv         time.Duration
		tags       []string
	}{
		{"Meet-STUN", "stun.l.google.com", 19302, 5 * time.Second, []string{tagUDPPath, "app:meet"}},
		{"Cloudflare-STUN", "stun.cloudflare.com", 3478, 5 * time.Second, []string{tagUDPPath}},
		{"Cloudflare-UDP53", "stun.cloudflare.com", 53, 15 * time.Second, []string{tagUDPPath, tagUDP53}},
		{"Twilio-STUN-global", "global.stun.twilio.com", 3478, 5 * time.Second, []string{tagUDPPath}},
		{"Teams-Relay", "worldaz.tr.teams.microsoft.com", 3478, 5 * time.Second, []string{tagUDPPath, "app:teams"}},
	} {
		h.p.Targets = append(h.p.Targets, model.Target{Name: s.name, Host: s.host, Port: s.port, Group: "calls",
			Kinds: []model.ProbeKind{model.KindUDP}, Interval: s.iv, Tags: s.tags})
	}
	return h
}

var stunPorts = []string{"Meet-STUN", "Cloudflare-STUN", "Twilio-STUN-global", "Teams-Relay"}

func allUDP() []string { return append(slices.Clone(stunPorts), "Cloudflare-UDP53") }

func timedOut(time.Time) outcome { return outcome{lost: true, reason: model.ReasonTimeout} }

// between applies o in [t0+from, t0+to) and a healthy burst otherwise
// (to == 0: until the end).
func between(from, to time.Duration, o func(time.Time) outcome) func(time.Time) outcome {
	return func(at time.Time) outcome {
		if at.Before(t0.Add(from)) || (to > 0 && !at.Before(t0.Add(to))) {
			return outcome{rtt: 20 * time.Millisecond, jitter: 2 * time.Millisecond}
		}
		return o(at)
	}
}

// track runs d in 5s steps, recording when (offset from t0) each verdict
// kind first became current.
func (h *harness) track(d time.Duration, first map[model.VerdictKind]time.Duration) {
	h.t.Helper()
	for end := h.now.Add(d); h.now.Before(end); {
		h.run(5 * time.Second)
		if k := h.e.Current().Kind; first != nil {
			if _, ok := first[k]; !ok {
				first[k] = h.now.Sub(t0)
			}
		}
	}
}

// checkTCPOK: the TCP rules never fired.
func checkTCPOK(t *testing.T, h *harness) {
	t.Helper()
	for _, k := range []model.VerdictKind{model.VerdictNoNetwork, model.VerdictLAN, model.VerdictISP,
		model.VerdictUpstream, model.VerdictDNS, model.VerdictService} {
		if h.seen[k] {
			t.Errorf("TCP verdict %s seen: %v", k, h.seen)
		}
	}
}

func checkNoUDP(t *testing.T, h *harness) {
	t.Helper()
	for _, k := range []model.VerdictKind{model.VerdictUDPBlocked, model.VerdictSTUNBlocked, model.VerdictUDPDegraded} {
		if h.seen[k] {
			t.Errorf("%s seen: %v", k, h.seen)
		}
	}
	if v := h.e.Current(); v.Kind != model.VerdictOK {
		t.Errorf("verdict %+v", v)
	}
	if in := h.st.all(); len(in) != 0 {
		t.Errorf("incidents %+v", in)
	}
}

const (
	udpBlockedSummary  = "UDP is blocked or failing on this network (or a firewall on this computer is blocking fyisp): calls will fall back to slower TCP relays."
	stunBlockedSummary = "Call ports (STUN) are blocked on this network, but other UDP works: some call apps may fall back to slower relays."
)

// One dead STUN service (one provider, even with two targets) is that
// service's problem: no verdict.
func TestUDPBlockedNeedsTwoProviders(t *testing.T) {
	for name, dead := range map[string][]string{
		"one target":            {"Meet-STUN"},
		"one provider, 2 ports": {"Cloudflare-STUN", "Cloudflare-UDP53"},
	} {
		t.Run(name, func(t *testing.T) {
			h := stunHarness(t)
			h.with(targets(timedOut, dead...))
			h.run(10 * time.Minute)
			checkNoUDP(t, h)
			checkTCPOK(t, h)
		})
	}
}

// Dead servers on a port another STUN server still answers on are those
// servers' problem, not a block: e.g. Microsoft filtering the Teams relay
// during a Twilio outage, while Cloudflare answers on the same port 3478.
// A network that blocks one port is still caught (3478 below, while Google
// answers on 19302), and so is one where the last answering port goes quiet.
func TestUDPBlockedNotWhileSTUNAnswers(t *testing.T) {
	for name, dead := range map[string][]string{
		"relay and twilio":                 {"Teams-Relay", "Twilio-STUN-global"},
		"relay and google":                 {"Teams-Relay", "Meet-STUN"},
		"three providers":                  {"Teams-Relay", "Twilio-STUN-global", "Meet-STUN"},
		"three providers and cloudflare53": {"Teams-Relay", "Twilio-STUN-global", "Meet-STUN", "Cloudflare-UDP53"},
	} {
		t.Run(name, func(t *testing.T) {
			h := stunHarness(t)
			h.with(targets(timedOut, dead...))
			h.run(10 * time.Minute)
			checkNoUDP(t, h)
			checkTCPOK(t, h)
		})
	}
	// Only 3478 blocked (Cloudflare, Twilio and the relay), 19302 and 53
	// answer: stun_blocked from three providers.
	h := stunHarness(t)
	h.with(targets(timedOut, "Cloudflare-STUN", "Twilio-STUN-global", "Teams-Relay"))
	h.run(5 * time.Minute)
	if v := h.e.Current(); v.Kind != model.VerdictSTUNBlocked || v.Evidence["udp_blocked_providers"] != 3 ||
		!slices.Equal(v.Targets, []string{"Cloudflare-STUN", "Teams-Relay", "Twilio-STUN-global"}) {
		t.Errorf("3478 blocked: %+v", v)
	}
	checkTCPOK(t, h)
	// Cloudflare's STUN port, the last one answering, stops too: blocked.
	h = stunHarness(t)
	h.with(targets(timedOut, "Teams-Relay", "Twilio-STUN-global", "Meet-STUN"))
	h.with(targets(between(5*time.Minute, 0, timedOut), "Cloudflare-STUN"))
	first := map[model.VerdictKind]time.Duration{}
	h.track(8*time.Minute, first)
	if at, ok := first[model.VerdictSTUNBlocked]; !ok || at < 5*time.Minute || at > 6*time.Minute {
		t.Errorf("stun_blocked first at %v (%v), want within a minute of 5m; seen %v", at, ok, first)
	}
	if first[model.VerdictUDPBlocked] != 0 || first[model.VerdictUDPDegraded] != 0 {
		t.Errorf("first verdicts %v", first)
	}
}

// A network that blocks UDP from the start: nothing during the UDP warm-up
// (2 minutes, no answer yet), udp_blocked right after.
func TestUDPBlockedWarmUp(t *testing.T) {
	h := stunHarness(t)
	h.with(targets(timedOut, allUDP()...))
	first := map[model.VerdictKind]time.Duration{}
	h.track(4*time.Minute, first)
	at, ok := first[model.VerdictUDPBlocked]
	if !ok || at < udpWarmUp || at > udpWarmUp+20*time.Second {
		t.Fatalf("udp_blocked first at %v (%v), want just after %v; seen %v", at, ok, udpWarmUp, first)
	}
	if first[model.VerdictOK] >= at {
		t.Errorf("first verdicts %v", first)
	}
	v := h.e.Current()
	if v.Kind != model.VerdictUDPBlocked || v.Summary != udpBlockedSummary || v.Quiet ||
		!slices.Equal(v.Targets, []string{"Cloudflare-STUN", "Meet-STUN", "Teams-Relay", "Twilio-STUN-global"}) {
		t.Fatalf("verdict %+v", v)
	}
	if v.Evidence["udp_blocked_providers"] != 4 || v.Evidence["udp_providers"] != 4 || v.Evidence["udp53_ok"] != 0 ||
		v.Evidence["services_loss"] != 0 || v.Evidence["loss"] != 0 {
		t.Errorf("evidence %v", v.Evidence)
	}
	checkEvidence(t, v.Evidence)
	in := h.st.all()
	if len(in) != 1 || in[0].Kind != model.VerdictUDPBlocked || in[0].Start.Before(t0.Add(udpWarmUp)) || !in[0].End.IsZero() {
		t.Fatalf("incidents %+v", in)
	}
	checkTCPOK(t, h)
}

// Once UDP has answered, the warm-up is over: ten timed-out bursts in a row
// (50 s at 5 s) are enough.
func TestUDPBlockedAfterAnswers(t *testing.T) {
	h := stunHarness(t)
	h.with(targets(between(3*time.Minute, 0, timedOut), allUDP()...))
	first := map[model.VerdictKind]time.Duration{}
	h.track(5*time.Minute, first)
	at, ok := first[model.VerdictUDPBlocked]
	if !ok || at < 3*time.Minute+45*time.Second || at > 3*time.Minute+70*time.Second {
		t.Fatalf("udp_blocked first at %v (%v); seen %v", at, ok, first)
	}
	if h.seen[model.VerdictSTUNBlocked] {
		t.Errorf("stun_blocked although UDP 53 fails too: %v", first) // its recent answers don't count
	}
	checkTCPOK(t, h)
}

// Socket errors (refused, unreachable) are not evidence of a block, and
// they break a run of timeouts.
func TestUDPRefusedIsUnknown(t *testing.T) {
	for name, o := range map[string]func(time.Time) outcome{
		"refused": always(model.ReasonRefused),
		"interleaved": func(at time.Time) outcome {
			if at.Unix()%40 == 0 {
				return outcome{lost: true, reason: model.ReasonUnreachable}
			}
			return timedOut(at)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := stunHarness(t)
			h.with(targets(o, allUDP()...))
			h.run(8 * time.Minute)
			checkNoUDP(t, h)
			checkTCPOK(t, h)
		})
	}
}

// STUN ports blocked while UDP 53 answers: stun_blocked. When UDP 53 stops
// answering too, it becomes udp_blocked after 2 of its bursts (15 s apart)
// timed out.
func TestSTUNBlockedVsUDPBlocked(t *testing.T) {
	h := stunHarness(t)
	h.with(targets(timedOut, stunPorts...))
	h.with(targets(between(5*time.Minute, 0, timedOut), "Cloudflare-UDP53"))
	first := map[model.VerdictKind]time.Duration{}
	h.track(5*time.Minute, first)
	v := h.e.Current()
	if v.Kind != model.VerdictSTUNBlocked || v.Summary != stunBlockedSummary || v.Evidence["udp53_ok"] != 1 {
		t.Fatalf("verdict %+v", v)
	}
	if first[model.VerdictSTUNBlocked] > udpWarmUp+20*time.Second {
		t.Errorf("stun_blocked first at %v", first[model.VerdictSTUNBlocked])
	}
	h.track(3*time.Minute, first)
	v = h.e.Current()
	if v.Kind != model.VerdictUDPBlocked || v.Summary != udpBlockedSummary || v.Evidence["udp53_ok"] != 0 {
		t.Fatalf("verdict %+v", v)
	}
	if at := first[model.VerdictUDPBlocked]; at < 5*time.Minute+15*time.Second || at > 5*time.Minute+30*time.Second {
		t.Errorf("udp_blocked first at %v", at)
	}
	in := h.st.all()
	if len(in) != 2 || in[0].Kind != model.VerdictSTUNBlocked || in[0].End.IsZero() || in[1].Kind != model.VerdictUDPBlocked {
		t.Fatalf("incidents %+v", in)
	}
	checkTCPOK(t, h)
}

// When UDP comes back, the verdict is ok again within 2 minutes and the
// incident ends.
func TestUDPBlockedRecovery(t *testing.T) {
	h := stunHarness(t)
	h.with(targets(between(0, 5*time.Minute, timedOut), allUDP()...))
	h.run(5 * time.Minute)
	if k := h.e.Current().Kind; k != model.VerdictUDPBlocked {
		t.Fatalf("verdict %s", k)
	}
	h.run(2 * time.Minute)
	if v := h.e.Current(); v.Kind != model.VerdictOK || v.Quiet {
		t.Fatalf("verdict %+v 2 minutes after recovery", v)
	}
	in := h.st.all()
	if len(in) != 1 || in[0].End.IsZero() || in[0].End.Before(t0.Add(5*time.Minute)) || in[0].End.After(t0.Add(7*time.Minute)) {
		t.Fatalf("incidents %+v", in)
	}
	checkTCPOK(t, h)
}

// After 6 hours blocked the verdict is Quiet (the same incident goes on);
// recovery clears both within 2 minutes.
func TestUDPBlockedQuiet(t *testing.T) {
	if testing.Short() {
		t.Skip("simulates 6 hours")
	}
	h := stunHarness(t)
	h.with(targets(between(0, 6*time.Hour+10*time.Minute, timedOut), allUDP()...))
	h.run(6 * time.Hour)
	v := h.e.Current()
	if v.Kind != model.VerdictUDPBlocked || v.Quiet {
		t.Fatalf("verdict at 6h %+v", v)
	}
	since := v.Since
	for !v.Quiet && h.now.Before(t0.Add(6*time.Hour+5*time.Minute)) {
		h.run(5 * time.Second)
		v = h.e.Current()
	}
	if d := h.now.Sub(since); !v.Quiet || d < udpQuietAfter || d > udpQuietAfter+10*time.Second {
		t.Fatalf("quiet %v after %v blocked", v.Quiet, d)
	}
	if v.Kind != model.VerdictUDPBlocked || !v.Since.Equal(since) || v.Summary != udpBlockedSummary {
		t.Fatalf("quiet verdict %+v (since %v)", v, since)
	}
	h.run(t0.Add(6*time.Hour + 12*time.Minute).Sub(h.now))
	if v := h.e.Current(); v.Kind != model.VerdictOK || v.Quiet {
		t.Fatalf("verdict after recovery %+v", v)
	}
	if in := h.st.all(); len(in) != 1 || in[0].End.IsZero() {
		t.Fatalf("incidents %+v", in)
	}
	checkTCPOK(t, h)
}

// The TCP rules come first: an ISP problem during a UDP block is an ISP
// verdict, with the TCP rules' own summary.
func TestUDPBelowTCPVerdicts(t *testing.T) {
	h := stunHarness(t)
	h.with(layer(model.LayerEdge, between(4*time.Minute, 0, every(2, model.ReasonTimeout))))
	h.with(internet(between(4*time.Minute, 0, every(2, model.ReasonTimeout))))
	h.with(targets(timedOut, allUDP()...))
	h.run(4 * time.Minute)
	if k := h.e.Current().Kind; k != model.VerdictUDPBlocked {
		t.Fatalf("verdict %s", k)
	}
	h.run(2 * time.Minute)
	v := h.e.Current()
	if v.Kind != model.VerdictISP || v.Summary != "Your ISP's network is dropping packets: the router looks fine, the first hop past it loses 50%." {
		t.Fatalf("verdict %+v", v)
	}
	if _, ok := v.Evidence["udp_blocked_providers"]; ok {
		t.Errorf("evidence %v", v.Evidence)
	}
}

func TestUDPDegraded(t *testing.T) {
	lossy := func(time.Time) outcome {
		return outcome{rtt: 30 * time.Millisecond, jitter: 3 * time.Millisecond, udpLoss: 20}
	}
	jittery := func(time.Time) outcome { return outcome{rtt: 30 * time.Millisecond, jitter: 45 * time.Millisecond} }
	cases := []struct {
		name    string
		fx      []behavior
		want    bool
		summary string
		loss    float64
		late    time.Duration // how late after 3 minutes of the fault it may come (default 45s)
	}{
		{name: "loss on 2 providers", want: true, loss: 0.2,
			fx:      []behavior{targets(between(2*time.Minute, 0, lossy), "Meet-STUN", "Twilio-STUN-global")},
			summary: "Calls may sound choppy: the UDP path to 2 call services shows up to 20% packet loss, while the rest of your connection looks fine."},
		{name: "jitter on 2 providers", want: true,
			fx:      []behavior{targets(between(2*time.Minute, 0, jittery), "Cloudflare-STUN", "Twilio-STUN-global")},
			summary: "Calls may sound choppy: the UDP path to 2 call services shows up to 45 ms of jitter, while the rest of your connection looks fine."},
		// Jitter that dips under the threshold for 40s every 100s: the
		// breaks are shorter than udpPoorGrace and do not restart the clock
		// (it fires once 3 minutes have passed outside a break).
		{name: "jitter with short breaks", want: true, late: 2 * time.Minute,
			fx: []behavior{targets(between(2*time.Minute, 0, func(at time.Time) outcome {
				if at.Sub(t0)%(100*time.Second) < 40*time.Second {
					return outcome{rtt: 30 * time.Millisecond, jitter: 2 * time.Millisecond}
				}
				return jittery(at)
			}), "Cloudflare-STUN", "Twilio-STUN-global")},
			summary: "Calls may sound choppy: the UDP path to 2 call services shows up to 45 ms of jitter, while the rest of your connection looks fine."},
		{name: "one provider", fx: []behavior{targets(between(2*time.Minute, 0, lossy), "Meet-STUN")}},
		{name: "one provider, 2 targets", fx: []behavior{targets(between(2*time.Minute, 0, lossy), "Cloudflare-STUN", "Cloudflare-UDP53")}},
		{name: "under 3 minutes", fx: []behavior{targets(between(2*time.Minute, 3*time.Minute+30*time.Second, lossy), "Meet-STUN", "Twilio-STUN-global")}},
		{name: "dead, not degraded", fx: []behavior{targets(timedOut, "Meet-STUN"), targets(between(2*time.Minute, 0, lossy), "Twilio-STUN-global")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := stunHarness(t)
			h.fx = c.fx
			first := map[model.VerdictKind]time.Duration{}
			h.track(9*time.Minute, first)
			checkTCPOK(t, h)
			if !c.want {
				checkNoUDP(t, h)
				return
			}
			at, ok := first[model.VerdictUDPDegraded]
			late := cmp.Or(c.late, 45*time.Second)
			if !ok || at < 2*time.Minute+udpDegradedFor || at > 2*time.Minute+udpDegradedFor+late {
				t.Fatalf("udp_degraded first at %v (%v); seen %v", at, ok, first)
			}
			v := h.e.Current()
			if v.Kind != model.VerdictUDPDegraded || v.Summary != c.summary || v.Quiet || v.Evidence["udp_poor_providers"] != 2 ||
				v.Evidence["udp_loss"] != c.loss || len(v.Targets) != 2 {
				t.Fatalf("verdict %+v", v)
			}
			checkEvidence(t, v.Evidence)
			if in := h.st.all(); len(in) != 1 || in[0].Kind != model.VerdictUDPDegraded || in[0].PeakLoss != c.loss {
				t.Fatalf("incidents %+v", in)
			}
		})
	}
}

func TestUDPProvider(t *testing.T) {
	for host, want := range map[string]string{
		"stun.l.google.com":              "google.com",
		"global.stun.twilio.com":         "twilio.com",
		"worldaz.tr.teams.microsoft.com": "microsoft.com",
		"STUN.Cloudflare.com.":           "cloudflare.com",
		"stun.example.co.uk":             "example.co.uk",
		"example.com":                    "example.com",
		"192.0.2.7":                      "192.0.2.7",
		"[2001:db8::1]":                  "2001:db8::1",
	} {
		if got := UDPProvider(model.Target{Host: host}); got != want {
			t.Errorf("UDPProvider(%q) = %q, want %q", host, got, want)
		}
	}
}
