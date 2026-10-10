package report

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// speedView is the report's speed test section: a summary paragraph and
// one row per region (or server, for tests outside mode regions), hours in
// UTC like the rest of the report.
type speedView struct {
	Summary string
	Pattern string
	Finding *speedtest.Finding // where a slowdown points (public wording, hours in UTC); nil without one
	Rows    []speedRow
	// Owner and Public count the tests started by hand and from the share
	// link (the rest were scheduled); Origins says so, "" when all were.
	Owner, Public int
	Origins       string
}

type speedRow struct {
	Name, Server, Tests, Down, DownP10, Up, Dip, Trouble string
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

// speedNames gives region titles and public server labels (own servers
// are "Your server": a report never shows a host).
func speedNames(cat *speedtest.Catalog) (regions map[string]string, label func(string) string) {
	regions = map[string]string{}
	if cat == nil {
		return regions, func(string) string { return "Your server" }
	}
	for _, x := range cat.Regions {
		regions[x.ID] = x.Title
	}
	return regions, func(id string) string {
		if s, ok := cat.Server(id); ok {
			return s.Label
		}
		return "Your server"
	}
}

// speedSection summarizes r.speed, nil without tests.
func (r *run) speedSection() *speedView {
	if len(r.speed) == 0 {
		return nil
	}
	cat, _ := speedtest.Builtin()
	titles, label := speedNames(cat)
	var ok, busy, other int
	groups := map[string][]model.SpeedTest{}
	var order []string
	v := &speedView{}
	for _, t := range r.speed {
		switch t.Origin() {
		case "owner":
			v.Owner++
		case "public":
			v.Public++
		}
		switch t.Outcome {
		case model.SpeedOK:
			ok++
		case model.SpeedBusy:
			busy++
		default:
			other++
		}
		k := speedtest.GroupKey(t)
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], t)
	}
	v.Summary = fmt.Sprintf("%s ran in this range: %d measured", plural(len(r.speed), "speed test", "speed tests"), ok)
	if busy > 0 {
		v.Summary += fmt.Sprintf(", %d found the test servers busy (not counted as slow)", busy)
	}
	if other > 0 {
		v.Summary += fmt.Sprintf(", %d could not reach them", other)
	}
	v.Summary += ". Each test is iperf3 over IPv4 with 8 streams, 3 to 8 seconds each way after a ramp-up of up to 1 second, at most 200 MB." +
		" Only scheduled tests count toward the patterns below; Test now and Test all servers do not."
	if p := speedtest.PooledPattern(r.speed, 0); p != nil {
		v.Pattern = strings.TrimSuffix(p.Text, ".") + fmt.Sprintf(" (%02d:00–%02d:00 UTC, every region alike).", p.DipFrom, p.DipTo)
	} else if ok >= 48 {
		v.Pattern = "There is no clear time-of-day pattern."
	}
	if cat != nil {
		f := speedtest.FindFinding(speedtest.FindingInput{Tests: r.speed, Catalog: cat})
		switch f.Case {
		case speedtest.CaseISP, speedtest.CaseProvider, speedtest.CaseRoute, speedtest.CaseServer:
			f.Text = strings.TrimSuffix(f.TextPublic, ".") + " (hours in UTC)."
			v.Finding = &f
		}
	}
	// Regions first, then the other servers.
	slices.SortStableFunc(order, func(a, b string) int {
		_, ra := titles[a]
		_, rb := titles[b]
		switch {
		case ra && !rb:
			return -1
		case rb && !ra:
			return 1
		}
		return strings.Compare(a, b)
	})
	var worst string
	var worstDrop float64
	for _, k := range order {
		ts := groups[k]
		st := speedtest.Summarize(ts, 0)
		name, isRegion := titles[k]
		if !isRegion {
			name = label(k)
		}
		row := speedRow{Name: name, Tests: fmt.Sprint(len(ts)), Down: "–", DownP10: "–", Up: "–", Dip: "–"}
		used := map[string]int{}
		for _, t := range ts {
			if t.Outcome == model.SpeedOK {
				used[t.Server]++
			}
		}
		if len(used) > 0 {
			best := ""
			for s, n := range used {
				if best == "" || n > used[best] || (n == used[best] && s < best) {
					best = s
				}
			}
			row.Server = label(best)
			if n := len(used) - 1; n > 0 {
				row.Server += fmt.Sprintf(" (%s)", plural(n, "fallback server", "fallback servers"))
			}
		}
		if st.DownMedian != nil {
			row.Down, row.DownP10 = mbps(*st.DownMedian), mbps(*st.DownP10)
		}
		if st.UpMedian != nil {
			row.Up = mbps(*st.UpMedian)
		}
		if st.Dip != nil {
			row.Dip = fmt.Sprintf("%02d–%02d UTC, −%d%%", st.Dip.DipFrom, st.Dip.DipTo, int(st.Dip.Drop*100+0.5))
			if st.Dip.Drop > worstDrop {
				worst, worstDrop = name, st.Dip.Drop
			}
		}
		var trouble []string
		if st.Busy > 0 {
			trouble = append(trouble, fmt.Sprintf("%d busy", st.Busy))
		}
		if n := st.Unreachable + st.Failed; n > 0 {
			trouble = append(trouble, fmt.Sprintf("%d failed", n))
		}
		row.Trouble = strings.Join(trouble, ", ")
		v.Rows = append(v.Rows, row)
	}
	var asked []string
	if v.Owner > 0 {
		asked = append(asked, fmt.Sprintf("%d started by hand", v.Owner))
	}
	if v.Public > 0 {
		asked = append(asked, fmt.Sprintf("%d started from the share link", v.Public))
	}
	if len(asked) > 0 {
		v.Origins = "Of these, " + strings.Join(asked, " and ") + "; the rest were scheduled. Tests someone started are left out of the time-of-day patterns, but their probe gaps count like any other."
	}
	if worst != "" {
		v.Pattern = strings.TrimSpace(v.Pattern + fmt.Sprintf(" The largest dip: %s, %d%% slower at its worst hours.", worst, int(worstDrop*100+0.5)))
	}
	return v
}
