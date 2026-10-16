//go:build netns && linux

package netns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// TestHome10 runs the new Home (docs/HOME-API.md, v2: the answer card, the
// connection strip and the 11 tiles) against real faults. Every scenario
// starts fyisp on its own topology with a lab profile that feeds all 11
// tiles through tile: tags, keeps the network path group (gateway, ISP
// edge and anycast, as TestVerdict does) and has four STUN servers on three
// addresses (every IP-literal host is a provider of its own):
//
//	stun-a  203.0.113.30:3478, stun-a53 .30:53 (udp-53)  the shared call path, provider .30
//	stun-b  203.0.113.31:3478                            the shared call path, provider .31
//	meet-*  203.0.113.32:19302 and .32:3478              Google Meet's own (tile:meet)
//
// Zoom, Teams and Slack are judged on the shared path; Microsoft 365 has
// three targets (the majority rule), every other tile one.
//
// Before fyisp starts, 25 hours of good samples at the lab's measured RTT
// are written to its data directory through internal/store, so the usual
// latencies are known and the first day's fixed limits are off, as on an
// instance that has been running for a while (S9 starts fresh instead).
// A poller then records part=now every 5 s into
// FYISP_NETNS_OUT/<scenario>/timeline.jsonl (and a one-line-per-poll
// timeline.txt), and the pass criteria are checked on that recording: a
// failure logs the whole sequence. T0 is when a fault is applied, T1 when
// it is removed, I the probe interval (6 s; 15 s TCP in S1c).
func TestHome10(t *testing.T) {
	parallel(t)

	// S1: a 40 s drop past the router. While it lasts the card is red and
	// every tile Down; afterwards "Working again … dropped for about 40
	// seconds", every tile Good with a drop note, a red bar, the provider
	// hop noting the drop, and one problem from the provider; the strip is
	// good while Working again shows. Then, 90 s after the drop, Zoom stops
	// connecting: an app outage that starts more than 30 s after a drop
	// ended is the app's own (not absorbed into the drop, not blamed on the
	// provider), and it takes the headline while the drop moves to its sub
	// line (B5's precedence).
	t.Run("S1-drop-40s", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{})
		t0 := lab.ispDown(t)
		time.Sleep(40 * time.Second)
		t1 := lab.ispUp(t)
		time.Sleep(90 * time.Second)
		lab.checkDrop(t, t0, t1, 40*time.Second, 10*time.Second, true, t1.Add(60*time.Second), t1.Add(90*time.Second))

		z0 := time.Now()
		lab.nft(t, fmt.Sprintf("add rule inet fyt fault ip daddr %s tcp dport 443 reject with tcp reset", home10Addr("zoom")))
		time.Sleep(210 * time.Second)
		lab.flush(t)
		zs := lab.polls(z0.Add(lab.downBy()), time.Now())
		if len(zs) == 0 {
			t.Fatalf("no polls while Zoom was down")
		}
		for _, p := range zs {
			a := p.n.Answer
			if !strings.HasPrefix(a.Key, "app:zoom:") || a.Headline != "Your internet is fine. Zoom isn’t connecting." ||
				!strings.Contains(a.Sub, "Separately, your internet dropped for about ") || !strings.Contains(a.Sub, " and is back now.") {
				lab.errorf(t, p, "Zoom down after the drop: want the Zoom card with the drop in its sub line")
				break
			}
			// the tile's own cause comes with its first judged minute (up to
			// 100 s after it went down)
			if z := p.n.tile("zoom"); z.State != "down" || p.t.After(z0.Add(lab.downBy()+2*time.Minute)) && !strings.Contains(z.Reason, "Likely cause: Zoom’s side") {
				lab.errorf(t, p, "Zoom down after the drop: zoom %s %q, want down on Zoom’s side", z.State, z.Reason)
				break
			}
		}
		p := lab.last(t)
		if !slices.ContainsFunc(p.n.Problems.Items, func(it h10Problem) bool { return it.CauseKind == "app" && strings.Contains(it.What, "Zoom") }) {
			lab.errorf(t, p, "Zoom down after the drop: want a Zoom problem from the app's side")
		}
	})

	// S1b: a 20 s drop is no drop: from a minute after it the card is good,
	// every tile Good, and nothing remains (no Working again, no problem,
	// no drop note, no red bar).
	t.Run("S1b-drop-20s", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{})
		t0 := lab.ispDown(t)
		time.Sleep(20 * time.Second)
		t1 := lab.ispUp(t)
		time.Sleep(3 * time.Minute)
		after := lab.polls(t1.Add(60*time.Second), time.Now())
		if len(after) == 0 {
			t.Fatal("no polls after the drop")
		}
		for _, p := range after {
			n := p.n
			switch {
			case !home10Good(n):
				lab.errorf(t, p, "a 20 s drop: want the good card, every tile and the strip good")
			case len(n.Problems.Items) != 0:
				lab.errorf(t, p, "a 20 s drop: want no problems, got %d", len(n.Problems.Items))
			case slices.ContainsFunc(n.Tiles, func(x h10Tile) bool { return strings.HasPrefix(x.Note, "Dropped") }):
				lab.errorf(t, p, "a 20 s drop: want no drop note")
			case slices.ContainsFunc(n.Tiles, func(x h10Tile) bool { return x.barAt(t0).State == "down" }):
				lab.errorf(t, p, "a 20 s drop: want no red bar")
			default:
				continue
			}
			break
		}
	})

	// S1c: the same 40 s drop at the production cadence (TCP every 15 s;
	// STUN every 6 s, the shortest a --config file may set): its reported
	// length comes from the 1 s path probes, within 20 s and in 10 s steps.
	t.Run("S1c-drop-40s-cadence", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{tcpIv: 15 * time.Second})
		t0 := lab.ispDown(t)
		time.Sleep(40 * time.Second)
		t1 := lab.ispUp(t)
		time.Sleep(90 * time.Second)
		// At 15 s, a 40 s drop misses only 2 or 3 checks of a single-target
		// tile: not every tile need go Down while it lasts.
		lab.checkDrop(t, t0, t1, 40*time.Second, 20*time.Second, false, t1.Add(60*time.Second), t1.Add(90*time.Second))
	})

	// S2: a 2-minute provider outage: the provider is blamed while it
	// lasts, and "was down for about 2 minutes" after.
	t.Run("S2-outage-2m", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{})
		t0 := lab.ispDown(t)
		time.Sleep(120 * time.Second)
		t1 := lab.ispUp(t)
		time.Sleep(180 * time.Second)
		iv := lab.iv
		during := lab.polls(t0.Add(60*time.Second), t1)
		if len(during) == 0 {
			t.Fatal("no polls during the outage")
		}
		for _, p := range during {
			n := p.n
			a := n.Answer
			if !strings.HasPrefix(a.Key, "isp:") || a.Tone != "bad" || a.Tab != "✕ Provider problem" ||
				n.Path[0].State != "good" || n.Path[1].State != "down" {
				lab.errorf(t, p, "provider outage: want the provider card (isp, bad, ✕ Provider problem), router good, provider down")
				break
			}
			if x := n.notTile(func(x h10Tile) bool {
				return x.State == "down" && x.Since != nil && within(time.UnixMilli(*x.Since), t0, iv+time.Second)
			}); x != nil {
				lab.errorf(t, p, "provider outage: tile %s %s since %s, want down since T0 ± %s", x.ID, x.State, msClock(x.Since), iv)
				break
			}
		}
		lab.checkAgain(t, t1, t1.Add(60*time.Second), "Working again. Your internet was down for about 2 minutes (", "provider")
		p := lab.last(t)
		if items := p.n.Problems.Items; len(items) != 1 {
			lab.errorf(t, p, "want 1 problem, got %d", len(items))
		} else if it := items[0]; it.CauseKind != "provider" || it.Evidence == "" ||
			!within(time.UnixMilli(it.To), time.UnixMilli(it.From).Add(120*time.Second), 2*iv) {
			lab.errorf(t, p, "problem %+v: want cause provider with evidence, about 120 s ± %s", it, 2*iv)
		}
		if d := p.n.Answer.Day; d == nil || !strings.Contains(d.Text, "1 short problem, 1 likely from your internet provider") {
			lab.errorf(t, p, "want the 24-hour line to count 1 short problem from the provider")
		}
	})

	// S2b: 40% loss inside the provider for 2.5 minutes: the red provider
	// card (the amber "losing some data" one is for edge loss under 30%),
	// the call tiles out of Down within 3 minutes of the end, then "had
	// trouble for about N minutes" (the third Working again variant: traffic
	// kept flowing, so never "was down" or "Nothing could get through").
	t.Run("S2b-provider-lossy", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{})
		t0 := time.Now()
		lab.nsRun(t, lab.isp, "tc", "qdisc", "add", "dev", "i0", "root", "netem", "loss", "40%")
		time.Sleep(150 * time.Second)
		lab.nsRun(t, lab.isp, "tc", "qdisc", "del", "dev", "i0", "root")
		t1 := time.Now()
		// Calls may stay Choppy a while after Down; until they are Good,
		// their card (row 9) carries the Working again sentence.
		time.Sleep(10 * time.Minute)
		if p := lab.first(t0, t1, func(n *h10Now) bool {
			return strings.HasPrefix(n.Answer.Key, "isp:") && n.Path[1].State == "slow"
		}); p == nil {
			t.Errorf("40%% loss in the provider's network: the card never named the provider with its hop slow")
		} else if p.n.Answer.Tone != "bad" || p.n.Answer.Tab != "✕ Provider problem" {
			lab.errorf(t, *p, "want the red ✕ Provider problem card")
		}
		for _, p := range lab.polls(t0, time.Now()) {
			if strings.HasPrefix(p.n.Answer.Key, "isp_lossy:") {
				lab.errorf(t, p, "40%% loss in the provider's network: want the red card, not the amber one")
				break
			}
		}
		for _, p := range lab.polls(t1, time.Now()) {
			if slices.ContainsFunc(p.n.Problems.Items, func(it h10Problem) bool { return strings.HasPrefix(it.What, "Nothing could get through") }) {
				lab.errorf(t, p, "40%% loss: want no \"Nothing could get through\" problem, traffic kept flowing")
				break
			}
		}
		for _, p := range lab.polls(t1.Add(3*time.Minute), time.Now()) {
			if slices.ContainsFunc(p.n.Tiles, func(x h10Tile) bool { return x.State == "down" }) {
				lab.errorf(t, p, "a tile still down 3 minutes after the loss ended")
				break
			}
		}
		if p := lab.first(t1, time.Now(), func(n *h10Now) bool {
			return strings.Contains(n.Answer.Sub, "Separately, your internet had trouble for about ")
		}); p != nil {
			t.Logf("Working again in the app card's sub line %s after T1", p.t.Sub(t1).Round(time.Second))
		}
		lab.checkAgain(t, t1, t1.Add(10*time.Minute), "Working again. Your internet had trouble for about ", "provider")
	})

	// S3: 6% loss inside the provider for 7 minutes: calls Choppy, never
	// Down; every other tile Good at every poll; the card never red; the
	// provider blamed; Choppy ends 5 to 8 minutes after the loss (the
	// 3-minute call window, then 5 good minutes). Choppy comes within 6
	// minutes (the plan said 5): the first minutes' 3-minute windows hold
	// only a minute or two of loss, about 2%, then 3 bad minutes of 5 are
	// needed, and a minute is judged 40 s after its end (the engine holds
	// it for the slowest series, the 15 s UDP 53 one).
	t.Run("S3-provider-loss-6", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{})
		t0 := time.Now()
		lab.nsRun(t, lab.isp, "tc", "qdisc", "add", "dev", "i0", "root", "netem", "loss", "6%")
		time.Sleep(7 * time.Minute)
		lab.nsRun(t, lab.isp, "tc", "qdisc", "del", "dev", "i0", "root")
		t1 := time.Now()
		time.Sleep(9 * time.Minute)
		all := lab.polls(t0, time.Now())
		for _, p := range all {
			if x := p.n.notTile(func(x h10Tile) bool { return x.Kind == "call" || x.State == "good" }); x != nil {
				lab.errorf(t, p, "provider loss: %s %s, want every app and stream tile good at every poll", x.ID, x.State)
				break
			}
		}
		for _, p := range all {
			if p.n.Answer.Tone == "bad" || slices.ContainsFunc(p.n.Tiles, func(x h10Tile) bool { return x.Kind == "call" && x.State == "down" }) {
				lab.errorf(t, p, "provider loss: want no red card and no call tile down")
				break
			}
		}
		choppy := func(n *h10Now) bool {
			return n.notTile(func(x h10Tile) bool { return x.Kind != "call" || x.State == "slow" && x.Word == "Choppy" }) == nil
		}
		if p := lab.first(t0, t0.Add(6*time.Minute+home10Poll), choppy); p == nil {
			t.Errorf("provider loss: not every call tile Choppy by T0+6 min")
		} else {
			t.Logf("provider loss: every call tile Choppy %s after T0", p.t.Sub(t0).Round(time.Second))
			for _, q := range lab.polls(p.t, t1) {
				if !choppy(q.n) {
					lab.errorf(t, q, "provider loss: call tiles left Choppy before the loss ended")
					break
				}
			}
		}
		// The cause. With an isp incident from the verdict it is the
		// provider. Without one, the layers decide, and the edge's measured
		// loss in this lab sits right at the 2% layerLossy line (6% netem on
		// the replies, the edge's 1 s pings), so "the internet past your
		// provider" is accepted too. The thresholds are kept on purpose; the
		// flip-flop between the two is a follow-up after the release.
		inc, _ := lab.incidents(t, t0)
		isp := slices.ContainsFunc(inc, func(in incidentAPI) bool { return in.Kind == "isp" })
		p := lab.last(t)
		ok := func(it h10Problem) bool {
			return it.CauseKind == "provider" || !isp && it.Cause == "the internet past your provider"
		}
		if items := p.n.Problems.Items; len(items) == 0 || !slices.ContainsFunc(items, ok) ||
			slices.ContainsFunc(items, func(it h10Problem) bool { return !ok(it) }) {
			lab.errorf(t, p, "provider loss (isp incident: %v): want the problems blamed on the provider%s", isp,
				map[bool]string{false: " or the internet past it", true: ""}[isp])
		}
		for _, id := range []string{"zoom", "teams", "meet", "slack"} {
			g := lab.first(t1, time.Now(), func(n *h10Now) bool { return n.tile(id).State == "good" })
			switch {
			case g == nil:
				t.Errorf("%s: still not Good %s after the loss ended", id, time.Since(t1).Round(time.Second))
			case g.t.Before(t1.Add(5*time.Minute)) || g.t.After(t1.Add(8*time.Minute)):
				t.Errorf("%s: Good again %s after the loss ended, want 5 to 8 minutes", id, g.t.Sub(t1).Round(time.Second))
			default:
				t.Logf("%s: Good again %s after the loss ended", id, g.t.Sub(t1).Round(time.Second))
			}
		}
	})

	// S4: Zoom's own outage: a green card that says so and names the
	// meeting apps that work; nothing else changes.
	t.Run("S4-app-outage", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{})
		t0 := time.Now()
		lab.nft(t, fmt.Sprintf("add rule inet fyt fault ip daddr %s tcp dport 443 reject with tcp reset", home10Addr("zoom")))
		time.Sleep(4 * time.Minute)
		lab.flush(t)
		t1 := time.Now()
		time.Sleep(30 * time.Second)
		if p := lab.first(t0, t0.Add(lab.downBy()+home10Poll), func(n *h10Now) bool { return n.tile("zoom").State == "down" }); p == nil {
			t.Errorf("Zoom not down by T0+3I+10s+holdFor (%s)", lab.downBy())
		}
		for _, p := range lab.polls(t0.Add(lab.downBy()+home10Poll), t1) {
			n := p.n
			a := n.Answer
			if a.Tone != "ok" || a.Headline != "Your internet is fine. Zoom isn’t connecting." ||
				!strings.Contains(a.Sub, "Teams and Google Meet are working if you need to switch") {
				lab.errorf(t, p, "Zoom down: want the green Zoom card naming Teams and Google Meet")
				break
			}
			// the tile's own cause comes with its first judged minute (up to
			// 100 s after it went down)
			if z := n.tile("zoom"); z.State != "down" || p.t.After(t0.Add(lab.downBy()+2*time.Minute)) && !strings.Contains(z.Reason, "Likely cause: Zoom’s side") {
				lab.errorf(t, p, "Zoom down: zoom %s %q, want down on Zoom’s side", z.State, z.Reason)
				break
			}
			if x := n.notTile(func(x h10Tile) bool { return x.ID == "zoom" || x.State == "good" }); x != nil {
				lab.errorf(t, p, "Zoom down: %s %s, want the other 10 tiles good", x.ID, x.State)
				break
			}
			if slices.ContainsFunc(n.Path, func(x h10Hop) bool { return x.State != "good" }) {
				lab.errorf(t, p, "Zoom down: want the whole strip good")
				break
			}
		}
		p := lab.last(t)
		if !slices.ContainsFunc(p.n.Problems.Items, func(it h10Problem) bool { return it.CauseKind == "app" && strings.Contains(it.What, "Zoom") }) {
			lab.errorf(t, p, "want a Zoom problem from the app's side")
		}
	})

	// S4b: one of Microsoft 365's three targets down is no outage.
	t.Run("S4b-majority", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{})
		t0 := time.Now()
		lab.nft(t, fmt.Sprintf("add rule inet fyt fault ip daddr %s drop", home10Addr("m365-b")))
		time.Sleep(4 * time.Minute)
		lab.flush(t)
		t1 := time.Now()
		time.Sleep(30 * time.Second)
		for _, p := range lab.polls(t0, t1.Add(30*time.Second)) {
			if p.n.tile("m365").State != "good" || !strings.HasPrefix(p.n.Answer.Key, "good") {
				lab.errorf(t, p, "1 of 3 Microsoft 365 targets down: want m365 good and the good card")
				break
			}
		}
	})

	// S5: the router stops answering: the router is blamed, not the
	// provider, with the router wording (the lab's uplink is a veth).
	t.Run("S5-router-down", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{})
		lab.nsRun(t, lab.gw, "nft", "add table inet fytg")
		lab.nsRun(t, lab.gw, "nft", "add chain inet fytg gwfwd { type filter hook forward priority 0; policy drop; }")
		lab.nsRun(t, lab.gw, "nft", "add chain inet fytg gwin { type filter hook input priority 0; policy drop; }")
		t0 := time.Now()
		time.Sleep(120 * time.Second)
		lab.nsRun(t, lab.gw, "nft", "delete table inet fytg")
		t1 := time.Now()
		time.Sleep(180 * time.Second)
		p := lab.first(t0, t1, func(n *h10Now) bool { return strings.Contains(n.Answer.Headline, "home router") })
		if p == nil {
			t.Fatalf("router down: the headline never named the home router")
		}
		t.Logf("router named after %s", p.t.Sub(t0).Round(time.Second))
		from := t0.Add(lab.downBy() + home10Poll)
		if p.t.After(from) {
			from = p.t
		}
		for _, q := range lab.polls(from, t1) {
			n := q.n
			switch {
			case n.Path[0].State != "down" || n.Path[0].Label != "Your home router" || n.PathHint == "":
				lab.errorf(t, q, "router down: want the router hop down, labelled Your home router, with the path hint")
			case n.Path[1].State == "down" || strings.Contains(n.Answer.Headline, "provider"):
				lab.errorf(t, q, "router down: the provider is blamed")
			case n.notTile(func(x h10Tile) bool { return x.State == "down" }) != nil:
				lab.errorf(t, q, "router down: want every tile down")
			default:
				continue
			}
			break
		}
		lab.checkAgain(t, t1, t1.Add(60*time.Second), "Working again. Your internet was down for about 2 minutes (", "home")
	})

	// S6: jitter on the call path makes calls Choppy (the panel's jitter
	// figure is over its limit) while apps and streams stay Good; a 20 ms
	// ± 5 ms delay on everything first is no problem at all. The jitter is
	// put on the STUN traffic only (a prio qdisc filter): the plan's
	// 20 ms ± 25 ms normal gives an in-burst jitter of about 28 ms, under
	// the 30 ms limit, and the same netem on TCP makes the lab's app
	// minutes (10 checks) cross the 30 ms Slow limit by chance. 100 ms
	// ± 90 ms (uniform) gives about 60 ms of jitter at a 100 ms delay.
	// Choppy from jitter takes up to 6 minutes: a minute is bad once the
	// median of its 3-minute window's minute jitters is, and Choppy needs
	// 3 bad minutes of 5.
	t.Run("S6-jitter", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{})
		c0 := time.Now()
		lab.nsRun(t, lab.isp, "tc", "qdisc", "add", "dev", ispUplink, "root", "netem", "delay", "20ms", "5ms")
		time.Sleep(5 * time.Minute)
		lab.nsRun(t, lab.isp, "tc", "qdisc", "del", "dev", ispUplink, "root")
		c1 := time.Now()
		for _, p := range lab.polls(c0, c1) {
			if x := p.n.notTile(func(x h10Tile) bool { return x.State == "good" }); x != nil {
				lab.errorf(t, p, "delay 20ms ± 5ms: %s %s, want every tile good", x.ID, x.State)
				break
			}
		}
		t0 := time.Now()
		lab.shapeSTUN3(t, "delay", "100ms", "90ms")
		time.Sleep(6*time.Minute + 30*time.Second)
		for _, id := range []string{"zoom", "meet"} {
			d := lab.tileDetail(t, id)
			for _, m := range d.Metrics {
				if m.Label == "Jitter" {
					t.Logf("%s panel: jitter %s (%s), bad %v", id, m.Value, m.Limit, m.Bad != nil && *m.Bad)
					if m.Bad == nil || !*m.Bad {
						t.Errorf("%s panel: jitter %s not over its limit", id, m.Value)
					}
				}
			}
		}
		lab.nsRun(t, lab.isp, "tc", "qdisc", "del", "dev", ispUplink, "root")
		t1 := time.Now()
		lab.logPolls(t, "jitter")
		if p := lab.first(t0, t0.Add(6*time.Minute+home10Poll), func(n *h10Now) bool {
			return n.notTile(func(x h10Tile) bool { return x.Kind != "call" || x.Word == "Choppy" }) == nil
		}); p == nil {
			t.Errorf("jitter: not every call tile Choppy by T0+6 min")
		} else {
			t.Logf("jitter: every call tile Choppy %s after T0", p.t.Sub(t0).Round(time.Second))
		}
		for _, p := range lab.polls(t0, t1) {
			if x := p.n.notTile(func(x h10Tile) bool { return x.Kind == "call" || x.State == "good" }); x != nil {
				lab.errorf(t, p, "jitter: %s %s, want every app and stream tile good", x.ID, x.State)
				break
			}
		}
	})

	// S7: a latency ladder past the provider. Apps are Slow from 60 ms on
	// and never Down from latency; calls are Choppy at 200 ms and Down at
	// 450 ms; streams are never Down. The steps are 5, 6 and 4 minutes:
	// Slow and Choppy need 3 bad minutes of 5, and a call minute is bad
	// on delay once 2 of its window's 3 minutes are (about 4.5 minutes);
	// Down on delay shows at once.
	t.Run("S7-latency-ladder", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{})
		type step struct {
			delay    string
			from, to time.Time
		}
		var steps []step
		for i, d := range []string{"60ms", "200ms", "450ms"} {
			op := "change"
			if i == 0 {
				op = "add"
			}
			lab.nsRun(t, lab.isp, "tc", "qdisc", op, "dev", ispUplink, "root", "netem", "delay", d)
			s := step{delay: d, from: time.Now()}
			time.Sleep([]time.Duration{5 * time.Minute, 6 * time.Minute, 4 * time.Minute}[i])
			s.to = time.Now()
			steps = append(steps, s)
		}
		lab.nsRun(t, lab.isp, "tc", "qdisc", "del", "dev", ispUplink, "root")
		all := lab.polls(steps[0].from, time.Now())
		for _, p := range all {
			if x := p.n.notTile(func(x h10Tile) bool { return x.Kind == "call" || x.State != "down" }); x != nil {
				lab.errorf(t, p, "latency: %s down, want apps and streams never down from latency", x.ID)
				break
			}
		}
		for _, s := range steps {
			end := lab.polls(s.to.Add(-15*time.Second), s.to)
			if len(end) == 0 {
				t.Errorf("%s: no poll at the end of the step", s.delay)
				continue
			}
			p := end[len(end)-1]
			n := p.n
			for _, x := range n.Tiles {
				var ok bool
				switch {
				case x.Kind == "app":
					ok = x.State == "slow"
				case x.Kind == "stream":
					ok = x.State == "good" || x.State == "slow"
				case s.delay == "60ms":
					ok = x.State == "good"
				case s.delay == "200ms":
					ok = x.State == "slow" && x.Word == "Choppy"
				default:
					ok = x.State == "down"
				}
				if !ok {
					lab.errorf(t, p, "delay %s: %s (%s) %s at the end of the step", s.delay, x.ID, x.Kind, x.State)
				}
			}
			if s.delay != "450ms" {
				for _, q := range lab.polls(s.from, s.to) {
					if slices.ContainsFunc(q.n.Tiles, func(x h10Tile) bool { return x.Kind == "call" && x.State == "down" }) {
						lab.errorf(t, q, "delay %s: a call tile down", s.delay)
						break
					}
				}
			}
		}
	})

	// S8: fyisp stopped (SIGSTOP) for 150 s, like a sleeping laptop: the
	// first answer after it is stale, no tile says Down, the gap leaves no
	// drop and no problem, and the page is fresh again within 30 s.
	t.Run("S8-stale", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{})
		pid := lab.fy.cmd.Process.Pid
		// pause stops fyisp for 150 s and returns the answer to a request
		// queued while it was stopped, and when it was resumed.
		pause := func() (poll, time.Time) {
			if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
			time.Sleep(150 * time.Second)
			first := make(chan poll, 1)
			go func() {
				c := &http.Client{Timeout: time.Minute, Transport: &http.Transport{DialContext: dialIn(lab.cli), DisableKeepAlives: true}}
				first <- lab.fetchNow(c)
			}()
			time.Sleep(2 * time.Second)
			if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
				t.Fatal(err)
			}
			tc := time.Now()
			p := <-first
			lab.record(p)
			return p, tc
		}
		// On resuming, fyisp's probes and its engine tick race the queued
		// request: when a check made after the pause is in the answer, the
		// request lost the race and says nothing about the pause. Pause
		// again then (up to 3 times; it lost 1 of 5 runs).
		t0 := time.Now()
		var tc time.Time
		for try := 1; ; try++ {
			var p poll
			p, tc = pause()
			if p.err != nil {
				t.Fatalf("the first request after resuming: %v", p.err)
			}
			age := p.t.Sub(time.UnixMilli(p.n.LastCheck))
			switch {
			case p.n.Stale || age >= 2*time.Minute:
				t.Logf("first answer after resuming: stale %v, last check %s old, key %s", p.n.Stale, age.Round(time.Second), p.n.Answer.Key)
			case !time.UnixMilli(p.n.LastCheck).After(tc.Add(-time.Second)):
				lab.errorf(t, p, "the first answer after 150 s stopped: want stale, or last_check 2 min old")
			case try < 3:
				t.Logf("pause %d: a check made after resuming beat the queued request (last check %s old); pausing again", try, age.Round(time.Second))
				time.Sleep(time.Minute)
				continue
			default:
				lab.errorf(t, p, "3 pauses: a check made after resuming always beat the queued request")
			}
			break
		}
		time.Sleep(3 * time.Minute)
		if q := lab.first(tc, tc.Add(30*time.Second), func(n *h10Now) bool { return !n.Stale && !strings.HasPrefix(n.Answer.Key, "stale") }); q == nil {
			t.Errorf("still stale 30 s after resuming")
		}
		for _, q := range lab.polls(t0, time.Now()) {
			n := q.n
			switch {
			case n.notTile(func(x h10Tile) bool { return x.State != "down" }) != nil:
				lab.errorf(t, q, "after a pause: a tile is down")
			case n.Answer.Tone == "bad":
				lab.errorf(t, q, "after a pause: a red card")
			case len(n.Problems.Items) != 0:
				lab.errorf(t, q, "after a pause: want no problems, got %d", len(n.Problems.Items))
			case slices.ContainsFunc(n.Tiles, func(x h10Tile) bool { return strings.HasPrefix(x.Note, "Dropped") }):
				lab.errorf(t, q, "after a pause: a drop note")
			default:
				continue
			}
			break
		}
	})

	// S8b: fyisp not running for 6 minutes: on restart the page says
	// checking (not stale, though its last stored check is 6 minutes old);
	// afterwards the 5-minute bar inside the gap is grey, the gap is no
	// problem, the footer is unchanged, and the bars from before the stop
	// are rebuilt by the replay. The stop is 30 s before a 5-minute
	// boundary, so that a whole bar falls inside the 6 minutes.
	t.Run("S8b-not-running", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{})
		time.Sleep(time.Minute) // a few closed minutes of its own before the stop
		b := time.Now().Truncate(5 * time.Minute).Add(5 * time.Minute)
		if time.Until(b) < 45*time.Second {
			b = b.Add(5 * time.Minute)
		}
		time.Sleep(time.Until(b.Add(-30 * time.Second)))
		before := lab.last(t)
		ts := time.Now()
		lab.stopFyisp(t, lab.fy, 0)
		time.Sleep(6 * time.Minute)
		tr := time.Now()
		lab.start(t)
		ready := lab.waitPoll(t, "good after the restart", 4*time.Minute, home10Good)
		time.Sleep(time.Minute)
		for _, p := range lab.polls(tr, ready.t) {
			if p.n.Stale || strings.HasPrefix(p.n.Answer.Key, "stale") {
				lab.errorf(t, p, "after a restart: want checking, not stale")
				break
			}
		}
		if f := lab.polls(tr, ready.t); len(f) == 0 || !strings.HasPrefix(f[0].n.Answer.Key, "checking") {
			t.Errorf("after a restart: the first answer is not checking")
		}
		after := lab.last(t)
		// The text names the day relative to today ("yesterday 10:00 pm",
		// then "Oct 11"): compare it only within one local day.
		sameDay := time.UnixMilli(before.n.At).Format(time.DateOnly) == time.UnixMilli(after.n.At).Format(time.DateOnly)
		if after.n.WatchingSince != before.n.WatchingSince || sameDay && after.n.Footer != before.n.Footer {
			t.Errorf("footer %q (since %d) after the restart, want %q (since %d)", after.n.Footer, after.n.WatchingSince, before.n.Footer, before.n.WatchingSince)
		}
		if items := after.n.Problems.Items; len(items) != 0 {
			lab.errorf(t, after, "the gap: want no problems, got %d", len(items))
		}
		for _, x := range after.n.Tiles {
			gap := false
			for _, b := range x.Bars {
				from := time.UnixMilli(b.From)
				if !from.Before(ts) && !from.Add(5*time.Minute).After(tr) {
					gap = true
					if b.State != "none" {
						lab.errorf(t, after, "%s: the bar at %s inside the gap is %s, want none", x.ID, from.Format(time.TimeOnly), b.State)
					}
				}
			}
			if !gap {
				t.Errorf("%s: no bar inside the gap %s-%s", x.ID, ts.Format(time.TimeOnly), tr.Format(time.TimeOnly))
			}
			old := before.n.tile(x.ID)
			for _, b := range x.Bars {
				if time.UnixMilli(b.From).Add(5 * time.Minute).After(ts) {
					continue
				}
				if i := slices.IndexFunc(old.Bars, func(o h10Bar) bool { return o.From == b.From }); i >= 0 && old.Bars[i].State != b.State {
					t.Errorf("%s: the bar at %s was %s before the stop, %s after the replay", x.ID,
						time.UnixMilli(b.From).Format(time.TimeOnly), old.Bars[i].State, b.State)
				}
			}
		}
	})

	// S9 and S11: a fresh data directory. The page says checking first,
	// then that fyisp is still learning (fixed limits: no usual in the
	// panels, no 24-hour line). The listener binds 0.0.0.0 so that S11 can
	// ask from another machine: on the same machine, from the gateway's
	// namespace, and through a proxy (X-Forwarded-For) the first hop is
	// "Your home router" with the path hint, as the lab's uplink is no
	// Wi-Fi. (The same-machine rule itself is covered by the handler tests.)
	t.Run("S9-learning-S11-viewer", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{fresh: true, listen: "0.0.0.0:3000"})
		p := lab.waitPoll(t, "learning", 4*time.Minute, func(n *h10Now) bool {
			return strings.HasPrefix(n.Answer.Key, "good") && n.Learning != nil
		})
		ps := lab.polls(time.Time{}, p.t)
		if f := ps[0].n; !strings.HasPrefix(f.Answer.Key, "checking") || f.Problems.Summary != "Checking…" || f.Answer.Day != nil {
			lab.errorf(t, ps[0], "a fresh start: want checking first, with Checking… and no 24-hour line")
		}
		if l := p.n.Learning; !strings.Contains(l.Text, "still learning") {
			lab.errorf(t, p, "learning %+v: want the learning note", l)
		}
		if p.n.Answer.Day != nil {
			lab.errorf(t, p, "learning: want no 24-hour line")
		}
		d := lab.tileDetail(t, "m365")
		if len(d.Metrics) != 0 || d.UsualMs != nil {
			t.Errorf("m365 panel while learning: metrics %+v usual %v, want none (fixed limits)", d.Metrics, d.UsualMs)
		}

		viewers := []struct {
			name string
			get  func() (*http.Response, error)
		}{
			{"same machine", func() (*http.Response, error) { return lab.client.Get("http://" + listenAddr + homeAPI + "?part=now") }},
			{"another machine", func() (*http.Response, error) {
				c := &http.Client{Timeout: httpTimeout, Transport: &http.Transport{DialContext: dialIn(lab.gw), DisableKeepAlives: true}}
				return c.Get("http://10.99.1.2:3000" + homeAPI + "?part=now")
			}},
			{"through a proxy", func() (*http.Response, error) {
				r, _ := http.NewRequest(http.MethodGet, "http://"+listenAddr+homeAPI+"?part=now", nil)
				r.Header.Set("X-Forwarded-For", "192.0.2.10")
				return lab.client.Do(r)
			}},
		}
		for _, v := range viewers {
			resp, err := v.get()
			if err != nil {
				t.Errorf("%s: %v", v.name, err)
				continue
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lab.save(t, "viewer-"+strings.ReplaceAll(v.name, " ", "-")+".json", b)
			var n h10Now
			if err := json.Unmarshal(b, &n); err != nil || resp.StatusCode != http.StatusOK || len(n.Path) != 3 {
				t.Errorf("%s: %s %v: %s", v.name, resp.Status, err, b)
				continue
			}
			t.Logf("%s: %q, hint %q", v.name, n.Path[0].Label, n.PathHint)
			if n.Path[0].Label != "Your home router" || n.PathHint == "" {
				t.Errorf("%s: first hop %q, hint %q, want Your home router with the hint", v.name, n.Path[0].Label, n.PathHint)
			}
		}
	})

	// S10: all UDP to the STUN servers dropped: the blocked card, the call
	// tiles Choppy ("can't use its fast path"), everything else Good.
	t.Run("S10-udp-blocked", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{})
		t0 := time.Now()
		lab.nft(t, fmt.Sprintf("add rule inet fyt fault ip daddr { %s, %s, %s } meta l4proto udp drop", stunAddr, stunAddr2, stunAddr3))
		time.Sleep(150 * time.Second)
		v, _ := lab.verdict(t)
		lab.flush(t)
		t1 := time.Now()
		if v.Kind != "udp_blocked" {
			t.Errorf("verdict %s, want udp_blocked", v.Kind)
		}
		p := lab.first(t0, t1, func(n *h10Now) bool { return strings.HasPrefix(n.Answer.Key, "udp_blocked:") })
		if p == nil {
			t.Fatalf("UDP blocked: the card never said so")
		}
		t.Logf("blocked card after %s", p.t.Sub(t0).Round(time.Second))
		lab.checkBlocked(t, p.t, t1, "zoom", "teams", "meet", "slack")
		lab.checkWebGood(t, t0, t1)
		q := lab.last(t)
		if !slices.ContainsFunc(q.n.Problems.Items, func(it h10Problem) bool {
			return strings.Contains(it.What, "fast path") && strings.HasPrefix(it.Cause, "a firewall or VPN")
		}) {
			lab.errorf(t, q, "want a problem for the blocked fast path")
		}
	})

	// S10b: only the STUN port 3478 is dropped; Google's 19302 and UDP 53
	// answer: stun_blocked (the same card), and Meet, which has 19302, is
	// fine.
	t.Run("S10b-stun-3478-blocked", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{})
		t0 := time.Now()
		lab.nft(t, fmt.Sprintf("add rule inet fyt fault udp dport %d drop", stunPortA))
		time.Sleep(150 * time.Second)
		v, _ := lab.verdict(t)
		lab.flush(t)
		t1 := time.Now()
		if v.Kind != "stun_blocked" {
			t.Errorf("verdict %s, want stun_blocked", v.Kind)
		}
		p := lab.first(t0, t1, func(n *h10Now) bool { return strings.HasPrefix(n.Answer.Key, "udp_blocked:") })
		if p == nil {
			t.Fatalf("STUN 3478 blocked: the card never said so")
		}
		lab.checkBlocked(t, p.t, t1, "zoom", "teams", "slack")
		for _, q := range lab.polls(t0, t1) {
			if m := q.n.tile("meet"); m.State != "good" {
				lab.errorf(t, q, "3478 blocked, 19302 open: meet %s, want good", m.State)
				break
			}
		}
		lab.checkWebGood(t, t0, t1)
	})

	// S10c: one STUN server on 3478 stops answering while another provider
	// on 3478, and Google on 19302, answer: the port is open, so the dead
	// server is its own problem: no blocked verdict, every tile Good.
	t.Run("S10c-one-stun-server-dead", func(t *testing.T) {
		parallel(t)
		lab := startHome10(t, home10Opts{})
		t0 := time.Now()
		lab.nft(t, fmt.Sprintf("add rule inet fyt fault ip daddr %s udp dport %d drop", stunAddr, stunPortA))
		time.Sleep(180 * time.Second)
		v, _ := lab.verdict(t)
		lab.flush(t)
		t1 := time.Now()
		if v.Kind == "udp_blocked" || v.Kind == "stun_blocked" {
			t.Errorf("verdict %s with 3478 open on another provider, want no block", v.Kind)
		}
		for _, p := range lab.polls(t0, t1) {
			if !strings.HasPrefix(p.n.Answer.Key, "good") || p.n.notTile(func(x h10Tile) bool { return x.State == "good" }) != nil {
				lab.errorf(t, p, "one STUN server dead: want the good card and every tile good")
				break
			}
		}
	})
}

