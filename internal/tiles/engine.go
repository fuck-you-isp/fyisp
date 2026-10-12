package tiles

import (
	"context"
	"log/slog"
	"math"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/fuck-you-isp/fyisp/internal/store"
	"github.com/fuck-you-isp/fyisp/internal/store/blob"
	"github.com/fuck-you-isp/fyisp/internal/verdict"
)

// The engine: a model.Sink next to verdict.Engine that feeds the rules.
//
// Wiring (cmd/fyisp):
//
//	tl := tiles.New(prof, profile.ResolveTiles(prof), tiles.Sources{Store: st, …}, tiles.Options{Log: log})
//	probeSinks = append(probeSinks, tl) // Observe never blocks
//	go tl.Run(ctx)                      // replay 8 days, then tick every 5 s
//
// Only the series that feed a tile and the path layers' ICMP series are
// tracked (34 series of ~1,420 targets with --profile all,calls); Observe drops
// every other sample before it takes the lock.
//
// Every tick is a point of the Unix-aligned tickEvery grid, live and in
// replay alike (see tracker): it flushes the trackers, applies the tiles'
// immediate signals, then summarizes every minute whose closeAt (minute
// end + minuteGrace) is at least hold old (the longest holdFor of the
// tracked series), so every check of the minute has arrived. Replay walks the stored samples in slot order through the
// same steps, feeding before each tick only the checks whose slot is not
// after it, so a replayed week ends in the very state the live engine
// would have reached.

// Timing.
const (
	tickEvery    = 5 * time.Second  // the tick grid (Unix-aligned)
	minuteGrace  = 20 * time.Second // a minute closes this long after its end (summarize's closeAt)
	keepMins     = 8 * 24 * 60      // tile and layer minutes kept: the problems' week and some
	keepFor      = 7*24*time.Hour + time.Hour
	replayChunk  = 24 * time.Hour // replay reads the store a day at a time
	bufferFrom   = 30 * time.Second
	maxBuffer    = 1 << 16          // samples held while replay runs
	incEvery     = 30 * time.Second // the incidents are re-read this often (the verdict saves every 30 s)
	speedEvery   = 10 * time.Minute // the stored speed tests are re-read this often
	lateLogEvery = time.Hour
	// maxProblems caps Problems.
	maxProblems = 100
	problemsFor = 7 * 24 * time.Hour
)

// nTiles is len(profile.Tiles): the Home always shows them all.
const nTiles = 11

// Sources are what the engine reads besides the samples. Every field is
// optional.
type Sources struct {
	Store store.Reader // Raw, for the replay
	// SpeedTests lists the stored speed tests (store.SQLite.SpeedTests):
	// problems during one are labelled with it.
	SpeedTests func(ctx context.Context, from, to time.Time) ([]model.SpeedTest, error)
	// Incidents are the verdict engine's (verdict.Engine.Incidents).
	Incidents func(ctx context.Context, from, to time.Time) ([]model.Incident, error)
	// Verdict and Layers are the verdict engine's current verdict and path
	// layer health (verdict.MetricsSource).
	Verdict func() model.Verdict
	Layers  func() map[string]*bool
	// SpeedWins are the live speed-test windows (nil: speed tests off).
	SpeedWins *speedtest.Windows
	// PathChanges subscribes to network path changes (netinfo.Watcher's
	// Subscribe): a new gateway or ISP edge resets the usual latencies.
	PathChanges func() (<-chan netinfo.Path, func())
}

// Options tune an engine.
type Options struct {
	Now      func() time.Time // default time.Now
	Interval time.Duration    // the tick timer (default 5 s); ticks stay on the 5 s grid
	Replay   time.Duration    // history replayed at start (default 8 days: 7 shown, 1 seeding the usual)
	Log      *slog.Logger
}

// Engine judges the Home tiles from the probe samples.
type Engine interface {
	model.Sink
	// Run replays the stored history, then ticks until ctx is done.
	Run(ctx context.Context) error
	// Ready reports that the replay is done.
	Ready() bool
	// Snapshot is the state at the latest tick: never nil, immutable.
	// Until Ready its card is checking and its tiles have no data.
	Snapshot() *Snapshot
	// Chart is a tile's last 24 hours; false for an unknown or off tile.
	Chart(id string) (*Chart, bool)
	// Problems are the problems that ended after from or go on, newest
	// first, at most 100 of the last 7 days.
	Problems(from time.Time) []Episode
}

