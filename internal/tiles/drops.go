package tiles

import (
	"math/bits"
	"slices"
	"time"
)

// Drop is a period of total loss of dropMin or more: of a tile (most of its
// endpoints), or of the connection.
type Drop struct {
	// Start is the first slot that should have answered (the one after the
	// last success), End the first success after: End − Start is the
	// estimate (first OK after − last OK before) − interval. End is zero
	// while the drop lasts.
	Start, End time.Time
	Dur        time.Duration // End − Start (or now − Start), rounded half down to durRound
}

// Ended reports a drop that is over.
func (d Drop) Ended() bool { return !d.End.IsZero() }

// roundDur rounds to durRound, halves down: 45 s is "40 s", 46 s "50 s";
// never to 0 s (a drop in progress can be 5 s old).
func roundDur(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return max(durRound, (d+durRound/2-1)/durRound*durRound)
}

// failRun is a failure run of one series: its lost checks from the first
// to the last before the next success. Gaps (not measured) are ignored,
// unless they are a sleep (see tracker). Times are Unix ms: a week of runs
// is kept.
type failRun struct {
	firstMs, lastMs int64 // the first and the last lost check
	// fromMs is the slot after the last success before the run (the first
	// lost check when there was none recently), toMs the first success
	// after it (0 while the run is open; the slot after its last check
	// when a sleep ended it).
	fromMs, toMs int64
	everyMs      int32
	lost         int32
	// prior: the last success before the run was within sleepFactor
	// intervals of it. A run after a sleep, a restart or no data at all is
	// never a drop: nobody knows when it started.
	prior bool
}

// msTime is a Unix ms time; 0 is the zero time.
func msTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func (r failRun) open() bool { return r.toMs == 0 }

// est is the run's estimated length: (first OK after − last OK before) −
// interval. An open run is estimated as if its next check succeeded.
func (r failRun) est() time.Duration {
	end := r.toMs
	if r.open() {
		end = r.lastMs + int64(r.everyMs)
	}
	return time.Duration(end-r.fromMs) * time.Millisecond
}

// window is the run's estimated outage; to is zero while it is open.
func (r failRun) window() window { return window{msTime(r.fromMs), msTime(r.toMs)} }

// runRule is how a series' runs are judged.
type runRule int8

const (
	ruleWeb  runRule = iota // TCP/HTTPS/ICMP endpoints of a tile
	ruleUDP                 // STUN bursts
	rulePath                // path layers: their minutes count every lost check
)

// dropLike reports a run that counts as a drop (open or not): after a
// recent success, tileDropChecks or more lost checks on a web endpoint,
// dropMin or more, or tileDropChecks bursts or more, on a UDP one. Its
// checks count only as the drop, never as loss; any other run's checks are
// loss. So a 20 s blip of the whole connection, too short for a drop,
// leaves no Choppy calls behind; a burst or two lost is loss. Path runs
// are never left out.
func (r failRun) dropLike(rule runRule) bool {
	switch rule {
	case ruleWeb:
		return r.prior && r.lost >= tileDropChecks
	case ruleUDP:
		return r.prior && (r.est() >= dropMin || r.lost >= tileDropChecks)
	}
	return false
}

// kept reports a run worth keeping once the minutes it touches are judged:
// a drop-like run, or a path run of connDropNow or more (pathRuns,
// layerDrops). The others matter only to summarize.
func (r failRun) kept(rule runRule) bool {
	if rule == rulePath {
		return r.est() >= connDropNow
	}
	return r.dropLike(rule)
}

// pruneRuns drops, in place, the closed runs that ended before judged (the
// start of the oldest minute not yet summarized) and are not kept, so a
// lossy series' runs stay bounded over the week.
func pruneRuns(rs []failRun, judged time.Time, rule runRule) []failRun {
	cut := judged.UnixMilli()
	return slices.DeleteFunc(rs, func(r failRun) bool { return !r.open() && r.toMs < cut && !r.kept(rule) })
}

// holdFor is how long a series' results are held before they are applied:
// a result for slot s arrives by s + phase (< every, probe/sched.go) +
// the probe's timeout (resultLag), and later slots may answer first.
func holdFor(every time.Duration) time.Duration { return every + resultLag }

