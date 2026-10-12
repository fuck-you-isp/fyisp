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
	Dur        time.Duration // End − Start (or now − Start), rounded to durRound
}

// Ended reports a drop that is over.
func (d Drop) Ended() bool { return !d.End.IsZero() }

// failRun is a failure run of one series: its lost checks from the first
// to the last before the next success. Gaps (not measured) are ignored.
type failRun struct {
	i, lost     int       // index of the first lost check in the sorted tail, and how many
	first, last time.Time // the first and the last lost check
	// from is the slot after the last success before the run (the first
	// lost check when there was none recently), to the first success after
	// it (zero while the run is open).
	from, to time.Time
	every    time.Duration
	// prior: the last success before the run was within sleepFactor
	// intervals of it. A run after a sleep, a restart or no data at all
	// is never a drop: nobody knows when it started.
	prior bool
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

// runs returns a series' failure runs, oldest first.
func runs(t tail) []failRun {
	cs := t.sorted()
	var out []failRun
	for i := 0; i < len(cs); i++ {
		if !cs[i].lost {
			continue
		}
		r := failRun{i: i, first: cs[i].at, from: cs[i].at, every: t.every}
		if i > 0 && cs[i].at.Sub(cs[i-1].at) <= sleepFactor*t.every {
			r.prior, r.from = true, cs[i-1].at.Add(t.every)
		}
		for i < len(cs) && cs[i].lost {
			r.last = cs[i].at
			r.lost++
			i++
		}
		if i < len(cs) {
			r.to = cs[i].at
		}
		out = append(out, r)
	}
	return out
}

// pathRuns returns the runs of total loss of a 1 s (or 5 s) path series
// for connection drops: runs of connDropNow or more, where those with less
// than dropEndedOK of success between them are one (a drop does not end at
// its first answer), and the newest is open until the series has been OK
// for dropEndedOK. Shorter runs (scattered loss) are left out.
func pathRuns(t tail) []failRun {
	var out []failRun
	for _, r := range runs(t) {
		if r.est() < connDropNow {
			continue
		}
		if n := len(out); n > 0 && !out[n-1].open() && r.first.Sub(out[n-1].to) < dropEndedOK {
			p := &out[n-1]
			p.lost += r.lost
			p.last, p.to = r.last, r.to
			continue
		}
		out = append(out, r)
	}
	if n := len(out); n > 0 && !out[n-1].open() {
		cs := t.sorted()
		if cs[len(cs)-1].at.Add(t.every).Sub(out[n-1].to) < dropEndedOK {
			out[n-1].to = time.Time{}
		}
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
// oldest first. A span that reaches now is open.
func cover(sets [][]window, need int, now time.Time) []window {
	if need < 1 {
		need = 1
	}
	type ev struct {
		at time.Time
		d  int
	}
	var evs []ev
	for _, s := range sets {
		// a set's own windows may overlap (a member's several series):
		// count each set once
		for _, w := range union(s, now) {
			evs = append(evs, ev{w.from, 1}, ev{w.end(now), -1})
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
			out = append(out, window{start, e.at})
		}
	}
	for i := range out {
		if !out[i].to.Before(now) {
			out[i].to = time.Time{}
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
		out = append(out, Drop{Start: w.from, End: w.to, Dur: w.dur(now).Round(durRound)})
	}
	return out
}

func longEnough(ws []window, now time.Time) []window {
	return slices.DeleteFunc(ws, func(w window) bool { return w.dur(now) < dropMin })
}

// tileDrops finds a tile's drops: more than half of its web endpoints in a
// failure run together, each run tileDropChecks or more failed checks long,
// or more than half of its UDP members in a failure run of dropMin or more;
// either for dropMin or more. A member of several series (a provider) is
// in a run while any of them is. Runs after a sleep or restart never count.
func tileDrops(web []tail, udp [][]tail, now time.Time) []Drop {
	var all []window
	var sets [][]window
	for _, t := range web {
		if len(t.checks) == 0 {
			continue
		}
		var ws []window
		for _, r := range runs(t) {
			if r.prior && r.lost >= tileDropChecks {
				ws = append(ws, r.window())
			}
		}
		sets = append(sets, ws)
	}
	all = append(all, longEnough(cover(sets, len(sets)/2+1, now), now)...)
	sets = sets[:0]
	for _, m := range udp {
		var ws []window
		has := false
		for _, t := range m {
			has = has || len(t.checks) > 0
			for _, r := range runs(t) {
				if r.prior && r.est() >= dropMin {
					ws = append(ws, r.window())
				}
			}
		}
		if has {
			sets = append(sets, ws)
		}
	}
	all = append(all, longEnough(cover(sets, len(sets)/2+1, now), now)...)
	return toDrops(union(all, now), now)
}

// Path layers, in Snapshot order.
const (
	layerGateway = iota
	layerEdge
	layerAnycast
	nLayers
)

// connDrops finds the connection's drops: a path layer (gateway, ISP edge,
// anycast; their 1 s / 5 s ICMP series) losing everything for dropMin or
// more, or more than half of the covered tiles in a tile drop together.
// When a path layer saw the drop, its series give the duration. A layer
// with several series loses everything only when all of them do.
//
// cur is the drop in progress: the gateway or the edge has lost everything
// for connDropNow, and has not yet been OK for dropEndedOK. It may still
// end too short to be a drop.
func connDrops(layers [nLayers][]tail, tiles [][]Drop, covered int, now time.Time) (ended []Drop, cur *Drop) {
	var path []window
	for l, ts := range layers {
		var sets [][]window
		inProgress := l != layerAnycast
		var start time.Time
		var dur time.Duration
		for _, t := range ts {
			if len(t.checks) == 0 {
				continue
			}
			var ws []window
			var open *failRun
			for _, r := range pathRuns(t) {
				if r.prior {
					ws = append(ws, r.window())
					if r.open() {
						open = &r
					}
				}
			}
			sets = append(sets, ws)
			// in progress when every series of the layer is
			switch {
			case open == nil || open.est() < connDropNow:
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
			continue
		}
		if inProgress && (cur == nil || start.Before(cur.Start)) {
			cur = &Drop{Start: start, Dur: dur.Round(durRound)}
		}
		for _, w := range cover(sets, len(sets), now) {
			if !w.to.IsZero() && w.dur(now) >= dropMin {
				path = append(path, w)
			}
		}
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