// ser is one tracked series. A STUN target is one series (KindUDP): its
// burst's jitter and loss samples merge into the burst's check by slot.
type ser struct {
	key   model.SeriesKey
	rule  runRule
	every time.Duration
	layer int // a path layer, or -1
	// stream: a web endpoint of a stream tile (Down only on failures)
	stream bool
	tr     tracker
	runs   []failRun // closed; pruned once their minutes are judged (pruneRuns), then after keepFor
	raw    []check   // the checks of the minutes not summarized yet
	// the latest closed minutes, oldest first (the call window)
	mins  [callMinutes]minute
	state State     // the latest minute's judgement
	call  callStats // UDP: the latest call window
	u     usual     // web: judged against it; UDP: shown only
	buf   []failRun
}

const callMinutes = int(callWindow / time.Minute)

// tile is one tile's state.
type tile struct {
	def    profile.ResolvedTile
	idx    int
	off    bool
	stream bool
	web    []int   // series
	udp    [][]int // members: an own STUN target, or a shared-path provider's targets
	source string  // CallOwn, CallShared, CallTCPOnly
	mw, mu [][]*tracker
	w      word
	runs   []wordRun // closed, for keepFor
	mins   [keepMins]State
	plot   [keepMins]float32 // the minute's charted latency (ms); 0: none
	// as of the latest closed minute
	measured int
	callM    *CallMetrics
}

type engine struct {
	o   Options
	src Sources
	log *slog.Logger

	// immutable after New
	index map[model.SeriesKey]int32 // tracked keys → series (STUN jitter and loss keys too)
	ss    []*ser
	tiles [nTiles]*tile
	webUs []*usual // the web series' usuals (relearn)
	paths [nLayers][]int
	hold  time.Duration // the longest holdFor of the tracked series

	mu           sync.Mutex
	last         time.Time // the latest tick
	nextMin      time.Time // the first minute not summarized; zero before the first tick
	lastRel      time.Time
	firstData    time.Time
	lastMeasured time.Time
	layerMins    [nLayers][keepMins]minute
	anyData      [keepMins]bool
	broad        []window // broad slowdowns, the newest maybe open
	gateway      netip.Addr
	edge         netip.Addr
	incidents    []model.Incident
	incAt        time.Time
	stored       speedWindows
	storedAt     time.Time
	lateLogged   int
	lateLogAt    time.Time

	live    atomic.Bool
	ready   atomic.Bool
	snap    atomic.Pointer[Snapshot]
	bufMu   sync.Mutex
	buffer  []bufSample
	bufFrom int64
	bufLost bool
	bufMax  int
}

// bufSample is a sample held while replay runs.
type bufSample struct {
	ser  int32
	kind model.ProbeKind
	ms   int64
	val  uint32 // µs; UDP loss: %
	lost bool
}

// New makes an engine for p's tiles (profile.ResolveTiles(p)).
func New(p *model.Profile, tiles []profile.ResolvedTile, src Sources, o Options) Engine {
	return newEngine(p, tiles, src, o)
}

