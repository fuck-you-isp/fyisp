// Package tiles judges the fixed Home tiles (profile.Tiles) minute by
// minute: each tile's word (Good, Slow or Choppy, Down), its 5-minute bars,
// its drops, the usual latency of its endpoints, the connection card's state
// and the problems of the last 7 days with their likely causes.
//
// This file and its siblings hold the rules only: pure functions and small
// state machines with no I/O, the time always passed in. The engine (a
// model.Sink next to verdict.Engine) feeds them from the probe samples.
//
// The verdict engine (internal/verdict) stays the judge of the connection:
// it looks at a 60 s window every 5 s and owns the incident log. The tiles
// describe apps over minutes, so their thresholds differ:
//
//   - verdict lossBad 20% with ≥ 2 lost (verdict/rules.go): a target is
//     unhealthy. A web tile-minute is Down when more than half of its
//     checks fail (downShare) and Slow from a quarter (webSlowLoss).
//   - verdict spike 3× / +50 ms and normal 2.5× / p95+30 ms (7-day
//     baseline): the tiles compare with a 24 h usual of good minutes only,
//     Slow at 2× AND +30 ms (slowFactor). Latency alone never makes an app
//     or stream tile Down: only failed checks do (a 150 ms TCP connect to
//     a service that usually takes 24 ms is slow, not down).
//   - verdict Slow hint 2× / +10 ms: the tiles want +30 ms, so 5 ms → 12 ms
//     is not Slow.
//   - verdict udp_degraded 5% loss or 30 ms jitter on ≥ 2 providers for
//     3 min (verdict/udp.go): a call tile is Choppy from 2% loss (in at
//     least 2 bursts), 30 ms jitter or 150 ms delay over 3 min. A call tile
//     can go Choppy well before udp_degraded: tiles describe apps, the card
//     describes the connection.
//   - callq JitterFairMs 30 ms: the same jitter limit as callChoppyJitterMs.
package tiles

import (
	"math"
	"slices"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/profile"
)

// Call tiles: judged on their UDP endpoints over the last callWindow.
const (
	callWindow         = 3 * time.Minute
	callChoppyDelayMs  = 150.0 // median delay above this: Choppy
	callChoppyJitterMs = 30.0  // median burst jitter above this: Choppy
	callChoppyLossPct  = 2     // mean burst loss (%) at or above this ...
	// ... in at least this many bursts: Choppy. One lost burst is 2.8% of
	// a 36-burst window, a stall of ~100 ms that call software hides.
	callChoppyLossBursts = 2
	callDownDelayMs      = 400.0 // median delay above this: Down
	callDownLossPct      = 10    // mean burst loss (%) at or above this ...
	callDownLossMins     = 2     // ... in each of the last this many minutes: Down
	// callMinBursts bursts are needed in the window, and at least half the
	// bursts the cadence should give (36 at 5 s, 12 for UDP 53 at 15 s);
	// fewer is "not enough data".
	callMinBursts = 6
)

// Web (app and stream) endpoints, per minute, against the usual latency.
const (
	slowFactor, slowExtraMs = 2.0, 30.0 // Slow: median > 2× usual AND > usual + 30 ms
	downShare               = 0.5       // Down: more than this share of the checks lost (the only Down of a web minute)
	// webSlowLoss of a minute's checks lost (1 of 4 at 15 s) is Slow, on
	// app and stream tiles alike: partial loss is never Good. The verdict
	// calls a target unhealthy only at lossBad 20% with ≥ 2 lost.
	webSlowLoss = 0.25
	minChecks   = 2 // measured checks per endpoint-minute (15 s cadence gives 4)
)

// Learning: fixed limits while the usual is unknown.
const (
	learnFor    = 24 * time.Hour // after the first stored sample
	learnSlowMs = 300.0
)

// Word hysteresis and the immediate outage ("Down now").
const (
	badOf, badWindow = 3, 5 // Slow/Choppy when 3 of the last 5 judged minutes are bad
	goodRun          = 5    // back to Good after 5 good minutes in a row
	immFailMulti     = 2    // latest failed checks/bursts on most members: Down now ...
	immFailSingle    = 3    // ... of a single-endpoint group (one front-door hiccup is not an outage)
	immClearOK       = 2    // checks/bursts OK of the latest immClearWin on at least half the members: the outage is over
	immClearWin      = 3    // (not "in a row": an endpoint answering every other check is no outage)
)