// ---------------------------------------------------------------------------
// The lab.

const (
	home10Poll = 5 * time.Second
	home10Seed = 25 * time.Hour
	homeAPI    = "/api/home"
)

// home10Web are the lab's TCP targets and their tiles, on home10Addrs in
// this order.
var home10Web = []struct{ name, tile string }{
	{"zoom", "zoom"}, {"teams", "teams"}, {"meet", "meet"}, {"slack", "slack"},
	{"m365-a", "m365"}, {"m365-b", "m365"}, {"m365-c", "m365"},
	{"gws", "gws"}, {"github", "github"}, {"dropbox", "dropbox"}, {"onedrive", "onedrive"},
	{"youtube", "youtube"}, {"netflix", "netflix"},
}

// home10Addrs are the TCP targets' addresses (203.0.113.100-.112, clear of
// hopshare's .40-.46 and iperf's .60-.63).
var home10Addrs = func() []netip.Addr {
	var out []netip.Addr
	for i := range home10Web {
		out = append(out, netip.AddrFrom4([4]byte{203, 0, 113, byte(100 + i)}))
	}
	return out
}()

func home10Addr(name string) netip.Addr {
	return home10Addrs[slices.IndexFunc(home10Web, func(w struct{ name, tile string }) bool { return w.name == name })]
}