func newEngine(p *model.Profile, rts []profile.ResolvedTile, src Sources, o Options) *engine {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Interval <= 0 {
		o.Interval = tickEvery
	}
	if o.Replay == 0 {
		o.Replay = 8 * 24 * time.Hour
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	e := &engine{o: o, src: src, log: o.Log, index: map[model.SeriesKey]int32{}, bufMax: maxBuffer}
	add := func(t model.Target, k model.ProbeKind, rule runRule, layer int) int {
		key := model.SeriesKey{Target: t.Name, Kind: k}
		if i, ok := e.index[key]; ok {
			return int(i)
		}
		i := len(e.ss)
		e.ss = append(e.ss, &ser{key: key, rule: rule, every: seriesEvery(t, k), layer: layer, state: None})
		e.index[key] = int32(i)
		if k == model.KindUDP {
			e.index[model.SeriesKey{Target: t.Name, Kind: model.KindUDPJitter}] = int32(i)
			e.index[model.SeriesKey{Target: t.Name, Kind: model.KindUDPLoss}] = int32(i)
		}
		return i
	}
	if p != nil {
		for _, t := range p.Targets {
			l := -1
			switch t.Layer {
			case model.LayerGateway:
				l = layerGateway
			case model.LayerEdge:
				l = layerEdge
			case model.LayerAnycast:
				l = layerAnycast
			}
			if l >= 0 && slices.Contains(t.ProbeKinds(), model.KindICMP) {
				e.paths[l] = append(e.paths[l], add(t, model.KindICMP, rulePath, l))
			}
		}
	}
	for i := range nTiles {
		t := &tile{idx: i, off: true}
		e.tiles[i] = t
		if i >= len(rts) {
			t.def = profile.ResolvedTile{TileDef: profile.Tiles[i]}
			continue
		}
		t.def = rts[i]
		t.off = rts[i].Off()
		t.stream = t.def.Kind == profile.TileStream
		for _, tg := range t.def.Web {
			s := add(tg, webKind(tg), ruleWeb, -1)
			e.ss[s].stream = t.stream
			t.web = append(t.web, s)
		}
		byProv := map[string]int{}
		for _, tg := range t.def.UDP {
			s := add(tg, model.KindUDP, ruleUDP, -1)
			if !t.def.SharedUDP {
				t.udp = append(t.udp, []int{s})
				continue
			}
			pv := verdict.UDPProvider(tg)
			if j, ok := byProv[pv]; ok {
				t.udp[j] = append(t.udp[j], s)
				continue
			}
			byProv[pv] = len(t.udp)
			t.udp = append(t.udp, []int{s})
		}
		switch {
		case t.def.Kind != profile.TileCall:
		case len(t.udp) == 0:
			t.source = CallTCPOnly
		case t.def.SharedUDP:
			t.source = CallShared
		default:
			t.source = CallOwn
		}
		for _, s := range t.web {
			t.mw = append(t.mw, []*tracker{&e.ss[s].tr})
		}
		for _, m := range t.udp {
			var trs []*tracker
			for _, s := range m {
				trs = append(trs, &e.ss[s].tr)
			}
			t.mu = append(t.mu, trs)
		}
	}
	for _, s := range e.ss {
		s.tr.every = s.every
		e.hold = max(e.hold, holdFor(s.every))
		if s.rule == ruleWeb {
			e.webUs = append(e.webUs, &s.u)
		}
	}
	e.snap.Store(e.checking(o.Now()))
	return e
}

// webKind is the kind a web endpoint is judged on: HTTPS, else TCP, else
// ICMP.
func webKind(t model.Target) model.ProbeKind {
	ks := t.ProbeKinds()
	for _, k := range []model.ProbeKind{model.KindHTTPS, model.KindTCP, model.KindICMP} {
		if slices.Contains(ks, k) {
			return k
		}
	}
	return ks[0]
}

// seriesEvery is a series' probe interval, as the scheduler runs it:
// Target.Interval (default profile.DefaultInterval), ICMP a third of it.
func seriesEvery(t model.Target, k model.ProbeKind) time.Duration {
	d := t.Interval
	if d <= 0 {
		d = profile.DefaultInterval
	}
	if k == model.KindICMP {
		d /= 3
	}
	return d
}

// rttUs is an RTT in µs at the store's resolution, so replayed and live
// samples are judged alike.
func rttUs(d time.Duration) uint32 {
	q := (d + blob.Unit/2) / blob.Unit * blob.Unit
	return uint32(min(q/time.Microsecond, math.MaxUint32-1))
}

// Observe implements model.Sink. Untracked keys return before any lock.
func (e *engine) Observe(s model.Sample) {
	i, ok := e.index[s.Key]
	if !ok || (s.Lost && s.Reason == model.ReasonGap) {
		return
	}
	b := bufSample{ser: i, kind: s.Key.Kind, ms: s.Slot.UnixMilli(), lost: s.Lost}
	if !s.Lost {
		b.val = sampleVal(s.Key.Kind, s.RTT)
	}
	if !e.live.Load() && e.buffered(b) {
		return
	}
	e.mu.Lock()
	e.feed(b)
	e.mu.Unlock()
}

// sampleVal is a sample's value: an RTT in µs, a burst's loss in %.
func sampleVal(k model.ProbeKind, d time.Duration) uint32 {
	if k == model.KindUDPLoss {
		return uint32(min(max(d/time.Millisecond, 0), 100))
	}
	return rttUs(d)
}

// buffered buffers a sample while replay runs; false once the engine is
// live.
func (e *engine) buffered(b bufSample) bool {
	e.bufMu.Lock()
	defer e.bufMu.Unlock()
	if e.live.Load() {
		return false
	}
	if b.ms < e.bufFrom {
		return true // replay reads it from the store
	}
	if len(e.buffer) >= e.bufMax {
		e.buffer = slices.Delete(e.buffer, 0, len(e.buffer)/8) // the oldest go
		if !e.bufLost {
			e.bufLost = true
			e.log.Warn("tiles: too many samples during replay; the oldest were dropped", "kept", len(e.buffer))
		}
	}
	e.buffer = append(e.buffer, b)
	return true
}

// feed applies one sample. The caller holds mu.
func (e *engine) feed(b bufSample) {
	s := e.ss[b.ser]
	at := msTime(b.ms)
	if e.firstData.IsZero() || at.Before(e.firstData) {
		e.firstData = at
	}
	if at.After(e.lastMeasured) {
		e.lastMeasured = at
	}
	switch b.kind {
	case model.KindUDPJitter, model.KindUDPLoss:
		c := s.find(b.ms)
		if c == nil || c.lost || b.lost {
			return
		}
		if b.kind == model.KindUDPJitter {
			c.jitUs = b.val
		} else {
			c.lossPct = uint8(b.val)
		}
		return
	}
	if s.find(b.ms) != nil {
		return // a duplicate
	}
	c := check{ms: b.ms, rttUs: b.val, jitUs: noJitter}
	if b.lost {
		c = check{ms: b.ms, lost: true, lossPct: 100, jitUs: noJitter}
	}
	s.raw = append(s.raw, c)
	s.tr.add(c)
}

// find returns the raw check of a slot, if any.
func (s *ser) find(ms int64) *check {
	for i := len(s.raw) - 1; i >= 0; i-- {
		if s.raw[i].ms == ms {
			return &s.raw[i]
		}
	}
	return nil
}

// Run implements Engine.
func (e *engine) Run(ctx context.Context) error {
	if e.src.PathChanges != nil {
		ch, cancel := e.src.PathChanges()
		defer cancel()
		go e.watchPath(ctx, ch)
	}
	start := time.Now()
	if err := e.replay(ctx, e.o.Now().Truncate(tickEvery)); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		e.log.Warn("tiles: replay failed; starting without history", "err", err)
	}
	e.goLive(ctx, e.o.Now())
	e.log.Info("tiles ready", "series", len(e.ss), "replay", time.Since(start).Round(time.Millisecond))
	t := time.NewTicker(e.o.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			e.advance(ctx, e.o.Now())
		}
	}
}