// tracker follows one series' failure runs, one check at a time, so a run
// of any length is seen whole (a raw tail would forget its start).
//
// The contract for the engine, live and in replay alike:
//   - add only queues a check; flush(now) applies, in slot order, those
//     with a slot at most now − holdFor(every), by when every result for
//     them has arrived. A check older than the newest applied one, or a
//     duplicate, is ignored; one more than clockBack older means the wall
//     clock was set back, and the tracker starts over.
//   - The engine ticks on the Unix-aligned 5 s grid (now.Truncate(5 s)),
//     flushes every tracker, then evaluates immediate and the drops. Replay
//     is one slot-ordered walk over all series that, before each grid
//     tick, adds the checks whose slot is not after it (or a whole chunk
//     ahead: held checks are not applied early).
//
// So every tick sees exactly the same applied checks live and in replay,
// whatever the tick's phase or the results' arrival order.
type tracker struct {
	every  time.Duration
	pend   []check // queued, in slot order, from head on
	head   int
	last   int64   // the newest applied check (Unix ms); 0: none
	lastOK int64   // the newest success
	cur    failRun // the open run (lost 0: none)
	recent uint8   // the latest results, newest in bit 0 (1: answered)
	okAt   int64   // the slot from which immClearOK of the latest immClearWin answered; 0: not now
	late   int     // checks ignored because a newer slot was already applied
}

// lateChecks counts the checks ignored because they arrived after a newer
// slot was applied (more than holdFor late): the engine logs or exports it.
func (t *tracker) lateChecks() int { return t.late }

// add queues one check (gaps are not checks).
func (t *tracker) add(c check) {
	if c.gap {
		return
	}
	if t.last != 0 && c.ms < t.last-clockBack.Milliseconds() {
		*t = tracker{every: t.every, late: t.late} // the clock was set back
	}
	if c.ms <= t.last {
		t.late++
		return
	}
	q := t.pend[t.head:]
	i, found := slices.BinarySearchFunc(q, c.ms, func(x check, ms int64) int { return int(x.ms - ms) })
	if found {
		return
	}
	t.pend = slices.Insert(t.pend, t.head+i, c)
}

// flush applies the queued checks held long enough at now and returns the
// runs that closed.
func (t *tracker) flush(now time.Time) []failRun {
	var out []failRun
	cut := now.Add(-holdFor(t.every)).UnixMilli()
	for t.head < len(t.pend) && t.pend[t.head].ms <= cut {
		if r, ok := t.apply(t.pend[t.head]); ok {
			out = append(out, r)
		}
		t.head++
	}
	if t.head > 0 && 2*t.head >= len(t.pend) { // reuse the front: the queue holds about one hold's checks
		t.pend = t.pend[:copy(t.pend, t.pend[t.head:])]
		t.head = 0
	}
	return out
}

func (t *tracker) apply(c check) (closed failRun, ok bool) {
	every := t.every.Milliseconds()
	slept := t.last != 0 && c.ms-t.last > sleepFactor*every
	if slept {
		t.recent = 0
	}
	if t.cur.lost > 0 && (slept || !c.lost) {
		// a sleep inside a run ends it where the checks stopped
		closed, ok = t.cur, true
		closed.toMs = c.ms
		if slept {
			closed.toMs = t.last + every
		}
		t.cur = failRun{}
	}
	t.recent <<= 1
	if c.lost {
		if t.cur.lost == 0 {
			prior := t.lastOK != 0 && t.lastOK == t.last && !slept
			from := c.ms
			if prior {
				from = t.lastOK + every
			}
			t.cur = failRun{firstMs: c.ms, fromMs: from, prior: prior, everyMs: int32(every)}
		}
		t.cur.lastMs = c.ms
		t.cur.lost++
	} else {
		t.recent |= 1
		t.lastOK = c.ms
	}
	if bits.OnesCount8(t.recent&(1<<immClearWin-1)) >= immClearOK {
		if t.okAt == 0 {
			t.okAt = c.ms
		}
	} else {
		t.okAt = 0
	}
	t.last = c.ms
	return closed, ok
}

// open returns the run in progress, if any.
func (t *tracker) open() (failRun, bool) { return t.cur, t.cur.lost > 0 }

