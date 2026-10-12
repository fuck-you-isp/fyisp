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
//     unhealthy. A web tile is Down only when more than half of a minute's
//     checks fail (downShare); scattered loss below that is not a word.
//   - verdict spike 3× / +50 ms and normal 2.5× / p95+30 ms (7-day
//     baseline): the tiles compare with a 24 h usual of good minutes only,
//     Slow at 2× AND +30 ms, Down at 3× AND +100 ms (slowFactor/ downFactor).
//   - verdict Slow hint 2× / +10 ms: the tiles want +30 ms, so 5 ms → 12 ms
//     is not Slow.
//   - verdict udp_degraded 5% loss or 30 ms jitter on ≥ 2 providers for
//     3 min (verdict/udp.go): a call tile is Choppy from 2% loss, 30 ms jitter
//     or 150 ms delay over 3 min. A call tile can go Choppy well before
//     udp_degraded: tiles describe apps, the card describes the connection.
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
	callChoppyLoss     = 0.02  // mean burst loss at or above this: Choppy
	callDownDelayMs    = 400.0 // median delay above this: Down
	callDownLoss       = 0.10  // mean burst loss at or above this ...
	callDownLossMins   = 2     // ... in each of the last this many minutes: Down
	// callMinBursts bursts are needed in the window, and at least half the
	// bursts the cadence should give (36 at 5 s, 12 for UDP 53 at 15 s);
	// fewer is "not enough data".
	callMinBursts = 6
	lossEps       = 1e-9 // a mean of burst losses at a limit counts as reaching it
)

// Web (app and stream) endpoints, per minute, against the usual latency.
const (
	slowFactor, slowExtraMs = 2.0, 30.0  // Slow: median > 2× usual AND > usual + 30 ms
	downFactor, downExtraMs = 3.0, 100.0 // Down: median > 3× usual AND > usual + 100 ms
	downShare               = 0.5        // Down: more than this share of the checks lost
	minChecks               = 2          // measured checks per endpoint-minute (15 s cadence gives 4)
)

// Learning: fixed limits while the usual is unknown.
const (
	learnFor    = 24 * time.Hour // after the first stored sample
	learnSlowMs = 300.0
	learnDownMs = 1000.0
)

// Word hysteresis and the immediate outage ("Down now").
const (
	badOf, badWindow = 3, 5 // Slow/Choppy when 3 of the last 5 judged minutes are bad
	goodRun          = 5    // back to Good after 5 good minutes in a row
	immFailMulti     = 2    // latest failed checks/bursts on most members: Down now ...
	immFailSingle    = 3    // ... of a single-endpoint group (one front-door hiccup is not an outage)
	immClearOK       = 2    // latest checks/bursts OK on most members: the outage is over
)

