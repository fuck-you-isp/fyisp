package speedtest

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
)

// The finding: "What we found", one sentence on where a time-of-day
// slowdown is, generated here and reused by Details, Home and the report.
// It compares every destination in the same hours (a cross-section, not
// per-server patterns alone):
//
//   - most regions and ≥ 2 providers dip together: your provider's own
//     network (the ISP edge's idle RTT tells its local network from
//     further in);
//   - one provider dips in ≥ 2 regions, the others don't: the link between
//     your provider and that one;
//   - one region dips, whichever provider: the long-distance route;
//   - one server dips, the rest of its metro doesn't: its own busy time;
//   - nothing stands out, or not enough data yet.
//
// Confidence tiers: "points to" with ≥ 7 days, full coverage and a wired
// connection; "most likely" with 5-6 days or Wi-Fi/unknown; "early sign"
// with 3-4 days. A household caveat is added on Wi-Fi, or when the router's
// own delay under load rises nearly as much as the provider's.

// Finding cases.
const (
	CaseNotEnough = "not_enough_data"
	CaseNothing   = "nothing"
	CaseISP       = "isp"      // your provider's own network
	CaseProvider  = "provider" // the link to one provider
	CaseRoute     = "route"    // the long-distance route to one region
	CaseServer    = "server"   // one server's busy time
)

// Finding tiers.
const (
	TierPointsTo   = "points_to"
	TierMostLikely = "most_likely"
	TierEarly      = "early_sign"
)

// Finding is the localisation of a time-of-day slowdown.
type Finding struct {
	Case       string   `json:"case"`
	Tier       string   `json:"tier,omitempty"`
	Text       string   `json:"text"`        // the owner's wording
	TextPublic string   `json:"text_public"` // the share link's and report's wording
	Evidence   string   `json:"evidence"`
	Caveat     string   `json:"caveat,omitempty"`
	Hours      []int    `json:"hours"`
	Regions    []string `json:"regions"`   // region IDs involved
	Providers  []string `json:"providers"` // provider names involved
	Servers    []string `json:"servers"`   // server IDs involved (case server)
	Drop       float64  `json:"drop"`
	Up         bool     `json:"up"`   // about uploads
	Days       int      `json:"days"` // days with scored scheduled tests
	Tests      int      `json:"tests"`
}

// FindingInput is what the finding is computed from.
type FindingInput struct {
	Tests   []model.SpeedTest // the span's tests (any trigger: only scheduled ones count)
	TZ      time.Duration
	Catalog *Catalog
	// EdgeIdle is the ISP edge's idle RTT median (ms) per local hour (NaN:
	// unknown): the access-network control.
	EdgeIdle [24]float64
}

// MinDays is the days of tests a finding needs.
const MinDays = 3

// FindFinding computes the finding.
func FindFinding(in FindingInput) Finding {
	var tests []model.SpeedTest
	days := map[string]bool{}
	for _, t := range in.Tests {
		if Scheduled(t) && t.Outcome == model.SpeedOK && t.ScoreDown != nil {
			tests = append(tests, t)
			days[t.Start.Add(in.TZ).UTC().Format("2006-01-02")] = true
		}
	}
	f := Finding{Days: len(days), Tests: len(tests), Hours: []int{}, Regions: []string{}, Providers: []string{}, Servers: []string{}}
	if f.Days < MinDays {
		f.Case = CaseNotEnough
		f.Text = fmt.Sprintf("We need about %d days of tests to compare times of day. %d of %d days so far.", MinDays, f.Days, MinDays)
		f.TextPublic, f.Evidence = f.Text, fmt.Sprintf("Based on %s so far.", plural(len(tests), "test", "tests"))
		return f
	}
	f.Evidence = fmt.Sprintf("Based on %s over %s.", plural(len(tests), "test", "tests"), plural(f.Days, "day", "days"))
	for _, up := range []bool{false, true} {
		if f.find(in, tests, up) {
			f.wording(in, tests)
			return f
		}
	}
	f.Case = CaseNothing
	f.Text = "No time of day stands out. Speeds stay within about 10% of normal."
	f.TextPublic = f.Text
	return f
}

// providerOf is a test's server's provider.
func providerOf(c *Catalog, id string) string {
	if c != nil {
		if s, ok := c.Server(id); ok {
			return s.Provider
		}
	}
	return "Your server"
}

// overlaps reports whether a dip shares an hour with hours.
func overlaps(p *Pattern, hours []int) bool {
	return p != nil && slices.ContainsFunc(p.Hours, func(h int) bool { return slices.Contains(hours, h) })
}

