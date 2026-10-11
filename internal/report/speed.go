package report

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// speedView is the report's speed test section: a summary paragraph and
// one row per region (or server, for tests outside mode regions), times of
// day and days in the owner's zone (Zone, printed with every time).
type speedView struct {
	Summary string
	Pattern string
	Zone    string             // e.g. "EDT (UTC−4)"
	Finding *speedtest.Finding // where a slowdown points (public wording); nil without one
	// SlowestHead and Slowest: the servers slowest in the dip's hours
	// (none: no list).
	SlowestHead string
	Slowest     []slowRow
	// For tier-1 support: the connection the tests ran on, latency under
	// load at the provider's first router, and what to ask the provider
	// ("" when there's nothing to ask).
	Connection, Latency, Ask string
	Rows                     []speedRow
	// Owner and Public count the tests started by hand and from the share
	// link (the rest were scheduled); Origins says so, "" when all were.
	Owner, Public int
	Origins       string
	// TooFast: some rows are too fast to measure from here (the method
	// note says they're left out).
	TooFast bool
}

type slowRow struct{ Text, Aria string }

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
	r.speedLoc = time.UTC
	if r.b.d.Zone != nil {
		if loc := r.b.d.Zone(ctx); loc != nil {
			r.speedLoc = loc
		}
	}
	return nil
}

