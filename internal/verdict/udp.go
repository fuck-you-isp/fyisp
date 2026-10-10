package verdict

import (
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Call path (UDP) verdicts. UDP targets send STUN bursts (model.KindUDP and
// its siblings); their loss never enters the rules in rules.go. Instead,
// when those rules find nothing (TCP services and the network path look
// fine), three verdicts of their own may replace ok, in this order:
//
//  1. udp_blocked / stun_blocked: targets from ≥ 2 independent providers
//     have each timed out on their last 10 bursts in a row, and the run is
//     past the UDP warm-up (the first answered burst, or 2 minutes after
//     start, whichever comes first: a network that blocks UDP from the start
//     is still caught). Only timeouts count; a socket error (refused,
//     unreachable, DNS) resets the run, as it is no evidence of a block.
//     Targets tagged udp-53 (STUN on UDP port 53) are left out of the count:
//     when one of them answered in the last 2 minutes and its latest 2
//     bursts did not both time out, the verdict is stun_blocked (the STUN
//     ports are blocked, UDP itself is not), otherwise udp_blocked. After 6 hours blocked without a break the
//     verdict is marked Quiet (hotel and office networks block UDP for
//     good); it stays Quiet until the verdict ends.
//  2. udp_degraded: ≥ 2 providers of udp-path targets are poor, and have
//     been for ≥ 3 minutes (breaks shorter than a minute do not restart
//     the 3 minutes: near a threshold, a 1-minute window dips under it now
//     and then, and the condition would otherwise flap). A target is poor over the last minute when it
//     has ≥ 2 measured bursts, at least one answered, and either loses
//     ≥ 5% of its requests (the mean of its bursts' loss) or has a median
//     burst jitter ≥ 30 ms (callq.JitterFairMs, the jitter at which the
//     Calls band is capped to Fair); a provider is poor when at least half
//     of its judged targets are. These are plain thresholds on the network, not a
//     call-quality score: 5% loss is where audio codecs' concealment starts
//     to be heard, 30 ms of jitter is where jitter buffers grow enough to
//     add noticeable delay. A target that answers nothing is dead, not
//     degraded (a STUN server outage is not this network's fault).
//
// Providers. Independence is judged by the registrable part of the host's
// domain (udpProvider), not by the catalog's provider ID: calls-stun.yml
// lists Google's, Cloudflare's, Twilio's and Microsoft's servers under one
// provider, and default.yml's copies carry none. One dead STUN service
// therefore never raises a verdict.
//
// Evidence (besides the TCP rules' keys): udp_providers (providers judged),
// udp_blocked_providers and udp53_ok (1 or 0, only with a udp-53 target)
// for the blocked kinds; udp_poor_providers, udp_loss (the worst poor
// target's loss, a fraction) and udp_jitter_ms (its worst median jitter)
// for udp_degraded. The headline loss (PeakLoss) is 0 for the blocked kinds
// and udp_loss for udp_degraded. Targets are the blocked or poor targets.
const (
	udpBlockedRun   = 10                 // timed-out bursts in a row before a target counts as blocked
	udpMinProviders = 2                  // independent providers that must agree
	udpWarmUp       = 2 * time.Minute    // blocked verdicts wait this long after start without any answer
	udpAnsweredIn   = keepRaw            // udp-53 answering this recently means UDP itself works ...
	udp53Failing    = 2                  // ... unless its latest bursts timed out this many times in a row
	udpQuietAfter   = 6 * time.Hour      // a blocked verdict this old is Quiet
	udpLossPoor     = 0.05               // mean burst loss that makes a target poor
	udpJitterPoorMs = callq.JitterFairMs // median burst jitter (ms) that makes a target poor (the Calls Fair cap)
	udpMinBursts    = 2                  // measured bursts a target needs in the window to be judged poor
	udpDegradedFor  = 3 * time.Minute    // poor providers must stay poor this long ...
	udpPoorGrace    = time.Minute        // ... where shorter breaks do not restart it
	tagUDPPath      = "udp-path"         // targets on the call path
	tagUDP53        = "udp-53"           // STUN on UDP port 53
)

// ustat is one UDP target's recent bursts.
type ustat struct {
	t        model.Target
	provider string
	timeouts int     // bursts timed out in a row, up to the latest
	everOK   bool    // some burst was ever answered
	answered bool    // some burst was answered in the last udpAnsweredIn
	okN      int     // answered bursts in the window
	bursts   int     // bursts with a measured loss in the window
	loss     float64 // mean burst loss over the window (fraction)
	jitter   float64 // ms: median burst jitter over the window
	jitterN  int     // bursts with a measured jitter in the window
}

func (u *ustat) poor() bool {
	return u.bursts >= udpMinBursts && u.okN > 0 &&
		(u.loss >= udpLossPoor || (u.jitterN >= udpMinBursts && u.jitter >= udpJitterPoorMs))
}

func hasTag(t model.Target, tag string) bool { return slices.Contains(t.Tags, tag) }

// collectUDP summarizes the UDP targets of p over the window (now-window,
// now]. Called with e.mu held, after collect (which trims raw samples).
func (e *engine) collectUDP(now time.Time, p *model.Profile) []*ustat {
	if p == nil {
		return nil
	}
	nowMs := now.UnixMilli()
	from := nowMs - window.Milliseconds()
	since := nowMs - udpAnsweredIn.Milliseconds()
	var out []*ustat
	var jit []uint32
	for _, t := range p.Targets {
		if !t.IsUDP() {
			continue
		}
		u := &ustat{t: t, provider: udpProvider(t)}
		key := func(k model.ProbeKind) *series { return e.series[model.SeriesKey{Target: t.Name, Kind: k}] }
		if sr := key(model.KindUDP); sr != nil {
			u.timeouts, u.everOK = sr.timeouts, sr.everOK
			for _, r := range sr.raw {
				if r.lost || r.ms <= since {
					continue
				}
				u.answered = true
				if r.ms > from {
					u.okN++
				}
			}
		}
		if sr := key(model.KindUDPLoss); sr != nil {
			sum := 0.0
			for _, r := range sr.raw {
				if r.ms > from && !r.lost {
					u.bursts++
					sum += float64(r.rtt) / 1000 / 100 // 1 ms = 1%
				}
			}
			if u.bursts > 0 {
				u.loss = round3(sum / float64(u.bursts))
			}
		}
		if sr := key(model.KindUDPJitter); sr != nil {
			jit = jit[:0]
			for _, r := range sr.raw {
				if r.ms > from && !r.lost {
					jit = append(jit, r.rtt)
				}
			}
			if u.jitterN = len(jit); u.jitterN > 0 {
				u.jitter = float64(medianU32(jit)) / 1000
			}
		}
		out = append(out, u)
	}
	return out
}

// judgeUDP applies the UDP rules (above) to j when the TCP rules found
// nothing. It always advances the udp_degraded timer.
func (e *engine) judgeUDP(now time.Time, p *model.Profile, us []*ustat, j *judgement) {
	type prov struct{ judged, poor int }
	provs := map[string]bool{}
	blockedProv := map[string]bool{}
	poorProv := map[string]*prov{}
	var blocked, poorT []*ustat
	anyOK, has53, ok53 := false, false, false
	for _, u := range us {
		anyOK = anyOK || u.everOK
		if hasTag(u.t, tagUDPPath) && u.bursts >= udpMinBursts {
			pp := poorProv[u.provider]
			if pp == nil {
				pp = &prov{}
				poorProv[u.provider] = pp
			}
			pp.judged++
			if u.poor() {
				pp.poor++
				poorT = append(poorT, u)
			}
		}
		if hasTag(u.t, tagUDP53) {
			has53 = true
			ok53 = ok53 || (u.answered && u.timeouts < udp53Failing)
			continue
		}
		provs[u.provider] = true
		if u.timeouts >= udpBlockedRun {
			blockedProv[u.provider] = true
			blocked = append(blocked, u)
		}
	}
	nPoor := 0
	for _, pp := range poorProv {
		if 2*pp.poor >= pp.judged && pp.poor > 0 {
			nPoor++
		}
	}
	switch {
	case nPoor >= udpMinProviders:
		if e.udpPoorSince.IsZero() {
			e.udpPoorSince = now
		}
		e.udpPoorGap = time.Time{}
	case e.udpPoorSince.IsZero():
	case e.udpPoorGap.IsZero():
		e.udpPoorGap = now
	case now.Sub(e.udpPoorGap) >= udpPoorGrace:
		e.udpPoorSince, e.udpPoorGap = time.Time{}, time.Time{}
	}
	if j.v.Kind != model.VerdictOK {
		return
	}
	names := newNamer(p)
	ev := j.v.Evidence
	if ev == nil {
		ev = map[string]float64{}
		j.v.Evidence = ev
	}
	set := func(k model.VerdictKind, loss float64, summary string, ts []*ustat) {
		j.v.Kind, j.loss = k, loss
		j.v.Summary = names.clean(k, summary)
		j.v.Targets = udpNames(ts)
		ev["loss"] = round3(loss)
	}
	warm := anyOK || (!e.start.IsZero() && now.Sub(e.start) >= udpWarmUp)
	if warm && len(blockedProv) >= udpMinProviders {
		ev["udp_providers"] = float64(len(provs))
		ev["udp_blocked_providers"] = float64(len(blockedProv))
		if has53 {
			ev["udp53_ok"] = b2f(ok53)
		}
		if ok53 {
			set(model.VerdictSTUNBlocked, 0, "Call ports (STUN) are blocked on this network, but other UDP works: some call apps may fall back to slower relays.", blocked)
		} else {
			set(model.VerdictUDPBlocked, 0, "UDP is blocked or failing on this network (or a firewall on this computer is blocking fyisp): calls will fall back to slower TCP relays.", blocked)
		}
		return
	}
	if nPoor >= udpMinProviders && now.Sub(e.udpPoorSince) >= udpDegradedFor {
		worstLoss, worstJit := 0.0, 0.0
		for _, u := range poorT {
			worstLoss = max(worstLoss, u.loss)
			if u.jitterN >= udpMinBursts {
				worstJit = max(worstJit, u.jitter)
			}
		}
		ev["udp_providers"] = float64(len(poorProv))
		ev["udp_poor_providers"] = float64(nPoor)
		ev["udp_loss"] = round3(worstLoss)
		ev["udp_jitter_ms"] = round3(worstJit)
		var what []string
		if worstLoss >= udpLossPoor {
			what = append(what, "up to "+pct(worstLoss)+" packet loss")
		}
		if worstJit >= udpJitterPoorMs {
			what = append(what, "up to "+ms(worstJit)+" of jitter")
		}
		set(model.VerdictUDPDegraded, worstLoss,
			fmt.Sprintf("Calls may sound choppy: the UDP path to %d call services shows %s, while the rest of your connection looks fine.", nPoor, strings.Join(what, " and ")), poorT)
	}
}

// markQuiet sets Quiet on a blocked verdict that has lasted udpQuietAfter
// without a break (udp_blocked and stun_blocked count as one).
func (e *engine) markQuiet(now time.Time) {
	e.vmu.Lock()
	defer e.vmu.Unlock()
	if !isBlocked(e.cur.Kind) {
		e.blockedSince = time.Time{}
		return
	}
	if e.blockedSince.IsZero() {
		e.blockedSince = e.cur.Since
	}
	e.cur.Quiet = now.Sub(e.blockedSince) >= udpQuietAfter
}

func isBlocked(k model.VerdictKind) bool {
	return k == model.VerdictUDPBlocked || k == model.VerdictSTUNBlocked
}

// udpProvider is the operator behind a UDP target: the registrable part of
// its host's domain ("stun.l.google.com" -> "google.com", with a 3-label
// guess for names such as "x.co.uk"), or the host itself for an address.
func udpProvider(t model.Target) string {
	h := strings.ToLower(strings.TrimSuffix(strings.Trim(t.Host, "[]"), "."))
	if h == "" {
		return t.Name
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return h
	}
	l := strings.Split(h, ".")
	n := 2
	if len(l) >= 3 && len(l[len(l)-1]) == 2 && len(l[len(l)-2]) <= 3 {
		n = 3
	}
	if len(l) <= n {
		return h
	}
	return strings.Join(l[len(l)-n:], ".")
}

func udpNames(us []*ustat) []string {
	var out []string
	for _, u := range us {
		out = append(out, u.t.Name)
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