// find tries the cases in order on download (or upload) scores.
func (f *Finding) find(in FindingInput, tests []model.SpeedTest, up bool) bool {
	byRegion, byProvider, byServer := map[string][]model.SpeedTest{}, map[string][]model.SpeedTest{}, map[string][]model.SpeedTest{}
	var noFallback []model.SpeedTest
	for _, t := range tests {
		if t.Fallback {
			continue
		}
		noFallback = append(noFallback, t)
		if t.Region != "" {
			byRegion[t.Region] = append(byRegion[t.Region], t)
		}
		byProvider[providerOf(in.Catalog, t.Server)] = append(byProvider[providerOf(in.Catalog, t.Server)], t)
		byServer[t.Server] = append(byServer[t.Server], t)
	}
	dips := func(groups map[string][]model.SpeedTest) (with, all []string, by map[string]*Pattern) {
		by = map[string]*Pattern{}
		for k, ts := range groups {
			if p := FindDip(HourMedians(ts, in.TZ, up), up); p != nil {
				by[k] = p
			}
			if med := HourMedians(ts, in.TZ, up); countValid(med) >= minHours {
				all = append(all, k)
			}
		}
		for k := range by {
			with = append(with, k)
		}
		slices.Sort(with)
		slices.Sort(all)
		return with, all, by
	}
	pooled := FindDip(HourMedians(noFallback, in.TZ, up), up)
	regWith, regAll, regDips := dips(byRegion)
	provWith, _, provDips := dips(byProvider)
	srvWith, _, srvDips := dips(byServer)
	f.Up = up
	set := func(c string, p *Pattern) {
		f.Case, f.Hours, f.Drop = c, p.Hours, p.Drop
	}
	if pooled != nil {
		var regs, provs []string
		for _, r := range regWith {
			if overlaps(regDips[r], pooled.Hours) {
				regs = append(regs, r)
			}
		}
		for _, p := range provWith {
			if overlaps(provDips[p], pooled.Hours) {
				provs = append(provs, p)
			}
		}
		if len(regAll) > 0 && 2*len(regs) >= len(regAll) && len(provs) >= 2 {
			set(CaseISP, pooled)
			f.Regions, f.Providers = regs, provs
			return true
		}
	}
	// One provider in ≥ 2 regions, the others flat at those hours.
	for _, p := range provWith {
		var regs []string
		for r, ts := range byRegion {
			var mine []model.SpeedTest
			for _, t := range ts {
				if providerOf(in.Catalog, t.Server) == p {
					mine = append(mine, t)
				}
			}
			if overlaps(FindDip(HourMedians(mine, in.TZ, up), up), provDips[p].Hours) {
				regs = append(regs, r)
			}
		}
		others := 0
		for _, q := range provWith {
			if q != p && overlaps(provDips[q], provDips[p].Hours) {
				others++
			}
		}
		if len(regs) >= 2 && others == 0 && len(provWith) < len(byProvider) {
			slices.Sort(regs)
			set(CaseProvider, provDips[p])
			f.Providers, f.Regions = []string{p}, regs
			return true
		}
	}
	// One region, the other regions flat.
	if len(regWith) == 1 && len(regAll) >= 2 {
		r := regWith[0]
		set(CaseRoute, regDips[r])
		f.Regions = []string{r}
		for _, t := range byRegion[r] {
			if p := providerOf(in.Catalog, t.Server); !slices.Contains(f.Providers, p) {
				f.Providers = append(f.Providers, p)
			}
		}
		slices.Sort(f.Providers)
		return true
	}
	// One server, the rest of its metro flat.
	if len(srvWith) == 1 {
		id := srvWith[0]
		metro := ""
		if s, ok := in.Catalog.Server(id); ok {
			metro = s.Metro
		}
		flat := 0
		for k := range byServer {
			if s, ok := in.Catalog.Server(k); ok && k != id && s.Metro == metro && countValid(HourMedians(byServer[k], in.TZ, up)) >= minHours {
				flat++
			}
		}
		if flat > 0 || len(byServer) > 1 {
			set(CaseServer, srvDips[id])
			f.Servers, f.Providers = []string{id}, []string{providerOf(in.Catalog, id)}
			return true
		}
	}
	return false
}

func countValid(v [24]float64) int {
	n := 0
	for _, x := range v {
		if !math.IsNaN(x) {
			n++
		}
	}
	return n
}

