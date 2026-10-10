package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// TestFakeSpeedFinding: the fake's every-region evening dip reads as still
// learning with 2 days, then as the ISP's network at every backfill (it
// once read as one server's busy time at 4 days).
func TestFakeSpeedFinding(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Date(2026, 10, 10, 15, 37, 0, 0, time.UTC)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, days := range []int{2, 3, 4, 5, 6, 7, 10} {
		s := newFakeSpeed(ctx, now, time.Duration(days)*24*time.Hour, log)
		ts, _ := s.SpeedTests(ctx, now.Add(-30*24*time.Hour), now)
		for _, loc := range []*time.Location{time.UTC, time.FixedZone("", 7200)} {
			f := speedtest.FindFinding(speedtest.FindingInput{Tests: ts, Catalog: s.Catalog(), Loc: loc})
			want := speedtest.CaseISP
			if days == 2 {
				want = speedtest.CaseNotEnough
			}
			if f.Case != want || (days == 4 && f.Tier != speedtest.TierEarly) {
				t.Errorf("%d days, %v: %s %s: %s", days, loc, f.Case, f.Tier, f.Text)
			}
		}
	}
}
