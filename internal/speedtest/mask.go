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
// tests run; Mask turns the samples of probes that overlapped one into
// "not measured" before they reach the store, the verdict engine, metrics
// and warm-up. Charts then show a gap (shade it with the windows), the
// verdict and call minutes never count it, and no incident can start from
// it. LoadTap sits before Mask: it keeps the raw RTTs for latency under
// load.

// Window is one test's span.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Windows is the recent test windows plus the running one. Safe for
// concurrent use.
type Windows struct {
	mu     sync.Mutex
	list   []Window // ended, oldest first, the last keepWindows
	active time.Time
}

// keepWindows bounds the windows kept for masking: samples arrive within
// seconds, so the last few are enough.
const keepWindows = 32

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
		if len(w.list) > keepWindows {
			w.list = slices.Delete(w.list, 0, len(w.list)-keepWindows)
		}
	}
	return win
}

// Add records a past window (e.g. loaded from the store at start).
func (w *Windows) Add(win Window) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.list = append(w.list, win)
	sort.Slice(w.list, func(i, j int) bool { return w.list[i].Start.Before(w.list[j].Start) })
	if len(w.list) > keepWindows {
		w.list = slices.Delete(w.list, 0, len(w.list)-keepWindows)
	}
}

// Overlaps reports whether [from, to] overlaps a window (a running test's
// window is open-ended).
func (w *Windows) Overlaps(from, to time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active.IsZero() && !to.Before(w.active) {
		return true
	}
	for i := len(w.list) - 1; i >= 0; i-- {
		x := w.list[i]
		if x.End.Before(from) {
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

// maskMargin covers a probe's timeout after its fire time (at most 5s,
// HTTPS) plus scheduling slack.
const maskMargin = 6 * time.Second

// Mask is a model.Sink that blanks samples of probes that may have run
// during a speed test. Interval returns a series' slot interval: a probe
// with slot s fires within [s, s+interval) and ends within maskMargin.
type Mask struct {
	Inner    model.Sink
	Windows  *Windows
	Interval func(model.SeriesKey) time.Duration
}

// Observe implements model.Sink.
func (m Mask) Observe(s model.Sample) {
	if !(s.Lost && s.Reason == model.ReasonGap) {
		iv := 15 * time.Second
		if m.Interval != nil {
			iv = m.Interval(s.Key)
		}
		if m.Windows.Overlaps(s.Slot, s.Slot.Add(iv+maskMargin)) {
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
