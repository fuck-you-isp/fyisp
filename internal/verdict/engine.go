package verdict

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Tuning. The rules and thresholds are documented in rules.go.
const (
	// DefaultInterval is how often Run evaluates.
	DefaultInterval = 5 * time.Second

	window       = 60 * time.Second // judged window
	keepRaw      = 2 * window       // raw samples kept per series
	maxRaw       = 2048             // hard cap per series (1s series keep 120)
	baselineMins = 30               // latency baseline: medians of the last 30 closed minutes
	minBaseline  = 5                // closed minutes needed before latency spikes are judged
	maxMinuteRTT = 1200             // RTTs kept for the open minute's median

	warmUp       = 60 * time.Second // warming_up after start (and after a clock jump)
	holdEvals    = 2                // evaluations a new non-ok kind must hold
	recoverAfter = 60 * time.Second // ok time needed to return to ok
	saveEvery    = 30 * time.Second // incident update / heartbeat period
	staleAfter   = 2 * time.Minute  // open incidents older than this at start are closed
	jumpAfter    = 30 * time.Second // an evaluation gap longer than this (sleep, clock step) restarts warm-up
)

// Options configures an engine.
type Options struct {
	// Now is the clock (default time.Now). Samples are placed by their Slot,
	// the window by Now.
	Now func() time.Time
	// Interval between evaluations in Run (default 5s).
	Interval time.Duration
	// Log receives persistence errors. Default: discard.
	Log *slog.Logger
	// Baseline optionally returns a series' long-term normal at a time
	// (baseline.Source.Get). When it knows a series, latency spikes are
	// judged against that normal instead of the last 30 minutes (see
	// rules.go), and the engine reports how each target compares to its
	// normal (evidence *_vs_normal, Slow, EachVsNormal). Nil: 30-minute
	// baselines only, as before.
	Baseline func(model.SeriesKey, time.Time) (model.Baseline, bool)
}

// SlowTarget is a target whose median RTT over the last minute is well above
// its long-term normal (see Engine.Slow). Kind is the probe kind compared.
type SlowTarget struct {
	Target   string          `json:"target"`
	Kind     model.ProbeKind `json:"kind"`
	Ratio    float64         `json:"ratio"` // NowMs / NormalMs
	NowMs    float64         `json:"now_ms"`
	NormalMs float64         `json:"normal_ms"`
}

// IncidentRecovery is optionally implemented by an IncidentStore (the
// SQLite store does). It lists incidents left without an end, with the time
// each was last saved, so that Run can close incidents a crash left open.
type IncidentRecovery interface {
	OpenIncidents(ctx context.Context, fn func(in model.Incident, updated time.Time)) error
}

// rec is one sample, compactly.
type rec struct {
	ms     int64  // slot, unix ms
	rtt    uint32 // microseconds, when !lost
	lost   bool
	reason model.Reason
}

// minute is one closed minute of successful RTTs (the latency baseline).
type minute struct {
	idx    int64  // unix minute; 0 = empty
	median uint32 // microseconds
}

type series struct {
	raw    []rec // ordered by ms, the last keepRaw
	everOK bool  // some sample ever succeeded (kinds that never did are ignored when others did)
	curMin int64
	curRTT []uint32
	mins   [baselineMins]minute
}

type engine struct {
	profile func() *model.Profile
	st      IncidentStore
	o       Options

	mu     sync.Mutex // guards series
	series map[model.SeriesKey]*series

	vmu    sync.Mutex // guards cur, layers and normals (read by Current/Layers/Slow)
	cur    model.Verdict
	layers map[string]*bool
	// normals: every (target, kind) with a long-term normal and ≥ 3
	// successes in the last window, at the last evaluation.
	normals []SlowTarget

	// Evaluation state; only touched by step (the Run goroutine).
	start     time.Time
	lastEval  time.Time
	curLoss   float64
	cand      model.VerdictKind
	candN     int
	candSince time.Time
	okSince   time.Time // start of the current streak of ok evaluations
	open      *model.Incident
	openSaved time.Time
	retry     []*model.Incident // closed incidents whose save failed
	adopt     []recovered       // open incidents from a previous run, decided at the end of warm-up
	recovered bool
}

