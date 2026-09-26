package verdict

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Rules, evaluated every 5s over the last 60s of samples (not-measured gaps
// ignored). A target (or a layer) is unhealthy when it loses ≥ 20% of its
// probes, or when the median RTT of one of its kinds (over ≥ 3 successes)
// spikes. A kind that never succeeded is ignored when another kind of the
// same target did (e.g. a target that blocks ICMP).
//
// Latency spikes. When Options.Baseline knows the series' long-term normal
// (7 days, same UTC hour of day once 6 such hours exist; see
// internal/baseline), a spike is a window median above
// max(2.5 × normal median, normal p95 + 30 ms). Otherwise the short-term
// rule applies: above max(3 × baseline, baseline + 50 ms), the baseline
// being the median of the per-minute medians of the preceding 30 minutes
// (judged once 5 such minutes exist).
//
// Why those constants: the long-term normal is a better reference than the
// last 30 minutes (congestion that builds up slowly drags the 30-minute
// baseline up with it and is never flagged; the normal also works right
// after start), so it can afford a lower factor, 2.5 instead of 3. Its p95
// captures the series' usual jitter, which the median alone does not: a
// jittery Wi-Fi hop or a far-away region with an occasional slow path
// must stay clearly beyond what 1 probe in 20 normally sees, plus 30 ms
// (the one-minute median of ~12-60 probes is much steadier than single
// probes, so "median above the old p95" is already rare without a fault,
// and 30 ms keeps low-latency series such as the router at 2 ms, p95 5 ms,
// from flagging below 35 ms). Examples: normal 30 ms (p95 40) flags above
// 75 ms; normal 42 (p95 60) above 105; normal 220 (p95 240) above 550;
// normal 30 with a jittery p95 of 150 only above 180. The ratio shown to
// people ("3× slower than your normal") is window median / normal median.
//
// Evidence (keys are lower-case identifiers; the web UI relies on the first
// ones): gateway_loss, edge_loss, anycast_loss (pooled loss of the layer,
// omitted while the layer is unknown), services_loss (pooled loss of the
// internet targets), gateway_rtt_ms, edge_rtt_ms, anycast_rtt_ms (median of
// the layer's targets' window median RTTs, when any succeeded); then
// <layer>_unhealthy, <layer>_targets, <layer>_baseline_ms (when spiking:
// the long-term normal median when known), <layer>_vs_normal and
// services_vs_normal (median over the layer's / internet targets that have
// a long-term normal of each target's largest window-median / normal-median
// ratio over its kinds; omitted when none has one), internet_targets,
// internet_unhealthy, dns_failing and loss (the headline loss, also the
// incident's PeakLoss candidate).
//
// Summaries say "N× slower than your normal (98 ms vs 42 ms)" when every
// target they describe is unhealthy by latency alone against a long-term
// normal (numbers: median of those targets' window medians and normals);
// otherwise they keep the short-term wording ("answers in 400 ms instead of
// the usual 30 ms").
//
// Layers: targets with Target.Layer gateway, isp-edge or anycast. A layer is
// unknown without samples (the rules then skip it) and unhealthy when a
// majority of its targets with samples is unhealthy.
//
// In order, the first match wins:
//
//  1. no_network: no samples at all; or nothing succeeded and most losses
//     are "no network"/"unreachable"; or every known layer loses everything
//     that way while no internet target succeeds.
//  2. lan: the gateway layer is to blame (innermost lossy layer, below).
//  3. isp: the ISP-edge layer is to blame (the gateway is not).
//  4. upstream: the anycast layer is unhealthy and the loss did not start
//     further in (below), or ≥ 50% of the internet (non-layer) targets are
//     unhealthy ignoring DNS failures, with at least 3 of them and spanning
//     more than one group (one group failing alone is that service's
//     problem).
//  5. dns: ≥ 30% of the internet targets lose ≥ 20% of probes to DNS.
//  6. service: some internet targets are unhealthy.
//  7. ok.
//
// Innermost lossy layer (rules 2-4). Loss on an inner link shows on every
// layer beyond it too, and while the one-minute window fills after the
// loss starts, a layer further out may cross a threshold first by chance:
// early on each layer has only a few lost samples (gateway and edge are
// probed ~60 times a minute, the anycast layer ~48). So:
//
//   - A layer is lossy when it lost ≥ 5 samples and ≥ 10% of them; no
//     verdict blames a layer for loss with fewer than 5 lost samples.
//   - The gateway (then the edge) is to blame when it is lossy and either
//     unhealthy or every known layer beyond it loses at least half as much
//     (the loss propagates outward).
//   - A layer beyond the gateway is blamed for loss only when it is
//     significantly worse than every known layer inside it: its loss
//     exceeds theirs by more than 2.5 standard errors of a two-proportion
//     test on the window's counts (pooled p). Otherwise the verdict waits
//     until the inner layer is clearly lossy or clearly clean.
//   - The anycast layer additionally needs every known inner layer below
//     10% loss and below half the anycast loss. With no inner layer known
//     it is judged alone.
//
// A latency spike needs no such rule: an inner spike makes that layer
// unhealthy itself and is caught first; a layer unhealthy by latency alone
// (loss < 10%) is not held back.
const (
	lossBad            = 0.2
	spikeFactor        = 3.0
	spikeMarginMs      = 50.0
	upstreamShare      = 0.5
	minUpstreamTargets = 3
	dnsShare           = 0.3
	lossInner          = 0.1 // a layer losing this much (and minLost samples) is lossy
	minLost            = 5   // lost samples needed before a layer is blamed for loss
	propagated         = 0.5 // outer layers must lose at least this share of the inner loss
	zWorse             = 2.5 // standard errors by which an outer layer must be worse than an inner one

	normalFactor   = 2.5  // long-term spike: window median > normalFactor × normal median ...
	normalMarginMs = 30.0 // ... and > normal p95 + normalMarginMs
	slowFactor     = 2.0  // Engine.Slow: window median > slowFactor × normal median ...
	slowMinExtraMs = 10.0 // ... and at least this much above it (2 ms -> 5 ms is not news)
)

func spikeThreshold(baseMs float64) float64 { return max(spikeFactor*baseMs, baseMs+spikeMarginMs) }

// normalThreshold is the long-term spike threshold (b.MedianMs > 0).
func normalThreshold(b model.Baseline) float64 {
	return max(normalFactor*b.MedianMs, max(b.P95Ms, b.MedianMs)+normalMarginMs)
}

type layerStat struct {
	name             string
	targets, bad     int // with samples / unhealthy
	n, lost, nonet   int
	meds             []float64 // targets' window median RTTs (ms)
	ratios           []float64 // targets' vsNormal (> 0 only)
	worst            *tstat    // the unhealthy target with most loss (or spike)
	unhealthy, known bool
	allLostUnreach   bool
}

func (l *layerStat) loss() float64 {
	if l.n == 0 {
		return 0
	}
	return float64(l.lost) / float64(l.n)
}

type judgement struct {
	v      model.Verdict // Since unset
	loss   float64       // headline loss for PeakLoss
	layers map[string]*bool
}

// judge applies the rules to one window.
func judge(p *model.Profile, ts []*tstat) judgement {
	ls := map[string]*layerStat{}
	for _, l := range []string{model.LayerGateway, model.LayerEdge, model.LayerAnycast} {
		ls[l] = &layerStat{name: l}
	}
	var inet []*tstat // internet targets with samples
	var n, lost, nonet, inetOK, inetN, inetLost int
	for _, t := range ts {
		if t.n == 0 {
			continue
		}
		n += t.n
		lost += t.lost
		nonet += t.nonet
		l := ls[t.t.Layer]
		if l == nil {
			inet = append(inet, t)
			inetOK += t.n - t.lost
			inetN += t.n
			inetLost += t.lost
			continue
		}
		l.targets++
		l.n += t.n
		l.lost += t.lost
		l.nonet += t.nonet
		if t.med > 0 {
			l.meds = append(l.meds, t.med)
		}
		if t.vsNormal > 0 {
			l.ratios = append(l.ratios, t.vsNormal)
		}
		if t.unhealthy() {
			l.bad++
			if l.worst == nil || t.loss() > l.worst.loss() {
				l.worst = t
			}
		}
	}
	j := judgement{layers: map[string]*bool{}}
	ev := map[string]float64{}
	for name, l := range ls {
		l.known = l.targets > 0
		if !l.known {
			j.layers[name] = nil
			continue
		}
		l.unhealthy = 2*l.bad > l.targets
		l.allLostUnreach = l.lost == l.n && 2*l.nonet > l.lost
		h := !l.unhealthy
		j.layers[name] = &h
		key := evKey(name)
		ev[key+"_loss"] = round3(l.loss())
		ev[key+"_unhealthy"] = float64(l.bad)
		ev[key+"_targets"] = float64(l.targets)
		if len(l.meds) > 0 {
			slices.Sort(l.meds)
			ev[key+"_rtt_ms"] = round3(l.meds[(len(l.meds)-1)/2])
		}
		if l.worst != nil && l.worst.spike {
			ev[key+"_baseline_ms"] = round3(l.worst.base)
		}
		if len(l.ratios) > 0 {
			ev[key+"_vs_normal"] = round3(lowerMedian(l.ratios))
		}
	}
	if inetN > 0 {
		ev["services_loss"] = round3(float64(inetLost) / float64(inetN))
	}
	var inetRatios []float64
	for _, t := range inet {
		if t.vsNormal > 0 {
			inetRatios = append(inetRatios, t.vsNormal)
		}
	}
	if len(inetRatios) > 0 {
		ev["services_vs_normal"] = round3(lowerMedian(inetRatios))
	}
	var badUp, dnsBad, bad []*tstat
	for _, t := range inet {
		if t.lossNoDNS() >= lossBad || t.spike {
			badUp = append(badUp, t)
		}
		if float64(t.dns) >= lossBad*float64(t.n) {
			dnsBad = append(dnsBad, t)
		}
		if t.unhealthy() {
			bad = append(bad, t)
		}
	}
	ev["internet_targets"] = float64(len(inet))
	ev["internet_unhealthy"] = float64(len(bad))
	ev["dns_failing"] = float64(len(dnsBad))
	j.v.Evidence = ev

	gw, edge, ac := ls[model.LayerGateway], ls[model.LayerEdge], ls[model.LayerAnycast]
	names := newNamer(p)
	set := func(k model.VerdictKind, loss float64, summary string) judgement {
		j.v.Kind, j.loss = k, loss
		j.v.Summary = names.clean(k, summary)
		ev["loss"] = round3(loss)
		return j
	}

	// 1. no network
	knownLayers, deadLayers := 0, 0
	for _, l := range ls {
		if l.known {
			knownLayers++
			if l.allLostUnreach {
				deadLayers++
			}
		}
	}
	switch {
	case n == 0,
		lost == n && 2*nonet > lost,
		knownLayers > 0 && deadLayers == knownLayers && inetOK == 0:
		return set(model.VerdictNoNetwork, 1, "This device has no working network connection.")
	}

	// 2. LAN
	if innermost(gw, edge, ac) {
		return set(model.VerdictLAN, gw.loss(), layerSentence(gw, "Your Wi-Fi or router"))
	}
	// 3. ISP
	if innermost(edge, ac) && worse(edge, gw) {
		var s string
		w := edge.worst
		lead := "Your ISP's network is dropping packets: "
		if gw.known {
			lead += "the router is fine, the first hop past it loses " + pct(edge.loss()) + "."
		} else {
			lead += "the first hop past your router loses " + pct(edge.loss()) + "."
		}
		s = lead
		if edge.loss() < lossBad && w != nil && w.spike {
			s = "Your ISP's network is slow: "
			if gw.known {
				s += "the router is fine, but the first hop past it "
			} else {
				s += "the first hop past your router "
			}
			if w.longTerm {
				s += "is " + slower(w.rtt, w.base) + "."
			} else {
				s += "answers in " + ms(w.rtt) + " instead of the usual " + ms(w.base) + "."
			}
		}
		return set(model.VerdictISP, edge.loss(), s)
	}
	// 4. upstream
	beyond := "Problems on the internet: "
	switch {
	case edge.known:
		beyond = "Problems beyond your ISP: "
	case gw.known:
		beyond = "Problems beyond your router: "
	}
	if ac.unhealthy && beyondInner(gw, edge, ac) {
		what := "responding slowly"
		unreach := true
		var bad []*tstat
		for _, t := range ts {
			if t.t.Layer == model.LayerAnycast && t.unhealthy() {
				if t.loss() >= lossBad {
					what = "dropping packets"
				}
				unreach = unreach && t.lost == t.n
				bad = append(bad, t)
			}
		}
		if unreach {
			what = "unreachable"
		} else if s, ok := slowerAll(bad); ok {
			what = s
		}
		return set(model.VerdictUpstream, ac.loss(),
			fmt.Sprintf("%s%d of %d public DNS anycast servers are %s.", beyond, ac.bad, ac.targets, what))
	}
	if len(badUp) >= minUpstreamTargets && float64(len(badUp)) >= upstreamShare*float64(len(inet)) && len(groupsOf(badUp)) > 1 {
		loss := 0.0
		for _, t := range badUp {
			loss += t.lossNoDNS()
		}
		what := "having trouble"
		if s, ok := slowerAll(badUp); ok {
			what = s
		}
		return set(model.VerdictUpstream, loss/float64(len(badUp)),
			fmt.Sprintf("%s%d of %d monitored services are %s.", beyond, len(badUp), len(inet), what))
	}
	// 5. DNS
	if len(inet) > 0 && float64(len(dnsBad)) >= dnsShare*float64(len(inet)) {
		worst := 0.0
		for _, t := range dnsBad {
			worst = max(worst, float64(t.dns)/float64(t.n))
		}
		j.v.Targets = sortedNames(dnsBad)
		return set(model.VerdictDNS, worst,
			fmt.Sprintf("Name lookups are failing: %d of %d services can't be found, but the network itself looks fine.", len(dnsBad), len(inet)))
	}
	// 6. service
	if len(bad) > 0 {
		worst := 0.0
		for _, t := range bad {
			worst = max(worst, t.loss())
		}
		j.v.Targets = sortedNames(bad)
		return set(model.VerdictService, worst, serviceSentence(bad, inet, names))
	}
	return set(model.VerdictOK, 0, "Everything looks fine.")
}

// lossy: the layer lost ≥ minLost samples and ≥ lossInner of them.
func (l *layerStat) lossy() bool {
	return l.known && l.lost >= minLost && l.loss() >= lossInner
}

// spikeOnly: unhealthy by latency, not by loss.
func (l *layerStat) spikeOnly() bool { return l.unhealthy && l.loss() < lossInner }

// innermost reports whether l is to blame, outer being the layers beyond
// it: l is unhealthy by latency alone, or lossy and either unhealthy or
// every known outer layer loses at least propagated × as much.
func innermost(l *layerStat, outer ...*layerStat) bool {
	switch {
	case !l.known:
		return false
	case l.spikeOnly():
		return true
	case !l.lossy():
		return false
	case l.unhealthy:
		return true
	}
	for _, o := range outer {
		if o.known && o.loss() < propagated*l.loss() {
			return false
		}
	}
	return true
}

// worse reports whether the loss of out can be blamed on out rather than on
// the inner layer in: in is unknown, or out is unhealthy by latency alone,
// or out's loss exceeds in's by more than zWorse standard errors (pooled
// two-proportion test on the window's counts).
func worse(out, in *layerStat) bool {
	if !in.known || out.spikeOnly() {
		return true
	}
	if in.n == 0 || out.n == 0 {
		return false
	}
	p := float64(in.lost+out.lost) / float64(in.n+out.n)
	se := math.Sqrt(p * (1 - p) * (1/float64(in.n) + 1/float64(out.n)))
	d := out.loss() - in.loss()
	if se == 0 {
		return d > 0
	}
	return d > zWorse*se
}

// beyondInner reports whether an unhealthy anycast layer is a problem
// beyond the ISP: it is unhealthy by latency alone, or it lost ≥ minLost
// samples and every known inner layer loses < lossInner, less than
// propagated × the anycast loss, and significantly less (worse).
func beyondInner(gw, edge, ac *layerStat) bool {
	if ac.spikeOnly() {
		return true
	}
	if ac.lost < minLost {
		return false
	}
	for _, in := range []*layerStat{gw, edge} {
		if in.known && !(in.loss() < lossInner && in.loss() < propagated*ac.loss() && worse(ac, in)) {
			return false
		}
	}
	return true
}

func layerSentence(l *layerStat, who string) string {
	if l.loss() < lossBad && l.worst != nil && l.worst.spike {
		if l.worst.longTerm {
			return fmt.Sprintf("%s is %s.", who, slower(l.worst.rtt, l.worst.base))
		}
		return fmt.Sprintf("%s is slow: it answers in %s instead of the usual %s.", who, ms(l.worst.rtt), ms(l.worst.base))
	}
	return fmt.Sprintf("%s is dropping %s of packets.", who, pct(l.loss()))
}

// serviceSentence describes the unhealthy internet targets: one target, a
// single group (whole or part), or several groups.
func serviceSentence(bad, inet []*tstat, nm *namer) string {
	groups := groupsOf(bad)
	slow, allSlow := slowerAll(bad)
	if allSlow {
		switch {
		case len(bad) == 1:
			return fmt.Sprintf("%s is %s.", nm.target(bad[0].t.Name), slow)
		case len(groups) == 1 && groups[0] != "":
			total := 0
			for _, t := range inet {
				if t.t.Group == groups[0] {
					total++
				}
			}
			if len(bad) == total {
				return fmt.Sprintf("%s is %s.", nm.group(groups[0]), slow)
			}
			return fmt.Sprintf("%s is %s on %d of its %d targets.", nm.group(groups[0]), slow, len(bad), total)
		}
		var names []string
		for _, n := range sortedNames(bad) {
			names = append(names, nm.target(n))
		}
		return fmt.Sprintf("%d services are %s: %s.", len(bad), slow, list(slices.Compact(names), 3))
	}
	if len(bad) == 1 {
		t := bad[0]
		name := nm.target(t.t.Name)
		if t.loss() >= lossBad {
			return fmt.Sprintf("Only %s looks affected: it loses %s of probes.", name, pct(t.loss()))
		}
		return fmt.Sprintf("Only %s looks affected: it answers in %s instead of the usual %s.", name, ms(t.rtt), ms(t.base))
	}
	if len(groups) == 1 && groups[0] != "" {
		total := 0
		for _, t := range inet {
			if t.t.Group == groups[0] {
				total++
			}
		}
		g := nm.group(groups[0])
		if len(bad) == total {
			return fmt.Sprintf("Only %s looks affected (all %d of its targets).", g, total)
		}
		return fmt.Sprintf("Only %s looks affected (%d of %d targets).", g, len(bad), total)
	}
	var names []string
	for _, n := range sortedNames(bad) {
		names = append(names, nm.target(n))
	}
	names = slices.Compact(names)
	return fmt.Sprintf("%d services look affected: %s.", len(bad), list(names, 3))
}

// slowerAll describes ts as slower than their normal ("3× slower than your
// normal (90 ms vs 30 ms)", medians over ts) when every one of them is
// unhealthy by latency alone (loss < lossBad) against a long-term normal.
func slowerAll(ts []*tstat) (string, bool) {
	if len(ts) == 0 {
		return "", false
	}
	var nows, norms []float64
	for _, t := range ts {
		if !t.spike || !t.longTerm || t.loss() >= lossBad {
			return "", false
		}
		nows = append(nows, t.rtt)
		norms = append(norms, t.base)
	}
	return slower(lowerMedian(nows), lowerMedian(norms)), true
}

// slower: "3× slower than your normal (90 ms vs 30 ms)".
func slower(nowMs, normalMs float64) string {
	return fmt.Sprintf("%s× slower than your normal (%s vs %s)", fmtRatio(nowMs/normalMs), ms(nowMs), ms(normalMs))
}

// fmtRatio formats a ratio like baseline.FormatRatio: one decimal below 10
// without a trailing ".0" ("2.3", "3"), whole numbers from 10.
func fmtRatio(r float64) string {
	if r >= 9.95 {
		return strconv.Itoa(int(math.Round(r)))
	}
	return strconv.FormatFloat(math.Round(r*10)/10, 'f', -1, 64)
}

// lowerMedian sorts v and returns its lower median.
func lowerMedian(v []float64) float64 {
	slices.Sort(v)
	return v[(len(v)-1)/2]
}

// list joins up to max items: "a, b and c", "a, b, c and 2 more".
func list(items []string, maxN int) string {
	if len(items) > maxN {
		return strings.Join(items[:maxN], ", ") + fmt.Sprintf(" and %d more", len(items)-maxN)
	}
	if len(items) <= 1 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

func groupsOf(ts []*tstat) []string {
	var g []string
	for _, t := range ts {
		g = append(g, t.t.Group)
	}
	slices.Sort(g)
	return slices.Compact(g)
}

func sortedNames(ts []*tstat) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.t.Name)
	}
	slices.Sort(out)
	return out
}

