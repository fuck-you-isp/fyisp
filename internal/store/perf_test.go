package store

import (
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store/synth"
)

// perfInterval is the spike S3 mix: 60 series at 15s, 20 at 5s, 10 at 1s
// (18 samples/s, as in step 0).
func perfInterval(i int) time.Duration {
	switch {
	case i < 60:
		return 15 * time.Second
	case i < 80:
		return 5 * time.Second
	}
	return time.Second
}

// TestPerf (FYISP_PERF=1) fills 48h of raw data through Observe/Flush for 90
// series, reports bytes/sample, adds 90 days of summary rows, and times
// 90-series panels in both tiers.
func TestPerf(t *testing.T) {
	if os.Getenv("FYISP_PERF") != "1" {
		t.Skip("set FYISP_PERF=1")
	}
	var keys []model.SeriesKey
	ivs := map[model.SeriesKey]time.Duration{}
	for i := range 90 {
		k := model.SeriesKey{Target: fmt.Sprintf("target-%02d", i), Kind: model.ProbeKind(1 + i%3)}
		keys = append(keys, k)
		ivs[k] = perfInterval(i)
	}
	end := t0.Add(90 * 24 * time.Hour)
	rawFrom := end.Add(-48 * time.Hour)
	now := rawFrom
	dir := t.TempDir()
	s := openT(t, dir, Options{Interval: func(k model.SeriesKey) time.Duration { return ivs[k] }, Now: func() time.Time { return now }})
	defer s.Close()

	gens := make([]*synth.Series, 90)
	for i := range gens {
		gens[i] = synth.New(42, i)
	}
	var nsamples int64
	fillStart := time.Now()
	for m := rawFrom; m.Before(end); m = m.Add(time.Minute) {
		for i, k := range keys {
			for tt := m; tt.Before(m.Add(time.Minute)); tt = tt.Add(ivs[k]) {
				s.Observe(sampleOf(k, tt, gens[i].Next()))
				nsamples++
			}
		}
		now = m.Add(time.Minute)
		if err := s.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	fill := time.Since(fillStart)
	if _, err := checkpoint(ctx, s.db.w); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats(ctx)
	var sumBytes int64
	s.db.w.QueryRow(`SELECT sum(pgsize) FROM dbstat WHERE name IN ('summary_1h', 'sqlite_autoindex_summary_1h_1')`).Scan(&sumBytes)
	t.Logf("48h raw: %d samples via Observe + 2880 per-minute Flushes in %v (%.1f µs/flush-minute of 90 series); db+wal %d B = %.3f B/sample (summary_1h %.3f B/sample)",
		nsamples, fill.Round(time.Millisecond), float64(fill.Microseconds())/2880, st.FileBytes, float64(st.FileBytes)/float64(nsamples), float64(sumBytes)/float64(nsamples))

	// 88 more days of hourly summaries (as if pruned raw data were kept only as summaries).
	rnd := rand.New(rand.NewPCG(1, 2))
	tx, _ := s.db.w.Begin()
	ins, _ := tx.Prepare(`INSERT INTO summary_1h VALUES(?,?,?)`)
	for day := t0.Unix() / 86400; day < rawFrom.Unix()/86400; day++ {
		for i, k := range keys {
			var hs []hourSummary
			for h := day * 24; h < day*24+24; h++ {
				n := int64(time.Hour / ivs[k])
				u := summary{lost: int64(rnd.IntN(5))}
				u.by[1] = u.lost
				u.n = n - u.lost
				base := int64(500 + i*100 + rnd.IntN(100))
				u.min, u.max, u.p95, u.sum = base, base+900+int64(rnd.IntN(5000)), base+300, (base+50)*u.n
				hs = append(hs, hourSummary{h, u})
			}
			if _, err := ins.Exec(day, s.ids[k], appendDay(nil, day, hs)); err != nil {
				t.Fatal(err)
			}
		}
	}
	ins.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	checkpoint(ctx, s.db.w)

	// Put a partly observed current hour in memory too.
	for i, k := range keys {
		for tt := end; tt.Before(end.Add(20 * time.Minute)); tt = tt.Add(ivs[k]) {
			s.Observe(sampleOf(k, tt, gens[i].Next()))
		}
	}
	now = end.Add(20 * time.Minute)

	timeIt := func(name string, q PanelQuery, wantTier string) time.Duration {
		var ds []time.Duration
		for range 15 {
			start := time.Now()
			p, err := s.Panel(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			ds = append(ds, time.Since(start))
			if p.Tier != wantTier {
				t.Fatalf("%s: tier %s", name, p.Tier)
			}
		}
		slices.Sort(ds)
		t.Logf("%-28s p50 %7.2f ms  max %7.2f ms", name, float64(ds[len(ds)/2].Microseconds())/1000, float64(ds[len(ds)-1].Microseconds())/1000)
		return ds[len(ds)/2]
	}
	raw48 := timeIt("Panel 90 series x 48h raw", PanelQuery{Keys: keys, From: now.Add(-48 * time.Hour), To: now, MaxPoints: 1000}, TierRaw)
	timeIt("Panel 12 series x 48h raw", PanelQuery{Keys: keys[:12], From: now.Add(-48 * time.Hour), To: now, MaxPoints: 1000}, TierRaw)
	timeIt("Panel 90 series x 1h raw", PanelQuery{Keys: keys, From: now.Add(-time.Hour), To: now, MaxPoints: 1000}, TierRaw)
	h90 := timeIt("Panel 90 series x 90d 1h", PanelQuery{Keys: keys, From: now.Add(-90 * 24 * time.Hour), To: now, MaxPoints: 1000}, TierHourly)
	timeIt("Panel 90 series x 7d 1h", PanelQuery{Keys: keys, From: now.Add(-7 * 24 * time.Hour), To: now, MaxPoints: 1000}, TierHourly)
	var n int
	start := time.Now()
	s.Raw(ctx, keys, now.Add(-48*time.Hour), now, func(RawPoint) error { n++; return nil })
	t.Logf("Raw 90 series x 48h: %d points in %v", n, time.Since(start).Round(time.Millisecond))
	if raw48 > 100*time.Millisecond || h90 > 100*time.Millisecond {
		t.Errorf("over the 100 ms budget")
	}
}