type recovered struct {
	in      model.Incident
	updated time.Time
}

// New returns an Engine. profile returns the current profile (it may change
// on reload, may return nil). st may be nil (no incident log).
func New(profile func() *model.Profile, st IncidentStore, o Options) Engine {
	return newEngine(profile, st, o)
}

func newEngine(profile func() *model.Profile, st IncidentStore, o Options) *engine {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &engine{
		profile: profile, st: st, o: o,
		series: map[model.SeriesKey]*series{},
		cur:    warmingUp(time.Time{}),
	}
}

// MetricsSource adapts an Engine made by New for metrics.Collector.SetVerdictSource.
func MetricsSource(e Engine) func() (model.Verdict, map[string]*bool) {
	return func() (model.Verdict, map[string]*bool) {
		var l map[string]*bool
		if lr, ok := e.(interface{ Layers() map[string]*bool }); ok {
			l = lr.Layers()
		}
		return e.Current(), l
	}
}

// SlowSource adapts an Engine made by New for the UI's "slower than your
// normal" line: it returns e's Slow (nil for other Engines).
func SlowSource(e Engine) func() []SlowTarget {
	return func() []SlowTarget {
		if s, ok := e.(interface{ Slow() []SlowTarget }); ok {
			return s.Slow()
		}
		return nil
	}
}

// VsNormalSource adapts an Engine made by New for
// metrics.Collector.SetVsNormalSource (a no-op for other Engines).
func VsNormalSource(e Engine) func(yield func(name string, kind model.ProbeKind, ratio float64)) {
	return func(yield func(string, model.ProbeKind, float64)) {
		if s, ok := e.(interface {
			EachVsNormal(func(string, model.ProbeKind, float64))
		}); ok {
			s.EachVsNormal(yield)
		}
	}
}

// Slow lists the targets whose median RTT over the last minute (at the last
// evaluation) is more than slowFactor × their long-term normal and at least
// slowMinExtraMs above it: one entry per target (its slowest kind), slowest
// first. It is a signal for the UI, not a verdict: no hysteresis, no
// incident, and it does not change Current. Empty without Options.Baseline.
func (e *engine) Slow() []SlowTarget {
	e.vmu.Lock()
	defer e.vmu.Unlock()
	var out []SlowTarget
	for _, n := range e.normals {
		if n.Ratio <= slowFactor || n.NowMs-n.NormalMs < slowMinExtraMs {
			continue
		}
		if i := slices.IndexFunc(out, func(o SlowTarget) bool { return o.Target == n.Target }); i >= 0 {
			if n.Ratio > out[i].Ratio {
				out[i] = n
			}
			continue
		}
		out = append(out, n)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Ratio != out[j].Ratio {
			return out[i].Ratio > out[j].Ratio
		}
		return out[i].Target < out[j].Target
	})
	return out
}

// EachVsNormal calls fn for every (target, kind) with a long-term normal and
// enough successful probes in the last minute, with the ratio of the
// window's median RTT to the normal median (at the last evaluation).
func (e *engine) EachVsNormal(fn func(name string, kind model.ProbeKind, ratio float64)) {
	e.vmu.Lock()
	n := slices.Clone(e.normals)
	e.vmu.Unlock()
	for _, x := range n {
		fn(x.Target, x.Kind, x.Ratio)
	}
}