// wording writes the sentences, the tier, the caveat and the evidence.
func (f *Finding) wording(in FindingInput, tests []model.SpeedTest) {
	when := hoursText(f.Hours)
	pct := int(f.Drop*100 + 0.5)
	slow := "slows"
	what := "Every region"
	if f.Up {
		slow = "uploads slow"
	}
	// Coverage: each dip hour × providers × tests.
	minTests, minProv := math.MaxInt, math.MaxInt
	connWifi, connWired := 0, 0
	var gwRise, edgeRise []float64
	for _, h := range f.Hours {
		n := 0
		provs := map[string]bool{}
		for _, t := range tests {
			if t.Start.Add(in.TZ).UTC().Hour() == h && !t.Fallback {
				n++
				provs[providerOf(in.Catalog, t.Server)] = true
			}
		}
		minTests, minProv = min(minTests, n), min(minProv, len(provs))
	}
	for _, t := range tests {
		x := ParseExtra(t.Extra)
		switch x.Connection {
		case "wifi":
			connWifi++
		case "wired":
			connWired++
		}
		if slices.Contains(f.Hours, t.Start.Add(in.TZ).UTC().Hour()) {
			g, e := x.Latency[LayerGateway], x.Latency[LayerEdge]
			if g.IdleMs != nil && g.Down != nil && e.IdleMs != nil && e.Down != nil {
				gwRise = append(gwRise, g.Down.P50-*g.IdleMs)
				edgeRise = append(edgeRise, e.Down.P50-*e.IdleMs)
			}
		}
	}
	full := minProv >= 2 && minTests >= MinPerHour
	wired := connWired > 0 && connWifi == 0
	switch {
	case f.Days >= 7 && full && wired:
		f.Tier = TierPointsTo
	case f.Days >= 5:
		f.Tier = TierMostLikely
	default:
		f.Tier = TierEarly
	}
	household := connWifi > 0
	if len(gwRise) >= MinPerHour {
		gr, er := callq.Median(gwRise), callq.Median(edgeRise)
		household = household || (er > 5 && gr >= 0.7*er)
	}
	if household {
		f.Caveat = "Wi-Fi, or other people at home using the internet at that time, can cause this too."
	}
	region := func(id string) string {
		for _, r := range in.Catalog.Regions {
			if r.ID == id {
				return r.Title
			}
		}
		return id
	}
	var claim, who string
	switch f.Case {
	case CaseISP:
		with := "with every provider"
		if len(f.Providers) < 3 {
			with = "with " + joinAnd(f.Providers)
		}
		claim = fmt.Sprintf("%s %s from about %s, %s: about %d%% below normal.", what, slow, when, with, pct)
		who = "your internet provider's own network"
		if ctl := edgeControl(in.EdgeIdle, f.Hours); ctl != "" {
			claim += " " + ctl
		}
	case CaseProvider:
		var regs []string
		for _, r := range f.Regions {
			regs = append(regs, region(r))
		}
		claim = fmt.Sprintf("Only %s servers %s from about %s (%s); other providers stay normal.", f.Providers[0], slowVerb(f.Up), when, joinAnd(regs))
		who = "the link between your internet provider and " + f.Providers[0]
	case CaseRoute:
		claim = fmt.Sprintf("Only tests to %s %s, from about %s, whichever provider.", region(f.Regions[0]), slowVerb(f.Up), when)
		who = "the long-distance route to " + region(f.Regions[0])
	case CaseServer:
		label := f.Servers[0]
		if s, ok := in.Catalog.Server(label); ok {
			label = s.Label
		}
		claim = fmt.Sprintf("Only %s %s from about %s.", label, slowVerb1(f.Up), when)
		f.Text = claim + " That's most likely that server's own busy time, not your connection."
		f.TextPublic = claim + " That's most likely that server's own busy time, not the connection."
		f.evidence(minTests, minProv)
		return
	}
	var tail string
	switch f.Tier {
	case TierPointsTo:
		tail = "That points to " + who + "."
	case TierMostLikely:
		tail = "So it's most likely " + who + "."
	default:
		tail = "It may be " + who + ". We'll know more in a few days."
	}
	f.Text = claim + " " + tail
	f.TextPublic = strings.NewReplacer("your internet provider", "the internet provider", "your connection", "the connection", "Your provider's", "The provider's").Replace(f.Text)
	f.evidence(minTests, minProv)
}

func (f *Finding) evidence(minTests, minProv int) {
	part, _ := partOfDay(&Pattern{DipFrom: f.Hours[0], Hours: f.Hours})
	f.Evidence = fmt.Sprintf("Based on %s over %s; each %s hour was tested at least %s across %s.",
		plural(f.Tests, "test", "tests"), plural(f.Days, "day", "days"), strings.ToLower(strings.TrimSuffix(part, "s")),
		plural(minTests, "time", "times"), plural(minProv, "provider", "providers"))
}

func slowVerb(up bool) string {
	if up {
		return "slow on uploads"
	}
	return "slow"
}

func slowVerb1(up bool) string {
	if up {
		return "slows on uploads"
	}
	return "slows"
}