// home10Profile is the lab profile: TCP targets every tcpIv, STUN every
// 6 s (stun-a53 every 15 s, like Cloudflare-UDP53).
func home10Profile(tcpIv time.Duration) string {
	var b strings.Builder
	b.WriteString("name: netns-home10\nversion: \"1\"\ngroups:\n  - {id: apps, title: Apps}\n  - {id: calls, title: Calls}\ntargets:\n")
	for i, w := range home10Web {
		fmt.Fprintf(&b, "  - {name: %s, host: %s, group: apps, interval: %s, kinds: [tcp], tags: [tile:%s]}\n", w.name, home10Addrs[i], tcpIv, w.tile)
	}
	stun := func(name string, a netip.Addr, port int, iv string, tags string) {
		fmt.Fprintf(&b, "  - {name: %s, host: %s, port: %d, group: calls, kinds: [udp], interval: %s, tags: [%s]}\n", name, a, port, iv, tags)
	}
	stun("stun-a", stunAddr, stunPortA, "6s", "udp-path")
	stun("stun-a53", stunAddr, stunPort53, "15s", "udp-path, udp-53")
	stun("stun-b", stunAddr2, stunPortA, "6s", "udp-path")
	stun("meet-stun", stunAddr3, stunPortGoogle, "6s", "udp-path, app:meet, tile:meet")
	stun("meet-stun-3478", stunAddr3, stunPortA, "6s", "udp-path, app:meet, tile:meet")
	return b.String()
}