// Ready implements Engine.
func (e *engine) Ready() bool { return e.ready.Load() }

// Snapshot implements Engine.
func (e *engine) Snapshot() *Snapshot { return e.snap.Load() }

// watchPath resets the usual latencies when the gateway or the ISP edge
// changes (a laptop that moved): the fixed limits apply until minGood good
// minutes. The first address seen is only noted.
func (e *engine) watchPath(ctx context.Context, ch <-chan netinfo.Path) {
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			e.pathChanged(p)
		}
	}
}

func (e *engine) pathChanged(p netinfo.Path) {
	e.mu.Lock()
	defer e.mu.Unlock()
	moved := func(old *netip.Addr, now netip.Addr) bool {
		if !now.IsValid() {
			return false
		}
		ch := old.IsValid() && *old != now
		*old = now
		return ch
	}
	gw, edge := moved(&e.gateway, p.Gateway), moved(&e.edge, p.Edge)
	if !gw && !edge {
		return
	}
	for _, s := range e.ss {
		s.u.reset()
	}
	e.log.Info("tiles: network path changed; relearning the usual latencies", "gateway", gw, "edge", edge)
}

// advance ticks every grid point up to wall and publishes a snapshot.
func (e *engine) advance(ctx context.Context, wall time.Time) {
	now := wall.Truncate(tickEvery)
	in := e.inputs(ctx, now)
	e.mu.Lock()
	e.tickTo(now)
	s := e.snapshot(now, in, e.ready.Load())
	late := e.lateCount()
	e.mu.Unlock()
	e.snap.Store(s)
	if late > e.lateLogged && now.Sub(e.lateLogAt) >= lateLogEvery {
		e.log.Info("tiles: samples arrived too late to count", "total", late, "new", late-e.lateLogged)
		e.lateLogged, e.lateLogAt = late, now
	}
}

// tickTo runs the ticks after the latest one up to now (a grid point).
// After a long sleep it skips what the minutes cannot hold. The caller
// holds mu.
func (e *engine) tickTo(now time.Time) {
	switch {
	case e.last.IsZero():
		e.start(now)
	case now.Before(e.last.Add(-clockBack)): // the clock was set back
		e.start(now)
	case now.Sub(e.last) > keepMins*time.Minute:
		e.start(now.Add(-keepMins * time.Minute))
	}
	for t := e.last.Add(tickEvery); !t.After(now); t = t.Add(tickEvery) {
		e.tick(t)
	}
}

