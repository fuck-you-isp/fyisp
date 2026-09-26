package store

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

func TestFakePanelBuckets(t *testing.T) {
	f := NewFake()
	k := model.SeriesKey{Target: "a", Kind: model.KindHTTPS}
	t0 := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	f.Observe(model.Sample{Key: k, Slot: t0, RTT: 10 * time.Millisecond})
	f.Observe(model.Sample{Key: k, Slot: t0.Add(15 * time.Second), RTT: 20 * time.Millisecond})
	f.Observe(model.Sample{Key: k, Slot: t0.Add(30 * time.Second), Lost: true, Reason: model.ReasonDNS})
	f.Observe(model.Sample{Key: k, Slot: t0.Add(45 * time.Second), Lost: true, Reason: model.ReasonGap})
	r, err := f.Panel(context.Background(), PanelQuery{Keys: []model.SeriesKey{k}, From: t0, To: t0.Add(59 * time.Second), MaxPoints: 1})
	if err != nil {
		t.Fatal(err)
	}
	c := r.Series[0]
	if r.Tier != TierRaw || c.N[0] != 2 || c.Lost[0] != 1 || c.LostBy[model.ReasonDNS][0] != 1 {
		t.Fatalf("unexpected result: %+v", r)
	}
	if math.Abs(float64(c.Mean[0])-15) > 1e-3 || c.Min[0] != 10 || c.Max[0] != 20 {
		t.Fatalf("stats: %+v", c)
	}
}

func TestBucketStep(t *testing.T) {
	t0 := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		d    time.Duration
		pts  int
		tier string
		want time.Duration
	}{
		{30 * time.Minute, 1000, TierRaw, 5 * time.Second},
		{48 * time.Hour, 1000, TierRaw, 5 * time.Minute},
		{90 * 24 * time.Hour, 1000, TierHourly, 3 * time.Hour},
		{7 * 24 * time.Hour, 1000, TierHourly, time.Hour},
	}
	for _, c := range cases {
		if got := BucketStep(t0, t0.Add(c.d), c.pts, c.tier); got != c.want {
			t.Errorf("BucketStep(%v, %d, %s) = %v, want %v", c.d, c.pts, c.tier, got, c.want)
		}
	}
}
