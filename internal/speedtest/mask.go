package speedtest

import (
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Self-interference: a speed test fills the link, so every probe that runs
// during it measures the test, not the connection. Windows records when
// tests run (plus MaskTail after each): the charts shade them, and the Home
// tiles attribute a problem in one to the test (Overlaps). The samples are
// stored as measured. LoadTap keeps the gateway's and the edge's raw RTTs
// for latency under load.

// MaskTail extends every test's window: queues take a moment to drain.
const MaskTail = 2 * time.Second

// AbortTail extends a test's window when it was cut short (cancelled, timed
// out, failed): the sockets are reset (SO_LINGER 0), but data already in
// flight still arrives for a moment.
const AbortTail = 3 * time.Second

// Window is one test's span.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Windows is the test windows of the last keepWindows, plus the running
// one. Safe for concurrent use.
type Windows struct {
	mu     sync.Mutex
	list   []Window // ended, oldest first
	active time.Time
}

// keepWindows is how long ended windows are kept: samples arrive within
// seconds, and Share looks back a day.
const keepWindows = 25 * time.Hour

func (w *Windows) trim(now time.Time) {
	i := 0
	for i < len(w.list) && now.Sub(w.list[i].End) > keepWindows {
		i++
	}
	w.list = w.list[i:]
}

// Begin marks a test running from t.
func (w *Windows) Begin(t time.Time) {
	w.mu.Lock()
	w.active = t
	w.mu.Unlock()
}

// End closes the running test's window at t and returns it.
func (w *Windows) End(t time.Time) Window {
	w.mu.Lock()
	defer w.mu.Unlock()
	win := Window{Start: w.active, End: t}
	w.active = time.Time{}
	if !win.Start.IsZero() {
		w.list = append(w.list, win)
		w.trim(t)
	}
	return win
}

// Add records a past window (e.g. loaded from the store at start).
func (w *Windows) Add(win Window) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.list = append(w.list, win)
	sort.Slice(w.list, func(i, j int) bool { return w.list[i].Start.Before(w.list[j].Start) })
	w.trim(win.End)
}

// Overlaps reports whether [from, to] overlaps a window extended by
// MaskTail (a running test's window is open-ended).
func (w *Windows) Overlaps(from, to time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active.IsZero() && !to.Before(w.active) {
		return true
	}
	for i := len(w.list) - 1; i >= 0; i-- {
		x := w.list[i]
		if x.End.Add(MaskTail).Before(from) {
			break // older windows end earlier still (tests never overlap)
		}
		if !x.Start.After(to) {
			return true
		}
	}
	return false
}

// SweepGap is the unmasked time left between a sweep's tests, so the path
// probes still get real samples while every server is tested.
const SweepGap = 4 * time.Second

// LastEnd is the end of the newest ended window (zero for none).
func (w *Windows) LastEnd() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	var last time.Time
	for _, x := range w.list {
		if x.End.After(last) {
			last = x.End
		}
	}
	return last
}

// Running reports whether a test runs now.
func (w *Windows) Running() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.active.IsZero()
}

// Share is the fraction of [now-d, now] masked: windows plus their tails.
func (w *Windows) Share(now time.Time, d time.Duration) float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	from := now.Add(-d)
	spans := slices.Clone(w.list)
	if !w.active.IsZero() {
		spans = append(spans, Window{Start: w.active, End: now})
	}
	var sum time.Duration
	for _, x := range spans {
		a, b := x.Start, x.End.Add(MaskTail)
		if a.Before(from) {
			a = from
		}
		if b.After(now) {
			b = now
		}
		if b.After(a) {
			sum += b.Sub(a)
		}
	}
	return float64(sum) / float64(d)
}

// Layers of latency under load.
const (
	LayerGateway = "gateway" // the home router (ICMP): queues in the home network
	LayerEdge    = "edge"    // the ISP edge (ICMP): the first hop past the router, behind the WAN queue
)