// start puts the first tick at t (a grid point).
func (e *engine) start(t time.Time) {
	e.last = t.Add(-tickEvery)
	e.nextMin = t.Truncate(time.Minute)
	e.lastRel = time.Time{}
}

// LateSamples counts the checks ignored because they arrived after a newer
// slot was applied (more than holdFor late).
func (e *engine) LateSamples() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lateCount()
}

func (e *engine) lateCount() int {
	n := 0
	for _, s := range e.ss {
		n += s.tr.lateChecks()
	}
	return n
}

// tick is one grid tick: flush the trackers, apply the tiles' immediate
// signals, close the minutes due. The caller holds mu.
func (e *engine) tick(now time.Time) {
	e.last = now
	for _, s := range e.ss {
		s.runs = append(s.runs, s.tr.flush(now)...)
	}
	var sigs [2]signal
	var ats [2]time.Time
	for _, t := range e.tiles {
		if t.off {
			continue
		}
		n := 0
		for _, g := range [][][]*tracker{t.mw, t.mu} {
			if len(g) > 0 {
				sigs[n], ats[n] = immediate(g)
				n++
			}
		}
		sg, at := tileSignal(sigs[:n], ats[:n])
		if r := t.w.outageSignal(sg, at); r != nil {
			t.runs = append(t.runs, *r)
		}
	}
	for !e.nextMin.Add(time.Minute + minuteGrace + e.hold).After(now) {
		e.closeMinute(e.nextMin)
		e.nextMin = e.nextMin.Add(time.Minute)
	}
}

func minIdx(t time.Time) int { return int(t.Unix() / 60 % keepMins) }

// closeMinute summarizes and judges the minute from, every series and
// tile. The caller holds mu.
func (e *engine) closeMinute(from time.Time) {
	closeAt := from.Add(time.Minute + minuteGrace)
	end := from.Add(time.Minute).UnixMilli()
	// the usuals are recomputed on a grid of their own, so the replay's
	// earlier start makes no difference
	usAt := closeAt.Truncate(usualEvery)
	learn := learning(closeAt, e.firstData)
	cut := closeAt.Add(-keepFor).UnixMilli()
	k := minIdx(from)
	var lm [nLayers]minute
	data := false
	for _, s := range e.ss {
		s.runs = pruneRuns(s.runs, from, s.rule)
		if n := len(s.runs); n > 0 && !s.runs[0].open() && s.runs[0].toMs < cut {
			s.runs = slices.DeleteFunc(s.runs, func(r failRun) bool { return !r.open() && r.toMs < cut })
		}
		m := summarize(s.raw, s.runsFor(from), s.rule, from, closeAt)
		s.raw = slices.DeleteFunc(s.raw, func(c check) bool { return c.ms < end })
		data = data || m.n() > 0
		copy(s.mins[:], s.mins[1:])
		s.mins[callMinutes-1] = m
		switch s.rule {
		case ruleWeb:
			us, ok := s.u.value(usAt)
			s.state = judgeWeb(m, limitsFor(us, ok, learn), s.stream)
			s.u.add(from, m, s.state)
		case ruleUDP:
			s.state, s.call = judgeCall(s.mins[:], s.every)
			s.u.value(usAt)
			s.u.add(from, m, s.state)
		case rulePath:
			l := &lm[s.layer]
			l.ok += m.ok
			l.lost += m.lost
		}
	}
	if usAt != e.lastRel {
		relearn(e.webUs, usAt)
		e.lastRel = usAt
	}
	for l := range lm {
		e.layerMins[l][k] = lm[l]
	}
	e.anyData[k] = data
	cov := 0
	var bad []*tile
	for _, t := range e.tiles {
		if t.off {
			continue
		}
		cov++
		st := e.tileMinute(t)
		t.mins[k] = st
		t.plot[k] = e.plotMs(t)
		if r := t.w.step(from, st); r != nil {
			t.runs = append(t.runs, *r)
		}
		if n := len(t.runs); n > 0 && t.runs[0].End.Before(msTime(cut)) {
			t.runs = slices.DeleteFunc(t.runs, func(r wordRun) bool { return r.End.Before(msTime(cut)) })
		}
		if s, _ := t.w.state(); s.Bad() {
			bad = append(bad, t)
		}
	}
	e.broadMinute(from, bad, cov, lm)
}

