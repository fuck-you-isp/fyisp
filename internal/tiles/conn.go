package tiles

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Connection states (ConnState.Kind) besides the verdict kinds.
const (
	ConnStale        = "stale"
	ConnChecking     = "checking"
	ConnDrop         = "drop"  // a connection drop in progress
	ConnBroad        = "broad" // most apps slow while the path is fine
	ConnApp          = "app"   // some apps have problems, the connection is fine
	ConnWorkingAgain = "working_again"
	ConnGood         = "good"
)

// Tones and icons of the answer card.
const (
	ToneOK       = "ok"
	ToneWarn     = "warn"
	ToneBad      = "bad"
	ToneStale    = "stale"
	ToneChecking = "checking"
	ToneInfo     = "info"
)

// ConnState is the answer card: the first matching row of
//
//  1. stale; 2. checking (not ready, or the verdict is warming up);
//  3. verdict no_network / lan / isp / upstream, unless its incident
//     started inside a connection drop or a path blip that ended less
//     than verdictLag ago, or inside one (or lagSlack after it) since
//     which the gateway and the edge have been clean (then skip it: the
//     verdict's window still holds the drop). An isp verdict from partial
//     loss, with no drop in progress, is amber (Lossy);
//  4. a connection drop in progress; 5. verdict dns;
//  6. udp_blocked / stun_blocked (Quiet: info); 7. udp_degraded;
//  8. broad slowdown; 9. app-only problems (with Working again appended
//     when it applies); 10. Working again; 11. good.
//
// Off tiles never take part.
type ConnState struct {
	Kind  string // a Conn* constant or a model.VerdictKind
	Key   string // stable while the card's text is: the client announces changes of it (ConnApp: plus Again's end)
	Tone  string
	Icon  State // Good, Slow, Down or None
	Since time.Time
	Quiet bool     // a long-standing udp_blocked/stun_blocked
	Lossy bool     // isp: the provider loses some data, it has not dropped
	Tiles []string // ConnApp and ConnBroad: the tiles not Good, in tile order
	// Again is the recent connection problem behind ConnWorkingAgain, also
	// set with ConnApp when it applies.
	Again *Recovered
}

// Copy variants of "Working again".
const (
	AgainDropped = "dropped" // full loss under againDropped: "dropped for about 40 seconds at …"
	AgainDown    = "down"    // full loss: "was down for about N minutes (…–…)"
	AgainTrouble = "trouble" // partial or lossy: "had trouble for about …"
)

// Recovered is a connection problem that ended less than recoveredFor ago.
type Recovered struct {
	Start, End time.Time
	Dur        time.Duration // the drop's for a drop, else End − Start
	Variant    string
	Cause      Cause
	Episode    string // its ID
}

// workingAgain finds the newest connection episode of kind lan, isp,
// upstream, no_network or a connection drop that ended less than
// recoveredFor ago. dns, the UDP kinds and broad slowdowns never count.
func workingAgain(eps []Episode, now time.Time) *Recovered {
	var e *Episode
	for i, x := range eps {
		if !x.Connection || x.Ongoing() || now.Sub(x.End) >= recoveredFor {
			continue
		}
		if x.Kind != EpDrop && !pathKind(x.Verdict) {
			continue
		}
		if e == nil || x.End.After(e.End) {
			e = &eps[i]
		}
	}
	if e == nil {
		return nil
	}
	r := &Recovered{Start: e.Start, End: e.End, Dur: e.End.Sub(e.Start), Cause: e.Cause, Episode: e.ID}
	if e.DropDur > 0 {
		r.Dur = e.DropDur
	}
	switch {
	case e.Sev != Down:
		r.Variant = AgainTrouble
	case r.Dur < againDropped:
		r.Variant = AgainDropped
	default:
		r.Variant = AgainDown
	}
	return r
}

// tileNow is a tile's word for the card.
type tileNow struct {
	id     string
	vendor string
	state  State // Off for an off tile
	since  time.Time
}

// monitored counts the tiles that are not off.
func monitored(ts []tileNow) int {
	n := 0
	for _, t := range ts {
		if t.state != Off {
			n++
		}
	}
	return n
}

// connIn is what the card is decided from.
type connIn struct {
	now     time.Time
	stale   bool
	ready   bool
	verdict model.Verdict
	dropNow *Drop  // a connection drop in progress
	drops   []Drop // the connection drops and the path blips (ended ones matter)
	// clean reports the gateway and the edge clean since from (nil: unknown)
	clean    func(from time.Time) bool
	layersOK bool // no path layer is unhealthy
	tiles    []tileNow
	eps      []Episode // for Working again
}