// runSet is what a drop decision needs of one series: its failure runs,
// oldest first (the newest may be open), and its newest check.
type runSet struct {
	runs  []failRun
	last  time.Time // zero: the series has no checks
	every time.Duration
}

// pathRuns returns the runs of total loss of a 1 s (or 5 s) path series for
// connection drops: runs of connDropNow or more, where those with less
// than dropEndedOK of success between them are one (a drop does not end at
// its first answer; a link flapping faster than that reads as one long
// drop), and the newest is open until the series has been OK for
// dropEndedOK. Shorter runs (scattered loss) are left out; a run after a
// sleep is never joined to the one before.
func pathRuns(s runSet) []failRun {
	var out []failRun
	for _, r := range s.runs {
		if r.est() < connDropNow {
			continue
		}
		if n := len(out); n > 0 && !out[n-1].open() && r.prior && r.firstMs-out[n-1].toMs < dropEndedOK.Milliseconds() {
			p := &out[n-1]
			p.lost += r.lost
			p.lastMs, p.toMs = r.lastMs, r.toMs
			continue
		}
		out = append(out, r)
	}
	if n := len(out); n > 0 && !out[n-1].open() && s.last.Add(s.every).UnixMilli()-out[n-1].toMs < dropEndedOK.Milliseconds() {
		out[n-1].toMs = 0
	}
	return out
}

// window is a time span; to is zero while it is open.
type window struct{ from, to time.Time }

func (w window) end(now time.Time) time.Time {
	if w.to.IsZero() {
		return now
	}
	return w.to
}

func (w window) dur(now time.Time) time.Duration { return w.end(now).Sub(w.from) }

func (w window) overlaps(o window, now time.Time) bool {
	return w.from.Before(o.end(now)) && o.from.Before(w.end(now))
}

// cover returns the spans in which at least need of the sets have a window,
// oldest first; a span that ends with an open window is open.
func cover(sets [][]window, need int, now time.Time) []window {
	if need < 1 {
		need = 1
	}
	type ev struct {
		at time.Time
		d  int
	}
	openEnd := now.Add(time.Nanosecond) // after any closed window
	var evs []ev
	for _, s := range sets {
		// a set's own windows may overlap (a member's several series):
		// count each set once
		for _, w := range union(s, now) {
			end := w.to
			if end.IsZero() {
				end = openEnd
			}
			evs = append(evs, ev{w.from, 1}, ev{end, -1})
		}
	}
	slices.SortFunc(evs, func(a, b ev) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return a.d - b.d // ends before starts: touching windows are not together
	})
	var out []window
	n := 0
	var start time.Time
	for _, e := range evs {
		was := n >= need
		n += e.d
		switch {
		case !was && n >= need:
			start = e.at
		case was && n < need && e.at.After(start):
			w := window{start, e.at}
			if w.to.After(now) {
				w.to = time.Time{}
			}
			out = append(out, w)
		}
	}
	return out
}

// union merges overlapping or touching windows, oldest first.
func union(ws []window, now time.Time) []window {
	s := slices.Clone(ws)
	slices.SortFunc(s, func(a, b window) int { return a.from.Compare(b.from) })
	var out []window
	for _, w := range s {
		if n := len(out); n > 0 && !w.from.After(out[n-1].end(now)) {
			if w.to.IsZero() || out[n-1].to.IsZero() {
				out[n-1].to = time.Time{}
			} else if w.to.After(out[n-1].to) {
				out[n-1].to = w.to
			}
			continue
		}
		out = append(out, w)
	}
	return out
}

func toDrops(ws []window, now time.Time) []Drop {
	var out []Drop
	for _, w := range ws {
		out = append(out, Drop{Start: w.from, End: w.to, Dur: roundDur(w.dur(now))})
	}
	return out
}

func longEnough(ws []window, now time.Time) []window {
	return slices.DeleteFunc(ws, func(w window) bool { return w.dur(now) < dropMin })
}

