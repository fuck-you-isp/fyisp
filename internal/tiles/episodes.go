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
}

// Episode kinds.
const (
	EpDrop    = "drop"    // total loss for dropMin or more
	EpOutage  = "outage"  // Down
	EpSlow    = "slow"    // Slow, or a lossy connection
	EpChoppy  = "choppy"  // a call tile's Slow
	EpBlocked = "blocked" // UDP or STUN blocked
)

// Ongoing reports an episode that has not ended.
func (e Episode) Ongoing() bool { return e.End.IsZero() }

func (e Episode) window() window { return window{e.Start, e.End} }

// incidentEpisode is the connection episode of a verdict incident: lan,
// isp, upstream and no_network (Down when the incident lost everything,
// otherwise a Slow lossy or slow period), dns (Down), udp_blocked and
// stun_blocked. Other kinds make no episode: udp_degraded shows as Choppy
// call tiles, service as the tiles themselves.
func incidentEpisode(in model.Incident) (Episode, bool) {
	e := Episode{Start: in.Start, End: in.End, Connection: true, Verdict: in.Kind, Sev: Down, Kind: EpOutage}
	switch in.Kind {
	case model.VerdictLAN, model.VerdictISP, model.VerdictUpstream:
		if in.PeakLoss < fullLoss {
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

// dropEpisode is a drop's episode: of the connection when tile is "".
func dropEpisode(d Drop, tile string) Episode {
	e := Episode{Start: d.Start, End: d.End, Sev: Down, Kind: EpDrop, DropDur: d.Dur, Connection: tile == ""}
	if tile != "" {
		e.Tiles = []string{tile}
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
	return Episode{Start: from, End: to, Sev: Slow, Kind: EpSlow, Connection: true}
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

// near reports whether o overlaps e widened by absorbBefore and absorbAfter.
func (e Episode) near(o Episode, now time.Time) bool {
	w := window{e.Start.Add(-absorbBefore), e.End}
	if !e.Ongoing() {
		w.to = e.End.Add(absorbAfter)
	}
	return w.overlaps(o.window(), now)
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
	for _, t := range o.Tiles {
		if !slices.Contains(e.Tiles, t) {
			e.Tiles = append(e.Tiles, t)
		}
	}
	slices.SortFunc(e.Tiles, func(a, b string) int { return tileOrder(a) - tileOrder(b) })
}

func tileOrder(id string) int {
	return slices.IndexFunc(profile.Tiles, func(d profile.TileDef) bool { return d.ID == id })
}

// groupEpisodes turns the problem parts into the problems list, newest
// first:
//
//  1. Connection parts (connection drops, incidentEpisode, broadEpisode)
//     are episodes; a connection drop absorbs the incidents near it
//     ([drop − absorbBefore, end + absorbAfter]): one problem, not two.
//  2. Tile parts (runEpisode, dropEpisode of a tile) fully covered by a
//     connection episode fold into it and add their tiles.
//  3. The other tile parts of one tile less than sameTileMerge apart are
//     one episode. Then each gets its cause, and tile episodes with the
//     same cause (who and vendor) that overlap or start within vendorMerge
//     are one problem ("Google Meet, Google Workspace and YouTube…").
//
// cause attributes an episode (see attribute); now ends ongoing spans.
func groupEpisodes(conn, tiles []Episode, cause func(Episode) Cause, now time.Time) []Episode {
	byStart := func(a, b Episode) int { return a.Start.Compare(b.Start) }
	conn = slices.Clone(conn)
	slices.SortStableFunc(conn, byStart)
	var cs []Episode
	for _, e := range conn {
		if e.Kind == EpDrop {
			cs = append(cs, e)
		}
	}
	for _, e := range conn {
		if e.Kind == EpDrop {
			continue
		}
		if i := slices.IndexFunc(cs, func(d Episode) bool { return d.Kind == EpDrop && e.Verdict != "" && d.near(e, now) }); i >= 0 {
			k := cs[i].Kind
			cs[i].merge(e)
			cs[i].Kind = k // still a drop
			continue
		}
		cs = append(cs, e)
	}
	slices.SortStableFunc(cs, byStart)

	tiles = slices.Clone(tiles)
	slices.SortStableFunc(tiles, byStart)
	var ts []Episode
	for _, e := range tiles {
		if i := slices.IndexFunc(cs, func(c Episode) bool { return c.absorbs(e) }); i >= 0 {
			for _, t := range e.Tiles {
				if !slices.Contains(cs[i].Tiles, t) {
					cs[i].Tiles = append(cs[i].Tiles, t)
				}
			}
			slices.SortFunc(cs[i].Tiles, func(a, b string) int { return tileOrder(a) - tileOrder(b) })
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
