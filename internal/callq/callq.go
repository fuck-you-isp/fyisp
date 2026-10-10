// Package callq scores call quality from STUN burst measurements: a
// simplified ITU-T G.107 E-model gives an R factor from round trip, jitter
// and loss, R maps to a MOS (1..4.5), and the MOS to a band (good, fair,
// poor, unusable). Measurements are scored per one-minute bucket; a window
// is judged by its worst minute, because a mean hides a 30-second stall.
//
// The model, after docs/CALL-QUALITY-PLAN.md ("Score"):
//
//	eff = rtt/2 + 2·jitter + 10                     (one-way delay, ms)
//	R   = 93.2 − eff/40          when eff < 160
//	R   = 93.2 − (eff−120)/10    otherwise
//	R  −= 2.0 per % loss up to 5 %, then 2.5 per % above (Opus FEC and
//	      packet-loss concealment make loss cheaper than in G.711's tables)
//	R   clamped to 0..100
//	MOS = 1 + 0.035·R + 7e-6·R·(R−60)·(100−R)
//
// The E-model sees jitter only as added delay, so on a short path even 30 ms
// of jitter still scores Good. Call apps disagree (a jitter buffer that big
// is heard), so a minute's band is capped by its jitter after the MOS band
// (see JitterFairMs): Fair from 30 ms, Poor from 50 ms, Unusable from 100 ms.
package callq