// runsFor returns the series' runs that can touch the minute from: the
// closed ones that end in it or later (runs never overlap, so they end in
// order), and the open one.
func (s *ser) runsFor(from time.Time) []failRun {
	lo := from.UnixMilli()
	i := len(s.runs)
	for i > 0 && s.runs[i-1].toMs >= lo {
		i--
	}
	rs := s.runs[i:]
	if r, ok := s.tr.open(); ok {
		s.buf = append(append(s.buf[:0], rs...), r)
		return s.buf
	}
	return rs
}

// runSet is the series' runs for the drop rules: the closed ones and the
// open one.
func (s *ser) set() runSet {
	rs := runSet{runs: slices.Clone(s.runs), every: s.every}
	if r, ok := s.tr.open(); ok {
		rs.runs = append(rs.runs, r)
	}
	if s.tr.last != 0 {
		rs.last = msTime(s.tr.last)
	}
	return rs
}

// tileMinute judges a tile's latest minute from its series' judgements,
// and notes its measured endpoints and call numbers.
func (e *engine) tileMinute(t *tile) State {
	var wb, ub [8]State
	web, udp := wb[:0], ub[:0]
	t.measured = 0
	for _, i := range t.web {
		s := e.ss[i]
		web = append(web, s.state)
		if s.mins[callMinutes-1].n() > 0 {
			t.measured++
		}
	}
	var calls []callStats
	for _, m := range t.udp {
		var vb [4]State
		vs := vb[:0]
		for _, i := range m {
			s := e.ss[i]
			vs = append(vs, s.state)
			if s.mins[callMinutes-1].n() > 0 {
				t.measured++
			}
			if s.call.bursts > 0 {
				calls = append(calls, s.call)
			}
		}
		udp = append(udp, vote(vs...))
	}
	if t.def.Kind == profile.TileCall {
		t.callM = callMetrics(t.source, calls)
	}
	return tileMinute(t.def.Kind, web, udp)
}

// callMetrics are a call tile's numbers: the medians over its UDP
// endpoints with bursts in the window.
func callMetrics(source string, cs []callStats) *CallMetrics {
	m := &CallMetrics{Source: source}
	if len(cs) == 0 {
		return m
	}
	var d, j, l []float64
	for _, c := range cs {
		m.Bursts += c.bursts
		d, l = append(d, c.delayMs), append(l, c.lossPct)
		if c.hasJitter {
			j = append(j, c.jitterMs)
		}
	}
	m.DelayMs, m.LossPct = median(d), median(l)
	if len(j) > 0 {
		m.JitterMs, m.HasJitter = median(j), true
	}
	return m
}

// plotMs is the tile's charted latency of the latest minute: the median of
// its endpoints' minute medians, the UDP ones for a call tile with UDP.
func (e *engine) plotMs(t *tile) float32 {
	var vb [16]float64
	v := vb[:0]
	ids := t.web
	if t.def.Kind == profile.TileCall && len(t.udp) > 0 {
		ids = nil
		for _, m := range t.udp {
			ids = append(ids, m...)
		}
	}
	for _, i := range ids {
		if m := e.ss[i].mins[callMinutes-1]; m.ok > 0 {
			v = append(v, m.medMs())
		}
	}
	if len(v) == 0 {
		return 0
	}
	return max(float32(median(v)), 0.001)
}

// broadMinute follows broad slowdowns minute by minute: broadShare of the
// covered tiles' words bad across broadVendors vendors while the path
// layers lost under layerLossy.
func (e *engine) broadMinute(from time.Time, bad []*tile, covered int, lm [nLayers]minute) {
	vendors := map[string]bool{}
	for _, t := range bad {
		vendors[vendorOf(t.def.TileDef)] = true
	}
	ok := true
	for _, m := range lm {
		ok = ok && (m.n() == 0 || float64(m.lost) < layerLossy*float64(m.n()))
	}
	on := ok && covered > 0 && float64(len(bad)) >= broadShare*float64(covered) && len(vendors) >= broadVendors
	n := len(e.broad)
	open := n > 0 && e.broad[n-1].to.IsZero()
	switch {
	case on && !open:
		e.broad = append(e.broad, window{from: from})
	case !on && open:
		e.broad[n-1].to = from
	}
	if n > 0 && !e.broad[0].to.IsZero() && e.broad[0].to.Before(from.Add(-keepFor)) {
		e.broad = e.broad[1:]
	}
}

func vendorOf(d profile.TileDef) string {
	if len(d.Vendors) > 0 {
		return d.Vendors[0]
	}
	return d.ID
}