// Observe records a sample. Not-measured gaps are ignored. It never blocks
// for long (one short mutex).
func (e *engine) Observe(s model.Sample) {
	if s.Lost && s.Reason == model.ReasonGap {
		return
	}
	r := rec{ms: s.Slot.UnixMilli(), lost: s.Lost, reason: s.Reason}
	if !s.Lost {
		us := s.RTT.Microseconds()
		r.rtt = uint32(min(max(us, 1), 1<<32-1))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	sr := e.series[s.Key]
	if sr == nil {
		sr = &series{}
		e.series[s.Key] = sr
	}
	n := len(sr.raw)
	if n == 0 || sr.raw[n-1].ms < r.ms {
		sr.raw = append(sr.raw, r)
	} else {
		i := sort.Search(n, func(i int) bool { return sr.raw[i].ms >= r.ms })
		if sr.raw[i].ms == r.ms {
			return // duplicate slot
		}
		sr.raw = slices.Insert(sr.raw, i, r)
	}
	if len(sr.raw) > maxRaw {
		sr.raw = append(sr.raw[:0], sr.raw[len(sr.raw)-maxRaw:]...)
	}
	if s.Lost {
		return
	}
	sr.everOK = true
	m := floorDiv(r.ms, 60_000)
	if m > sr.curMin {
		sr.closeMinute()
		sr.curMin = m
	}
	if m == sr.curMin && len(sr.curRTT) < maxMinuteRTT {
		sr.curRTT = append(sr.curRTT, r.rtt)
	}
}

func (sr *series) closeMinute() {
	if len(sr.curRTT) == 0 {
		return
	}
	sr.mins[mod(sr.curMin, baselineMins)] = minute{idx: sr.curMin, median: medianU32(sr.curRTT)}
	sr.curRTT = sr.curRTT[:0]
}

// Current returns the current (hysteresis-filtered) verdict.
func (e *engine) Current() model.Verdict {
	e.vmu.Lock()
	defer e.vmu.Unlock()
	v := e.cur
	v.Targets = slices.Clone(v.Targets)
	if v.Evidence != nil {
		ev := make(map[string]float64, len(v.Evidence))
		for k, x := range v.Evidence {
			ev[k] = x
		}
		v.Evidence = ev
	}
	return v
}

// Layers returns the health of each path layer (model.LayerGateway,
// LayerEdge, LayerAnycast) at the last evaluation, without hysteresis; nil
// means unknown (no samples in the last minute).
func (e *engine) Layers() map[string]*bool {
	e.vmu.Lock()
	defer e.vmu.Unlock()
	out := make(map[string]*bool, 3)
	for _, l := range []string{model.LayerGateway, model.LayerEdge, model.LayerAnycast} {
		if h := e.layers[l]; h != nil {
			b := *h
			out[l] = &b
		} else {
			out[l] = nil
		}
	}
	return out
}

// Incidents returns the stored incidents overlapping [from, to], newest first.
func (e *engine) Incidents(ctx context.Context, from, to time.Time) ([]model.Incident, error) {
	if e.st == nil {
		return nil, nil
	}
	return e.st.Incidents(ctx, from, to)
}

// Run recovers incidents a previous run left open, then evaluates every
// Interval until ctx is done. On return the open incident (if any) is saved
// but left open, so a restart within staleAfter continues it.
func (e *engine) Run(ctx context.Context) error {
	e.recoverIncidents(ctx)
	t := time.NewTicker(e.o.Interval)
	defer t.Stop()
	e.step(ctx, e.o.Now())
	for {
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if e.open != nil {
				e.save(sctx, e.open)
			}
			cancel()
			return nil
		case <-t.C:
			e.step(ctx, e.o.Now())
		}
	}
}

// recoverIncidents handles incidents without an end found at startup (the
// previous run crashed or was stopped during an incident). Those last saved
// more than staleAfter ago are closed with End = their last save; newer
// ones are kept aside until warm-up ends: if the verdict then has the same
// kind the incident continues, otherwise it is closed at its last save.
// Without IncidentRecovery the store's ongoing incidents are found with
// Incidents and their Start stands in for the last save.
func (e *engine) recoverIncidents(ctx context.Context) {
	if e.recovered || e.st == nil {
		return
	}
	e.recovered = true
	now := e.o.Now()
	var found []recovered
	if rs, ok := e.st.(IncidentRecovery); ok {
		if err := rs.OpenIncidents(ctx, func(in model.Incident, up time.Time) { found = append(found, recovered{in, up}) }); err != nil {
			e.o.Log.Warn("verdict: listing open incidents", "err", err)
			return
		}
	} else {
		all, err := e.st.Incidents(ctx, now.Add(-staleAfter), now.Add(100*365*24*time.Hour))
		if err != nil {
			e.o.Log.Warn("verdict: listing open incidents", "err", err)
			return
		}
		for _, in := range all {
			if in.End.IsZero() {
				found = append(found, recovered{in, in.Start})
			}
		}
	}
	for _, r := range found {
		if r.updated.Before(now.Add(-staleAfter)) {
			e.closeRecovered(ctx, r)
		} else {
			e.adopt = append(e.adopt, r)
		}
	}
}

