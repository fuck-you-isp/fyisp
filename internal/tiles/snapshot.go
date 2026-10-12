package tiles

import (
	"context"
	"hash/fnv"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// Snapshot is the Home's state at one tick. It is immutable: the engine
// builds a new one every tick, and readers must not change it (its slices
// and pointers included).
type Snapshot struct {
	At           time.Time
	LastMeasured time.Time // the newest measured sample of any tracked series
	FirstData    time.Time // the oldest (of the replayed week)
	Learning     bool
	Conn         ConnState
	Layers       [nLayers]LayerSnap // gateway, ISP edge, anycast
	Tiles        [nTiles]TileSnap   // profile.Tiles' order
	Monitored    int                // tiles not off
	Day          Counts             // the problems of the last 24 hours
	// Rev changes when the problems do (a hash: a memo key, not a count).
	Rev uint64
	// Eps are the problems of the week, newest first (read them with
	// Problems: the same tick as the tiles).
	Eps []Episode
}

// LayerSnap is one path layer.
type LayerSnap struct {
	// State: Good; Slow when the verdict finds it unhealthy; Down while
	// it has lost everything (a drop in progress); None when unknown.
	State State
	Since time.Time // Down: since the drop started; else zero
	// LastDrop is its newest drop (dropMin or more) that ended less than
	// dropNoteFor ago.
	LastDrop *Drop
}

// TileSnap is one tile.
type TileSnap struct {
	ID    string
	State State // Good, Slow, Down, None (no data yet) or Off
	Since time.Time
	// LastDrop is the newest drop of the tile (or of the connection)
	// that ended less than dropNoteFor ago, only while the word is Good.
	LastDrop *Drop
	Bars     [nBars]Bar
	Call     *CallMetrics // call tiles
	// UsualMs is the tile's usual latency (the median of its endpoints'
	// usuals; a call tile's UDP ones); SlowAboveMs its Slow limit (app and
	// stream tiles). nil while learning or unknown.
	UsualMs, SlowAboveMs *float64
	Measured, Total      int // endpoints with checks in the latest minute, of all
	// Cause is the likely cause of a tile not Good when it is the app's
	// side, the call path, a speed test or unknown; empty when the
	// connection is to blame (the card and the strip say so).
	Cause Cause
}

// Call sources (CallMetrics.Source).
const (
	CallOwn     = "own"      // the app's own STUN targets
	CallShared  = "shared"   // the shared call path
	CallTCPOnly = "tcp_only" // no UDP on this setup: call quality is not checked
)

// CallMetrics are a call tile's numbers over callWindow.
type CallMetrics struct {
	Source    string
	Bursts    int // 0: no data
	DelayMs   float64
	JitterMs  float64
	HasJitter bool
	LossPct   float64
}

// inputs are what a tick reads outside the engine's lock.
type inputs struct {
	verdict   model.Verdict
	layers    map[string]*bool
	incidents []model.Incident // nil: not re-read
	incOK     bool
	speed     []model.SpeedTest
	speedOK   bool
}

func (e *engine) inputs(ctx context.Context, now time.Time) inputs {
	in := inputs{verdict: model.Verdict{Kind: model.VerdictOK}}
	if e.src.Verdict != nil {
		in.verdict = e.src.Verdict()
	}
	if e.src.Layers != nil {
		in.layers = e.src.Layers()
	}
	if e.src.Incidents != nil && (e.incAt.IsZero() || now.Sub(e.incAt) >= incEvery || now.Before(e.incAt)) {
		e.incAt = now
		xs, err := e.src.Incidents(ctx, now.Add(-keepFor), now)
		if err == nil {
			in.incidents, in.incOK = xs, true
		} else if ctx.Err() == nil {
			e.log.Warn("tiles: reading the incidents", "err", err)
		}
	}
	if e.src.SpeedTests != nil && (e.storedAt.IsZero() || now.Sub(e.storedAt) >= speedEvery || now.Before(e.storedAt)) {
		e.storedAt = now
		xs, err := e.src.SpeedTests(ctx, now.Add(-keepFor-time.Hour), now)
		if err == nil {
			in.speed, in.speedOK = xs, true
		} else if ctx.Err() == nil {
			e.log.Warn("tiles: reading the speed tests", "err", err)
		}
	}
	return in
}

// use takes in's re-read sources. The caller holds mu.
func (e *engine) use(in inputs) {
	e.verdict = in.verdict
	if in.incOK {
		e.incidents = in.incidents
	}
	if in.speedOK {
		e.stored = e.stored[:0]
		for _, t := range in.speed {
			e.stored = append(e.stored, speedWindow(t))
		}
		slices.SortFunc(e.stored, func(a, b speedtest.Window) int { return a.Start.Compare(b.Start) })
	}
}

// speedBoth are the stored speed tests and the live windows.
type speedBoth struct {
	stored speedWindows // by start; tests never overlap
	live   overlapper   // nil: none
}

func (s speedBoth) Overlaps(from, to time.Time) bool {
	// only the last test that started by to can reach from: the ones
	// before it ended earlier still (a week holds ~1,700 tests)
	i, _ := slices.BinarySearchFunc(s.stored, to, func(w speedtest.Window, t time.Time) int {
		if w.Start.After(t) {
			return 1
		}
		return -1
	})
	return (i > 0 && s.stored[i-1:i].Overlaps(from, to)) || (s.live != nil && s.live.Overlaps(from, to))
}

func (e *engine) speed() overlapper {
	b := speedBoth{stored: e.stored}
	if e.src.SpeedWins != nil {
		b.live = e.src.SpeedWins
	}
	if b.live == nil && len(b.stored) == 0 {
		return nil
	}
	return b
}

// checking is the snapshot before the replay is done.
func (e *engine) checking(now time.Time) *Snapshot {
	s := &Snapshot{At: now, Conn: ConnState{Kind: ConnChecking, Key: ConnChecking, Tone: ToneChecking, Icon: None}}
	first := now.Truncate(barStep).Add(-(nBars - 1) * barStep)
	for i, t := range e.tiles {
		ts := &s.Tiles[i]
		ts.ID, ts.Total, ts.State = t.def.ID, t.total(), None
		if t.off {
			ts.State = Off
			continue
		}
		s.Monitored++
		for j := range ts.Bars {
			ts.Bars[j] = Bar{From: first.Add(time.Duration(j) * barStep), State: None}
		}
	}
	for l := range s.Layers {
		s.Layers[l].State = None
	}
	return s
}

func (t *tile) total() int {
	n := len(t.web)
	for _, m := range t.udp {
		n += len(m)
	}
	return n
}

// layerKeys are the verdict's names of the layers, in layer order.
var layerKeys = [nLayers]string{model.LayerGateway, model.LayerEdge, model.LayerAnycast}

// snapshot builds the state at now. The caller holds mu.
func (e *engine) snapshot(now time.Time, in inputs, ready bool) *Snapshot {
	e.use(in)
	v := e.view(now)
	s := &Snapshot{At: now, LastMeasured: e.lastMeasured, FirstData: e.firstData, Learning: learning(now, e.firstData), Eps: v.eps}
	sp := e.speed()
	learn := s.Learning
	first := now.Truncate(barStep).Add(-(nBars - 1) * barStep)
	mins := e.closedFrom(first)
	var tn []tileNow
	for i, t := range e.tiles {
		ts := &s.Tiles[i]
		ts.ID, ts.Total = t.def.ID, t.total()
		if t.off {
			ts.State = Off
			tn = append(tn, tileNow{id: t.def.ID, vendor: vendorOf(t.def.TileDef), state: Off})
			continue
		}
		st, since := t.w.state()
		ts.State, ts.Since = st, since
		tn = append(tn, tileNow{id: t.def.ID, vendor: vendorOf(t.def.TileDef), state: st, since: since})
		drops := slices.Concat(v.tileDrops[i], v.connDrops)
		if d := lastDrop(drops, st, now); d != nil {
			c := *d
			ts.LastDrop = &c
		}
		ts.Bars = bars(now, first, t.states(mins), drops, tileEpisodes(v.eps, t.def.ID), sp)
		if t.callM != nil {
			c := *t.callM
			ts.Call = &c
		}
		ts.UsualMs, ts.SlowAboveMs = e.tileUsual(t, learn)
		ts.Measured = t.measured
		ts.Cause = tileCause(v.eps, t.def.ID, st)
	}
	for l := range s.Layers {
		ls := &s.Layers[l]
		h, known := in.layers[layerKeys[l]]
		switch {
		case v.layerCur[l] != nil:
			ls.State, ls.Since = Down, v.layerCur[l].Start
		case !known || h == nil:
			ls.State = None
		case *h:
			ls.State = Good
		default:
			ls.State = Slow
		}
		for _, w := range v.layerEnded[l] {
			if d := w.dur(now); d >= dropMin && now.Sub(w.to) < dropNoteFor && (ls.LastDrop == nil || w.to.After(ls.LastDrop.End)) {
				ls.LastDrop = &Drop{Start: w.from, End: w.to, Dur: roundDur(d)}
			}
		}
	}
	layersOK := true
	for _, h := range in.layers {
		layersOK = layersOK && (h == nil || *h)
	}
	s.Conn = connState(connIn{now: now, stale: stale(now, e.lastMeasured), ready: ready, verdict: in.verdict,
		dropNow: v.connCur, drops: slices.Concat(v.connDrops, v.blips), layersOK: layersOK, tiles: tn, eps: v.eps,
		clean: func(k model.VerdictKind, from time.Time) bool {
			return e.layersClean(v, from, time.Time{}, blamed(k)...)
		},
		edgeLoss: e.ispPeak(in.verdict, now)})
	// After a drop or a blip the verdict's layer health lags by its 60 s
	// window: while the card no longer blames the path, for lagMax at
	// most, a layer that has been clean (of loss) since is Good.
	switch s.Conn.Kind {
	case ConnWorkingAgain, ConnApp, ConnGood:
		var end time.Time
		for _, d := range slices.Concat(v.connDrops, v.blips) {
			if d.End.After(end) && now.Sub(d.End) < lagMax {
				end = d.End
			}
		}
		for l := range s.Layers {
			if ls := &s.Layers[l]; !end.IsZero() && ls.State == Slow && e.layersClean(v, end, time.Time{}, l) {
				ls.State = Good
			}
		}
	}
	s.Monitored = monitored(tn)
	s.Day = dayCounts(v.eps, now)
	s.Rev = epsHash(v.eps)
	return s
}

// ispPeak is the highest loss seen of the current isp incident: the
// verdict's 60 s window, and while the incident is young (its window still
// holds the clean checks from before) the edge's own last edgeRecent of
// checks. Kept per incident, so a dip of the noisy estimate does not turn a
// red card amber for a poll.
func (e *engine) ispPeak(v model.Verdict, now time.Time) float64 {
	if v.Kind != model.VerdictISP {
		e.peak = lossPeak{}
		return 0
	}
	if !e.peak.since.Equal(v.Since) {
		e.peak = lossPeak{since: v.Since}
	}
	e.peak.loss = max(e.peak.loss, v.Evidence["loss"])
	if now.Sub(v.Since) < edgeRecent {
		// the edge's checks since the loss began (after the first lost
		// one: the clean ones before would dilute it, the first lost one
		// would inflate 6%), with one σ on top: a few dozen checks are a
		// noisy estimate, and 40% must not read under 30%
		if l, n := e.onsetLoss(layerEdge, now.Add(-edgeRecent)); n >= 10 {
			e.peak.loss = max(e.peak.loss, l+math.Sqrt(l*(1-l)/float64(n)))
		}
	}
	return e.peak.loss
}

// lossPeak is the highest loss seen of the isp incident since since.
type lossPeak struct {
	since time.Time
	loss  float64
}

// onsetLoss is the share of a layer's checks since from that were lost,
// counted after each series' first lost one, and how many there were.
func (e *engine) onsetLoss(l int, from time.Time) (float64, int) {
	n, lost := 0, 0
	for _, i := range e.paths[l] {
		began := false
		for _, c := range e.ss[i].raw {
			if c.gap || c.ms <= from.UnixMilli() {
				continue
			}
			if !began {
				began = c.lost
				continue
			}
			n++
			if c.lost {
				lost++
			}
		}
	}
	if n == 0 {
		return 0, 0
	}
	return float64(lost) / float64(n), n
}

// closedFrom returns the closed minutes from from on: [from, nextMin).
func (e *engine) closedFrom(from time.Time) []time.Time {
	var out []time.Time
	for t := from; t.Before(e.nextMin); t = t.Add(time.Minute) {
		out = append(out, t)
	}
	return out
}

// states returns the tile-minutes of the given minutes.
func (t *tile) states(mins []time.Time) []State {
	out := make([]State, len(mins))
	for i, m := range mins {
		out[i] = t.minute(m)
	}
	return out
}

// minute is the tile-minute at m (None before the first).
func (t *tile) minute(m time.Time) State {
	if s := t.mins[minIdx(m)]; s != "" {
		return s
	}
	return None
}

// tileUsual is the median of the known usuals of the tile's endpoints
// (the UDP ones for a call tile with UDP) and, for app and stream tiles,
// the Slow limit it gives.
func (e *engine) tileUsual(t *tile, learn bool) (*float64, *float64) {
	if learn {
		return nil, nil
	}
	ids := t.web
	call := t.def.Kind == profile.TileCall
	if call && len(t.udp) > 0 {
		ids = nil
		for _, m := range t.udp {
			ids = append(ids, m...)
		}
	}
	var v []float64
	for _, i := range ids {
		if u := e.ss[i].u.ms; u > 0 {
			v = append(v, u)
		}
	}
	if len(v) == 0 {
		return nil, nil
	}
	u := median(v)
	if call {
		return &u, nil
	}
	sl := limitsFor(u, true, false).slowMs
	return &u, &sl
}

// tileEpisodes are the episodes a tile is part of, and the connection's.
func tileEpisodes(eps []Episode, id string) []Episode {
	var out []Episode
	for _, e := range eps {
		if e.Connection || slices.Contains(e.Tiles, id) {
			out = append(out, e)
		}
	}
	return out
}

// tileCause is the cause of the newest episode of a tile that is not
// Good, when the connection is not to blame.
func tileCause(eps []Episode, id string, st State) Cause {
	if !st.Bad() {
		return Cause{}
	}
	for _, e := range eps { // newest first
		if !slices.Contains(e.Tiles, id) {
			continue
		}
		if e.Kind == EpBlocked {
			return Cause{} // the network blocks the calls' fast path: the card says so
		}
		switch e.Cause.Who {
		case WhoApp, WhoCallsPath, WhoSpeedtest, WhoUnknown:
			return e.Cause
		}
		return Cause{}
	}
	return Cause{}
}

func epsHash(eps []Episode) uint64 {
	h := fnv.New64a()
	b := make([]byte, 0, 128)
	for _, e := range eps {
		b = append(b[:0], e.ID...)
		b = strconv.AppendInt(b, e.End.UnixMilli(), 10)
		b = append(b, e.Sev...)
		b = append(b, e.Kind...)
		b = append(b, e.Cause.Who...)
		b = append(b, e.Cause.Vendor...)
		b = append(b, e.Cause.Evidence...)
		for _, t := range e.Tiles {
			b = append(b, t...)
		}
		b = strconv.AppendInt(b, int64(e.DropDur), 10)
		h.Write(b)
	}
	return h.Sum64()
}

// Problems implements Engine.
func (e *engine) Problems(from time.Time) []Episode { return e.snap.Load().Problems(from) }

// Problems are the snapshot's problems that ended after from or go on,
// newest first, at most maxProblems.
func (s *Snapshot) Problems(from time.Time) []Episode {
	var out []Episode
	for _, x := range s.Eps {
		if len(out) == maxProblems {
			break
		}
		if x.Ongoing() || !x.End.Before(from) {
			out = append(out, x)
		}
	}
	return out
}