type home10Opts struct {
	tcpIv  time.Duration // default probeIv (6 s)
	fresh  bool          // no seeded history (S9)
	listen string        // default listenAddr
}

// home10 is one scenario's lab: a topology, fyisp with the home10 profile
// and a persistent data directory, and the recorded timeline.
type home10 struct {
	*harness
	o    home10Opts
	iv   time.Duration
	data string
	fy   *fyisp

	mu    sync.Mutex
	tl    []poll
	jsonl *os.File
	t0    time.Time // the first poll
}

// poll is one part=now answer (or the error instead), at t.
type poll struct {
	t   time.Time
	err error
	n   *h10Now
	raw []byte
}

func startHome10(t *testing.T, o home10Opts) *home10 {
	if o.tcpIv == 0 {
		o.tcpIv = probeIv
	}
	if o.listen == "" {
		o.listen = listenAddr
	}
	h := newHarness(t)
	lab := &home10{harness: h, o: o, iv: o.tcpIv}
	// Faults that take the provider's router down drop what it is sent
	// itself (the ISP edge's pings) too.
	h.nsRun(t, h.isp, "nft", "add chain inet fyt faultin { type filter hook input priority 0; policy accept; }")
	h.profile = filepath.Join(h.dir, "home10.yml")
	mustWrite(t, h.profile, home10Profile(o.tcpIv))
	prof, err := profile.Load(h.profile)
	if err != nil {
		t.Fatal(err)
	}
	lab.data = filepath.Join(h.dir, "data")
	h.mkdirOwned(t, lab.data, 0o700)
	if !o.fresh {
		lab.seed(t, prof, lab.labRTT(t))
	}
	lab.start(t)
	lab.startPoller(t)
	p := lab.waitPoll(t, "the good card", 4*time.Minute, func(n *h10Now) bool {
		return home10Good(n) && (o.fresh || n.Learning == nil)
	})
	t.Logf("good %s after the start", p.t.Sub(lab.t0).Round(time.Second))
	switch n := p.n; {
	case len(n.Tiles) != 11 || slices.ContainsFunc(n.Tiles, func(x h10Tile) bool { return x.State == "off" }):
		lab.errorf(t, p, "tiles %+v, want all 11 monitored", n.Tiles)
	case n.Footer == "" || n.WatchingSince == 0:
		lab.errorf(t, p, "no footer")
	case !o.fresh && time.Since(time.UnixMilli(n.WatchingSince)) < 24*time.Hour:
		lab.errorf(t, p, "watching since %s, want the seeded history's start", msClock(&n.WatchingSince))
	}
	return lab
}

