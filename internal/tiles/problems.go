package tiles

import (
	"slices"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// view is what the drops and problems look like at one tick.
type view struct {
	now        time.Time
	tileDrops  [nTiles][]Drop
	layerEnded [nLayers][]window
	layerCur   [nLayers]*Drop
	connDrops  []Drop // ended
	connCur    *Drop  // in progress
	// blips are the path layers' ended runs of total loss too short for a
	// drop (connDropNow to dropMin) outside any connection drop: they
	// leave nothing behind, but the verdict reacts to them
	blips []Drop
	// lagBlips are the blips the path was clean after: the ones that
	// leave nothing behind (the others are part of a lossy spell)
	lagBlips []Drop
	eps      []Episode
}

// view computes the drops and the problems at now. The caller holds mu.
func (e *engine) view(now time.Time) *view {
	v := &view{now: now}
	sets := make([]runSet, len(e.ss))
	done := make([]bool, len(e.ss))
	set := func(i int) runSet {
		if !done[i] {
			sets[i], done[i] = e.ss[i].set(), true
		}
		return sets[i]
	}
	var tds [][]Drop
	for i, t := range e.tiles {
		if t.off {
			continue
		}
		var web []runSet
		for _, s := range t.web {
			web = append(web, set(s))
		}
		var udp [][]runSet
		for _, m := range t.udp {
			var rs []runSet
			for _, s := range m {
				rs = append(rs, set(s))
			}
			udp = append(udp, rs)
		}
		v.tileDrops[i] = tileDrops(web, udp, now)
		tds = append(tds, v.tileDrops[i])
	}
	var layers [nLayers][]runSet
	for l, ids := range e.paths {
		for _, i := range ids {
			layers[l] = append(layers[l], set(i))
		}
		v.layerEnded[l], v.layerCur[l] = layerDrops(layers[l], now)
	}
	v.connDrops, v.connCur = connDrops(layers, tds, len(tds), now)
	var short []window
	for _, ws := range v.layerEnded {
		for _, w := range ws {
			if w.dur(now) < dropMin {
				short = append(short, w)
			}
		}
	}
	for _, b := range toDrops(union(short, now), now) {
		in := func(d Drop) bool { return (window{d.Start, d.End}).overlaps(window{b.Start, b.End}, now) }
		if b.Dur < dropMin && !slices.ContainsFunc(v.connDrops, in) && (v.connCur == nil || !in(*v.connCur)) {
			v.blips = append(v.blips, b)
		}
	}
	v.eps = e.episodes(v)
	return v
}

// lagged returns the drop or blip that ended at most lagSlack before a
// lan, isp, upstream or no_network verdict incident of kind k starting at
// start began (and at most absorbBefore after it), when the incident ended
// (to; zero: now) at most lagMax after it and the layers it blames have
// been clean from its end to then: the incident is the verdict's 60 s
// window still holding the drop (nil: none).
func (e *engine) lagged(v *view, k model.VerdictKind, start, to time.Time) *Drop {
	if to.IsZero() {
		to = v.now
	}
	for _, ds := range [][]Drop{v.connDrops, v.blips} {
		for i, d := range ds {
			if d.Ended() && !start.Before(d.Start.Add(-absorbBefore)) && !start.After(d.End.Add(lagSlack)) &&
				!to.After(d.End.Add(lagMax)) && e.layersClean(v, d.End, to, blamed(k)...) && !e.tilesDropped(v, d.End.Add(dropLate), to) {
				return &ds[i]
			}
		}
	}
	return nil
}

// tilesDropped reports that more than half of the covered tiles were in
// a tile drop after from (up to to): the apps were not back, whatever the
// path's ICMP said.
func (e *engine) tilesDropped(v *view, from, to time.Time) bool {
	cov, n := 0, 0
	w := window{from, to}
	for i, t := range e.tiles {
		if t.off {
			continue
		}
		cov++
		if slices.ContainsFunc(v.tileDrops[i], func(d Drop) bool { return (window{d.Start, d.End}).overlaps(w, v.now) }) {
			n++
		}
	}
	return to.After(from) && 2*n > cov
}

// blamed are the path layers a verdict kind says are fine up to the one it
// blames: they must be clean for its incident to be a drop's lag.
func blamed(k model.VerdictKind) []int {
	switch k {
	case model.VerdictLAN:
		return []int{layerGateway}
	case model.VerdictISP:
		return []int{layerGateway, layerEdge}
	}
	return []int{layerGateway, layerEdge, layerAnycast}
}

// layersClean reports that the layers lost nothing of note after from, up
// to to (zero: now; a span of lagMax at most is walked): each was measured
// then, no run of total loss of its started, and its closed minutes
// wholly inside, and its checks not summarized yet, lost under
// layerLossy. A layer without checks is not clean.
func (e *engine) layersClean(v *view, from, to time.Time, layers ...int) bool {
	if to.IsZero() || to.After(v.now) {
		to = v.now
	}
	if lim := from.Add(lagMax + time.Minute); to.After(lim) {
		to = lim
	}
	ep := window{from, to}
	hi := e.nextMin // exclusive
	if to.Before(hi) {
		hi = to.Truncate(time.Minute)
	}
	for _, l := range layers {
		if slices.ContainsFunc(e.layerWindows(v, l), func(w window) bool { return w.from.After(from) && w.overlaps(ep, v.now) }) {
			return false
		}
		n, lost := 0, 0
		for t := from.Truncate(time.Minute); t.Before(e.nextMin) && !t.After(to); t = t.Add(time.Minute) {
			m := e.layerMins[l][minIdx(t)]
			n += m.n() // the partial minutes at either end count as measured ...
			// ... the whole ones inside count for loss too
			if t.After(from) && t.Before(hi) && m.n() > 0 && float64(m.lost) >= layerLossy*float64(m.n()) {
				return false
			}
		}
		// the minutes not summarized yet, from their raw checks: loss
		// right after a drop must not wait for its minute to close
		rn := 0
		for _, i := range e.paths[l] {
			for _, c := range e.ss[i].raw {
				if c.gap || c.ms <= from.UnixMilli() || c.ms > to.UnixMilli() {
					continue
				}
				rn++
				if c.lost {
					lost++
				}
			}
		}
		if n+rn == 0 || lost >= 2 && float64(lost) >= layerLossy*float64(rn) {
			return false
		}
	}
	return true
}

// episodes groups the problem parts of the last week (groupEpisodes).
func (e *engine) episodes(v *view) []Episode {
	now := v.now
	var conn []Episode
	for _, d := range v.connDrops {
		conn = append(conn, dropEpisode(d, ""))
	}
	if c := v.connCur; c != nil && c.Dur >= dropMin {
		conn = append(conn, dropEpisode(*c, ""))
	}
	for _, d := range v.blips {
		// a blip leaves nothing behind only when the path was clean after
		// it: one inside a lossy spell is part of the spell
		if !e.layersClean(v, d.End, d.End.Add(lagMax), layerGateway, layerEdge, layerAnycast) {
			continue
		}
		v.lagBlips = append(v.lagBlips, d)
		x := dropEpisode(d, "")
		x.Kind = epBlip
		conn = append(conn, x)
	}
	// the incidents are re-read every incEvery: the verdict's own may not
	// be among them yet (the card already shows it)
	incs := e.incidents
	cloned := false
	if vk := e.verdict; !vk.Since.IsZero() && !slices.ContainsFunc(incs, func(in model.Incident) bool {
		return in.Kind == vk.Kind && in.End.IsZero() && in.Start.Sub(vk.Since).Abs() < time.Second
	}) {
		if _, ok := incidentEpisode(model.Incident{Kind: vk.Kind}, false); ok {
			// its peak loss so far is not known: the window's loss stands in
			incs, cloned = append(slices.Clip(incs), model.Incident{Kind: vk.Kind, Start: vk.Since, PeakLoss: vk.Evidence["loss"]}), true
		}
	}
	// an incident that is a drop's (or a blip's) lag ends with it: it is
	// absorbed, is no second ongoing problem, and explains nothing after
	for i, in := range incs {
		if !pathKind(in.Kind) {
			continue
		}
		d := e.lagged(v, in.Kind, in.Start, in.End)
		if d == nil || !in.End.IsZero() && !in.End.After(d.End) {
			continue
		}
		if !cloned {
			incs, cloned = slices.Clone(incs), true
		}
		incs[i].End = d.End
		if !d.End.After(in.Start) { // it began in the lagSlack after the drop
			incs[i].End = in.Start.Add(time.Second)
		}
	}
	for _, in := range incs {
		if x, ok := incidentEpisode(in, e.incidentDropped(in, v)); ok {
			conn = append(conn, x)
		}
	}
	for _, w := range e.broad {
		conn = append(conn, broadEpisode(w.from, w.to))
	}
	var parts []Episode
	for i, t := range e.tiles {
		if t.off {
			continue
		}
		for _, r := range t.runs {
			parts = append(parts, runEpisode(t.def.ID, t.def.Kind, r))
		}
		if r, ok := t.w.open(); ok {
			parts = append(parts, runEpisode(t.def.ID, t.def.Kind, r))
		}
		for _, d := range v.tileDrops[i] {
			parts = append(parts, dropEpisode(d, t.def.ID))
		}
	}
	sp := e.speed()
	var buf []minute
	eps := groupEpisodes(conn, parts, func(x Episode) Cause {
		in := attrIn{from: x.Start, to: x.End, now: now, incidents: incs, speed: sp}
		// a call tile's minute judges the call window that ends with it:
		// the loss behind a calls-only Choppy problem lies that much
		// earlier (its last minutes are the window emptying)
		evidence := func(from, to time.Time) [nLayers]layerEv {
			var ls [nLayers]layerEv
			for l := range ls {
				buf = e.layerMinutes(buf[:0], l, from, to)
				ls[l] = layerEvidence(buf, e.layerWindows(v, l), from, to, now)
			}
			return ls
		}
		in.layers = evidence(x.Start, x.End)
		if !x.Connection && x.Sev == Slow && len(x.Tiles) > 0 && !slices.ContainsFunc(x.Tiles, func(id string) bool {
			i := tileOrder(id)
			return i < 0 || profile.Tiles[i].Kind != profile.TileCall
		}) {
			// a call tile's minute judges the call window that ends with
			// it: the loss behind a calls-only Choppy problem may lie that
			// much earlier (once over, its last minutes are the window
			// emptying). The lossier of the two spans at the edge counts:
			// an ongoing problem's own span holds its loss.
			lf, lt := x.Start.Add(-(callWindow - time.Minute)), x.End
			if !lt.IsZero() {
				lt = lt.Add(-(callWindow - time.Minute))
				if lt.Before(x.Start) {
					lt = x.Start
				}
			}
			if sh := evidence(lf, lt); sh[layerEdge].loss > in.layers[layerEdge].loss {
				in.layers = sh
			}
		}
		in.tiles = e.tileEvs(x, v)
		return attribute(in)
	}, now)
	cut := now.Add(-problemsFor)
	return slices.DeleteFunc(eps, func(x Episode) bool { return !x.Ongoing() && x.End.Before(cut) })
}

// incidentDropped reports that the layer an incident blames lost
// everything during it for dropMin or more (any layer for no_network): a
// few seconds of total loss inside heavy loss is still loss, not a drop.
func (e *engine) incidentDropped(in model.Incident, v *view) bool {
	ls := []int{layerGateway, layerEdge, layerAnycast}
	switch in.Kind {
	case model.VerdictLAN:
		ls = ls[:1]
	case model.VerdictISP:
		ls = ls[1:2]
	case model.VerdictUpstream:
		ls = ls[2:]
	}
	ep := window{in.Start, in.End}
	for _, l := range ls {
		for _, w := range e.layerWindows(v, l) {
			if w.dur(v.now) >= dropMin && w.overlaps(ep, v.now) {
				return true
			}
		}
	}
	return false
}

// layerWindows are a layer's runs of total loss: the ended ones and the
// one in progress.
func (e *engine) layerWindows(v *view, l int) []window {
	ws := v.layerEnded[l]
	if c := v.layerCur[l]; c != nil {
		ws = append(slices.Clip(ws), window{from: c.Start})
	}
	return ws
}

// span returns the closed minutes in [from, to] (to zero: now) that the
// minute arrays still hold.
func (e *engine) span(from, to time.Time) (time.Time, time.Time) {
	lo := from.Truncate(time.Minute)
	if oldest := e.nextMin.Add(-keepMins * time.Minute); lo.Before(oldest) {
		lo = oldest
	}
	hi := e.nextMin // exclusive
	if !to.IsZero() && to.Before(hi) {
		hi = to.Truncate(time.Minute).Add(time.Minute)
	}
	return lo, hi
}

// layerMinutes appends a layer's minutes in [from, to] to buf.
func (e *engine) layerMinutes(buf []minute, l int, from, to time.Time) []minute {
	lo, hi := e.span(from, to)
	for t := lo; t.Before(hi); t = t.Add(time.Minute) {
		if m := e.layerMins[l][minIdx(t)]; m.n() > 0 {
			buf = append(buf, m)
		}
	}
	return buf
}

// tileEvs are the covered tiles over an episode: the worst of their words
// in it (word runs and drops overlapping it) and, for its own tiles, its
// severity; Good when it has minutes with data there, else None. A lone
// Slow minute (one lost check) does not make a tile bad: its word does
// not change.
func (e *engine) tileEvs(x Episode, v *view) []tileEv {
	lo, hi := e.span(x.Start, x.End)
	ep := x.window()
	var out []tileEv
	for i, t := range e.tiles {
		if t.off {
			continue
		}
		ev := tileEv{id: t.def.ID, vendor: vendorOf(t.def.TileDef), kind: t.def.Kind, worst: None}
		for m := lo; m.Before(hi) && ev.worst != Down; m = m.Add(time.Minute) {
			switch s := t.minute(m); s {
			case Down:
				ev.worst = Down
			case Good, Slow:
				ev.worst = worse(ev.worst, Good)
			}
		}
		for _, r := range t.runs {
			if r.End.After(ep.from) && (window{r.Start, r.End}).overlaps(ep, v.now) {
				ev.worst = worse(ev.worst, r.Sev)
			}
		}
		if r, ok := t.w.open(); ok && (window{r.Start, time.Time{}}).overlaps(ep, v.now) {
			ev.worst = worse(ev.worst, r.Sev)
		}
		for _, d := range v.tileDrops[i] {
			if (window{d.Start, d.End}).overlaps(ep, v.now) {
				ev.worst = Down
			}
		}
		if slices.Contains(x.Tiles, t.def.ID) {
			ev.worst = worse(ev.worst, x.Sev)
		}
		out = append(out, ev)
	}
	return out
}

// speedWindow is a stored speed test's window.
func speedWindow(t model.SpeedTest) speedtest.Window {
	return speedtest.Window{Start: t.Start, End: t.End}
}
