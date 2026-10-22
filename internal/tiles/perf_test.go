package tiles

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// TestPerfEngineReplay (FYISP_PERF=1, without -race) times the engine's
// start on the live instance's profile (--profile all,calls): 8 days of
// its tracked series in a SQLite store, read, decoded and replayed, then a
// live tick. Like the live database, the store also holds ~3,700
// untracked series (every target of --profile all, at 15 min here): the
// table is keyed by (hour, series), so they are what a careless Raw
// query would scan. The budget is 2 s here (5 s on a Pi 4).
func TestPerfEngineReplay(t *testing.T) {
	if os.Getenv("FYISP_PERF") != "1" {
		t.Skip("set FYISP_PERF=1")
	}
	ctx := context.Background()
	p := bigProfile(t)
	src := newEngine(p, profile.ResolveTiles(p), Sources{}, Options{})
	ss := eSeries(src)
	end := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	start := end.Add(-8 * 24 * time.Hour)
	now := start
	const nOther, otherEvery = 3700, 15 * time.Minute
	every := func(k model.SeriesKey) time.Duration {
		if i, ok := src.index[k]; ok {
			return src.ss[i].every
		}
		return otherEvery
	}
	others := make([]model.SeriesKey, nOther)
	for i := range others {
		others[i] = model.SeriesKey{Target: fmt.Sprintf("Other-%04d", i), Kind: model.KindTCP}
	}
	st, err := store.Open(t.TempDir(), store.Options{Interval: every, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fill := time.Now()
	var n int
	for h := start; h.Before(end); h = h.Add(time.Hour) {
		var evs []eEvent
		if h.Hour() == 9 { // a daily problem or two
			evs = []eEvent{
				{h.Add(10 * time.Minute), h.Add(10*time.Minute + 40*time.Second), eAll, 0},
				{h.Add(30 * time.Minute), h.Add(45 * time.Minute), eNames("M365-Outlook", "M365-Portal", "M365-Login"), 4},
			}
		}
		for _, a := range eGen(ss, h, h.Add(time.Hour), evs, uint64(h.Unix())) {
			if _, ok := src.index[a.s.Key]; ok {
				st.Observe(a.s)
				n++
			}
		}
		for tt := h; tt.Before(h.Add(time.Hour)); tt = tt.Add(otherEvery) {
			for _, k := range others {
				st.Observe(model.Sample{Key: k, Slot: tt, RTT: 20 * time.Millisecond})
			}
		}
		now = h.Add(time.Hour + 3*time.Minute)
		if err := st.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("filled %d samples of %d tracked series (%d targets) and %d untracked series in %v", n, len(src.ss), len(p.Targets), nOther, time.Since(fill).Round(time.Millisecond))

	var keys []model.SeriesKey
	for k := range src.index {
		keys = append(keys, k)
	}
	a := time.Now()
	var pts int
	if err := st.Raw(ctx, keys, start, end, func(store.RawPoint) error { pts++; return nil }); err != nil {
		t.Fatal(err)
	}
	t.Logf("store.Raw alone: %d points in %v", pts, time.Since(a).Round(time.Millisecond))

	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	if f := os.Getenv("FYISP_PERF_CPU"); f != "" {
		out, err := os.Create(f)
		if err != nil {
			t.Fatal(err)
		}
		_ = pprof.StartCPUProfile(out)
		defer pprof.StopCPUProfile()
	}
	a = time.Now()
	e := newEngine(p, profile.ResolveTiles(p), Sources{Store: st, SpeedWindows: st.SpeedWindows, Incidents: st.Incidents}, Options{})
	if err := e.replay(ctx, end); err != nil {
		t.Fatal(err)
	}
	walk := time.Since(a)
	e.goLive(ctx, end)
	total := time.Since(a)
	runtime.GC()
	runtime.ReadMemStats(&m1)
	t.Logf("REPLAY 8 days end to end: %v (read+decode+walk %v, first snapshot %v); engine heap %.1f MB",
		total.Round(time.Millisecond), walk.Round(time.Millisecond), (total - walk).Round(time.Millisecond), float64(m1.HeapAlloc-min(m0.HeapAlloc, m1.HeapAlloc))/1e6)
	a = time.Now()
	const ticks = 100
	for i := 1; i <= ticks; i++ {
		e.advance(ctx, end.Add(time.Duration(i)*tickEvery))
	}
	pprof.StopCPUProfile()
	t.Logf("live tick with snapshot: %v", (time.Since(a) / ticks).Round(time.Microsecond))
	snap := e.Snapshot()
	t.Logf("snapshot: %d problems, card %s, monitored %d", len(snap.Eps), snap.Conn.Kind, snap.Monitored)
	if total > 2*time.Second {
		t.Errorf("replay %v: over the 2 s budget", total)
	}

	// Observe: ns per sample for the untracked and the tracked targets
	other := untracked(e, p, end)
	bu := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			e.Observe(other[i%len(other)])
		}
	})
	s := e.ss[0] // a 1 s path series, a tick every 5 samples (timed apart)
	var obs time.Duration
	const nObs = 100_000
	slot := e.last
	for i := 0; i < nObs; i += 5 {
		a := time.Now()
		for range 5 {
			slot = slot.Add(s.every)
			e.Observe(model.Sample{Key: s.key, Slot: slot, RTT: time.Millisecond})
		}
		obs += time.Since(a)
		e.mu.Lock()
		e.tickTo(slot.Truncate(tickEvery))
		e.mu.Unlock()
	}
	bt := obs / nObs
	t.Logf("Observe untracked: %d ns/op, %d allocs/op; tracked: %d ns/op", bu.NsPerOp(), bu.AllocsPerOp(), bt.Nanoseconds())
}