func evKey(layer string) string {
	if layer == model.LayerEdge {
		return "edge"
	}
	return layer
}

// pct formats a fraction as a whole percentage ("34%"); ms formats
// milliseconds as a whole number. Neither ever prints a decimal point: the
// only digits.digits sequence a summary may contain is a ratio written
// right before "×" (fmtRatio), which the sanitizer allows.
func pct(f float64) string { return fmt.Sprintf("%d%%", int(math.Round(f*100))) }
func ms(v float64) string  { return fmt.Sprintf("%d ms", max(1, int(math.Round(v)))) }

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// ---- keeping hosts and addresses out of summaries ----

var (
	// ipLike matches IPv4-ish digits.digits and IPv6-ish hex:hex.
	ipLike = regexp.MustCompile(`\d+\.\d+|[0-9A-Fa-f]*:[0-9A-Fa-f]*:`)
	// hostLike matches a dotted name ending in a letter label (example.com).
	hostLike = regexp.MustCompile(`(?i)[a-z0-9-]+(\.[a-z0-9-]+)*\.[a-z]{2,}`)
	// ratioTok is a ratio as fmtRatio writes it before "×" ("2.3×"): one
	// decimal, not part of a longer dotted number (so "1.2.3.4×" stays
	// address-like). It is removed before the ipLike check.
	ratioTok = regexp.MustCompile(`(^|[^0-9.])[0-9]+\.[0-9]×`)
)

