package main

import (
	"context"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// TestFakeTraceLossLabels: the fake's hops carry the store's loss
// classification. Steady loss below store.HopLossMin gets no label but hop
// 5's rate-limiting; above it, real loss starts at hop 3.
func TestFakeTraceLossLabels(t *testing.T) {
	now := time.Date(2026, 10, 10, 15, 37, 0, 0, time.UTC)
	f := newFakeTrace(store.NewFake(), now, 6*time.Hour)
	for target, realFrom := range map[string]int{"CDN-Cloudflare": 0, "Google DNS 8.8.8.8": 3} {
		hs, err := f.Hops(context.Background(), target, model.TraceICMP, now.Add(-6*time.Hour), now)
		if err != nil || len(hs) != 12 {
			t.Fatalf("%s: %d hops, %v", target, len(hs), err)
		}
		for _, h := range hs {
			real := realFrom > 0 && h.Hop >= realFrom && h.Hop != 5
			if h.LossContinues != real || h.RateLimited != (h.Hop == 5) {
				t.Errorf("%s hop %d (loss %.3f): continues %v limited %v, want %v %v",
					target, h.Hop, h.Loss, h.LossContinues, h.RateLimited, real, h.Hop == 5)
			}
		}
	}
}
