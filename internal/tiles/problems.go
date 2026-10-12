package tiles

import (
	"slices"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
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
	eps        []Episode
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
	v.eps = e.episodes(v)
	return v
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
	for _, in := range e.incidents {
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
		in := attrIn{from: x.Start, to: x.End, now: now, incidents: e.incidents, speed: sp}
		for l := range in.layers {
			buf = e.layerMinutes(buf[:0], l, x.Start, x.End)
			in.layers[l] = layerEvidence(buf, e.layerWindows(v, l), x.Start, x.End, now)
		}
		in.tiles = e.tileEvs(x, v)
		return attribute(in)
	}, now)
	cut := now.Add(-problemsFor)
	return slices.DeleteFunc(eps, func(x Episode) bool { return !x.Ongoing() && x.End.Before(cut) })
}

// incidentDropped reports that the layer an incident blames lost
// everything during it (any layer for no_network).
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
			if w.overlaps(ep, v.now) {
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