func (e *engine) closeRecovered(ctx context.Context, r recovered) {
	in := r.in
	in.End = r.updated
	if in.End.Before(in.Start) {
		in.End = in.Start
	}
	e.save(ctx, &in)
}

// save persists in; a failed save of a closed incident is retried on later
// evaluations (a failed open one is retried by the heartbeat).
func (e *engine) save(ctx context.Context, in *model.Incident) bool {
	if e.st == nil {
		return true
	}
	if err := e.st.SaveIncident(ctx, in); err != nil {
		e.o.Log.Warn("verdict: saving incident", "kind", in.Kind, "err", err)
		if !in.End.IsZero() && !slices.Contains(e.retry, in) {
			e.retry = append(e.retry, in)
		}
		return false
	}
	return true
}

// step is one evaluation at now.
func (e *engine) step(ctx context.Context, now time.Time) {
	p := e.profile()
	e.mu.Lock()
	ts := e.collect(now, p)
	e.mu.Unlock()
	var normals []SlowTarget
	for _, t := range ts {
		normals = append(normals, t.normals...)
	}
	e.vmu.Lock()
	e.normals = normals
	e.vmu.Unlock()
	e.apply(ctx, now, judge(p, ts))
}

// apply runs warm-up, hysteresis and the incident log for one judgement.
func (e *engine) apply(ctx context.Context, now time.Time, j judgement) {
	if len(e.retry) > 0 {
		r := e.retry
		e.retry = nil
		for _, in := range r {
			e.save(ctx, in)
		}
	}
	if e.start.IsZero() {
		e.start = now
	} else if gap := now.Sub(e.lastEval); gap > jumpAfter || gap < 0 {
		// The machine slept or the clock stepped: nothing was evaluated
		// meanwhile, so the open incident ends where we last saw it.
		e.o.Log.Info("verdict: evaluation gap; warming up again", "gap", gap)
		if e.open != nil {
			e.open.End = e.lastEval
			e.save(ctx, e.open)
			e.open = nil
		}
		e.start = now
		e.cand, e.candN, e.okSince = "", 0, time.Time{}
		e.setCurrent(warmingUp(now), 0)
	}
	e.lastEval = now

	e.vmu.Lock()
	e.layers = j.layers
	e.vmu.Unlock()

	if now.Sub(e.start) < warmUp {
		if e.cur.Kind != model.VerdictWarmingUp || !e.cur.Since.Equal(e.start) {
			e.setCurrent(warmingUp(e.start), 0)
		}
		return
	}
	raw := j.v
	if raw.Kind == model.VerdictOK {
		if e.okSince.IsZero() {
			e.okSince = now
		}
		e.cand, e.candN = "", 0
	} else {
		e.okSince = time.Time{}
		if e.cand == raw.Kind {
			e.candN++
		} else {
			e.cand, e.candN, e.candSince = raw.Kind, 1, now
		}
	}

	switch {
	case e.cur.Kind == model.VerdictWarmingUp:
		// The window is full: take the judgement as it is.
		raw.Since = now
		if in := e.decideAdopted(ctx, raw.Kind); in != nil {
			raw.Since = in.Start
			in.PeakLoss = max(in.PeakLoss, j.loss)
			in.Targets = union(in.Targets, raw.Targets)
			e.setCurrent(raw, j.loss)
			e.open = in
			if e.save(ctx, in) {
				e.openSaved = now
			}
		} else {
			e.transition(ctx, now, raw, j.loss)
		}
	case raw.Kind == e.cur.Kind:
		raw.Since = e.cur.Since
		e.setCurrent(raw, j.loss)
		e.updateOpen(ctx, now, raw, j.loss)
	case raw.Kind == model.VerdictOK:
		if now.Sub(e.okSince) >= recoverAfter {
			raw.Since = e.okSince
			e.transition(ctx, now, raw, j.loss)
		} else {
			e.heartbeat(ctx, now)
		}
	case e.candN >= holdEvals:
		raw.Since = e.candSince
		e.transition(ctx, now, raw, j.loss)
	default:
		e.heartbeat(ctx, now)
	}
}

