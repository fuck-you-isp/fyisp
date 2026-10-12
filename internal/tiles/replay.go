package tiles

import (
	"cmp"
	"context"
	"math"
	"slices"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// replay walks the stored samples of [t0 − Replay, t0) through the same
// ticks as live (see engine), a day of the store at a time. Samples that
// arrive meanwhile are buffered (Observe) from t0 − bufferFrom on; goLive
// applies them.
func (e *engine) replay(ctx context.Context, t0 time.Time) error {
	from := t0.Add(-e.o.Replay).Truncate(time.Minute)
	e.bufMu.Lock()
	e.bufFrom = t0.Add(-bufferFrom).UnixMilli()
	e.bufMu.Unlock()
	e.mu.Lock()
	e.start(from)
	e.mu.Unlock()
	if e.src.Store == nil || e.o.Replay <= 0 {
		return nil
	}
	var keys []model.SeriesKey
	for k := range e.index {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b model.SeriesKey) int {
		return cmp.Or(cmp.Compare(a.Target, b.Target), cmp.Compare(a.Kind, b.Kind))
	})
	var pts, out []bufSample
	var last model.SeriesKey
	var idx int32
	for at := from; at.Before(t0); at = at.Add(replayChunk) {
		to := at.Add(replayChunk)
		if to.After(t0) {
			to = t0
		}
		pts = pts[:0]
		err := e.src.Store.Raw(ctx, keys, at, to.Add(-time.Millisecond), func(p store.RawPoint) error {
			if p.Lost && p.Reason == model.ReasonGap {
				return nil
			}
			if p.Key != last { // the store returns the keys one after another
				last, idx = p.Key, e.index[p.Key]
			}
			b := bufSample{ser: idx, kind: p.Key.Kind, ms: p.TS.UnixMilli(), lost: p.Lost}
			if !p.Lost {
				b.val = sampleVal(p.Key.Kind, time.Duration(math.Round(p.RTTms*1e6)))
			}
			pts = append(pts, b)
			return nil
		})
		if err != nil {
			return err
		}
		e.mu.Lock()
		out = byTick(pts, out, e.last.Add(tickEvery))
		e.walk(out, to.Add(-tickEvery))
		e.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// sortSamples orders samples by slot, then series, then a burst's KindUDP
// before its jitter and loss.
func sortSamples(pts []bufSample) {
	slices.SortFunc(pts, func(a, b bufSample) int {
		return cmp.Or(cmp.Compare(a.ms, b.ms), cmp.Compare(a.ser, b.ser), cmp.Compare(a.kind, b.kind))
	})
}

// byTick orders samples by the first tick (of the grid from first on) at
// or after their slot, keeping their order within a tick: a counting sort,
// as the tick is all that matters to walk (the trackers order checks by
// slot) besides a burst's KindUDP before its jitter and loss, which the
// store's key order gives.
func byTick(pts, out []bufSample, first time.Time) []bufSample {
	base, step := first.UnixMilli(), tickEvery.Milliseconds()
	tickOf := func(ms int64) int {
		if ms <= base {
			return 0
		}
		return int((ms - base + step - 1) / step)
	}
	n := 0
	for _, p := range pts {
		n = max(n, tickOf(p.ms)+1)
	}
	at := make([]int, n+1)
	for _, p := range pts {
		at[tickOf(p.ms)+1]++
	}
	for i := 1; i <= n; i++ {
		at[i] += at[i-1]
	}
	out = slices.Grow(out[:0], len(pts))[:len(pts)]
	for _, p := range pts {
		k := tickOf(p.ms)
		out[at[k]] = p
		at[k]++
	}
	return out
}

// walk feeds samples (in tick order) and ticks the grid up to until: before each
// tick, the samples whose slot is not after it. The rest are fed at the
// end. The caller holds mu.
func (e *engine) walk(pts []bufSample, until time.Time) {
	i := 0
	for t := e.last.Add(tickEvery); !t.After(until); t = t.Add(tickEvery) {
		lim := t.UnixMilli()
		for ; i < len(pts) && pts[i].ms <= lim; i++ {
			e.feed(pts[i])
		}
		e.tick(t)
	}
	for ; i < len(pts); i++ {
		e.feed(pts[i])
	}
}

// goLive applies the buffered samples (those the replay did not read),
// ticks up to now and makes Observe apply samples directly.
func (e *engine) goLive(ctx context.Context, wall time.Time) {
	now := wall.Truncate(tickEvery)
	in := e.inputs(ctx, now)
	e.mu.Lock()
	e.bufMu.Lock()
	buf := e.buffer
	e.buffer = nil
	sortSamples(buf)
	buf = slices.CompactFunc(buf, func(a, b bufSample) bool { return a.ms == b.ms && a.ser == b.ser && a.kind == b.kind })
	if e.last.IsZero() { // no replay
		e.start(now)
	}
	e.walk(buf, now) // feed() drops what the replay already read
	e.live.Store(true)
	e.bufMu.Unlock()
	e.ready.Store(true)
	s := e.snapshot(now, in, true)
	e.mu.Unlock()
	e.snap.Store(s)
}
