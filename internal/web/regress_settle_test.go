package web

import (
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/store"
)

// Finding 5: a slot whose probe may still be running (it fires up to one
// interval after the slot start and may take the probe timeout) is not
// counted as not measured.
func TestNotMeasuredWaitsForProbeTimeout(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 3, 0, time.UTC)
	iv := 15 * time.Second
	// One 30s bucket starting 32s ago; its only slot is 18s ago (fires up
	// to 15s later, may take 5s more), and has not reported yet.
	res := &store.PanelResult{Start: now.Add(-32 * time.Second), Step: 30 * time.Second}
	c := &store.SeriesCols{N: []uint32{0}, Lost: []uint32{0}}
	if g := notMeasured(res, c, iv, res.Start, now, now, 1); g[0] != 0 {
		t.Errorf("slot still within its probe timeout reported as %d not measured", g[0])
	}
	// Later, once it is past interval+timeout, the missing slot counts.
	later := now.Add(10 * time.Second)
	if g := notMeasured(res, c, iv, res.Start, later, later, 1); g[0] != 1 {
		t.Errorf("settled missing slot: not measured = %d, want 1", g[0])
	}
}