// hourRange is local hours [from, to) on a 12-hour clock: "8–11 pm",
// "11 pm–1 am".
func hourRange(from, to int) string {
	c := func(h int) (int, string) {
		h = (h%24 + 24) % 24
		ampm := "am"
		if h >= 12 {
			ampm = "pm"
		}
		if h %= 12; h == 0 {
			h = 12
		}
		return h, ampm
	}
	a, pa := c(from)
	b, pb := c(to)
	if pa == pb && from%24 < to%24 {
		return fmt.Sprintf("%d–%d %s", a, b, pb)
	}
	return fmt.Sprintf("%d %s–%d %s", a, pa, b, pb)
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

// speedDisplay names servers as the share link does: "Region · Provider,
// City" for catalog servers, "Your server N" for the owner's own (never a
// host, label or address).
func speedDisplay(cat *speedtest.Catalog, ts []model.SpeedTest) func(string) string {
	var own []string
	for _, t := range ts {
		if _, ok := cat.Server(t.Server); !ok && t.Server != "" && !slices.Contains(own, t.Server) {
			own = append(own, t.Server)
		}
	}
	slices.Sort(own)
	return func(id string) string {
		if s, ok := cat.Server(id); ok {
			d := s.Provider + ", " + s.City
			if rid := cat.RegionOf(id); rid != "" {
				for _, r := range cat.Regions {
					if r.ID == rid {
						d = r.Title + " · " + d
					}
				}
			}
			return d
		}
		if i := slices.Index(own, id); i >= 0 && len(own) > 1 {
			return fmt.Sprintf("Your server %d", i+1)
		}
		return "Your server"
	}
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
	var ok, busy, other, cancelled int
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
		case model.SpeedCancelled:
			cancelled++
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
	if cancelled > 0 {
		v.Summary += fmt.Sprintf(", %d were cancelled", cancelled)
	}
	v.Summary += ". " + speedMethod(r.speed) + " Only scheduled tests count toward the patterns below; Test now and Test all servers do not."
	loc := r.speedLoc
	if loc == nil {
		loc = time.UTC
	}
	v.Zone = speedtest.ZoneLabel(loc, r.to)
	if p := speedtest.PooledPattern(r.speed, loc); p != nil {
		v.Pattern = fmt.Sprintf("Pooled over every server, tests read about %d%% lower from %s %s than the same servers' usual results.",
			int(p.Drop*100+0.5), hourRange(p.DipFrom, p.DipTo), v.Zone)
	} else if ok >= 48 {
		v.Pattern = "Pooled over every server, there is no clear time-of-day pattern."
	}
	v.Connection, v.Latency = speedConnection(r.speed), speedLatency(r.speed)
	if cat != nil {
		f := speedtest.FindFinding(speedtest.FindingInput{Tests: r.speed, Catalog: cat, Loc: loc, From: r.from, To: r.to})
		switch f.Case {
		case speedtest.CaseISP, speedtest.CaseProvider, speedtest.CaseRoute, speedtest.CaseServer, speedtest.CaseMild, speedtest.CaseMixed, speedtest.CaseTooFast:
			f.Text = strings.TrimSuffix(f.TextPublic, ".") + " (times in " + v.Zone + ")."
			f.Caveat = f.CaveatPublic
			if r.o.Redact { // the share link's rules: no start date, no networks by name
				f.Evidence, f.DaysNote, f.Network = f.EvidencePublic, f.DaysNotePublic, ""
			} else if f.Network != "" {
				f.Evidence += " The slow tests left the provider's network through " + f.Network + "."
			}
			for _, n := range []string{f.DaysNote, f.EarlyNote} {
				if n != "" {
					f.Evidence += " " + n
				}
			}
			v.Finding = &f
			v.Ask = speedAsk(f, cat, v.Zone)
			if len(f.Slowest) > 0 {
				v.SlowestHead = "Slowest servers from " + hourRange(f.Hours[0], f.Hours[len(f.Hours)-1]+1) + " " + v.Zone
				display := speedDisplay(cat, r.speed)
				for _, s := range f.Slowest {
					d := display(s.Server)
					v.Slowest = append(v.Slowest, slowRow{
						Text: fmt.Sprintf("%s: %.0f%% of its usual result (median %.0f vs usual %.0f Mbit/s)", d, s.Pct*100, s.MedianBps/1e6, s.UsualBps/1e6),
						Aria: fmt.Sprintf("%s: %.0f%% of its usual result, median %.0f versus usual %.0f megabits per second",
							strings.ReplaceAll(d, " · ", ", "), s.Pct*100, s.MedianBps/1e6, s.UsualBps/1e6)})
				}
			}
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
		st := speedtest.Summarize(ts, loc)
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
			row.Dip = fmt.Sprintf("%s %s, −%d%%", hourRange(st.Dip.DipFrom, st.Dip.DipTo), v.Zone, int(st.Dip.Drop*100+0.5))
			if st.Dip.Drop > worstDrop {
				worst, worstDrop = name, st.Dip.Drop
			}
		}
		if st.TooFast { // the coverage guard: out of the patterns
			row.Dip = "Too fast to measure from here"
			v.TooFast = true
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

// speedMethod states how the tests ran, with the durations actually
// measured: on a fast line the 100 MB cap ends a direction in about a
// second, and the report must not claim more.
func speedMethod(ts []model.SpeedTest) string {
	var down, up []float64
	capped, dirs := 0, 0
	for _, t := range ts {
		if t.Outcome != model.SpeedOK {
			continue
		}
		for _, d := range []struct {
			secs  float64
			short bool
			into  *[]float64
		}{{t.DownSecs, t.DownShort, &down}, {t.UpSecs, t.UpShort, &up}} {
			if d.secs > 0 {
				*d.into = append(*d.into, d.secs)
				dirs++
				if d.short {
					capped++
				}
			}
		}
	}
	s := "Each test is iperf3 over IPv4 with 8 streams, download then upload, after a ramp-up of up to 1 second and at most 100 MB each way"
	dur := func(v []float64) string {
		return fmt.Sprintf("a median of %.1f s (%.1f–%.1f s)", callq.Median(v), slices.Min(v), slices.Max(v))
	}
	if len(down) > 0 && len(up) > 0 {
		s += fmt.Sprintf(": downloads were measured for %s, uploads for %s", dur(down), dur(up))
		if capped > 0 {
			s += fmt.Sprintf("; %d of %d directions stopped at the 100 MB cap, so they show changes over the day better than the line's top speed", capped, dirs)
		}
	}
	return s + "."
}

// speedConnection is the tests' connection type, for "are you wired?".
func speedConnection(ts []model.SpeedTest) string {
	n := map[string]int{}
	bridged, all := 0, 0
	for _, t := range ts {
		if t.Outcome != model.SpeedOK {
			continue
		}
		all++
		n[cmp.Or(t.Conn, model.ConnUnknown)]++
		if speedtest.ParseExtra(t.Extra).Bridged {
			bridged++
		}
	}
	if all == 0 {
		return ""
	}
	var parts []string
	for _, c := range []struct{ k, name string }{{model.ConnWired, "wired"}, {model.ConnWiFi, "on Wi-Fi"}, {model.ConnUnknown, "on a link of unknown type"}} {
		if n[c.k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[c.k], c.name))
		}
	}
	s := "Connection: of " + plural(all, "measured test", "measured tests") + ", " + strings.Join(parts, ", ") + "."
	if bridged > 0 {
		s += fmt.Sprintf(" %d ran behind another router (a container, a VM or a second router), so the provider's first router may have been the home router.", bridged)
	}
	return s
}

// speedLatency is the delay at the provider's first router under load, by
// direction (the medians over the tests), with the probes lost.
func speedLatency(ts []model.SpeedTest) string {
	var idle, d50, d95, u50, u95 []float64
	var dn, dl, un, ul int
	for _, t := range ts {
		if t.Outcome != model.SpeedOK || t.Latency == nil {
			continue
		}
		e := t.Latency.Edge
		if e.Idle != nil {
			idle = append(idle, e.Idle.P50Ms)
		}
		if e.Down != nil {
			d50, d95 = append(d50, e.Down.P50Ms), append(d95, e.Down.P95Ms)
			dn, dl = dn+e.Down.N, dl+e.Down.Lost
		}
		if e.Up != nil {
			u50, u95 = append(u50, e.Up.P50Ms), append(u95, e.Up.P95Ms)
			un, ul = un+e.Up.N, ul+e.Up.Lost
		}
	}
	if len(idle) == 0 || (len(d50) == 0 && len(u50) == 0) {
		return ""
	}
	s := fmt.Sprintf("Delay to the provider's first router (pinged every 150 ms during each test; a lost ping counts as 1 s): %.0f ms idle", callq.Median(idle))
	if len(d50) > 0 {
		s += fmt.Sprintf(", %.0f ms while downloading (95th percentile %.0f ms, %.1f%% lost)", callq.Median(d50), callq.Median(d95), 100*float64(dl)/float64(max(dn, 1)))
	}
	if len(u50) > 0 {
		s += fmt.Sprintf(", %.0f ms while uploading (95th percentile %.0f ms, %.1f%% lost)", callq.Median(u50), callq.Median(u95), 100*float64(ul)/float64(max(un, 1)))
	}
	return s + "."
}

// speedAsk is what to ask the provider for the finding's case.
func speedAsk(f speedtest.Finding, cat *speedtest.Catalog, zone string) string {
	if len(f.Hours) == 0 {
		return ""
	}
	when := "from " + hourRange(f.Hours[0], f.Hours[len(f.Hours)-1]+1) + " " + zone
	switch f.Case {
	case speedtest.CaseISP:
		return "Ask your provider whether the node or segment serving this line is congested " + when + ", and for its utilisation at those hours."
	case speedtest.CaseProvider:
		to := cmp.Or(f.Network, joinWords(f.Providers))
		return "Ask your provider to escalate to their peering team: congestion towards " + to + " " + when + "."
	case speedtest.CaseRoute:
		title := f.Regions[0]
		for _, r := range cat.Regions {
			if r.ID == f.Regions[0] {
				title = r.Title
			}
		}
		return "Ask your provider about congestion on their long-distance route to " + title + " " + when + "."
	}
	return ""
}

func joinWords(xs []string) string { return strings.Join(xs, ", ") }