func (e *engine) setCurrent(v model.Verdict, loss float64) {
	e.curLoss = loss
	e.vmu.Lock()
	e.cur = v
	e.vmu.Unlock()
}

// transition makes v (with v.Since set) current, closing the open incident
// at v.Since and opening a new one when v is a problem.
func (e *engine) transition(ctx context.Context, now time.Time, v model.Verdict, loss float64) {
	if e.open != nil {
		e.open.End = v.Since
		if e.open.End.Before(e.open.Start) {
			e.open.End = e.open.Start
		}
		e.save(ctx, e.open)
		e.open = nil
	}
	e.setCurrent(v, loss)
	if isProblem(v.Kind) {
		e.open = &model.Incident{
			Start: v.Since, Kind: v.Kind, Summary: v.Summary,
			Targets: slices.Clone(v.Targets), PeakLoss: loss,
		}
		e.save(ctx, e.open)
		e.openSaved = now
	}
}

// decideAdopted returns the recovered incident to continue (one of kind k,
// if k is a problem) and closes the others at their last save.
func (e *engine) decideAdopted(ctx context.Context, k model.VerdictKind) *model.Incident {
	var cont *model.Incident
	for _, r := range e.adopt {
		if cont == nil && isProblem(k) && r.in.Kind == k {
			in := r.in
			cont = &in
			continue
		}
		e.closeRecovered(ctx, r)
	}
	e.adopt = nil
	return cont
}

// updateOpen folds the latest judgement of the same kind into the open
// incident: peak loss, affected targets (union) and the summary of the worst
// moment. It is saved at most every saveEvery.
func (e *engine) updateOpen(ctx context.Context, now time.Time, v model.Verdict, loss float64) {
	if e.open == nil {
		return
	}
	if loss > e.open.PeakLoss {
		e.open.PeakLoss = loss
		e.open.Summary = v.Summary
	}
	e.open.Targets = union(e.open.Targets, v.Targets)
	e.heartbeat(ctx, now)
}

// heartbeat saves the open incident every saveEvery (also when unchanged:
// its last save is where a crash would end it).
func (e *engine) heartbeat(ctx context.Context, now time.Time) {
	if e.open == nil || (e.open.ID != 0 && now.Sub(e.openSaved) < saveEvery) {
		return
	}
	if e.save(ctx, e.open) {
		e.openSaved = now
	}
}

func isProblem(k model.VerdictKind) bool {
	return k != model.VerdictOK && k != model.VerdictWarmingUp
}

func warmingUp(since time.Time) model.Verdict {
	return model.Verdict{Kind: model.VerdictWarmingUp, Since: since, Summary: "Collecting data: a verdict will be ready within a minute."}
}

func union(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}

// ---- window statistics ----

// tstat is one target's last minute.
type tstat struct {
	t                   model.Target
	n, lost, dns, nonet int     // samples, lost, lost to DNS, lost to no network / unreachable
	spike               bool    // some kind's median RTT far above its baseline
	rtt, base           float64 // ms: window median and baseline of the spiking (else some) kind
	longTerm            bool    // base is a long-term normal (Options.Baseline), not the 30-minute one
	med                 float64 // ms: median of every successful RTT in the window; 0 if none
	vsNormal            float64 // largest window median / long-term normal over the kinds; 0 if none
	normals             []SlowTarget
}

func (t *tstat) loss() float64 {
	if t.n == 0 {
		return 0
	}
	return float64(t.lost) / float64(t.n)
}

// lossNoDNS ignores name-resolution failures (judged by the dns rule).
func (t *tstat) lossNoDNS() float64 {
	if t.n-t.dns <= 0 {
		return 0
	}
	return float64(t.lost-t.dns) / float64(t.n-t.dns)
}

// unhealthy: lossy (≥ lossBad, and at least minTargetLost samples), or a
// latency spike.
func (t *tstat) unhealthy() bool {
	return t.n > 0 && ((t.lost >= minTargetLost && t.loss() >= lossBad) || t.spike)
}