// edgeControl reads the ISP edge's idle RTT in the dip hours against the
// other hours: rising, the slowdown is in the provider's local network.
func edgeControl(edge [24]float64, hours []int) string {
	var in, out []float64
	for h, v := range edge {
		if math.IsNaN(v) {
			continue
		}
		if slices.Contains(hours, h) {
			in = append(in, v)
		} else {
			out = append(out, v)
		}
	}
	if len(in) == 0 || len(out) < 6 {
		return ""
	}
	a, b := callq.Median(in), callq.Median(out)
	if a-b >= max(2, 0.3*b) {
		return fmt.Sprintf("Your provider's first router also answers slower then (%.0f ms instead of %.0f ms), so it's in its local network.", a, b)
	}
	return "Your provider's first router answers as usual then, so it's further into its network or its links out."
}

// hoursText is "8 to 11 pm" for local hours [first, last+1).
func hoursText(hours []int) string {
	if len(hours) == 0 {
		return ""
	}
	from, to := hours[0], (hours[len(hours)-1]+1)%24
	h12 := func(h int) (int, string) {
		switch {
		case h == 0:
			return 12, "am"
		case h < 12:
			return h, "am"
		case h == 12:
			return 12, "pm"
		}
		return h - 12, "pm"
	}
	a, as := h12(from)
	b, bs := h12(to)
	if to == 0 {
		return fmt.Sprintf("%d %s to midnight", a, as)
	}
	if as == bs {
		return fmt.Sprintf("%d to %d %s", a, b, bs)
	}
	return fmt.Sprintf("%d %s to %d %s", a, as, b, bs)
}

func joinAnd(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return thousands(n) + " " + many
}

func thousands(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// HomeSpeed is Home's line about speed: nil when Home says nothing.
type HomeSpeed struct {
	Text   string `json:"text"`
	Kind   string `json:"kind"`   // warmup, pattern or steady
	Impact string `json:"impact"` // ok, or video_risk when the dip hours may be too slow for video calls
}

// Video-call thresholds of the Home line (group HD video, with headroom).
const (
	videoDownBps = 10e6
	videoUpBps   = 5e6
)

// HomeLine is Home's speed line (the answer card shows no problem; Home
// hides the line otherwise). It shows:
//   - while warming up (fewer than MinDays days): that a pattern needs days;
//   - a broad pattern only (case isp, every dip hour tested with ≥ 2
//     providers), with its impact on video calls;
//   - a calm "about the same" when no time stands out, unless fewer than
//     70% of the scheduled tests of the last day succeeded.
//
// Narrow patterns stay in Details. With most tests failing (≥ 50% in 24 h)
// there is no line.
func HomeLine(f Finding, tests []model.SpeedTest, tz time.Duration, now time.Time) *HomeSpeed {
	var n, ok int
	for _, t := range tests {
		if Scheduled(t) && !t.Start.Before(now.Add(-24*time.Hour)) {
			n++
			if t.Outcome == model.SpeedOK {
				ok++
			}
		}
	}
	if n > 0 && 2*ok <= n {
		return nil
	}
	switch f.Case {
	case CaseNotEnough:
		return &HomeSpeed{Text: fmt.Sprintf("Speed: checking at different times of day. We need about %d days to see a pattern.", MinDays), Kind: "warmup", Impact: "ok"}
	case CaseNothing:
		if n == 0 || float64(ok) < 0.7*float64(n) {
			return nil
		}
		return &HomeSpeed{Text: "Speed: about the same at every time of day this week.", Kind: "steady", Impact: "ok"}
	case CaseISP:
	default:
		return nil
	}
	// The dip hours' slow end (p10) decides the impact.
	var down, up []float64
	for _, t := range tests {
		if Scheduled(t) && t.Outcome == model.SpeedOK && !t.Fallback && slices.Contains(f.Hours, t.Start.Add(tz).UTC().Hour()) {
			if t.DownBps != nil {
				down = append(down, *t.DownBps)
			}
			if t.UpBps != nil {
				up = append(up, *t.UpBps)
			}
		}
	}
	part, ref := partOfDay(&Pattern{DipFrom: f.Hours[0], Hours: f.Hours})
	what := part + " are slower"
	if f.Up {
		what = "Uploads are slower in the " + lowerFirst(part)
	}
	text := fmt.Sprintf("%s: about %d%% below your %s speed (around %s).", what, int(f.Drop*100+0.5), ref, hoursText(f.Hours))
	impact := "ok"
	if (len(down) > 0 && callq.Quantile(down, 0.1) < videoDownBps) || (len(up) > 0 && callq.Quantile(up, 0.1) < videoUpBps) {
		impact = "video_risk"
		text += " At those times it may be too slow for video calls."
	} else {
		text += " That's still plenty for video calls."
	}
	return &HomeSpeed{Text: text, Kind: "pattern", Impact: impact}
}
