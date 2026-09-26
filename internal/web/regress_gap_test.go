package web

import (
	"context"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// Finding 4: a fresh start at 10:40 viewed over 30 minutes shows no "not
// measured" before 10:40 (real store: Stats.Oldest is hour-rounded).
func TestPanelNoGapBeforeFirstRun(t *testing.T) {
	dir := t.TempDir()
	first := time.Date(2026, 9, 26, 10, 40, 0, 0, time.UTC)
	now := time.Date(2026, 9, 26, 10, 45, 0, 0, time.UTC)
	st, err := store.Open(dir, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := &model.Profile{Groups: []model.Group{{ID: "g", Title: "G"}},
		Targets: []model.Target{{Name: "A", Host: "a.example", Group: "g", Kinds: []model.ProbeKind{model.KindHTTPS}}}}
	k := model.SeriesKey{Target: "A", Kind: model.KindHTTPS}
	for ts := first; ts.Before(now); ts = ts.Add(15 * time.Second) {
		st.Observe(model.Sample{Key: k, Slot: ts, RTT: 10 * time.Millisecond})
	}
	d := &Deps{Profile: func() *model.Profile { return p }, Store: st}
	pp := panelParams{group: "g", from: now.Add(-30 * time.Minute), to: now, points: 1000}
	pd, err := d.queryPanel(context.Background(), pp, now)
	if err != nil {
		t.Fatal(err)
	}
	before := 0
	for i, g := range pd.gap[0] {
		if pd.res.Start.Add(time.Duration(i) * pd.res.Step).Before(first) {
			before += int(g)
		}
	}
	if before > 0 {
		t.Errorf("%d slots before fyisp ever ran are shown as not measured", before)
	}
}

// Finding 4: a target added later than the others is not "not measured"
// before its first sample, while real holes still are.
func TestPanelGapStartsAtSeriesFirstSample(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	f := store.NewFake()
	p := &model.Profile{Groups: []model.Group{{ID: "g", Title: "G"}}, Targets: []model.Target{
		{Name: "Old", Host: "o.example", Group: "g", Kinds: []model.ProbeKind{model.KindHTTPS}},
		{Name: "New", Host: "n.example", Group: "g", Kinds: []model.ProbeKind{model.KindHTTPS}},
	}}
	for ts := now.Add(-40 * time.Minute); ts.Before(now); ts = ts.Add(15 * time.Second) {
		if age := now.Sub(ts); age > 30*time.Minute && age <= 35*time.Minute {
			continue // a real hole for Old
		}
		f.Observe(model.Sample{Key: model.SeriesKey{Target: "Old", Kind: model.KindHTTPS}, Slot: ts, RTT: time.Millisecond})
		if now.Sub(ts) <= 10*time.Minute { // New was added 10 minutes ago
			f.Observe(model.Sample{Key: model.SeriesKey{Target: "New", Kind: model.KindHTTPS}, Slot: ts, RTT: time.Millisecond})
		}
	}
	d := &Deps{Profile: func() *model.Profile { return p }, Store: f}
	pd, err := d.queryPanel(context.Background(), panelParams{group: "g", from: now.Add(-40 * time.Minute), to: now, points: 1000}, now)
	if err != nil {
		t.Fatal(err)
	}
	sum := func(v []uint32) (n int) {
		for _, x := range v {
			n += int(x)
		}
		return
	}
	if g := sum(pd.gap[0]); g < 19 || g > 20 {
		t.Errorf("Old: not measured = %d, want the 5-minute hole (20)", g)
	}
	if g := sum(pd.gap[1]); g != 0 {
		t.Errorf("New: not measured = %d before its first sample, want 0", g)
	}
}