// collect trims old samples and summarizes the window (now-window, now] for
// every target of p. Called with e.mu held.
func (e *engine) collect(now time.Time, p *model.Profile) []*tstat {
	nowMs := now.UnixMilli()
	from := nowMs - window.Milliseconds()
	keep := nowMs - keepRaw.Milliseconds()
	nowMin := floorDiv(nowMs, 60_000)
	for k, sr := range e.series {
		i := sort.Search(len(sr.raw), func(i int) bool { return sr.raw[i].ms >= keep })
		if i > 0 {
			sr.raw = append(sr.raw[:0], sr.raw[i:]...)
		}
		if len(sr.raw) == 0 && sr.curMin < nowMin-baselineMins {
			delete(e.series, k)
		}
	}
	if p == nil {
		return nil
	}
	// Baseline: closed minutes entirely before the window.
	baseTo := floorDiv(from, 60_000) // minutes < baseTo end at or before `from`
	var scratch, pooled []uint32
	out := make([]*tstat, 0, len(p.Targets))
	for _, t := range p.Targets {
		ts := &tstat{t: t}
		pooled = pooled[:0]
		var srs [3]*series
		kinds := [3]model.ProbeKind{model.KindHTTPS, model.KindTCP, model.KindICMP}
		anyOK := false
		for i, k := range kinds {
			srs[i] = e.series[model.SeriesKey{Target: t.Name, Kind: k}]
			anyOK = anyOK || (srs[i] != nil && srs[i].everOK)
		}
		for ki, sr := range srs {
			if sr == nil || (anyOK && !sr.everOK) {
				continue // e.g. ICMP blocked by a target that answers HTTPS
			}
			scratch = scratch[:0]
			for j := sort.Search(len(sr.raw), func(i int) bool { return sr.raw[i].ms > from }); j < len(sr.raw); j++ {
				r := sr.raw[j]
				ts.n++
				if !r.lost {
					scratch = append(scratch, r.rtt)
					continue
				}
				ts.lost++
				switch r.reason {
				case model.ReasonDNS:
					ts.dns++
				case model.ReasonNoNetwork, model.ReasonUnreachable:
					ts.nonet++
				}
			}
			pooled = append(pooled, scratch...)
			if len(scratch) < 3 {
				continue
			}
			med := float64(medianU32(scratch)) / 1000
			if e.o.Baseline != nil {
				if b, ok := e.o.Baseline(model.SeriesKey{Target: t.Name, Kind: kinds[ki]}, now); ok && b.MedianMs > 0 {
					r := med / b.MedianMs
					ts.normals = append(ts.normals, SlowTarget{Target: t.Name, Kind: kinds[ki], Ratio: round3(r), NowMs: round3(med), NormalMs: round3(b.MedianMs)})
					ts.vsNormal = max(ts.vsNormal, r)
					ts.judgeKind(med, b.MedianMs, med > normalThreshold(b), true)
					continue
				}
			}
			var meds []uint32
			for _, m := range sr.mins {
				if m.idx != 0 && m.idx < baseTo && m.idx >= baseTo-baselineMins {
					meds = append(meds, m.median)
				}
			}
			if sr.curMin < baseTo && sr.curMin >= baseTo-baselineMins && len(sr.curRTT) > 0 {
				meds = append(meds, medianU32(slices.Clone(sr.curRTT))) // open minute already over
			}
			if len(meds) < minBaseline {
				continue
			}
			base := float64(medianU32(meds)) / 1000
			ts.judgeKind(med, base, med > spikeThreshold(base), false)
		}
		if len(pooled) > 0 {
			ts.med = float64(medianU32(pooled)) / 1000
		}
		out = append(out, ts)
	}
	return out
}

// judgeKind folds one kind's window median and baseline into t: the
// spiking kind with the largest ratio wins, else the first kind judged.
func (t *tstat) judgeKind(med, base float64, spike, longTerm bool) {
	if spike && (!t.spike || med/base > t.rtt/t.base) {
		t.spike, t.rtt, t.base, t.longTerm = true, med, base, longTerm
	} else if !t.spike && t.rtt == 0 {
		t.rtt, t.base, t.longTerm = med, base, longTerm
	}
}

// medianU32 sorts v and returns its (lower) median.
func medianU32(v []uint32) uint32 {
	slices.Sort(v)
	return v[(len(v)-1)/2]
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func mod(a, b int64) int64 { return a - floorDiv(a, b)*b }
