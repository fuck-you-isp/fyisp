package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

func TestSpeedTests(t *testing.T) {
	now := t0.Add(100 * 24 * time.Hour)
	s := openT(t, t.TempDir(), Options{Now: func() time.Time { return now }})
	defer s.Close()
	f, r, ms := 4.12e8, int64(112), 1.5
	full := model.SpeedTest{Start: now.Add(-time.Hour), End: now.Add(-time.Hour + 33*time.Second), Trigger: "schedule", Outcome: model.SpeedOK,
		Region: "uk", Server: "Clouvider-London", Port: 5203, Attempts: 2, DownBps: &f, UpBps: &f, DownRetrans: &r, IdleGatewayMs: &ms, LoadedGatewayMs: &ms, IdleCallMs: &ms, LoadedCallMs: &ms}
	busy := model.SpeedTest{Start: now.Add(-30 * time.Minute), End: now.Add(-30*time.Minute + 6*time.Second), Trigger: "manual", Outcome: model.SpeedBusy,
		Region: "eu-central", Server: "Eranium-Amsterdam", Fallback: true, Port: 5207, Attempts: 3}
	old := model.SpeedTest{Start: now.Add(-95 * 24 * time.Hour), End: now.Add(-95 * 24 * time.Hour), Trigger: "schedule", Outcome: model.SpeedUnreachable}
	for _, x := range []*model.SpeedTest{&old, &full, &busy} {
		if err := s.SaveSpeedTest(ctx, x); err != nil || x.ID == 0 {
			t.Fatal(err)
		}
	}
	got, err := s.SpeedTests(ctx, now.Add(-2*time.Hour), now)
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	for i, want := range []model.SpeedTest{full, busy} {
		want.Start, want.End = want.Start.Truncate(time.Millisecond), want.End.Truncate(time.Millisecond)
		if !reflect.DeepEqual(got[i], want) {
			t.Errorf("test %d:\n got %+v\nwant %+v", i, got[i], want)
		}
	}
	if got[1].DownBps != nil || got[1].UpRetrans != nil {
		t.Error("nulls not kept")
	}
	// Settings.
	if _, ok, err := s.Setting(ctx, "speedtest_enabled"); ok || err != nil {
		t.Fatal(ok, err)
	}
	s.SetSetting(ctx, "speedtest_enabled", "on")
	s.SetSetting(ctx, "speedtest_enabled", "off")
	if v, ok, err := s.Setting(ctx, "speedtest_enabled"); v != "off" || !ok || err != nil {
		t.Errorf("%q %v %v", v, ok, err)
	}
	// Retention drops old tests with the hours (needs some samples).
	k := model.SeriesKey{Target: "x", Kind: model.KindTCP}
	for ts := now.Add(-96 * 24 * time.Hour); ts.Before(now); ts = ts.Add(time.Hour) {
		s.Observe(model.Sample{Key: k, Slot: ts, RTT: time.Millisecond})
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(ctx, now.Add(-90*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.SpeedTests(ctx, time.Time{}, now); len(all) != 2 {
		t.Errorf("after prune: %d tests", len(all))
	}
}
