package tiles

import (
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
// unless they are a sleep (see tracker).
type failRun struct {
	first, last time.Time // the first and the last lost check
	// from is the slot after the last success before the run (the first
	// lost check when there was none recently), to the first success after
	// it (zero while the run is open; the slot after its last check when a
	// sleep ended it).
	from, to time.Time
	lost     int
	// prior: the last success before the run was within sleepFactor
	// intervals of it. A run after a sleep, a restart or no data at all is
	// never a drop: nobody knows when it started.
	prior bool
	every time.Duration
}

func (r failRun) open() bool { return r.to.IsZero() }

// est is the run's estimated length: (first OK after − last OK before) −
// interval. An open run is estimated as if its next check succeeded.
func (r failRun) est() time.Duration {
	if r.open() {
		return r.last.Add(r.every).Sub(r.from)
	}
	return r.to.Sub(r.from)
}

// window is the run's estimated outage; to is zero while it is open.
func (r failRun) window() window { return window{r.from, r.to} }

// runRule is how a series' runs are judged.
type runRule int8

const (
	ruleWeb  runRule = iota // TCP/HTTPS/ICMP endpoints of a tile
	ruleUDP                 // STUN bursts
	rulePath                // path layers: runs are never left out of their minutes
)

// dropLike reports a run that counts as a drop (open or not): after a
// recent success, tileDropChecks or more lost checks on a web endpoint,
// dropMin or more on a UDP one. Its checks count only as the drop, never as
// loss; any other run's checks are loss.
func (r failRun) dropLike(rule runRule) bool {
	switch rule {
	case ruleWeb:
		return r.prior && r.lost >= tileDropChecks
	case ruleUDP:
		return r.prior && r.est() >= dropMin
	}
	return false
}

// tracker follows one series' failure runs, one check at a time, so a run
// of any length is seen whole (a raw tail would forget its start). Results
// are applied in slot order: a late one (a timeout) waits up to reorderFor
// behind newer slots, and one older than the newest applied check is
// ignored. Replay feeds the same checks through the same tracker.
type tracker struct {
	every  time.Duration
	pend   []check // not applied yet, in slot order
	last   int64   // the newest applied check (Unix ms); 0: none
	lastOK int64   // the newest success
	cur    failRun // the open run (lost 0: none)
	okN    int     // successes in a row up to the newest check
	okAt   int64   // the slot of the immClearOK-th of them
}

// add takes one check (gaps are not checks) and returns the runs it closed.
func (t *tracker) add(c check) []failRun {
	if c.gap || c.ms <= t.last {
		return nil
	}
	i, found := slices.BinarySearchFunc(t.pend, c.ms, func(x check, ms int64) int { return int(x.ms - ms) })
	if found {
		return nil
	}
	t.pend = slices.Insert(t.pend, i, c)
	return t.flush(t.pend[len(t.pend)-1].at())
}

// flush applies the checks older than reorderFor at now and returns the
// runs that closed. Call it before judging anything at now.
func (t *tracker) flush(now time.Time) []failRun {
	var out []failRun
	cut := now.Add(-reorderFor).UnixMilli()
	n := 0
	for n < len(t.pend) && t.pend[n].ms <= cut {
		if r := t.apply(t.pend[n]); r != nil {
			out = append(out, *r)
		}
		n++
	}
	t.pend = slices.Delete(t.pend, 0, n)
	return out
}

func (t *tracker) apply(c check) *failRun {
	every := t.every.Milliseconds()
	slept := t.last != 0 && c.ms-t.last > sleepFactor*every
	var closed *failRun
	if t.cur.lost > 0 && (slept || !c.lost) {
		// a sleep inside a run ends it where the checks stopped
		to := c.ms
		if slept {
			to = t.last + every
		}
		r := t.cur
		r.to = time.UnixMilli(to).UTC()
		closed, t.cur = &r, failRun{}
	}
	if c.lost {
		if t.cur.lost == 0 {
			prior := t.lastOK != 0 && t.lastOK == t.last && !slept
			from := c.ms
			if prior {
				from = t.lastOK + every
			}
			t.cur = failRun{first: c.at(), from: time.UnixMilli(from).UTC(), prior: prior, every: t.every}
		}
		t.cur.last = c.at()
		t.cur.lost++
		t.okN = 0
	} else {
		if t.okN++; t.okN == immClearOK {
			t.okAt = c.ms
		}
		t.lastOK = c.ms
	}
	t.last = c.ms
	return closed
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
		if n := len(out); n > 0 && !out[n-1].open() && r.prior && r.first.Sub(out[n-1].to) < dropEndedOK {
			p := &out[n-1]
			p.lost += r.lost
			p.last, p.to = r.last, r.to
			continue
		}
		out = append(out, r)
	}
	if n := len(out); n > 0 && !out[n-1].open() && s.last.Add(s.every).Sub(out[n-1].to) < dropEndedOK {
		out[n-1].to = time.Time{}
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
			start, dur = open.from, open.est()
		default:
			if open.from.After(start) {
				start = open.from
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