// Drops.
const (
	dropMin        = 30 * time.Second // total loss this long is a drop
	tileDropChecks = 3                // failed checks in a row a web endpoint needs for a tile drop
	dropNoteFor    = 30 * time.Minute // a tile notes its newest drop this long
	recoveredFor   = 30 * time.Minute // "Working again" this long after a connection problem
	dropEndedOK    = 10 * time.Second // 1 s path probes OK this long: a connection drop ended
	connDropNow    = 5 * time.Second  // gateway or edge fully lost this long: a drop in progress
	sleepFactor    = 2                // checks further apart than 2× the interval: a sleep or restart between them
	durRound       = 10 * time.Second // drop durations are rounded to this (half down)
	resultLag      = 5 * time.Second  // the longest probe timeout (HTTPS, probe/runner.go): see holdFor
	clockBack      = 2 * time.Minute  // a check this far behind the newest: the wall clock was set back
	againDropped   = time.Minute      // full loss shorter than this "dropped"; longer "was down"
)

// Bars.
const (
	barStep     = 5 * time.Minute
	nBars       = 24
	barSlowMins = 2 // slow minutes that make a bar slow
	barNoneMins = 3 // minutes without data that make a bar none (fyisp not running)
)

// Usual latency.
const (
	usualWindow  = 24 * time.Hour
	usualEvery   = 10 * time.Minute // recompute period
	relearnAfter = 60 * time.Minute // a stable shift this long is a route change
	minGood      = 60               // good minutes a usual needs (fixed limits until then)
	shiftData    = 45               // minutes with data the shift window needs (of 60)
	shiftStable  = 1.3              // p90/p10 of a stable shift
	shiftDown    = 1.5              // a downward shift: median < usual/1.5
)

// Connection state, grouping and attribution.
const (
	staleAfter   = 2 * time.Minute
	verdictLag   = 65 * time.Second // the verdict's 60 s window (and a tick) outlasts a drop by this much
	absorbBefore = time.Minute      // a connection episode absorbs what starts this long before it ...
	absorbAfter  = 2 * time.Minute  // ... and ends this long after it
	dropLate     = 30 * time.Second // a drop absorbs only the tile parts that start before its end + this
	lagSlack     = 10 * time.Second // a verdict incident starting this soon after a drop ended can still be its lag
	// lagMax bounds that lag: the verdict's 60 s window and its recovery
	// (netns: incidents end 100–110 s after a drop). An incident still on
	// this long after the drop ended is a problem of its own.
	lagMax = 150 * time.Second
	// outIsolated: a path outage this soon after another is part of a
	// flapping link (loss), not a blip of its own (pathOut)
	outIsolated = 2 * time.Minute
	// lossyRunMax: a total-loss run of the edge this long is an outage,
	// even inside heavy loss (40% random loss has 5 s runs, not 15 s)
	lossyRunMax   = 15 * time.Second
	blockedLead   = 3 * time.Minute // a blocked-UDP episode takes call-tile problems that started this long before it
	causeSlack    = time.Minute     // incidents this close to an episode explain it (web/home.go homePathLead)
	vendorMerge   = 2 * time.Minute // tile episodes with one cause starting this close are one problem
	sameTileMerge = 5 * time.Minute // episodes of one tile closer than this are one problem
	broadShare    = 0.6             // broad slowdown: this share of the covered tiles bad ...
	broadVendors  = 3               // ... across this many vendors
	fullLoss      = 0.9             // an incident with this peak loss lost everything
	// lossyBelow: an isp verdict whose loss at the edge is under this, with
	// no drop in progress, is partial loss: the amber "losing some data"
	// card. 6% is a degraded line (calls choppy, sites fine); 40% breaks
	// most things and keeps the red "having trouble" card.
	lossyBelow = 0.3
	edgeRecent = 30 * time.Second // the edge's latest checks the card reads besides the verdict's window
	layerLossy = 0.02             // a layer losing this much is lossy
	layerClean = 0.01             // a layer losing less is clean
)

// State is a tile's word, a minute's judgement or a bar's colour.
type State string

const (
	Good State = "good"
	Slow State = "slow" // the word is "Choppy" on call tiles
	Down State = "down"
	None State = "none" // no data
	Off  State = "off"  // the profile has no targets for the tile
)

// Bad reports Slow or Down.
func (s State) Bad() bool { return s == Slow || s == Down }

func rank(s State) int {
	switch s {
	case Good:
		return 1
	case Slow:
		return 2
	case Down:
		return 3
	}
	return 0
}