// home10Good: the good card, every tile Good, the strip good.
func home10Good(n *h10Now) bool {
	return strings.HasPrefix(n.Answer.Key, "good") && !n.Stale &&
		n.notTile(func(x h10Tile) bool { return x.State == "good" }) == nil &&
		!slices.ContainsFunc(n.Path, func(x h10Hop) bool { return x.State != "good" })
}

// start starts fyisp on the lab's data directory.
func (lab *home10) start(t *testing.T) {
	t.Helper()
	fy := lab.startFyispOn(t, lab.o.listen, "--data-dir", lab.data)
	lab.waitReady(t, fy)
	checkUnprivileged(t, fy.cmd.Process.Pid)
	lab.stopAtEnd(t, fy)
	lab.fy = fy
}

// labRTT is the median TCP connect time to a lab target from fyisp's
// namespace: the seeded history's RTT.
func (lab *home10) labRTT(t *testing.T) time.Duration {
	t.Helper()
	var ds []time.Duration
	for range 9 {
		t0 := time.Now()
		c, err := dialIn(lab.cli)(context.Background(), "tcp4", netip.AddrPortFrom(home10Addrs[0], 443).String())
		if err != nil {
			t.Fatal(err)
		}
		ds = append(ds, time.Since(t0))
		c.Close()
	}
	slices.Sort(ds)
	t.Logf("lab TCP connect RTT %s (median of %d)", ds[len(ds)/2], len(ds))
	return ds[len(ds)/2]
}

