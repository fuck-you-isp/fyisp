package tiles

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
)

// Episode is one problem: a connection problem with the tiles it hit, or
// one or more tiles' problem with one cause.
type Episode struct {
	// ID is "c-<startms>" for a connection episode, "t-<startms>-<tile,…>"
	// otherwise: stable across replays and restarts.
	ID         string
	Start, End time.Time // End zero: ongoing
	Sev        State     // Slow or Down: the worst of its parts
	Kind       string    // EpDrop, EpOutage, EpSlow, EpChoppy or EpBlocked
	Tiles      []string  // in tile order
	Connection bool
	// Verdict is the verdict incident behind a connection episode (also
	// one a connection drop absorbed); empty for drops and slowdowns.
	Verdict model.VerdictKind
	Cause   Cause
	DropDur time.Duration // the drop's duration, rounded; 0 when no drop
	// own is a connection episode's own span (the drop's, the incident's),
	// before the tile parts folded in widened Start and End: what
	// "Working again" says the connection did
	own window
}

// Episode kinds.
const (
	EpDrop    = "drop"    // total loss for dropMin or more
	EpOutage  = "outage"  // Down
	EpSlow    = "slow"    // Slow, or a lossy connection
	EpChoppy  = "choppy"  // a call tile's Slow
	EpBlocked = "blocked" // UDP or STUN blocked
	// epBlip is a path blip (view.blips): an episode only while grouping,
	// so that what it absorbs goes with it
	epBlip = "blip"
)

// Ongoing reports an episode that has not ended.
func (e Episode) Ongoing() bool { return e.End.IsZero() }

func (e Episode) window() window { return window{e.Start, e.End} }

// incidentEpisode is the connection episode of a verdict incident: lan,
// isp, upstream and no_network (Down when the incident lost everything: a
// peak loss of fullLoss, or its layer dropped (layerEv.dropped); otherwise
// a Slow lossy or slow period), dns (Down), udp_blocked and stun_blocked.
// Other kinds make no episode: udp_degraded shows as Choppy call tiles,
// service as the tiles themselves.
func incidentEpisode(in model.Incident, dropped bool) (Episode, bool) {
	e := Episode{Start: in.Start, End: in.End, Connection: true, Verdict: in.Kind, Sev: Down, Kind: EpOutage, own: window{in.Start, in.End}}
	switch in.Kind {
	case model.VerdictLAN, model.VerdictISP, model.VerdictUpstream:
		if in.PeakLoss < fullLoss && !dropped {
			e.Sev, e.Kind = Slow, EpSlow
		}
	case model.VerdictNoNetwork, model.VerdictDNS:
	case model.VerdictUDPBlocked, model.VerdictSTUNBlocked:
		e.Kind = EpBlocked
	default:
		return Episode{}, false
	}
	return e, true
}

// pathKind reports the verdict kinds of a broken path: lan, isp, upstream
// and no_network. A connection drop absorbs only these, and only they lead
// to "Working again".
func pathKind(k model.VerdictKind) bool {
	switch k {
	case model.VerdictLAN, model.VerdictISP, model.VerdictUpstream, model.VerdictNoNetwork:
		return true
	}
	return false
}

// dropEpisode is a drop's episode: of the connection when tile is "".
func dropEpisode(d Drop, tile string) Episode {
	e := Episode{Start: d.Start, End: d.End, Sev: Down, Kind: EpDrop, DropDur: d.Dur, Connection: tile == ""}
	if tile != "" {
		e.Tiles = []string{tile}
	} else {
		e.own = window{d.Start, d.End}
	}
	return e
}

// runEpisode is the episode of a tile's word run.
func runEpisode(tile string, kind profile.TileKind, r wordRun) Episode {
	e := Episode{Start: r.Start, End: r.End, Sev: r.Sev, Tiles: []string{tile}, Kind: EpOutage}
	if r.Sev == Slow {
		e.Kind = EpSlow
		if kind == profile.TileCall {
			e.Kind = EpChoppy
		}
	}
	return e
}

// broadEpisode is a broad slowdown's episode.
func broadEpisode(from, to time.Time) Episode {
	return Episode{Start: from, End: to, Sev: Slow, Kind: EpSlow, Connection: true, own: window{from, to}}
}