// LoadTap keeps the recent ICMP samples of the gateway and the ISP edge,
// lost ones included, by the probe's start (Sample.At; its arrival when
// unset): the idle side of latency under load
// (the loaded side comes from the test's own prober, see load.go). Safe for
// concurrent use.
type LoadTap struct {
	now    func() time.Time
	layers map[model.SeriesKey]string
	start  time.Time // when it began watching

	mu  sync.Mutex
	buf []tapSample
}

type tapSample struct {
	at    time.Time
	layer string
	s     loadSample
}

// tapKeep is how long samples are kept: the idle window before a test plus
// the longest test.
const tapKeep = idleWindow + 2*time.Minute

// idleWindow is the span before a test whose RTTs are "idle".
const idleWindow = 2 * time.Minute

// minIdle is the samples an idle median needs: with fewer it is missing,
// never a median of a handful.
const minIdle = 20

// NewLoadTap watches p's gateway and ISP edge targets (ICMP).
func NewLoadTap(p *model.Profile, now func() time.Time) *LoadTap {
	if now == nil {
		now = time.Now
	}
	t := &LoadTap{now: now, layers: map[model.SeriesKey]string{}, start: now()}
	if p == nil {
		return t
	}
	for _, tg := range p.Targets {
		switch tg.Layer {
		case model.LayerGateway:
			t.layers[model.SeriesKey{Target: tg.Name, Kind: model.KindICMP}] = LayerGateway
		case model.LayerEdge:
			t.layers[model.SeriesKey{Target: tg.Name, Kind: model.KindICMP}] = LayerEdge
		}
	}
	return t
}

// Observe implements model.Sink. A lost probe is kept as lost; only "not
// measured" samples are left out.
func (t *LoadTap) Observe(s model.Sample) {
	layer, ok := t.layers[s.Key]
	if !ok || (s.Lost && s.Reason == model.ReasonGap) {
		return
	}
	now := t.now()
	at := s.At
	if at.IsZero() {
		at = now
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	cut := 0
	for cut < len(t.buf) && now.Sub(t.buf[cut].at) > tapKeep {
		cut++
	}
	t.buf = append(t.buf[cut:], tapSample{at: at, layer: layer, s: loadSample{ms: float64(s.RTT) / float64(time.Millisecond), lost: s.Lost}})
}

// Stats summarises layer's samples taken in [from, to) (see statsOf),
// leaving out those during a test window or its MaskTail (w, may be nil):
// a test just before is not idle. nil with fewer than 3.
func (t *LoadTap) Stats(layer string, from, to time.Time, w *Windows) *model.LoadStats {
	return statsOf(t.samples(layer, from, to, w))
}

// Idle is layer's idle stats in the idleWindow before start: nil with
// fewer than minIdle samples. The first idleWindow after fyisp started is
// never idle (path discovery and the first traceroutes run then).
func (t *LoadTap) Idle(layer string, start time.Time, w *Windows) *model.LoadStats {
	from := start.Add(-idleWindow)
	if warm := t.start.Add(idleWindow); from.Before(warm) {
		from = warm
	}
	if !from.Before(start) {
		return nil
	}
	v := t.samples(layer, from, start, w)
	if len(v) < minIdle {
		return nil
	}
	return statsOf(v)
}

func (t *LoadTap) samples(layer string, from, to time.Time, w *Windows) []loadSample {
	t.mu.Lock()
	var v []loadSample
	var at []time.Time
	for _, x := range t.buf {
		if x.layer == layer && !x.at.Before(from) && x.at.Before(to) {
			v, at = append(v, x.s), append(at, x.at)
		}
	}
	t.mu.Unlock()
	if w != nil {
		k := 0
		for i := range v {
			if !w.Overlaps(at[i], at[i]) {
				v[k] = v[i]
				k++
			}
		}
		v = v[:k]
	}
	return v
}