// connState decides the card (see ConnState).
func connState(in connIn) ConnState {
	ms := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
	v := in.verdict
	switch {
	case in.stale:
		return ConnState{Kind: ConnStale, Key: ConnStale, Tone: ToneStale, Icon: None}
	case !in.ready || v.Kind == model.VerdictWarmingUp:
		return ConnState{Kind: ConnChecking, Key: ConnChecking, Tone: ToneChecking, Icon: None}
	}
	vk := func(kind string, tone string, icon State) ConnState {
		return ConnState{Kind: string(v.Kind), Key: kind + ":" + ms(v.Since), Tone: tone, Icon: icon, Since: v.Since, Quiet: v.Quiet}
	}
	switch v.Kind {
	case model.VerdictNoNetwork, model.VerdictLAN, model.VerdictISP, model.VerdictUpstream:
		// the verdict's 60 s window outlasts a drop: for verdictLag after
		// a drop ends, or while the path has been clean since, an
		// incident that started in it is that drop, already over
		if !slices.ContainsFunc(in.drops, func(d Drop) bool {
			if !d.Ended() || v.Since.Before(d.Start.Add(-absorbBefore)) || v.Since.After(d.End.Add(lagSlack)) {
				return false
			}
			clean := in.clean != nil && in.clean(d.End)
			return clean || !v.Since.After(d.End) && in.now.Before(d.End.Add(verdictLag))
		}) {
			if loss, ok := v.Evidence["loss"]; ok && v.Kind == model.VerdictISP && in.dropNow == nil && loss > 0 && loss < fullLoss {
				c := vk("isp_lossy", ToneWarn, Slow)
				c.Lossy = true
				return c
			}
			return vk(string(v.Kind), ToneBad, Down)
		}
	}
	if d := in.dropNow; d != nil {
		return ConnState{Kind: ConnDrop, Key: ConnDrop + ":" + ms(d.Start), Tone: ToneBad, Icon: Down, Since: d.Start}
	}
	switch v.Kind {
	case model.VerdictDNS:
		return vk(string(v.Kind), ToneWarn, Slow)
	case model.VerdictUDPBlocked, model.VerdictSTUNBlocked:
		if v.Quiet {
			return vk(string(model.VerdictUDPBlocked), ToneInfo, Slow)
		}
		return vk(string(model.VerdictUDPBlocked), ToneWarn, Slow)
	case model.VerdictUDPDegraded:
		return vk(string(v.Kind), ToneWarn, Slow)
	}
	var bad []tileNow
	vendors := map[string]bool{}
	for _, t := range in.tiles {
		if t.state.Bad() {
			bad = append(bad, t)
			vendors[t.vendor] = true
		}
	}
	since := func() time.Time {
		var s time.Time
		for _, t := range bad {
			if s.IsZero() || t.since.Before(s) {
				s = t.since
			}
		}
		return s
	}
	ids := func() []string {
		var out []string
		for _, t := range bad {
			out = append(out, t.id)
		}
		return out
	}
	n := monitored(in.tiles)
	if in.layersOK && n > 0 && float64(len(bad)) >= broadShare*float64(n) && len(vendors) >= broadVendors {
		s := since()
		return ConnState{Kind: ConnBroad, Key: ConnBroad + ":" + ms(s), Tone: ToneWarn, Icon: Slow, Since: s, Tiles: ids()}
	}
	again := workingAgain(in.eps, in.now)
	if len(bad) > 0 {
		s := since()
		key := ConnApp + ":" + strings.Join(ids(), ",") + ":" + ms(s)
		if again != nil { // the sub line gains the Working again sentence
			key += ":" + ms(again.End)
		}
		return ConnState{Kind: ConnApp, Key: key, Tone: ToneOK, Icon: Good, Since: s, Tiles: ids(), Again: again}
	}
	if again != nil {
		return ConnState{Kind: ConnWorkingAgain, Key: ConnWorkingAgain + ":" + ms(again.End),
			Tone: ToneWarn, Icon: Slow, Since: again.End, Again: again}
	}
	return ConnState{Kind: ConnGood, Key: ConnGood, Tone: ToneOK, Icon: Good}
}