// absorbs reports whether o lies within e widened by absorbBefore and
// absorbAfter (fully covered: an ongoing o needs an ongoing e).
func (e Episode) absorbs(o Episode) bool {
	if o.Start.Before(e.Start.Add(-absorbBefore)) {
		return false
	}
	switch {
	case e.Ongoing():
		return true
	case o.Ongoing():
		return false
	}
	return !o.End.After(e.End.Add(absorbAfter))
}

// merge adds o to e: the union of their spans and tiles, the worse
// severity (and that part's kind), the longer drop.
func (e *Episode) merge(o Episode) {
	if o.Start.Before(e.Start) {
		e.Start = o.Start
	}
	if e.Ongoing() || o.Ongoing() {
		e.End = time.Time{}
	} else if o.End.After(e.End) {
		e.End = o.End
	}
	if rank(o.Sev) > rank(e.Sev) {
		e.Sev, e.Kind = o.Sev, o.Kind
	}
	e.DropDur = max(e.DropDur, o.DropDur)
	if e.Verdict == "" {
		e.Verdict = o.Verdict
	}
	e.addTiles(o.Tiles)
}

// addTiles adds tiles to e's, in tile order.
func (e *Episode) addTiles(tiles []string) {
	for _, t := range tiles {
		if !slices.Contains(e.Tiles, t) {
			e.Tiles = append(e.Tiles, t)
		}
	}
	slices.SortFunc(e.Tiles, func(a, b string) int { return tileOrder(a) - tileOrder(b) })
}

// fold adds a tile part to a connection episode: its tiles, and its span
// (o ongoing: e goes on), not its severity, kind or drop.
func (e *Episode) fold(o Episode) {
	if o.Start.Before(e.Start) {
		e.Start = o.Start
	}
	switch {
	case o.Ongoing():
		e.End = time.Time{}
	case !e.Ongoing() && o.End.After(e.End):
		e.End = o.End
	}
	e.addTiles(o.Tiles)
}

func tileOrder(id string) int {
	return slices.IndexFunc(profile.Tiles, func(d profile.TileDef) bool { return d.ID == id })
}