// worse returns the worse of a and b (None and Off rank below Good).
func worse(a, b State) State {
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// check is one result of a tracked series: a TCP/HTTPS/ICMP check or a
// STUN burst (the engine merges a burst's three kinds by slot), or a slot
// that was not measured (model.ReasonGap: fyisp stopped, a speed test's
// mask), which only counts in minute.gap.
type check struct {
	ms      int64  // the slot, Unix ms
	rttUs   uint32 // when !lost; UDP the burst's median RTT
	jitUs   uint32 // UDP, when !lost: the burst's jitter; noJitter when not measured
	lossPct uint8  // UDP: % of the burst's requests lost (100 when lost)
	lost    bool   // failed; a burst without any answer
	gap     bool
}

const noJitter = math.MaxUint32

func (c check) at() time.Time { return time.UnixMilli(c.ms).UTC() }

// minute is one series' closed minute. Lost checks that belong to a drop
// (see failRun.dropLike), or to a run still open when the minute closes,
// are left out: a drop counts only as the drop, so a 40 s drop does not
// leave calls Choppy afterwards. Other lost checks are loss.
type minute struct {
	ok, lost, gap uint16
	medUs         uint32 // median RTT of the successes; UDP the burst RTTs
	// jitUs is the median jitter of the bursts that measured one (UDP), or
	// noJitter. A call window takes the median of these minute medians.
	jitUs      uint32
	lossPctSum uint32 // UDP: the bursts' loss (%), summed; a lost burst counts 100
	lossy      uint16 // UDP: bursts that lost anything (lost, or lossPct > 0)
}

func (m minute) n() int            { return int(m.ok) + int(m.lost) }
func (m minute) medMs() float64    { return float64(m.medUs) / 1000 }
func (m minute) hasJitter() bool   { return m.jitUs != noJitter && m.ok > 0 }
func (m minute) jitterMs() float64 { return float64(m.jitUs) / 1000 }

// lossPct is the mean burst loss in %.
func (m minute) lossPct() float64 {
	if m.n() == 0 {
		return 0
	}
	return float64(m.lossPctSum) / float64(m.n())
}

// summarize builds the minute starting at from from a series' checks (any
// order; others are ignored) and its failure runs as the tracker saw them,
// as of closeAt (minute end + 20 s): run ends after closeAt are ignored, so
// a run still open then is left out whenever it ended. The engine calls it
// once every check with a slot up to closeAt is applied (at the grid tick
// where now − holdFor(every) ≥ closeAt), so live and replay judge a minute
// alike. Pass only the runs that can touch the minute (pruneRuns keeps the
// list short): it scans them per lost check. rule says which runs are
// drops.
func summarize(cs []check, rs []failRun, rule runRule, from, closeAt time.Time) minute {
	return summarizeOut(cs, rs, rule, from, closeAt, nil)
}

// summarizeOut is summarize, also leaving out the lost checks of web and
// UDP series inside out: the path's own outages (pathOut), so a 20 s blip
// of the whole connection, too short for a drop, leaves no loss behind
// (calls Down or Choppy for minutes after it), while an endpoint's own
// short runs stay loss.
func summarizeOut(cs []check, rs []failRun, rule runRule, from, closeAt time.Time, out []window) minute {
	lo, hi, cl := from.UnixMilli(), from.Add(time.Minute).UnixMilli(), closeAt.UnixMilli()
	skip := func(c check) bool {
		if rule == rulePath {
			return false
		}
		for _, w := range out {
			if c.ms >= w.from.UnixMilli() && (w.to.IsZero() || c.ms < w.to.UnixMilli()) {
				return true
			}
		}
		for _, r := range rs {
			if c.ms < r.firstMs || c.ms > r.lastMs {
				continue
			}
			return r.toMs == 0 || r.toMs > cl || r.dropLike(rule)
		}
		return false
	}
	m := minute{jitUs: noJitter}
	var rb, jb [64]uint32 // a minute has at most 60 checks (1 s)
	rtts, jits := rb[:0], jb[:0]
	for _, c := range cs {
		switch {
		case c.ms < lo || c.ms >= hi:
		case c.gap:
			m.gap++
		case c.lost:
			if !skip(c) {
				m.lost++
				m.lossPctSum += 100
				m.lossy++
			}
		default:
			m.ok++
			rtts = append(rtts, c.rttUs)
			m.lossPctSum += uint32(c.lossPct)
			if c.lossPct > 0 {
				m.lossy++
			}
			if c.jitUs != noJitter {
				jits = append(jits, c.jitUs)
			}
		}
	}
	if len(rtts) > 0 {
		m.medUs = median(rtts)
	}
	if len(jits) > 0 {
		m.jitUs = median(jits)
	}
	return m
}

// median of a non-empty slice (the mean of the middle two for an even
// count). It sorts v in place.
func median[T ~uint32 | ~float64](v []T) T {
	slices.Sort(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// limits are a web endpoint's latency limit in ms (latency alone never
// makes it Down).
type limits struct{ slowMs float64 }

// limitsFor returns the limits for a usual latency: fixed while learning or
// while the usual is unknown (ok false).
func limitsFor(usualMs float64, ok, learning bool) limits {
	if learning || !ok {
		return limits{learnSlowMs}
	}
	return limits{slowMs: max(slowFactor*usualMs, usualMs+slowExtraMs)}
}

// judgeWeb judges one app or stream endpoint-minute: Down when most checks
// failed, Slow when a quarter did or on latency. Latency alone is never
// Down: apps and streams are Down only when their checks fail.
func judgeWeb(m minute, l limits) State {
	n := m.n()
	switch {
	case n < minChecks:
		return None
	case float64(m.lost) > downShare*float64(n):
		return Down
	case float64(m.lost) >= webSlowLoss*float64(n), m.medMs() > l.slowMs:
		return Slow
	}
	return Good
}

// callStats are the numbers a UDP endpoint is judged on over callWindow.
type callStats struct {
	bursts    int
	delayMs   float64 // median of the minute medians; 0 when nothing answered
	jitterMs  float64 // median of the minute jitters (each a median of its bursts)
	hasJitter bool
	lossPct   float64 // mean burst loss (%)
}

// judgeCall judges one UDP endpoint over callWindow from its minutes,
// oldest first (the last is the minute being judged; missing minutes are
// zero). every is the endpoint's burst interval.
func judgeCall(ms []minute, every time.Duration) (State, callStats) {
	var st callStats
	var mb, jb [8]float64
	meds, jits := mb[:0], jb[:0]
	var loss uint32
	lossy := 0
	for _, m := range ms {
		st.bursts += m.n()
		loss += m.lossPctSum
		lossy += int(m.lossy)
		if m.ok > 0 {
			meds = append(meds, m.medMs())
		}
		if m.hasJitter() {
			jits = append(jits, m.jitterMs())
		}
	}
	need := callMinBursts
	if every > 0 {
		need = max(need, int((callWindow/every+1)/2))
	}
	if st.bursts < need {
		return None, st
	}
	st.lossPct = float64(loss) / float64(st.bursts)
	if len(meds) > 0 {
		st.delayMs = median(meds)
	}
	if len(jits) > 0 {
		st.jitterMs, st.hasJitter = median(jits), true
	}
	lossDown := len(ms) >= callDownLossMins
	for _, m := range ms[max(0, len(ms)-callDownLossMins):] {
		// from callChoppyLossBursts lossy bursts a minute: one short outage
		// straddling two minutes (a burst or two lost in each) is no Down
		lossDown = lossDown && m.n() > 0 && m.lossPctSum >= callDownLossPct*uint32(m.n()) && int(m.lossy) >= callChoppyLossBursts
	}
	switch {
	case st.delayMs > callDownDelayMs || lossDown:
		return Down, st
	case st.delayMs > callChoppyDelayMs || st.jitterMs > callChoppyJitterMs,
		loss >= callChoppyLossPct*uint32(st.bursts) && lossy >= callChoppyLossBursts:
		return Slow, st
	}
	return Good, st
}

// vote combines the endpoints of one member of a group: a provider of the
// shared call path (Cloudflare-STUN and Cloudflare-UDP53 are one vote) is
// as bad as its worst endpoint with data.
func vote(ss ...State) State {
	out := None
	for _, s := range ss {
		out = worse(out, s)
	}
	return out
}

// majority combines the members of a group, or the groups' members of a
// tile: only members with data count; Down when more than half are Down,
// Slow when more than half are Slow or Down. A tie is not a majority.
func majority(ss []State) State {
	var n, down, bad int
	for _, s := range ss {
		if s == None {
			continue
		}
		n++
		if s == Down {
			down++
		}
		if s.Bad() {
			bad++
		}
	}
	switch {
	case n == 0:
		return None
	case 2*down > n:
		return Down
	case 2*bad > n:
		return Slow
	}
	return Good
}

// tileMinute judges a tile-minute from its web endpoints and, for call
// tiles, its UDP members (provider votes on the shared path). A call tile
// takes its quality from UDP and is also Down when its web group is
// ("not connecting"); without UDP data it is judged on TCP alone, where
// Choppy is impossible.
func tileMinute(kind profile.TileKind, web, udp []State) State {
	w := majority(web)
	if kind != profile.TileCall {
		return w
	}
	switch u := majority(udp); {
	case u == None && w == Slow:
		return Good
	case u == None, w == Down:
		return w
	default:
		return u
	}
}

// signal is a group's or a tile's immediate outage signal.
type signal int8

const (
	noData  signal = iota // no member has checks: the group takes no part
	hold                  // neither: the word keeps its outage state
	failing               // Down now
	cleared               // the latest checks are OK: any outage is over
)

// immediate is the "Down now" signal of one group (a tile's web endpoints,
// or its UDP members) from the members' run trackers, as flushed at the
// current grid tick:
//
//   - failing when more than half of the members with checks have their
//     latest k checks lost (k is immFailSingle for a single-member group,
//     immFailMulti otherwise); at is when that majority started failing;
//   - cleared when it is not failing and at least half have immClearOK
//     answers among their latest immClearWin checks (a tie clears, as it
//     is no majority for Down); at is the slot of the check that made it
//     so;
//   - noData when no member has checks.
//
// A member with several series (a provider) fails when any of them does and
// is OK when all are.
func immediate(members [][]*tracker) (signal, time.Time) {
	k := immFailMulti
	if len(members) == 1 {
		k = immFailSingle
	}
	var fb, ob [16]int64
	fails, oks := fb[:0], ob[:0]
	n := 0
	for _, m := range members {
		var has, fail bool
		good := true
		var start, okAt int64
		for _, t := range m {
			if t.last == 0 {
				continue
			}
			has = true
			if int(t.cur.lost) >= k {
				fail = true
				if start == 0 || t.cur.firstMs < start {
					start = t.cur.firstMs
				}
			}
			good = good && t.okAt != 0
			okAt = max(okAt, t.okAt)
		}
		if !has {
			continue
		}
		n++
		switch {
		case fail:
			fails = append(fails, start)
		case good:
			oks = append(oks, okAt)
		}
	}
	switch {
	case n == 0:
		return noData, time.Time{}
	case 2*len(fails) > n:
		slices.Sort(fails)
		return failing, msTime(fails[n/2])
	case 2*len(oks) >= n:
		slices.Sort(oks)
		return cleared, msTime(oks[(n+1)/2-1])
	}
	return hold, time.Time{}
}

// tileSignal combines a tile's group signals: failing when any group is
// (at the earliest), cleared when every group with checks is (at the
// latest). Groups without checks take no part.
func tileSignal(sigs []signal, ats []time.Time) (signal, time.Time) {
	out, at := noData, time.Time{}
	for i, s := range sigs {
		switch {
		case s == noData:
		case s == failing:
			if out != failing || ats[i].Before(at) {
				at = ats[i]
			}
			out = failing
		case out == failing:
		case s == hold || out == hold:
			out, at = hold, time.Time{}
		default: // cleared, and so far cleared or no data
			out = cleared
			if ats[i].After(at) {
				at = ats[i]
			}
		}
	}
	return out, at
}

// wordRun is a period in which a tile's word was not Good. End is zero while
// it lasts.
type wordRun struct {
	Start, End time.Time
	Sev        State // the worst: Slow or Down
}

// word is a tile's word with its hysteresis. The zero value has seen no
// data (None).
//
//   - Down from an outage (the immediate signal failing) shows at once, since
//     the first failed slot, and ends at the slot of the check that cleared
//     it: no 5-minute wait.
//   - Slow (Choppy) is entered when badOf of the last badWindow judged
//     minutes are bad, and left after goodRun good minutes in a row.
//   - Down from a Down minute without an outage (most of a web minute's
//     checks lost; on call tiles also delay over 400 ms or 10% loss in 2
//     minutes; never latency on app and stream tiles) shows at once
//     (since that minute) and is left after goodRun good minutes in a
//     row, or as soon as the last badWindow judged minutes hold no Down
//     minute: to Slow when badOf of them are bad (since the first of
//     those), else to Good. A call tile's Down from loss alone (lossDown)
//     is left sooner: as soon as a judged minute is not Down (the loss
//     rule looks at callDownLossMins minutes), the same way.
//   - None minutes hold the word and do not count.
//
// A run that ends on good minutes ends at the first of them, or at the
// end of an outage that cleared later. Runs of one tile can touch (an
// outage inside a Slow run's last minutes): episodes merge them
// (sameTileMerge).
type word struct {
	outage   bool
	outSince time.Time
	level    State // "" before the first judged minute
	lvlSince time.Time
	recent   [badWindow]judged // the last judged minutes, newest first
	nRecent  int
	good     int       // good minutes in a row
	goodFrom time.Time // the first of them
	cleared  time.Time // when the latest outage cleared
	cur      wordRun   // the open run (Start zero: none)
	lossDown bool      // the Down level comes from call loss alone
}

// judged is a judged tile-minute.
type judged struct {
	at time.Time
	s  State
}

// state returns the word and since when it holds (zero for Good and None).
func (w *word) state() (State, time.Time) {
	switch {
	case w.outage:
		return Down, w.outSince
	case w.level == "":
		return None, time.Time{}
	case w.level == Good:
		return Good, time.Time{}
	}
	return w.level, w.lvlSince
}

// open returns the run in progress, if any.
func (w *word) open() (wordRun, bool) { return w.cur, !w.cur.Start.IsZero() }

// outageSignal applies the tile's immediate signal (tileSignal) with its
// time. It returns the run that ended, if the word went back to Good.
func (w *word) outageSignal(sig signal, at time.Time) *wordRun {
	switch {
	case sig == failing && !w.outage:
		w.outage, w.outSince = true, at
	case sig == cleared && w.outage:
		w.outage, w.cleared = false, at
	default:
		return nil
	}
	return w.track(at)
}

// step applies the tile-minute that starts at at.
func (w *word) step(at time.Time, s State) *wordRun { return w.stepLoss(at, s, false) }

// stepLoss is step for a call tile-minute; lossDown: a Down minute that
// comes from UDP loss alone (callDownLossPct), which lifts sooner.
func (w *word) stepLoss(at time.Time, s State, lossDown bool) *wordRun {
	if s == None {
		return nil
	}
	copy(w.recent[1:], w.recent[:badWindow-1])
	w.recent[0].at, w.recent[0].s = at, s
	w.nRecent = min(w.nRecent+1, badWindow)
	if s == Good {
		if w.good == 0 {
			w.goodFrom = at
		}
		w.good++
	} else {
		w.good = 0
	}
	bad, first, down := w.bad()
	if s == Down {
		w.lossDown = lossDown && (w.level != Down || w.lossDown)
	}
	if w.level == Down && w.lossDown {
		// loss Down lifts as soon as the rule, over its last
		// callDownLossMins minutes, no longer finds the Down loss
		down = s == Down
	}
	switch {
	case s == Down && w.level != Down:
		w.level, w.lvlSince = Down, at
	case w.level != "" && w.level != Good && w.good >= goodRun:
		w.level = Good
		return w.track(w.endAt(w.goodFrom))
	case w.level == Down && !down && bad >= badOf:
		w.level, w.lvlSince = Slow, first
	case w.level == Down && !down:
		w.level = Good
		end := at
		if w.good > 0 {
			end = w.goodFrom
		}
		return w.track(w.endAt(end))
	case (w.level == "" || w.level == Good) && bad >= badOf:
		w.level, w.lvlSince = Slow, first
	case w.level == "":
		w.level = Good
	}
	return w.track(at)
}

// endAt is the end of a run that ends on good minutes from t on: no
// earlier than the latest outage in it cleared.
func (w *word) endAt(t time.Time) time.Time {
	if w.cleared.After(t) {
		return w.cleared
	}
	return t
}

// bad counts the bad minutes of the last badWindow judged ones, returns the
// first of them and whether one was Down.
func (w *word) bad() (int, time.Time, bool) {
	n, down := 0, false
	var first time.Time
	for i := range w.nRecent {
		if r := w.recent[i]; r.s.Bad() {
			n++
			first = r.at
			down = down || r.s == Down
		}
	}
	return n, first, down
}

// track opens, extends or closes the run after a change at time at (the
// end of a run that closes).
func (w *word) track(at time.Time) *wordRun {
	s, since := w.state()
	switch {
	case s.Bad() && w.cur.Start.IsZero():
		w.cur = wordRun{Start: since, Sev: s}
	case s.Bad():
		w.cur.Sev = worse(w.cur.Sev, s)
	case !w.cur.Start.IsZero():
		r := w.cur
		r.End = at
		w.cur = wordRun{}
		return &r
	}
	return nil
}