// seed writes home10Seed of good samples for every series of p, ending
// now, into the lab's data directory, the way fyisp's own store would
// have: the slot intervals are cmd/fyisp's (seriesInterval).
func (lab *home10) seed(t *testing.T, p *model.Profile, rtt time.Duration) {
	t.Helper()
	iv := map[string]time.Duration{}
	for _, tg := range p.Targets {
		iv[tg.Name] = cmpOr(tg.Interval, profile.DefaultInterval)
	}
	every := func(k model.SeriesKey) time.Duration {
		if k.Kind == model.KindICMP {
			return iv[k.Target] / 3
		}
		return iv[k.Target]
	}
	st, err := store.Open(lab.data, store.Options{Version: "netns-seed", Interval: every})
	if err != nil {
		t.Fatal(err)
	}
	var keys []model.SeriesKey
	for _, tg := range p.Targets {
		for _, k := range tg.ProbeKinds() {
			if k == model.KindUDP {
				for _, u := range model.UDPKinds {
					keys = append(keys, model.SeriesKey{Target: tg.Name, Kind: u})
				}
				continue
			}
			keys = append(keys, model.SeriesKey{Target: tg.Name, Kind: k})
		}
	}
	t0 := time.Now()
	end := t0.Truncate(time.Second)
	n := 0
	ctx := context.Background()
	for h := end.Add(-home10Seed).Truncate(time.Hour); h.Before(end); h = h.Add(time.Hour) {
		for i, k := range keys {
			e := every(k)
			for s := h; s.Before(h.Add(time.Hour)) && s.Before(end); s = s.Add(e) {
				// ±5 % around the lab's RTT, deterministic
				v := rtt * time.Duration(95+(s.Unix()/int64(e/time.Second)+int64(i))%11) / 100
				switch k.Kind {
				case model.KindUDPJitter:
					v /= 4
				case model.KindUDPLoss:
					v = 0 // 0 %
				}
				st.Observe(model.Sample{Key: k, Slot: s.UTC(), RTT: v})
				n++
			}
		}
		if err := st.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(lab.data, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(path, runUID, runUID)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded %d samples of %d series (%s of history) in %s", n, len(keys), home10Seed, time.Since(t0).Round(time.Millisecond))
}

func cmpOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// fetchNow GETs part=now with c.
func (lab *home10) fetchNow(c *http.Client) poll {
	resp, err := c.Get("http://" + listenAddr + homeAPI + "?part=now")
	if err != nil {
		return poll{t: time.Now(), err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	p := poll{t: time.Now(), err: err, raw: b}
	if err != nil {
		return p
	}
	if resp.StatusCode != http.StatusOK {
		p.err = fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))
		return p
	}
	var n h10Now
	if err := json.Unmarshal(b, &n); err != nil {
		p.err = err
		return p
	}
	p.n = &n
	if n.At > 0 {
		p.t = time.UnixMilli(n.At)
	}
	return p
}

// startPoller records part=now every home10Poll until the scenario ends,
// into timeline.jsonl (as it goes) and timeline.txt; a failed scenario
// logs the timeline.
func (lab *home10) startPoller(t *testing.T) {
	if lab.out != "" {
		f, err := os.Create(filepath.Join(lab.out, "timeline.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		giveBack(f.Name())
		lab.jsonl = f
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(home10Poll)
		defer tk.Stop()
		for {
			lab.record(lab.fetchNow(lab.client))
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		var sb strings.Builder
		for _, p := range lab.polls(time.Time{}, time.Now().Add(time.Hour)) {
			sb.WriteString(lab.line(p))
			sb.WriteByte('\n')
		}
		lab.save(t, "timeline.txt", []byte(sb.String()))
		if lab.jsonl != nil {
			lab.jsonl.Close()
		}
		if t.Failed() {
			lab.logPolls(t, "timeline")
		}
	})
}

// record adds a poll to the timeline.
func (lab *home10) record(p poll) {
	lab.mu.Lock()
	defer lab.mu.Unlock()
	if lab.t0.IsZero() {
		lab.t0 = p.t
	}
	lab.tl = append(lab.tl, p)
	if lab.jsonl != nil {
		e := struct {
			T   int64           `json:"t"`
			Err string          `json:"err,omitempty"`
			Now json.RawMessage `json:"now,omitempty"`
		}{T: p.t.UnixMilli()}
		if p.err != nil {
			e.Err = p.err.Error()
		} else {
			e.Now = p.raw
		}
		b, _ := json.Marshal(e)
		lab.jsonl.Write(append(b, '\n'))
	}
}

// polls are the answers (not the errors) at from to to, in time order.
func (lab *home10) polls(from, to time.Time) []poll {
	lab.mu.Lock()
	defer lab.mu.Unlock()
	var out []poll
	for _, p := range lab.tl {
		if p.n != nil && !p.t.Before(from) && !p.t.After(to) {
			out = append(out, p)
		}
	}
	slices.SortStableFunc(out, func(a, b poll) int { return a.t.Compare(b.t) })
	return out
}

// first is the first answer at from to to that satisfies ok, or nil.
func (lab *home10) first(from, to time.Time, ok func(*h10Now) bool) *poll {
	for _, p := range lab.polls(from, to) {
		if ok(p.n) {
			return &p
		}
	}
	return nil
}

// last is the latest answer.
func (lab *home10) last(t *testing.T) poll {
	t.Helper()
	ps := lab.polls(time.Time{}, time.Now().Add(time.Hour))
	if len(ps) == 0 {
		t.Fatal("no answers recorded")
	}
	return ps[len(ps)-1]
}

// waitPoll waits for an answer recorded from now on that satisfies ok.
func (lab *home10) waitPoll(t *testing.T, what string, timeout time.Duration, ok func(*h10Now) bool) poll {
	t.Helper()
	from := time.Now()
	for {
		if p := lab.first(from.Add(-time.Second), time.Now().Add(time.Hour), ok); p != nil {
			return *p
		}
		if time.Since(from) > timeout {
			lab.logPolls(t, "timeline")
			t.Fatalf("no %s within %s", what, timeout)
		}
		time.Sleep(time.Second)
	}
}

// line is a poll in one line: the time since the first poll, the card's
// key, tone and headline, the tiles (G good, S slow/Choppy, D down,
// N no data, O off; lower case with a note), the strip (g, s, d, u) and
// the number of problems.
func (lab *home10) line(p poll) string {
	at := p.t.Sub(lab.t0).Round(time.Second)
	if p.n == nil {
		return fmt.Sprintf("+%5.0fs %s error: %v", at.Seconds(), p.t.Format("15:04:05"), p.err)
	}
	n := p.n
	var ts, ps []byte
	for _, x := range n.Tiles {
		c := map[string]byte{"good": 'G', "slow": 'S', "down": 'D', "none": 'N', "off": 'O'}[x.State]
		if x.Note != "" && x.State == "good" {
			c += 'a' - 'A'
		}
		ts = append(ts, c)
	}
	for _, x := range n.Path {
		ps = append(ps, x.State[0])
	}
	key, _, _ := strings.Cut(n.Answer.Key, ":")
	stale := ""
	if n.Stale {
		stale = " STALE"
	}
	return fmt.Sprintf("+%5.0fs %s %-13s %-8s tiles=%s %s %s path=%s probs=%d%s | %s %s", at.Seconds(), p.t.Format("15:04:05"),
		key, n.Answer.Tone, ts[:4], ts[4:9], ts[9:], ps, len(n.Problems.Items), stale, n.Answer.Headline, n.Answer.Sub)
}

// logPolls logs the timeline so far.
func (lab *home10) logPolls(t *testing.T, what string) {
	t.Helper()
	var sb strings.Builder
	for _, p := range lab.polls(time.Time{}, time.Now().Add(time.Hour)) {
		sb.WriteString("\n    " + lab.line(p))
	}
	t.Logf("%s (%s, tiles %s):%s", what, homeAPI, strings.Join(home10TileIDs, " "), sb.String())
}

var home10TileIDs = func() []string {
	var out []string
	for _, d := range profile.Tiles {
		out = append(out, d.ID)
	}
	return out
}()

// errorf fails the scenario at poll p, with the answer.
func (lab *home10) errorf(t *testing.T, p poll, format string, args ...any) {
	t.Helper()
	t.Errorf("%s\n    at %s\n    %s", fmt.Sprintf(format, args...), lab.line(p), p.raw)
}

// downBy is how long after a full outage starts every tile is Down at the
// latest: three missed checks (a single-endpoint group), the probe timeout
// and a tick, plus the engine's hold (interval + 5 s, before a check is
// applied).
func (lab *home10) downBy() time.Duration {
	return 3*lab.iv + 10*time.Second + lab.iv + 5*time.Second
}

// ispDown drops everything the provider's router forwards or is sent
// itself, ispUp ends that; both return when they took effect.
func (lab *home10) ispDown(t *testing.T) time.Time {
	t.Helper()
	lab.nft(t, "add rule inet fyt fault drop")
	lab.nft(t, "add rule inet fyt faultin drop")
	return time.Now()
}

func (lab *home10) ispUp(t *testing.T) time.Time {
	t.Helper()
	lab.flush(t)
	return time.Now()
}

func (lab *home10) flush(t *testing.T) {
	t.Helper()
	lab.nft(t, "flush chain inet fyt fault")
	lab.nft(t, "flush chain inet fyt faultin")
}

// shapeSTUN3 is shapeSTUN for the three STUN addresses of the lab.
func (lab *home10) shapeSTUN3(t *testing.T, netem ...string) {
	t.Helper()
	dev := []string{"dev", ispUplink}
	tc := func(args ...string) { lab.nsRun(t, lab.isp, append([]string{"tc"}, args...)...) }
	tc(slices.Concat([]string{"qdisc", "add"}, dev, []string{"root", "handle", "1:", "prio", "bands", "3", "priomap"},
		slices.Repeat([]string{"1"}, 16))...)
	tc(slices.Concat([]string{"qdisc", "add"}, dev, []string{"parent", "1:3", "handle", "30:", "netem"}, netem)...)
	for _, a := range []netip.Addr{stunAddr, stunAddr2, stunAddr3} {
		tc(slices.Concat([]string{"filter", "add"}, dev, []string{"parent", "1:", "protocol", "ip", "prio", "1", "u32",
			"match", "ip", "dst", a.String() + "/32", "match", "ip", "protocol", "17", "0xff", "flowid", "1:3"})...)
	}
}

// checkDrop checks a connection drop from t0 to t1 of about want (± tol):
// with allDown, every tile Down and the card red by downBy; Working again
// by by, and from by to until: "dropped for about N seconds" (N within
// tol), every tile Good with its drop note, a red bar at t0, the provider
// hop noting the drop and one problem from the provider.
func (lab *home10) checkDrop(t *testing.T, t0, t1 time.Time, want, tol time.Duration, allDown bool, by, until time.Time) {
	t.Helper()
	if allDown {
		if p := lab.first(t0, t0.Add(lab.downBy()+home10Poll), func(n *h10Now) bool {
			return n.Answer.Tone == "bad" && n.notTile(func(x h10Tile) bool { return x.State == "down" }) == nil
		}); p == nil {
			t.Errorf("the card not red with every tile down by T0+%s", lab.downBy())
		} else {
			t.Logf("red with every tile down %s after T0", p.t.Sub(t0).Round(time.Second))
		}
	}
	p := lab.first(t1, by.Add(home10Poll), func(n *h10Now) bool { return strings.HasPrefix(n.Answer.Key, "working_again:") })
	if p == nil {
		t.Errorf("no Working again by T1+%s", by.Sub(t1))
		return
	}
	t.Logf("Working again %s after T1", p.t.Sub(t1).Round(time.Second))
	inTol := func(d time.Duration) bool { return d >= want-tol && d <= want+tol && d%(10*time.Second) == 0 }
	for _, q := range lab.polls(p.t, until) {
		n := q.n
		if !strings.HasPrefix(n.Answer.Key, "working_again:") {
			lab.errorf(t, q, "Working again ended early")
			return
		}
		if n.Path[1].State != "good" || n.Path[2].State != "good" {
			lab.errorf(t, q, "Working again: want the provider and the internet hops good")
			return
		}
		if q.t.Before(by) {
			continue // the rest holds from by on
		}
		if d, ok := aboutDur(n.Answer.Headline, "Working again. Your internet dropped for "); !ok || !inTol(d) {
			lab.errorf(t, q, "headline: want \"dropped for about N seconds\" with N within %s of %s", tol, want)
			return
		}
		for _, x := range n.Tiles {
			d, ok := noteDur(x.Note, "Dropped ")
			if x.State != "good" || !ok || !inTol(d) {
				lab.errorf(t, q, "%s %s with note %q: want good, noting a drop within %s of %s", x.ID, x.State, x.Note, tol, want)
				return
			}
			if b := x.barAt(t0); b.State != "down" {
				lab.errorf(t, q, "%s: the bar at T0 is %q, want down", x.ID, b.State)
				return
			}
		}
		if d, ok := noteDur(n.Path[1].Sub, "Dropped for "); n.Path[1].State != "good" || !ok || !inTol(d) {
			lab.errorf(t, q, "provider hop %+v: want good, noting the drop", n.Path[1])
			return
		}
		if items := n.Problems.Items; len(items) != 1 || items[0].CauseKind != "provider" {
			lab.errorf(t, q, "want exactly 1 problem, from the provider")
			return
		}
	}
}

// checkAgain checks that Working again (headline prefix, the problem's
// cause kind) follows a connection problem that ended at t1 by by, and
// holds to the end of the recording.
func (lab *home10) checkAgain(t *testing.T, t1, by time.Time, headline, causeKind string) {
	t.Helper()
	p := lab.first(t1, by.Add(home10Poll), func(n *h10Now) bool { return strings.HasPrefix(n.Answer.Key, "working_again:") })
	if p == nil {
		t.Errorf("no Working again by T1+%s", by.Sub(t1))
		return
	}
	t.Logf("Working again %s after T1", p.t.Sub(t1).Round(time.Second))
	for _, q := range lab.polls(p.t, time.Now()) {
		a := q.n.Answer
		if !strings.HasPrefix(a.Key, "working_again:") || !strings.HasPrefix(a.Headline, headline) || !strings.HasPrefix(a.Sub, "Likely cause: ") {
			lab.errorf(t, q, "want Working again (%q…) to hold", headline)
			return
		}
		if !slices.ContainsFunc(q.n.Problems.Items, func(it h10Problem) bool { return it.CauseKind == causeKind }) {
			lab.errorf(t, q, "want a problem with cause kind %s", causeKind)
			return
		}
	}
}

// checkBlocked: from from to to, the blocked card ("! Calls limited"),
// and the call tiles ids Choppy for their lost fast path.
func (lab *home10) checkBlocked(t *testing.T, from, to time.Time, ids ...string) {
	t.Helper()
	for _, p := range lab.polls(from, to) {
		n := p.n
		if !strings.HasPrefix(n.Answer.Key, "udp_blocked:") || n.Answer.Tab != "! Calls limited" ||
			n.Answer.Headline != "Calls can’t use their fast path on this network." {
			lab.errorf(t, p, "want the blocked card to hold")
			return
		}
		for _, id := range ids {
			if x := n.tile(id); x.State != "slow" || x.Word != "Choppy" || !strings.HasPrefix(x.Reason, "Can’t use its fast path on this network since ") {
				lab.errorf(t, p, "%s %s (%s) %q: want Choppy for its lost fast path", id, x.State, x.Word, x.Reason)
				return
			}
		}
	}
}

// checkWebGood: every app and stream tile good at every poll.
func (lab *home10) checkWebGood(t *testing.T, from, to time.Time) {
	t.Helper()
	for _, p := range lab.polls(from, to) {
		if x := p.n.notTile(func(x h10Tile) bool { return x.Kind == "call" || x.State == "good" }); x != nil {
			lab.errorf(t, p, "%s %s, want every app and stream tile good", x.ID, x.State)
			return
		}
	}
}

// tileDetail GETs part=tile for id.
func (lab *home10) tileDetail(t *testing.T, id string) h10TileDetail {
	t.Helper()
	b := lab.get(t, homeAPI+"?part=tile&id="+id)
	lab.save(t, "tile-"+id+"-"+strconv.FormatInt(time.Now().Unix(), 10)+".json", b)
	var d h10TileDetail
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("part=tile %s: %v: %s", id, err, b)
	}
	return d
}

// within reports |a − b| ≤ tol.
func within(a, b time.Time, tol time.Duration) bool { return a.Sub(b).Abs() <= tol }

func msClock(ms *int64) string {
	if ms == nil {
		return "-"
	}
	return time.UnixMilli(*ms).Format(time.TimeOnly)
}

var (
	aboutRE = regexp.MustCompile(`^about (\d+) seconds|^about 1 minute`)
	noteRE  = regexp.MustCompile(`^(\d+) s\b`)
)

// aboutDur reads "about 40 seconds" ("about 1 minute": 60 s) after prefix.
func aboutDur(s, prefix string) (time.Duration, bool) {
	rest, ok := strings.CutPrefix(s, prefix)
	m := aboutRE.FindStringSubmatch(rest)
	switch {
	case !ok || m == nil:
		return 0, false
	case m[1] == "":
		return time.Minute, true
	}
	n, _ := strconv.Atoi(m[1])
	return time.Duration(n) * time.Second, true
}

// noteDur reads "40 s" after prefix.
func noteDur(s, prefix string) (time.Duration, bool) {
	rest, ok := strings.CutPrefix(s, prefix)
	m := noteRE.FindStringSubmatch(rest)
	if !ok || m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	return time.Duration(n) * time.Second, true
}

// ---------------------------------------------------------------------------
// The parts of the answer the harness reads (docs/HOME-API.md).

type h10Now struct {
	At            int64  `json:"at"`
	LastCheck     int64  `json:"last_check"`
	Stale         bool   `json:"stale"`
	WatchingSince int64  `json:"watching_since"`
	Footer        string `json:"footer"`
	Learning      *struct {
		Text string `json:"text"`
	} `json:"learning"`
	Answer struct {
		Key      string `json:"key"`
		Tone     string `json:"tone"`
		Icon     string `json:"icon"`
		Tab      string `json:"tab"`
		Headline string `json:"headline"`
		Sub      string `json:"sub"`
		Day      *struct {
			Text string `json:"text"`
		} `json:"day"`
	} `json:"answer"`
	Path     []h10Hop  `json:"path"`
	PathHint string    `json:"path_hint"`
	Tiles    []h10Tile `json:"tiles"`
	Problems struct {
		Summary string       `json:"summary"`
		Items   []h10Problem `json:"items"`
	} `json:"problems"`
}

type h10Hop struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	State string `json:"state"`
	Sub   string `json:"sub"`
}

type h10Tile struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"`
	State  string   `json:"state"`
	Word   string   `json:"word"`
	Since  *int64   `json:"since"`
	Note   string   `json:"note"`
	Reason string   `json:"reason"`
	Bars   []h10Bar `json:"bars"`
}

type h10Bar struct {
	From  int64  `json:"from"`
	State string `json:"state"`
	Note  string `json:"note"`
}

type h10Problem struct {
	ID        string `json:"id"`
	From      int64  `json:"from"`
	To        int64  `json:"to"`
	Level     string `json:"level"`
	When      string `json:"when"`
	What      string `json:"what"`
	Cause     string `json:"cause"`
	CauseKind string `json:"cause_kind"`
	Evidence  string `json:"evidence"`
}

type h10TileDetail struct {
	State   string `json:"state"`
	Word    string `json:"word"`
	Metrics []struct {
		Label string `json:"label"`
		Value string `json:"value"`
		Limit string `json:"limit"`
		Bad   *bool  `json:"bad"`
	} `json:"metrics"`
	UsualMs *float64 `json:"usual_ms"`
}

// tile returns tile id (a zero tile if missing).
func (n *h10Now) tile(id string) h10Tile {
	if i := slices.IndexFunc(n.Tiles, func(x h10Tile) bool { return x.ID == id }); i >= 0 {
		return n.Tiles[i]
	}
	return h10Tile{ID: id}
}

// notTile returns the first tile that does not satisfy ok, or nil.
func (n *h10Now) notTile(ok func(h10Tile) bool) *h10Tile {
	for i := range n.Tiles {
		if !ok(n.Tiles[i]) {
			return &n.Tiles[i]
		}
	}
	return nil
}

// barAt returns the 5-minute bar holding t (a zero bar if none does).
func (x h10Tile) barAt(t time.Time) h10Bar {
	for _, b := range x.Bars {
		if from := time.UnixMilli(b.From); !t.Before(from) && t.Before(from.Add(5*time.Minute)) {
			return b
		}
	}
	return h10Bar{}
}
