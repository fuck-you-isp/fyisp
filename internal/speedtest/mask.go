package speedtest

import (
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Self-interference: a speed test fills the link, so every probe that runs
// during it measures the test, not the connection. Windows records when
// tests run; Mask turns the samples of probes that ran during one (or
// within MaskTail after it) into "not measured" before they reach the
// store, the verdict engine, metrics and warm-up. Charts then show a gap
// (shade it with the windows), the verdict and call minutes never count it,
// and no incident can start from it. LoadTap sits before Mask: it keeps the
// raw RTTs for latency under load.

// MaskTail is masked after every test: queues take a moment to drain.
const MaskTail = 2 * time.Second

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

// maskMargin covers, for a sample without At, a probe's timeout after its
// fire time (at most 5s, HTTPS) plus scheduling slack.
const maskMargin = 6 * time.Second

// Mask is a model.Sink that blanks samples of probes that ran during a
// speed test: exactly, from the probe's start (Sample.At) to its arrival,
// against the windows plus MaskTail. Samples without At fall back to their
// slot: a probe with slot s fires within [s, s+Interval) and ends within
// maskMargin.
type Mask struct {
	Inner    model.Sink
	Windows  *Windows
	Interval func(model.SeriesKey) time.Duration
	Now      func() time.Time // default time.Now
}

// Observe implements model.Sink.
func (m Mask) Observe(s model.Sample) {
	if !(s.Lost && s.Reason == model.ReasonGap) {
		var from, to time.Time
		if !s.At.IsZero() {
			from, to = s.At, time.Now()
			if m.Now != nil {
				to = m.Now()
			}
		} else {
			iv := 15 * time.Second
			if m.Interval != nil {
				iv = m.Interval(s.Key)
			}
			from, to = s.Slot, s.Slot.Add(iv+maskMargin)
		}
		if m.Windows.Overlaps(from, to) {
			s.Lost, s.Reason, s.RTT, s.Err, s.Reused = true, model.ReasonGap, 0, "", false
		}
	}
	m.Inner.Observe(s)
}

// Layers of latency under load.
const (
	LayerGateway = "gateway" // the home router (ICMP)
	LayerCall    = "call"    // the call path (core STUN targets, median of a burst)
)

// LoadTap keeps the recent RTTs of the gateway and the call path, by
// arrival time, for idle vs loaded latency. Put it in the fan-out before
// Mask. Safe for concurrent use.
type LoadTap struct {
	now    func() time.Time
	layers map[model.SeriesKey]string

	mu  sync.Mutex
	buf []tapSample
}

type tapSample struct {
	at    time.Time
	layer string
	ms    float64
}

// tapKeep is how long samples are kept: the idle window before a test plus
// the longest test.
const tapKeep = idleWindow + 2*time.Minute

// idleWindow is the span before a test whose RTTs are "idle".
const idleWindow = 2 * time.Minute

// NewLoadTap watches p's gateway (ICMP) and core call-path STUN targets.
func NewLoadTap(p *model.Profile, now func() time.Time) *LoadTap {
	if now == nil {
		now = time.Now
	}
	t := &LoadTap{now: now, layers: map[model.SeriesKey]string{}}
	if p == nil {
		return t
	}
	for _, tg := range p.Targets {
		switch {
		case tg.Layer == model.LayerGateway:
			t.layers[model.SeriesKey{Target: tg.Name, Kind: model.KindICMP}] = LayerGateway
		case callq.IsCorePath(tg):
			t.layers[model.SeriesKey{Target: tg.Name, Kind: model.KindUDP}] = LayerCall
		}
	}
	return t
}

// Observe implements model.Sink.
func (t *LoadTap) Observe(s model.Sample) {
	layer, ok := t.layers[s.Key]
	if !ok || s.Lost {
		return
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	cut := 0
	for cut < len(t.buf) && now.Sub(t.buf[cut].at) > tapKeep {
		cut++
	}
	t.buf = append(t.buf[cut:], tapSample{at: now, layer: layer, ms: float64(s.RTT) / float64(time.Millisecond)})
}

// Median is the median RTT (ms) of layer's samples that arrived in
// [from, to), and false when there are fewer than 3.
func (t *LoadTap) Median(layer string, from, to time.Time) (float64, bool) {
	t.mu.Lock()
	var v []float64
	for _, x := range t.buf {
		if x.layer == layer && !x.at.Before(from) && x.at.Before(to) {
			v = append(v, x.ms)
		}
	}
	t.mu.Unlock()
	if len(v) < 3 {
		return 0, false
	}
	return callq.Median(v), true
}