// tileDrops finds a tile's drops: more than half of its web endpoints in a
// drop-like run (dropLike) together, or more than half of its UDP members;
// either for dropMin or more. A member of several series (a provider) is in
// a run while any of them is. Series without checks take no part.
func tileDrops(web []runSet, udp [][]runSet, now time.Time) []Drop {
	sets := func(members [][]runSet, rule runRule) [][]window {
		var out [][]window
		for _, m := range members {
			var ws []window
			has := false
			for _, s := range m {
				has = has || !s.last.IsZero()
				for _, r := range s.runs {
					if r.dropLike(rule) {
						ws = append(ws, r.window())
					}
				}
			}
			if has {
				out = append(out, ws)
			}
		}
		return out
	}
	var wm [][]runSet
	for _, s := range web {
		wm = append(wm, []runSet{s})
	}
	ws, us := sets(wm, ruleWeb), sets(udp, ruleUDP)
	all := longEnough(cover(ws, len(ws)/2+1, now), now)
	all = append(all, longEnough(cover(us, len(us)/2+1, now), now)...)
	return toDrops(union(all, now), now)
}

// Path layers, in Snapshot order.
const (
	layerGateway = iota
	layerEdge
	layerAnycast
	nLayers
)

// layerDrops finds a path layer's runs of total loss (every series with
// checks in a pathRuns run at once): the ended ones of connDropNow or more
// (what attribution and connDrops keep), and the one in progress when
// every series is in an open run of connDropNow or more.
func layerDrops(series []runSet, now time.Time) (ended []window, cur *Drop) {
	var sets [][]window
	inProgress := true
	var start time.Time
	var dur time.Duration
	for _, s := range series {
		if s.last.IsZero() {
			continue
		}
		var ws []window
		var open *failRun
		for _, r := range pathRuns(s) {
			if !r.prior {
				continue
			}
			ws = append(ws, r.window())
			if r.open() {
				open = &r
			}
		}
		sets = append(sets, ws)
		switch {
		case open == nil:
			inProgress = false
		case start.IsZero():
			start, dur = msTime(open.fromMs), open.est()
		default:
			if f := msTime(open.fromMs); f.After(start) {
				start = f
			}
			dur = min(dur, open.est())
		}
	}
	if len(sets) == 0 {
		return nil, nil
	}
	if inProgress && dur >= connDropNow {
		cur = &Drop{Start: start, Dur: roundDur(dur)}
	}
	for _, w := range cover(sets, len(sets), now) {
		if !w.to.IsZero() && w.dur(now) >= connDropNow {
			ended = append(ended, w)
		}
	}
	return ended, cur
}

// connDrops finds the connection's drops: a path layer (gateway, ISP edge,
// anycast; their 1 s / 5 s ICMP series) losing everything for dropMin or
// more, or more than half of the covered tiles in a tile drop together.
// When a path layer saw the drop, it gives the duration. cur is the drop in
// progress, from the gateway or the edge (it may still end too short to be
// a drop); a drop seen only by the tiles is reported once it has ended.
func connDrops(layers [nLayers][]runSet, tiles [][]Drop, covered int, now time.Time) (ended []Drop, cur *Drop) {
	var path []window
	for l, series := range layers {
		ws, c := layerDrops(series, now)
		if c != nil && l != layerAnycast && (cur == nil || c.Start.Before(cur.Start)) {
			cur = c
		}
		path = append(path, longEnough(ws, now)...)
	}
	var sets [][]window
	for _, ds := range tiles {
		var ws []window
		for _, d := range ds {
			if d.Ended() {
				ws = append(ws, window{d.Start, d.End})
			}
		}
		sets = append(sets, ws)
	}
	all := path
	if covered > 0 {
		for _, w := range longEnough(cover(sets, covered/2+1, now), now) {
			if !slices.ContainsFunc(path, func(p window) bool { return p.overlaps(w, now) }) &&
				(cur == nil || !w.overlaps(window{cur.Start, time.Time{}}, now)) {
				all = append(all, w)
			}
		}
	}
	return toDrops(union(all, now), now), cur
}

// lastDrop is the tile's note: its newest drop that ended less than
// dropNoteFor ago, shown only while the word is Good.
func lastDrop(drops []Drop, word State, now time.Time) *Drop {
	if word != Good {
		return nil
	}
	var out *Drop
	for i, d := range drops {
		if d.Ended() && now.Sub(d.End) < dropNoteFor && (out == nil || d.End.After(out.End)) {
			out = &drops[i]
		}
	}
	return out
}