// Drops.
const (
	dropMin        = 30 * time.Second // total loss this long is a drop
	tileDropChecks = 3                // failed checks in a row a web endpoint needs for a tile drop
	dropNoteFor    = 30 * time.Minute // a tile notes its newest drop this long
	recoveredFor   = 30 * time.Minute // "Working again" this long after a connection problem
	dropEndedOK    = 10 * time.Second // 1 s path probes OK this long: a connection drop ended
	connDropNow    = 5 * time.Second  // gateway or edge fully lost this long: a drop in progress
	sleepFactor    = 2                // the last success before a run must be within 2× the interval
	durRound       = 10 * time.Second // drop durations are rounded to this
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
	staleAfter    = 2 * time.Minute
	absorbBefore  = time.Minute     // a connection episode absorbs what starts this long before it ...
	absorbAfter   = 2 * time.Minute // ... and ends this long after it
	causeSlack    = time.Minute     // incidents this close to an episode explain it (web/home.go homePathLead)
	vendorMerge   = 2 * time.Minute // tile episodes with one cause starting this close are one problem
	sameTileMerge = 5 * time.Minute // episodes of one tile closer than this are one problem
	broadShare    = 0.6             // broad slowdown: this share of the covered tiles bad ...
	broadVendors  = 3               // ... across this many vendors
	fullLoss      = 0.9             // an incident with this peak loss lost everything
	layerLossy    = 0.02            // a layer losing this much is lossy
	layerClean    = 0.01            // a layer losing less is clean
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
// STUN burst (the engine merges a burst's three kinds by slot). Samples
// that were not measured (model.ReasonGap) are not checks.
type check struct {
	at     time.Time
	lost   bool          // failed; a burst without any answer
	rtt    time.Duration // when !lost
	jitter time.Duration // UDP, when !lost; negative when not measured
	loss   float64       // UDP: share of the burst's requests lost (1 when lost)
}

// tail is the recent checks of one series, oldest first, and its interval.
type tail struct {
	every  time.Duration
	checks []check
}

// sorted returns the checks in time order: a late timeout can arrive after
// a later slot's success.
func (t tail) sorted() []check {
	if slices.IsSortedFunc(t.checks, cmpCheck) {
		return t.checks
	}
	c := slices.Clone(t.checks)
	slices.SortStableFunc(c, cmpCheck)
	return c
}

func cmpCheck(a, b check) int { return a.at.Compare(b.at) }

// minute is one series' closed minute. Checks inside a drop run are left
// out: a drop counts only as the drop, so a 40 s drop does not leave calls
// Choppy afterwards.
type minute struct {
	ok, lost uint16
	medUs    uint32  // median RTT of the successes; UDP the burst RTTs
	jitUs    uint32  // UDP: median jitter of the bursts that measured one; noJitter when none did
	loss     float64 // UDP: mean burst loss, a lost burst counting 1
}

const noJitter = math.MaxUint32

func (m minute) n() int            { return int(m.ok) + int(m.lost) }
func (m minute) medMs() float64    { return float64(m.medUs) / 1000 }
func (m minute) hasJitter() bool   { return m.jitUs != noJitter && m.ok > 0 }
func (m minute) jitterMs() float64 { return float64(m.jitUs) / 1000 }

// summarize builds the minute starting at from from a series' tail. Checks
// in failure runs of dropMin or more are left out, and so are those of a run
// still open at the tail's end (it may yet become a drop).
func summarize(t tail, from time.Time) minute {
	cs := t.sorted()
	skip := make([]bool, len(cs))
	for _, r := range runs(t) {
		if r.open() || r.est() >= dropMin {
			for i := r.i; i < r.i+r.lost; i++ {
				skip[i] = true
			}
		}
	}
	to := from.Add(time.Minute)
	m := minute{jitUs: noJitter}
	var rtts, jits []time.Duration
	var loss float64
	for i, c := range cs {
		if skip[i] || c.at.Before(from) || !c.at.Before(to) {
			continue
		}
		if c.lost {
			m.lost++
			loss++
			continue
		}
		m.ok++
		rtts = append(rtts, c.rtt)
		loss += c.loss
		if c.jitter >= 0 {
			jits = append(jits, c.jitter)
		}
	}
	if len(rtts) > 0 {
		m.medUs = uint32(median(rtts).Microseconds())
	}
	if len(jits) > 0 {
		m.jitUs = uint32(median(jits).Microseconds())
	}
	if n := m.n(); n > 0 {
		m.loss = loss / float64(n)
	}
	return m
}

// median of a non-empty slice (the mean of the middle two for an even
// count); it sorts a copy.
func median[T ~int64 | ~float64](v []T) T {
	s := slices.Clone(v)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// limits are a web endpoint's latency limits in ms.
type limits struct{ slowMs, downMs float64 }

// limitsFor returns the limits for a usual latency: fixed while learning or
// while the usual is unknown (ok false).
func limitsFor(usualMs float64, ok, learning bool) limits {
	if learning || !ok {
		return limits{learnSlowMs, learnDownMs}
	}
	return limits{
		slowMs: max(slowFactor*usualMs, usualMs+slowExtraMs),
		downMs: max(downFactor*usualMs, usualMs+downExtraMs),
	}
}

// judged is an endpoint's, a member's or a tile's minute: its state and
// whether every measured check failed.
type judged struct {
	s       State
	allLost bool
}

// judgeWeb judges one app or stream endpoint-minute. Streams are Down only
// when their checks fail, never on latency.
func judgeWeb(m minute, l limits, stream bool) judged {
	n := m.n()
	switch {
	case n < minChecks:
		return judged{s: None}
	case float64(m.lost) > downShare*float64(n):
		return judged{s: Down, allLost: m.ok == 0}
	case m.medMs() > l.downMs && !stream:
		return judged{s: Down}
	case m.medMs() > l.slowMs:
		return judged{s: Slow}
	}
	return judged{s: Good}
}

// callStats are the numbers a UDP endpoint is judged on over callWindow.
type callStats struct {
	bursts    int
	delayMs   float64 // median of the minute medians; 0 when nothing answered
	jitterMs  float64 // median of the minute jitters
	hasJitter bool
	loss      float64 // mean burst loss
}

// judgeCall judges one UDP endpoint over callWindow from its minutes,
// oldest first (the last is the minute being judged; missing minutes are
// zero). every is the endpoint's burst interval.
func judgeCall(ms []minute, every time.Duration) (judged, callStats) {
	var st callStats
	var meds, jits []float64
	var loss float64
	all := true
	for _, m := range ms {
		n := m.n()
		st.bursts += n
		loss += m.loss * float64(n)
		if m.ok > 0 {
			all = false
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
		return judged{s: None}, st
	}
	st.loss = loss / float64(st.bursts)
	if len(meds) > 0 {
		st.delayMs = median(meds)
	}
	if len(jits) > 0 {
		st.jitterMs, st.hasJitter = median(jits), true
	}
	lossDown := len(ms) >= callDownLossMins
	for _, m := range ms[max(0, len(ms)-callDownLossMins):] {
		lossDown = lossDown && m.n() > 0 && m.loss >= callDownLoss-lossEps
	}
	switch {
	case st.delayMs > callDownDelayMs || lossDown:
		return judged{s: Down, allLost: all}, st
	case st.delayMs > callChoppyDelayMs || st.jitterMs > callChoppyJitterMs || st.loss >= callChoppyLoss-lossEps:
		return judged{s: Slow}, st
	}
	return judged{s: Good}, st
}

// vote combines the endpoints of one member of a group: a provider of the
// shared call path (Cloudflare-STUN and Cloudflare-UDP53 are one vote) is
// as bad as its worst endpoint with data.
func vote(js ...judged) judged {
	out := judged{s: None, allLost: true}
	for _, j := range js {
		if j.s == None {
			continue
		}
		out.s = worse(out.s, j.s)
		out.allLost = out.allLost && j.allLost
	}
	if out.s == None {
		out.allLost = false
	}
	return out
}

// majority combines the members of a group, or the groups' members of a
// tile: only members with data count; Down when more than half are Down,
// Slow when more than half are Slow or Down. A tie is not a majority.
func majority(js []judged) judged {
	var n, down, bad int
	all := true
	for _, j := range js {
		if j.s == None {
			continue
		}
		n++
		if j.s == Down {
			down++
		}
		if j.s.Bad() {
			bad++
		}
		all = all && j.allLost
	}
	switch {
	case n == 0:
		return judged{s: None}
	case 2*down > n:
		return judged{s: Down, allLost: all}
	case 2*bad > n:
		return judged{s: Slow}
	}
	return judged{s: Good}
}

// tileMinute judges a tile-minute from its web endpoints and, for call
// tiles, its UDP members (provider votes on the shared path). A call tile
// takes its quality from UDP and is also Down when its web group is
// ("not connecting"); without UDP data it is judged on TCP alone, where
// Choppy is impossible.
func tileMinute(kind profile.TileKind, web, udp []judged) judged {
	w := majority(web)
	if kind != profile.TileCall {
		return w
	}
	u := majority(udp)
	switch {
	case u.s == None:
		if w.s == Slow {
			w.s = Good
		}
		return w
	case w.s == Down:
		return judged{s: Down, allLost: w.allLost && u.allLost}
	}
	if w.s != None {
		u.allLost = u.allLost && w.allLost
	}
	return u
}

// signal is a group's or a tile's immediate outage signal.
type signal int8

const (
	hold    signal = iota // neither: the word keeps its outage state
	failing               // Down now
	cleared               // the latest checks are OK: any outage is over
)

// immediate is the "Down now" signal of one group (a tile's web endpoints,
// or its UDP members) from the members' raw tails: failing when more than
// half of the members with checks have their latest k checks lost (k is
// immFailSingle for a single-member group, immFailMulti otherwise),
// cleared when more than half have their latest immClearOK checks OK. A
// member with several series (a provider) fails when any of them does and
// is OK when all are. since is when the majority started failing.
func immediate(members [][]tail) (signal, time.Time) {
	k := immFailMulti
	if len(members) == 1 {
		k = immFailSingle
	}
	var n, ok int
	var starts []time.Time
	for _, m := range members {
		var has, fail, good bool
		good = true
		var start time.Time
		for _, t := range m {
			cs := t.sorted()
			if len(cs) == 0 {
				continue
			}
			has = true
			if s, f := lostRun(cs, k); f {
				fail = true
				if start.IsZero() || s.Before(start) {
					start = s
				}
			}
			good = good && okRun(cs, immClearOK)
		}
		if !has {
			continue
		}
		n++
		if fail {
			starts = append(starts, start)
		} else if good {
			ok++
		}
	}
	switch {
	case n > 0 && 2*len(starts) > n:
		slices.SortFunc(starts, func(a, b time.Time) int { return a.Compare(b) })
		return failing, starts[n/2]
	case n > 0 && 2*ok > n:
		return cleared, time.Time{}
	}
	return hold, time.Time{}
}

// lostRun reports whether the latest k checks are lost, and the first lost
// check of that run.
func lostRun(cs []check, k int) (time.Time, bool) {
	if len(cs) < k {
		return time.Time{}, false
	}
	i := len(cs)
	for i > 0 && cs[i-1].lost {
		i--
	}
	if len(cs)-i < k {
		return time.Time{}, false
	}
	return cs[i].at, true
}

func okRun(cs []check, k int) bool {
	if len(cs) < k {
		return false
	}
	for _, c := range cs[len(cs)-k:] {
		if c.lost {
			return false
		}
	}
	return true
}

// tileSignal combines a tile's group signals: failing when any group is
// (since the earliest), cleared when every group with checks is cleared.
func tileSignal(sigs []signal, sinces []time.Time) (signal, time.Time) {
	var since time.Time
	fail, clear := false, len(sigs) > 0
	for i, s := range sigs {
		switch s {
		case failing:
			fail = true
			if since.IsZero() || sinces[i].Before(since) {
				since = sinces[i]
			}
		case hold:
			clear = false
		}
	}
	switch {
	case fail:
		return failing, since
	case clear:
		return cleared, time.Time{}
	}
	return hold, time.Time{}
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
//   - Down from an outage (an immediate Down, or a minute in which every
//     measured check failed) shows at once and clears as soon as the
//     immediate signal is cleared.
//   - Slow (Choppy) is entered when badOf of the last badWindow judged
//     minutes are bad, and left after goodRun good minutes in a row.
//   - Down from latency or partial loss shows at once and is left after
//     goodRun good minutes in a row.
//   - None minutes hold the word and do not count.
type word struct {
	outage   bool
	outSince time.Time
	level    State // "" before the first judged minute
	lvlSince time.Time
	recent   [badWindow]struct { // the last judged minutes, newest first
		at  time.Time
		bad bool
	}
	nRecent  int
	good     int       // good minutes in a row
	goodFrom time.Time // the first of them
	cur      wordRun   // the open run (Start zero: none)
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

// outageSignal applies the immediate signal at time at. It returns the run
// that ended, if the word went back to Good.
func (w *word) outageSignal(sig signal, since, at time.Time) *wordRun {
	switch {
	case sig == failing && !w.outage:
		w.outage, w.outSince = true, since
	case sig == cleared && w.outage:
		w.outage = false
	default:
		return nil
	}
	return w.track(at)
}

// step applies the tile-minute that starts at at.
func (w *word) step(at time.Time, m judged) *wordRun {
	if m.s == None {
		return nil
	}
	copy(w.recent[1:], w.recent[:badWindow-1])
	w.recent[0].at, w.recent[0].bad = at, m.s.Bad()
	w.nRecent = min(w.nRecent+1, badWindow)
	if m.s == Good {
		if w.good == 0 {
			w.goodFrom = at
		}
		w.good++
	} else {
		w.good = 0
	}
	if m.allLost && !w.outage {
		w.outage, w.outSince = true, at
	}
	switch {
	case m.s == Down && w.level != Down:
		w.level, w.lvlSince = Down, at
	case w.level == "" || w.level == Good:
		w.level = Good
		if bad, first := w.bad(); bad >= badOf {
			w.level, w.lvlSince = Slow, first
		}
	case w.good >= goodRun:
		w.level = Good
		return w.track(w.goodFrom)
	}
	return w.track(at)
}

// bad counts the bad minutes of the last badWindow judged ones and returns
// the first of them.
func (w *word) bad() (int, time.Time) {
	n := 0
	var first time.Time
	for i := range w.nRecent {
		if w.recent[i].bad {
			n++
			first = w.recent[i].at
		}
	}
	return n, first
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