import (
	"math"
	"slices"
	"strings"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Band is a MOS band (the G.107 user-satisfaction bands).
type Band string

const (
	Good     Band = "good"     // MOS ≥ 4.0
	Fair     Band = "fair"     // 3.6 ≤ MOS < 4.0
	Poor     Band = "poor"     // 3.1 ≤ MOS < 3.6
	Unusable Band = "unusable" // MOS < 3.1
)

// Band edges (MOS).
const (
	GoodMOS = 4.0
	FairMOS = 3.6
	PoorMOS = 3.1
)

// Jitter caps on the band (ms of in-burst jitter). They follow Microsoft's
// network targets for Teams media (jitter < 30 ms; more is a poor call):
// a minute with jitter ≥ JitterFairMs is at most Fair, ≥ JitterPoorMs at
// most Poor, ≥ JitterUnusableMs Unusable. A cap never improves a band. The
// udp_degraded verdict (internal/verdict) uses JitterFairMs as its jitter
// threshold, so a degraded banner never sits over a Good Calls card.
//
// A capped minute's MOS is lowered to CapMargin below the upper edge of the
// band it is capped to (3.9, 3.5 or 3.0), so that the MOS, the band and
// fyisp_call_mos always agree.
const (
	JitterFairMs     = 30.0
	JitterPoorMs     = 50.0
	JitterUnusableMs = 100.0
	CapMargin        = 0.1
)

// JitterCapMOS is the highest MOS a jitter (ms) allows: a cap's band edge
// less CapMargin, or +Inf below JitterFairMs (or when unknown).
func JitterCapMOS(jitterMs float64) float64 {
	switch JitterCap(jitterMs) {
	case Unusable:
		return PoorMOS - CapMargin
	case Poor:
		return FairMOS - CapMargin
	case Fair:
		return GoodMOS - CapMargin
	}
	return math.Inf(1)
}

// JitterCap returns the best band a jitter (ms) allows: Good below
// JitterFairMs or when the jitter is unknown (NaN).
func JitterCap(jitterMs float64) Band {
	switch {
	case jitterMs >= JitterUnusableMs:
		return Unusable
	case jitterMs >= JitterPoorMs:
		return Poor
	case jitterMs >= JitterFairMs:
		return Fair
	}
	return Good
}

// Capped returns the worse of b and the band jitterMs allows.
func Capped(b Band, jitterMs float64) Band {
	if c := JitterCap(jitterMs); Rank(c) < Rank(b) {
		return c
	}
	return b
}

// MinBursts is how many bursts (answered or timed out) a minute needs to be
// scored; fewer leave it unscored.
const MinBursts = 3

// R returns the E-model R factor for a round trip and jitter in ms and a
// loss in percent (0..100), clamped to 0..100.
func R(rttMs, jitterMs, lossPct float64) float64 {
	eff := rttMs/2 + 2*jitterMs + 10
	r := 93.2 - eff/40
	if eff >= 160 {
		r = 93.2 - (eff-120)/10
	}
	r -= lossPenalty(lossPct)
	return min(100, max(0, r))
}

func lossPenalty(pct float64) float64 {
	pct = min(100, max(0, pct))
	return 2*min(pct, 5) + 2.5*max(pct-5, 0)
}

// MOS maps an R factor to a mean opinion score (1 at R 0, 4.41 at R 93.2).
func MOS(r float64) float64 {
	r = min(100, max(0, r))
	return 1 + 0.035*r + 7e-6*r*(r-60)*(100-r)
}

// Score is MOS(R(rtt, jitter, loss)).
func Score(rttMs, jitterMs, lossPct float64) float64 { return MOS(R(rttMs, jitterMs, lossPct)) }

// BandOf returns the band of a MOS.
func BandOf(mos float64) Band {
	switch {
	case mos >= GoodMOS:
		return Good
	case mos >= FairMOS:
		return Fair
	case mos >= PoorMOS:
		return Poor
	}
	return Unusable
}

// Rank orders bands worst first (unusable 0 .. good 3); unknown is 4.
func Rank(b Band) int {
	switch b {
	case Unusable:
		return 0
	case Poor:
		return 1
	case Fair:
		return 2
	case Good:
		return 3
	}
	return 4
}

// Factor names what costs a score the most: "loss", "jitter" or "latency".
func Factor(rttMs, jitterMs, lossPct float64) string {
	eff := rttMs/2 + 2*jitterMs + 10
	delay := 93.2 - R(rttMs, jitterMs, 0) // the delay part of the impairment
	loss := lossPenalty(lossPct)
	jit := 0.0
	if eff > 0 {
		jit = delay * (2 * jitterMs) / eff
	}
	lat := delay - jit
	switch {
	case loss >= jit && loss >= lat:
		return "loss"
	case jit >= lat:
		return "jitter"
	}
	return "latency"
}

// Minute is one target's (or one combined group's) one-minute bucket. RTT
// and Jitter are means in ms, Loss is the mean burst loss in percent; each is
// NaN when unknown. Bursts counts answered and timed-out bursts (Answered
// the former), Unknown the bursts that failed otherwise (DNS, refused: not
// loss).
type Minute struct {
	RTT, Jitter, Loss         float64
	Bursts, Answered, Unknown int
}

// Empty is a Minute without data.
var Empty = Minute{RTT: math.NaN(), Jitter: math.NaN(), Loss: math.NaN()}

// Scored reports whether m has enough data to score.
func (m Minute) Scored() bool { return m.Bursts >= MinBursts && !math.IsNaN(m.Loss) }

// RawMOS scores m with the E-model alone. A minute whose bursts all timed
// out has no RTT: it is scored on its loss alone (100 % loss is unusable
// whatever the delay). Missing jitter (single answers) counts as 0.
func (m Minute) RawMOS() float64 {
	return Score(nanTo(m.RTT, 0), nanTo(m.Jitter, 0), nanTo(m.Loss, 0))
}

// MOS is m's score: RawMOS, lowered by the jitter caps (JitterCapMOS).
func (m Minute) MOS() float64 { return min(m.RawMOS(), JitterCapMOS(m.Jitter)) }

// Band is m's band: the band of its MOS, which the jitter caps have
// already lowered (always Capped(BandOf(m.RawMOS()), m.Jitter)).
func (m Minute) Band() Band { return BandOf(m.MOS()) }

// Factor is what costs m its band: "jitter" when a jitter cap decided it,
// else Factor of its round trip, jitter and loss.
func (m Minute) Factor() string {
	if m.Band() != BandOf(m.RawMOS()) {
		return "jitter"
	}
	return Factor(nanTo(m.RTT, 0), nanTo(m.Jitter, 0), nanTo(m.Loss, 0))
}

func nanTo(v, d float64) float64 {
	if math.IsNaN(v) {
		return d
	}
	return v
}

// Combine merges several targets' minutes into one: each metric is the
// median over the targets that have it (one bad server does not make the
// path bad), and the burst counts add up.
func Combine(ms []Minute) Minute {
	out := Empty
	var rtt, jit, loss []float64
	for _, m := range ms {
		out.Bursts += m.Bursts
		out.Answered += m.Answered
		out.Unknown += m.Unknown
		if m.Bursts == 0 {
			continue
		}
		if !math.IsNaN(m.RTT) {
			rtt = append(rtt, m.RTT)
		}
		if !math.IsNaN(m.Jitter) {
			jit = append(jit, m.Jitter)
		}
		if !math.IsNaN(m.Loss) {
			loss = append(loss, m.Loss)
		}
	}
	out.RTT, out.Jitter, out.Loss = Median(rtt), Median(jit), Median(loss)
	return out
}

// DeadAttempts: a target with at least this many bursts in a window (any
// outcome) and none answered is not answering, when another target did
// answer (its server is down or filtered, not the user's connection).
const DeadAttempts = 6

// NotAnswering reports, per target (each a window of minutes), whether it
// is not answering: at least DeadAttempts bursts and none answered, while
// another target answered. Pass every STUN target of the profile.
func NotAnswering(targets [][]Minute) []bool {
	answered := make([]int, len(targets))
	tried := make([]int, len(targets))
	alive := 0
	for i, ms := range targets {
		for _, m := range ms {
			answered[i] += m.Answered
			tried[i] += m.Bursts + m.Unknown
		}
		if answered[i] > 0 {
			alive++
		}
	}
	out := make([]bool, len(targets))
	for i := range targets {
		out[i] = answered[i] == 0 && tried[i] >= DeadAttempts && alive > 0
	}
	return out
}

// Group combines targets minute by minute (see Combine) into n minutes,
// leaving out the targets marked dead. A minute that fewer than half of the
// targets measured (the minute in progress, when only some have probed yet)
// keeps its counts but is not scored: a median over the wrong half would
// misjudge it. ok is false when targets were given but every one is dead.
func Group(targets [][]Minute, dead []bool, n int) (ms []Minute, ok bool) {
	var keep [][]Minute
	for i, t := range targets {
		if i >= len(dead) || !dead[i] {
			keep = append(keep, t)
		}
	}
	// Targets that measured anything in the window.
	active := 0
	for _, t := range keep {
		if slices.ContainsFunc(t, func(m Minute) bool { return m.Bursts > 0 }) {
			active++
		}
	}
	ms = make([]Minute, n)
	row := make([]Minute, len(keep))
	for i := range ms {
		have := 0
		for k := range keep {
			row[k] = Empty
			if i < len(keep[k]) {
				row[k] = keep[k][i]
			}
			if row[k].Bursts > 0 {
				have++
			}
		}
		ms[i] = Combine(row)
		if 2*have < active {
			ms[i].RTT, ms[i].Jitter, ms[i].Loss = math.NaN(), math.NaN(), math.NaN()
		}
	}
	return ms, len(keep) > 0 || len(targets) == 0
}

// Median returns the median of v (NaN when empty). v is not modified.
func Median(v []float64) float64 {
	return Quantile(v, 0.5)
}

// Quantile returns the q-quantile of v by linear interpolation (NaN when
// empty). v is not modified.
func Quantile(v []float64, q float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := slices.Clone(v)
	slices.Sort(s)
	pos := q * float64(len(s)-1)
	i := int(pos)
	if i >= len(s)-1 {
		return s[len(s)-1]
	}
	return s[i] + (s[i+1]-s[i])*(pos-float64(i))
}

// Summary judges a window of minutes by its worst scored minute: the one
// with the lowest MOS (jitter caps included).
type Summary struct {
	// MOS and Band are the worst scored minute's; Worst is its index, -1
	// when no minute is scored.
	MOS   float64
	Band  Band
	Worst int
	// RTT50 and RTT95 are the median and p95 of the scored minutes' RTTs;
	// Jitter and Loss (percent) are their means, weighted by bursts.
	RTT50, RTT95, Jitter, Loss float64
	// Bursts and Unknown add up every minute's (scored or not); Scored
	// counts the scored minutes.
	Bursts, Unknown, Scored int
	// Factor is what costs the worst minute its band (see Minute.Factor).
	Factor string
}

// Summarize scores each minute and returns the window's summary.
func Summarize(ms []Minute) Summary {
	s := Summary{MOS: math.NaN(), Worst: -1, RTT50: math.NaN(), RTT95: math.NaN(), Jitter: math.NaN(), Loss: math.NaN()}
	var rtts []float64
	var jSum, jW, lSum, lW float64
	for i, m := range ms {
		s.Bursts += m.Bursts
		s.Unknown += m.Unknown
		if !m.Scored() {
			continue
		}
		s.Scored++
		w := float64(m.Bursts)
		if !math.IsNaN(m.RTT) {
			rtts = append(rtts, m.RTT)
		}
		if !math.IsNaN(m.Jitter) {
			jSum += m.Jitter * w
			jW += w
		}
		lSum += m.Loss * w
		lW += w
		if mos := m.MOS(); s.Worst < 0 || mos < s.MOS {
			s.MOS, s.Worst = mos, i
		}
	}
	if s.Worst < 0 {
		return s
	}
	s.Band = BandOf(s.MOS)
	s.RTT50, s.RTT95 = Median(rtts), Quantile(rtts, 0.95)
	if jW > 0 {
		s.Jitter = jSum / jW
	}
	s.Loss = lSum / lW
	s.Factor = ms[s.Worst].Factor()
	return s
}

// Tags the Calls view maps targets by (see internal/profile/catalog/SCHEMA.md).
const (
	TagUDPPath = "udp-path" // a STUN target on the call path
	TagApp     = "app:"     // prefix: "app:teams", "app:meet", ...
)

// Apps are the call apps, in display order. Teams and Meet are measured over
// their own STUN servers; the others pick a media server per meeting and
// cannot be tested ahead of a call.
var Apps = []struct {
	ID         string
	Measurable bool
}{
	{"teams", true}, {"meet", true},
	{"zoom", false}, {"slack", false}, {"discord", false}, {"whatsapp", false}, {"webex", false},
}

// IsCorePath reports a call-path STUN target that measures the user's own
// connection: tagged udp-path, anycast or GeoDNS (no fixed far-away region
// such as Twilio's world map).
func IsCorePath(t model.Target) bool {
	return t.IsUDP() && slices.Contains(t.Tags, TagUDPPath) && (t.Geo == "" || t.Geo == "global")
}

// HasApp reports a target tagged for an app ("teams", "meet", ...).
func HasApp(t model.Target, app string) bool { return slices.Contains(t.Tags, TagApp+app) }

// IsApp reports a target tagged for any app.
func IsApp(t model.Target) bool {
	return slices.ContainsFunc(t.Tags, func(x string) bool { return strings.HasPrefix(x, TagApp) })
}