// namer maps profile names to display names that are safe for the public
// page: a target or group name that contains a probed host, or looks like a
// host or an address, is replaced by a generic phrase.
type namer struct {
	hosts  []string
	titles map[string]string
}

func newNamer(p *model.Profile) *namer {
	nm := &namer{titles: map[string]string{}}
	if p == nil {
		return nm
	}
	for _, g := range p.Groups {
		nm.titles[g.ID] = g.Title
	}
	for _, t := range p.Targets {
		for _, h := range append([]string{t.Host}, mapValues(t.HostOverrides)...) {
			if h != "" && !strings.HasPrefix(h, "@") {
				nm.hosts = append(nm.hosts, strings.ToLower(h))
			}
		}
	}
	return nm
}

func (nm *namer) unsafe(s string) bool {
	if ipLike.MatchString(ratioTok.ReplaceAllString(s, "$1")) || hostLike.MatchString(s) {
		return true
	}
	ls := strings.ToLower(s)
	for _, h := range nm.hosts {
		if strings.Contains(ls, h) {
			return true
		}
	}
	return false
}

func (nm *namer) target(name string) string {
	if name == "" || nm.unsafe(name) {
		return "one monitored service"
	}
	return name
}

func (nm *namer) group(id string) string {
	g := nm.titles[id]
	if g == "" {
		g = id
	}
	if g == "" || nm.unsafe(g) {
		return "one group of services"
	}
	return g
}

// generic are fallback summaries without any names.
var generic = map[model.VerdictKind]string{
	model.VerdictOK:        "Everything looks fine.",
	model.VerdictLAN:       "Your Wi-Fi or router is having problems.",
	model.VerdictISP:       "Your ISP's network is having problems.",
	model.VerdictUpstream:  "Problems beyond your ISP.",
	model.VerdictDNS:       "Name lookups are failing.",
	model.VerdictService:   "Some monitored services look affected.",
	model.VerdictNoNetwork: "This device has no working network connection.",
}

// clean is the last guard: a summary that still looks like it carries an
// address or a host is replaced by the generic one for its kind.
func (nm *namer) clean(k model.VerdictKind, s string) string {
	if nm.unsafe(s) {
		return generic[k]
	}
	return s
}

func mapValues[K comparable, V any](m map[K]V) []V {
	out := make([]V, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
