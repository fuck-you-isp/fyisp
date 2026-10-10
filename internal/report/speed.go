package report

import (
	"context"
	"fmt"
	"slices"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// speedView is the report's speed test section: one summary paragraph and
// speed by part of day (UTC, like the rest of the report).
type speedView struct {
	Summary string
	Pattern string
	Rows    []speedRow
}

type speedRow struct {
	Part, Hours, Tests, Down, DownP10, Up string
}

func (r *run) collectSpeed(ctx context.Context) error {
	if r.b.d.SpeedTests == nil {
		return nil
	}
	ts, err := r.b.d.SpeedTests(ctx, r.from, r.to)
	if err != nil {
		return fmt.Errorf("report: speed tests: %w", err)
	}
	r.speed = ts
	return nil
}

func mbps(bps float64) string {
	switch {
	case bps >= 100e6:
		return fmt.Sprintf("%.0f Mbit/s", bps/1e6)
	case bps >= 10e6:
		return fmt.Sprintf("%.1f Mbit/s", bps/1e6)
	}
	return fmt.Sprintf("%.2f Mbit/s", bps/1e6)
}

// speedSection summarizes r.speed, nil without tests.
func (r *run) speedSection() *speedView {
	if len(r.speed) == 0 {
		return nil
	}
	var ok, busy, other int
	var down, up []float64
	for _, t := range r.speed {
		switch t.Outcome {
		case model.SpeedOK:
			ok++
		case model.SpeedBusy:
			busy++
		default:
			other++
		}
		if t.Outcome == model.SpeedOK && t.DownBps != nil {
			down = append(down, *t.DownBps)
		}
		if t.Outcome == model.SpeedOK && t.UpBps != nil {
			up = append(up, *t.UpBps)
		}
	}
	v := &speedView{}
	v.Summary = fmt.Sprintf("%s ran in this range: %d measured", plural(len(r.speed), "speed test", "speed tests"), ok)
	if busy > 0 {
		v.Summary += fmt.Sprintf(", %d found the test server busy (not counted as slow)", busy)
	}
	if other > 0 {
		v.Summary += fmt.Sprintf(", %d could not reach it", other)
	}
	v.Summary += "."
	if len(down) > 0 && len(up) > 0 {
		v.Summary += fmt.Sprintf(" Typical download %s, upload %s (medians; iperf3 with 8 streams, 10 seconds each way).",
			mbps(callq.Median(down)), mbps(callq.Median(up)))
	}
	if p := speedtest.FindPattern(speedtest.ByHour(r.speed, 0)); p != nil {
		v.Pattern = p.Text[:len(p.Text)-1] + fmt.Sprintf(" (%02d:00–%02d:00 UTC).", p.DipFrom, p.DipTo)
	} else if len(down) >= 24 {
		v.Pattern = "There is no clear time-of-day pattern."
	}
	parts := []struct{ name, hours string }{{"Night", "00–06"}, {"Morning", "06–12"}, {"Afternoon", "12–18"}, {"Evening", "18–24"}}
	for i, p := range parts {
		var d, u []float64
		n := 0
		for _, t := range r.speed {
			if h := t.Start.UTC().Hour(); t.Outcome != model.SpeedOK || h/6 != i {
				continue
			}
			n++
			if t.DownBps != nil {
				d = append(d, *t.DownBps)
			}
			if t.UpBps != nil {
				u = append(u, *t.UpBps)
			}
		}
		row := speedRow{Part: p.name, Hours: p.hours, Tests: fmt.Sprint(n), Down: "–", DownP10: "–", Up: "–"}
		if len(d) > 0 {
			row.Down, row.DownP10 = mbps(callq.Median(d)), mbps(callq.Quantile(d, 0.1))
		}
		if len(u) > 0 {
			row.Up = mbps(callq.Median(slices.Clone(u)))
		}
		v.Rows = append(v.Rows, row)
	}
	return v
}