// groupEpisodes turns the problem parts into the problems list, newest
// first:
//
//  1. Connection parts (connection drops, path blips, incidentEpisode,
//     broadEpisode) are episodes; a connection drop absorbs the lan,
//     isp, upstream and no_network incidents and the slowdowns within
//     [drop − absorbBefore, end + absorbAfter]: one problem, not two. A
//     blip absorbs the slowdowns so, and only the incidents that end with
//     it (the engine ends an incident that is a blip's lag). They keep the drop's span: the incident is mostly the
//     verdict's window catching up. Other incidents (dns, blocked UDP)
//     and longer ones stay their own problems.
//  2. Tile parts (runEpisode, dropEpisode of a tile) fully covered by a
//     connection episode fold into it (into a drop or a blip only when
//     they start before its end + dropLate): they add their tiles and
//     widen its span (a 90 s app drop around a 1 min upstream incident is
//     one problem of 90 s; a drop keeps its measured span, so "Working
//     again" keeps its end), but its severity, kind and drop stay the
//     connection's own: the card's "Working again" reads them, and the
//     apps' drops must not make it say the connection was down.
//  3. The other tile parts of one tile less than sameTileMerge apart are
//     one episode. Then each gets its cause. Those of call tiles only, not
//     the app's side, that overlap a blocked-UDP episode and started at
//     most blockedLead before it fold into it (the verdict needs a minute
//     or two to call the block). Tile episodes with the same cause (who
//     and vendor) that overlap or start within vendorMerge are one
//     problem ("Google Meet, Google Workspace and YouTube…").
//  4. Blips are left out, with all they absorbed.
//
// cause attributes an episode (see attribute); now ends ongoing spans.
func groupEpisodes(conn, tiles []Episode, cause func(Episode) Cause, now time.Time) []Episode {
	byStart := func(a, b Episode) int { return a.Start.Compare(b.Start) }
	conn = slices.Clone(conn)
	slices.SortStableFunc(conn, byStart)
	dropLike := func(e Episode) bool { return e.Kind == EpDrop || e.Kind == epBlip }
	var cs []Episode
	for _, e := range conn {
		if dropLike(e) {
			cs = append(cs, e)
		}
	}
	for _, e := range conn {
		if dropLike(e) {
			continue
		}
		slowdown := e.Verdict == "" && e.Kind == EpSlow
		if i := slices.IndexFunc(cs, func(d Episode) bool {
			// a blip takes only the incident that is its lag (the engine
			// ended it with the blip): one within a lossy spell is the
			// spell's, and must stay
			lag := d.Kind != epBlip || slowdown || !e.Ongoing() && !e.End.After(d.End.Add(lagSlack))
			return dropLike(d) && (pathKind(e.Verdict) || slowdown) && lag && d.absorbs(e)
		}); i >= 0 {
			cs[i].addTiles(e.Tiles)
			if cs[i].Verdict == "" {
				cs[i].Verdict = e.Verdict
			}
			continue
		}
		cs = append(cs, e)
	}
	slices.SortStableFunc(cs, byStart)

	tiles = slices.Clone(tiles)
	slices.SortStableFunc(tiles, byStart)
	var ts []Episode
	for _, e := range tiles {
		if i := slices.IndexFunc(cs, func(c Episode) bool {
			return c.absorbs(e) && (!dropLike(c) || c.Ongoing() || !e.Start.After(c.End.Add(dropLate)))
		}); i >= 0 {
			if dropLike(cs[i]) {
				cs[i].addTiles(e.Tiles) // the drop's span is measured: it stays
			} else {
				cs[i].fold(e)
			}
			continue
		}
		if i := slices.IndexFunc(ts, func(o Episode) bool {
			return slices.Equal(o.Tiles, e.Tiles) && (o.Ongoing() || e.Start.Sub(o.End) < sameTileMerge)
		}); i >= 0 {
			ts[i].merge(e)
			continue
		}
		ts = append(ts, e)
	}
	for i := range ts {
		ts[i].Cause = cause(ts[i])
	}
	ts = slices.DeleteFunc(ts, func(e Episode) bool {
		if e.Cause.Who != WhoCallsPath && e.Cause.Who != WhoUnknown ||
			slices.ContainsFunc(e.Tiles, func(id string) bool { i := tileOrder(id); return i < 0 || profile.Tiles[i].Kind != profile.TileCall }) {
			return false
		}
		i := slices.IndexFunc(cs, func(c Episode) bool {
			return c.Kind == EpBlocked && c.window().overlaps(e.window(), now) && !e.Start.Before(c.Start.Add(-blockedLead)) &&
				(c.Ongoing() || !e.Ongoing())
		})
		if i < 0 {
			return false
		}
		cs[i].fold(e)
		return true
	})
	var out []Episode
	for _, e := range ts {
		if i := slices.IndexFunc(out, func(o Episode) bool {
			return o.Cause.Who == e.Cause.Who && o.Cause.Vendor == e.Cause.Vendor &&
				(o.window().overlaps(e.window(), now) || e.Start.Sub(o.Start) <= vendorMerge)
		}); i >= 0 {
			out[i].merge(e)
			continue
		}
		out = append(out, e)
	}
	for _, e := range cs {
		if e.Kind == epBlip {
			continue
		}
		e.Cause = cause(e)
		out = append(out, e)
	}
	seen := map[string]int{}
	for i := range out {
		id := episodeID(out[i])
		// two episodes of one kind starting in the same millisecond (a
		// drop and a slowdown): keep the IDs unique
		if seen[id]++; seen[id] > 1 {
			id += "-" + strconv.Itoa(seen[id])
		}
		out[i].ID = id
	}
	slices.SortStableFunc(out, func(a, b Episode) int { return b.Start.Compare(a.Start) })
	return out
}

func episodeID(e Episode) string {
	ms := strconv.FormatInt(e.Start.UnixMilli(), 10)
	if e.Connection {
		return "c-" + ms
	}
	return "t-" + ms + "-" + strings.Join(e.Tiles, ",")
}

// Counts are the problems of a period by cause kind. Speedtest and other
// causes count in Total only.
type Counts struct{ Total, Provider, Home, App int }

// dayCounts counts the episodes that ended in the last 24 h or are ongoing.
func dayCounts(eps []Episode, now time.Time) Counts {
	var c Counts
	for _, e := range eps {
		if !e.Ongoing() && e.End.Before(now.Add(-24*time.Hour)) {
			continue
		}
		c.Total++
		switch e.Cause.Kind() {
		case KindProvider:
			c.Provider++
		case KindHome:
			c.Home++
		case KindApp:
			c.App++
		}
	}
	return c
}
