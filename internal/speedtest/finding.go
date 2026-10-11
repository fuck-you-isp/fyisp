package speedtest

import (
	"cmp"
	"fmt"
	"maps"
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
// connection; "most likely" with ≥ 5 days and full coverage (every dip
// hour tested ≥ 3 times across ≥ 2 providers); "early sign" otherwise
// (3-4 days, or partial coverage). A household caveat is added on Wi-Fi, or when the router's
// own delay under load rises nearly as much as the provider's.

// Finding cases.
const (
	CaseNotEnough = "not_enough_data"
	CaseNothing   = "nothing"
	CaseISP       = "isp"      // your provider's own network
	CaseProvider  = "provider" // the link to one provider
	CaseRoute     = "route"    // the long-distance route to one region
	CaseServer    = "server"   // one server's busy time
	CaseMild      = "mild"     // a 10-25% dip: too small to point anywhere
	CaseMixed     = "mixed"    // some rows dip, in no pattern that points anywhere
	// CaseTooFast: every server is too fast to measure from here (the
	// coverage guard): the tests can't compare times of day.
	CaseTooFast = "too_fast"
)

// Finding tiers.
const (
	TierPointsTo   = "points_to"
	TierMostLikely = "most_likely"
	TierEarly      = "early_sign"
)

// Finding is the localisation of a time-of-day slowdown.
type Finding struct {
	Case       string `json:"case"`
	Tier       string `json:"tier,omitempty"`
	Text       string `json:"text"`        // the owner's wording
	TextPublic string `json:"text_public"` // the share link's and report's wording
	Evidence   string `json:"evidence"`
	Caveat     string `json:"caveat,omitempty"`
	// CaveatPublic is the caveat in the share link's and report's voice.
	CaveatPublic string   `json:"caveat_public,omitempty"`
	Hours        []int    `json:"hours"`
	Regions      []string `json:"regions"`   // region IDs involved
	Providers    []string `json:"providers"` // provider names involved
	Servers      []string `json:"servers"`   // server IDs involved (case server)
	// Network is the first network past the ISP that the slow tests share
	// (case provider: e.g. "LEVEL3 (AS3356)"), from the tests' handoff
	// traceroutes; "" when unknown or not shared. Via is the internet
	// exchange they meet at, when the traceroutes crossed one. Local
	// wording only.
	Network string  `json:"network,omitempty"`
	Via     string  `json:"via,omitempty"`
	Drop    float64 `json:"drop"`
	Up      bool    `json:"up"` // about uploads
	// Days is the local dates (in FindingInput.Loc) with a scored
	// scheduled test, First and Last the first and last such test.
	Days  int       `json:"days"`
	Tests int       `json:"tests"`
	First time.Time `json:"-"`
	Last  time.Time `json:"-"`
	// EvidencePublic is Evidence without dates (the share link and report
	// keep the owner's start date out). Coverage says why fewer than 7
	// days are covered (owner voice; CoveragePublic without the date).
	EvidencePublic string `json:"evidence_public,omitempty"`
	Coverage       string `json:"coverage,omitempty"`
	CoveragePublic string `json:"coverage_public,omitempty"`
	// Slowest is the servers slowest in the dip's hours (see slowest);
	// the caller sets their Display.
	Slowest []SlowServer `json:"slowest,omitempty"`
	// Case isp: how many regions and providers could tell (Regions and
	// Providers dipped), and the far regions left out (limited by
	// distance, they can't show the line's speed).
	RegionsJudged   int      `json:"regions_judged,omitempty"`
	ProvidersJudged int      `json:"providers_judged,omitempty"`
	FarRegions      []string `json:"far_regions,omitempty"`
	// SlowDays of DayCount days were slower at the dip's hours.
	SlowDays int `json:"slow_days,omitempty"`
	DayCount int `json:"day_count,omitempty"`
	// FarEvening (route and provider cases): the dip hours are evening at
	// the far end too, so its own busy time explains them as well.
	FarEvening bool `json:"far_evening,omitempty"`
	// Bridged: most tests ran behind another router (a container's bridge
	// network, a VM): the "ISP edge" may be the home router.
	Bridged bool `json:"bridged,omitempty"`

	group []model.SpeedTest // the case's own tests (the days count)
	span  int               // the days the tests span, in whole days: the tiers' count
	loc   *time.Location    // FindingInput.Loc
}

// FindingInput is what the finding is computed from.
type FindingInput struct {
	Tests   []model.SpeedTest // the span's tests (any trigger: only scheduled ones count)
	Loc     *time.Location    // local hours (nil: UTC); DST-correct with an IANA zone
	Catalog *Catalog
	// EdgeIdle is the ISP edge's idle RTT median (ms) per local hour (NaN:
	// unknown): the access-network control.
	EdgeIdle [24]float64
	// From and To are the window (zero From: no Coverage). When the tests
	// cover fewer of its days, Coverage says why.
	From, To time.Time
}

// MinDays is the days of tests a finding needs.
const MinDays = 3

// FindFinding computes the finding.
func FindFinding(in FindingInput) (f Finding) {
	fast := tooFast(in.Tests, in.Loc)
	servers, rawDays, rawN := map[string]bool{}, map[string]bool{}, 0
	for i := range in.Tests {
		if t := &in.Tests[i]; patternRow(t, false) && t.Outcome == model.SpeedOK {
			servers[t.Server] = true
			rawDays[t.Start.In(zone(in.Loc)).Format("2006-01-02")] = true
			rawN++
		}
	}
	out := 0
	for sv := range servers {
		if fast[0][sv] {
			out++
		}
	}
	if out > 0 && out == len(servers) && len(rawDays) >= MinDays {
		f = Finding{Case: CaseTooFast, Days: len(rawDays), Tests: rawN, Hours: []int{}, Regions: []string{}, Providers: []string{}, Servers: []string{},
			Text:       "Your line is too fast for these tests to compare times of day: too many tests reach their 100 MB limit while still speeding up.",
			TextPublic: "The line is too fast for these tests to compare times of day: too many tests reach their 100 MB limit while still speeding up."}
		f.Evidence = fmt.Sprintf("Based on %s on %s.", plural(rawN, "test", "tests"), plural(len(rawDays), "day", "days"))
		f.EvidencePublic = f.Evidence
		return f
	}
	defer func() {
		if out > 0 && f.Case != CaseNotEnough {
			left := fmt.Sprintf(" %d of %d servers were too fast to measure from here and are left out.", out, len(servers))
			f.Evidence += left
			f.EvidencePublic += left
		}
	}()
	in.Tests = Censored(in.Tests, in.Loc) // capped directions as patterns use them
	var tests []model.SpeedTest
	days := map[string]bool{}
	for _, t := range in.Tests {
		if !t.OnDemand() && t.Outcome == model.SpeedOK && t.ScoreDown != nil {
			tests = append(tests, t)
			days[t.Start.In(zone(in.Loc)).Format("2006-01-02")] = true
		}
	}
	f = Finding{Days: len(days), Tests: len(tests), Hours: []int{}, Regions: []string{}, Providers: []string{}, Servers: []string{}}
	if f.Days < MinDays {
		f.Case = CaseNotEnough
		f.Text = fmt.Sprintf("We need about %d days of tests to compare times of day. %d of %d days so far.", MinDays, f.Days, MinDays)
		f.TextPublic = fmt.Sprintf("About %d days of tests are needed to compare times of day; %d of %d so far.", MinDays, f.Days, MinDays)
		f.Evidence = fmt.Sprintf("Based on %s so far.", plural(len(tests), "test", "tests"))
		f.EvidencePublic = f.Evidence
		return f
	}
	// From here Days is the span the tests cover, in whole days: 4 days of
	// tests touch 5 dates, and the tiers must not count the partial ones.
	first, last := tests[0].Start, tests[0].Start
	for _, t := range tests {
		if t.Start.Before(first) {
			first = t.Start
		}
		if t.Start.After(last) {
			last = t.Start
		}
	}
	f.span = min(f.Days, max(1, int(math.Ceil(last.Sub(first).Hours()/24))))
	f.First, f.Last, f.loc = first, last, in.Loc
	f.coverage(in, tests)
	f.Evidence = fmt.Sprintf("Based on %s on %s between %s and %s.", plural(len(tests), "test", "tests"), plural(f.Days, "day", "days"),
		dayName(first, in.Loc), dayName(last, in.Loc))
	f.EvidencePublic = fmt.Sprintf("Based on %s on %s.", plural(len(tests), "test", "tests"), plural(f.Days, "day", "days"))
	if f.thin(in, tests) {
		return f
	}
	for _, up := range []bool{false, true} {
		if f.find(in, tests, up) {
			f.wording(in, tests)
			f.slowest(in, tests)
			return f
		}
	}
	f.quiet(in, tests)
	f.slowest(in, tests)
	return f
}

// SlowServer is one of the servers slowest in the dip's hours.
type SlowServer struct {
	Server    string  `json:"-"`       // its ID (local)
	Display   string  `json:"display"` // its public-safe name, set by the caller
	Pct       int     `json:"pct"`     // its dip hours' median, % of its usual
	MedianBps float64 `json:"median_bps"`
	UsualBps  float64 `json:"usual_bps"` // its median outside the dip hours
}

// MaxSlowest is the most servers Finding.Slowest lists.
const MaxSlowest = 3

// slowest lists up to MaxSlowest servers with at least MinPerHour
// non-fallback scheduled results in the dip's hours (and outside them)
// that read below their usual: in the isp and mild cases the fastest
// (nearest) first, else the slowest first. Lower bounds are left out.
func (f *Finding) slowest(in FindingInput, tests []model.SpeedTest) {
	switch f.Case {
	case CaseISP, CaseMild, CaseRoute, CaseProvider, CaseServer:
	default:
		return
	}
	if len(f.Hours) == 0 {
		return
	}
	in1, out := map[string][]float64{}, map[string][]float64{}
	for _, t := range tests {
		v := t.DownBps
		if f.Up {
			v = t.UpBps
		}
		if v == nil || t.Fallback || t.OnDemand() {
			continue
		}
		if lbDown, lbUp := LowerBounds(&t); (!f.Up && lbDown) || (f.Up && lbUp) {
			continue
		}
		if slices.Contains(f.Hours, localHour(t.Start, in.Loc)) {
			in1[t.Server] = append(in1[t.Server], *v)
		} else {
			out[t.Server] = append(out[t.Server], *v)
		}
	}
	var list []SlowServer
	for sv, v := range in1 {
		u := out[sv]
		if len(v) < MinPerHour || len(u) < MinPerHour {
			continue
		}
		m, usual := callq.Median(v), callq.Median(u)
		if usual <= 0 || m >= usual {
			continue
		}
		list = append(list, SlowServer{Server: sv, Pct: int(m/usual*100 + 0.5), MedianBps: math.Round(m), UsualBps: math.Round(usual)})
	}
	slices.SortFunc(list, func(a, b SlowServer) int {
		if a.Pct != b.Pct {
			return a.Pct - b.Pct
		}
		return strings.Compare(a.Server, b.Server)
	})
	list = list[:min(len(list), MaxSlowest)]
	if f.Case == CaseISP || f.Case == CaseMild {
		slices.SortStableFunc(list, func(a, b SlowServer) int { return cmp.Compare(b.UsualBps, a.UsualBps) })
	}
	f.Slowest = list
}

// quiet fills the finding when no case matched, true to what the rule can
// tell: a 10-25% dip (below the 25% the cases need) is "a little slower";
// rows dipping that fit no case are "some regions slow at times"; else no
// slowdown of 25% or more, with the actual worst hour's distance.
func (f *Finding) quiet(in FindingInput, tests []model.SpeedTest) {
	var noFallback []model.SpeedTest
	byRow := map[string][]model.SpeedTest{}
	for _, t := range tests {
		if !t.Fallback {
			noFallback = append(noFallback, t)
			byRow[GroupKey(t)] = append(byRow[GroupKey(t)], t)
		}
	}
	defer func() {
		f.TextPublic = strings.NewReplacer("your daytime", "the daytime", "your usual", "the usual").Replace(f.Text)
	}()
	pooled := hourMedians(noFallback, in.Loc, false)
	var rows []string
	for _, k := range slices.Sorted(maps.Keys(byRow)) {
		if p := FindDip(hourMedians(byRow[k], in.Loc, false), false); p != nil {
			name := k
			if r, ok := regionByID(in.Catalog, k); ok {
				name = r.Title
			} else if sv, ok := in.Catalog.Server(k); ok {
				name = sv.Label
			}
			rows = append(rows, fmt.Sprintf("%s %s", name, hoursText(p.Hours)))
		}
	}
	if len(rows) > 0 {
		f.Case = CaseMixed
		if len(rows) > 3 {
			rows = append(rows[:3], fmt.Sprintf("%d more", len(rows)-3))
		}
		f.Text = fmt.Sprintf("Some regions slow at times (%s), but not in a pattern that points to one place.", joinAnd(rows))
		return
	}
	if p := findDip(pooled, false, mildRatio); p != nil {
		// The drop to 5% (findDip rounds to 10%: too coarse here).
		var in, out []float64
		for h, v := range pooled {
			if !math.IsNaN(v) {
				if slices.Contains(p.Hours, h) {
					in = append(in, v)
				} else {
					out = append(out, v)
				}
			}
		}
		p.Drop = math.Round((1-callq.Median(in)/callq.Median(out))*20) / 20
		f.Case, f.Hours, f.Drop = CaseMild, p.Hours, p.Drop
		part, _ := partOfDay(p)
		f.Text = fmt.Sprintf("%s are a little slower: about %d%% below normal (around %s). That's within what speed tests vary by, so it doesn't point anywhere.",
			part, int(math.Round(p.Drop*100)), hoursText(p.Hours))
		return
	}
	f.Case = CaseNothing
	worst := 0.0
	var valid []float64
	for _, v := range pooled {
		if !math.IsNaN(v) {
			valid = append(valid, v)
		}
	}
	if len(valid) > 0 {
		worst = 1 - slices.Min(valid)/callq.Median(valid)
	}
	within := max(10, int(math.Ceil(worst*20))*5) // to 5%
	f.Text = fmt.Sprintf("No slowdown of 25%% or more stands out: every hour stays within about %d%% of normal.", min(within, 25))
}

// thin reports coverage too thin to judge, and fills the "still learning"
// finding: fewer than minHours hours of day have MinPerHour scored tests
// pooled, or fewer than half the rows (regions, or servers outside
// regions) have minHours such hours on their own. Saying "no time of day
// stands out" then would be wrong. A dip already visible in the pooled
// tests is named, as an early sign without a "where".
func (f *Finding) thin(in FindingInput, tests []model.SpeedTest) bool {
	var noFallback []model.SpeedTest
	groups := map[string][]model.SpeedTest{}
	for _, t := range tests {
		if !t.Fallback {
			noFallback = append(noFallback, t)
			groups[GroupKey(t)] = append(groups[GroupKey(t)], t)
		}
	}
	pooled := hourMedians(noFallback, in.Loc, false)
	covered, rows := countValid(pooled), 0
	for _, ts := range groups {
		if countValid(hourMedians(ts, in.Loc, false)) >= minHours {
			rows++
		}
	}
	if covered >= minHours && 2*rows >= len(groups) && rows > 0 {
		return false
	}
	// About how many more days: each row gets this many tests per hour of
	// day per day so far.
	perDay := float64(len(noFallback)) / float64(max(len(groups), 1)*24*f.span)
	more := 1
	if perDay > 0 {
		more = max(1, int(math.Ceil(float64(MinPerHour)/perDay))-f.span)
	}
	f.Case = CaseNotEnough
	wait := "about " + plural(more, "more day", "more days")
	if p := FindDip(pooled, false); p != nil {
		f.Tier, f.Hours, f.Drop = TierEarly, p.Hours, p.Drop
		f.Text = fmt.Sprintf("%s so far (around %s). Not every region has enough tests at each hour yet to say where: %s.",
			strings.TrimSuffix(p.Text, "."), hoursText(p.Hours), wait)
	} else {
		f.Text = fmt.Sprintf("Still learning: %d of 24 hours of the day have enough tests so far. We need %s to compare times of day.", covered, wait)
	}
	f.TextPublic = strings.NewReplacer("your daytime", "the daytime", "your usual", "the usual", "We need "+wait+" to compare times of day.", "More tests are needed: "+wait+".").Replace(f.Text)
	return true
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

// Group states at a dip's hours (see groupState).
const (
	stUnknown = iota // too few tests
	stDips           // at least half the dip's drop at those hours
	stFlat           // at most a quarter of it: normal then
	stUnclear        // in between
)

// groupState compares a group's scores (all its tests pooled, not hourly
// medians: groups are sparse) in the dip's hours with the other hours.
func groupState(ts []model.SpeedTest, loc *time.Location, up bool, hours []int, drop float64) int {
	var in, out []float64
	for _, t := range ts {
		sc := t.ScoreDown
		if up {
			sc = t.ScoreUp
		}
		if sc == nil || t.CappedByServer || (t.Outcome != model.SpeedOK && (up || t.DownBps == nil)) {
			continue
		}
		if slices.Contains(hours, localHour(t.Start, loc)) {
			in = append(in, *sc)
		} else {
			out = append(out, *sc)
		}
	}
	if len(in) < MinPerHour || len(out) < 2*MinPerHour {
		return stUnknown
	}
	r := callq.Median(in) / callq.Median(out)
	switch {
	case r <= 1-drop/2:
		return stDips
	case r >= 1-drop/4:
		return stFlat
	}
	return stUnclear
}

// states is every group's state at the hours, by state.
func states(groups map[string][]model.SpeedTest, loc *time.Location, up bool, p *Pattern) map[int][]string {
	out := map[int][]string{}
	for k, ts := range groups {
		st := groupState(ts, loc, up, p.Hours, p.Drop)
		out[st] = append(out[st], k)
	}
	for _, v := range out {
		slices.Sort(v)
	}
	return out
}

// find tries the cases on download (or upload) scores. Candidate dips are
// the pooled one (over the regions near enough to show the line's speed,
// then all), then each provider's, network's, region's and server's own;
// at a candidate's hours every group is dipping, flat, unclear or unknown
// (too few tests), and the cases ask for dips where they say and flat
// groups to compare with: a group with too few tests never counts as
// either.
//
// The ISP case is judged over the access-sensitive rows only: a region (or
// a server outside regions) whose own p90 reaches nearSpeed of the best
// row's. Far rows are limited by distance (and the byte cap) below the
// line's speed, so congestion on the line barely moves them.
func (f *Finding) find(in FindingInput, tests []model.SpeedTest, up bool) bool {
	var noFallback []model.SpeedTest
	for _, t := range tests {
		if !t.Fallback {
			noFallback = append(noFallback, t)
		}
	}
	near, far := accessRows(noFallback, up)
	var nearTests []model.SpeedTest
	for _, t := range noFallback {
		if near[GroupKey(t)] {
			nearTests = append(nearTests, t)
		}
	}
	group := func(ts []model.SpeedTest, key func(model.SpeedTest) string) map[string][]model.SpeedTest {
		m := map[string][]model.SpeedTest{}
		for _, t := range ts {
			if k := key(t); k != "" {
				m[k] = append(m[k], t)
			}
		}
		return m
	}
	region := func(t model.SpeedTest) string { return t.Region }
	provider := func(t model.SpeedTest) string { return providerOf(in.Catalog, t.Server) }
	byRegion, byProvider := group(noFallback, region), group(noFallback, provider)
	byServer, byNet := group(noFallback, func(t model.SpeedTest) string { return t.Server }), group(noFallback, handoffKey)
	nearRows, nearProv := group(nearTests, GroupKey), group(nearTests, provider)

	var cands []*Pattern
	for _, ts := range [][]model.SpeedTest{nearTests, noFallback} {
		if p := FindDip(hourMedians(ts, in.Loc, up), up); p != nil {
			cands = append(cands, p)
		}
	}
	for _, groups := range []map[string][]model.SpeedTest{byProvider, byNet, byRegion, byServer} {
		for _, k := range slices.Sorted(maps.Keys(groups)) {
			if p := FindDip(hourMedians(groups[k], in.Loc, up), up); p != nil {
				cands = append(cands, p)
			}
		}
	}
	f.Up = up
	regionsOf := func(ts []model.SpeedTest) []string {
		var out []string
		for _, t := range ts {
			if t.Region != "" && !slices.Contains(out, t.Region) {
				out = append(out, t.Region)
			}
		}
		slices.Sort(out)
		return out
	}
	// state of the tests of one provider in one region.
	inRegion := func(pv, r string, p *Pattern) int {
		var mine []model.SpeedTest
		for _, t := range byRegion[r] {
			if provider(t) == pv {
				mine = append(mine, t)
			}
		}
		return groupState(mine, in.Loc, up, p.Hours, p.Drop)
	}
	for _, p := range cands {
		set := func(c string) { f.Case, f.Hours, f.Drop = c, p.Hours, p.Drop }
		// Your ISP: over the near rows, every provider that can tell dips
		// (at least 2) and none is flat, and at least half the rows that
		// can tell dip.
		// Judged over the near rows first when some are far (far rows can't
		// show the line's speed), then over every row (far rows that dip
		// support it too).
		type rowSet struct {
			prov, reg map[string][]model.SpeedTest
			far       []string
			tests     []model.SpeedTest
			all       bool // every row that can tell must dip
		}
		allRows := rowSet{byProvider, group(noFallback, GroupKey), nil, noFallback, false}
		sets := []rowSet{allRows}
		if len(far) > 0 {
			every := allRows
			every.all = true
			sets = []rowSet{every, {nearProv, nearRows, far, nearTests, false}, allRows}
		}
		for _, rows := range sets {
			np, nr := states(rows.prov, in.Loc, up, p), states(rows.reg, in.Loc, up, p)
			judged := len(nr[stDips]) + len(nr[stFlat]) + len(nr[stUnclear])
			if rows.all && len(nr[stDips]) < judged {
				continue
			}
			if len(np[stDips]) >= 2 && len(np[stFlat]) == 0 && len(nr[stDips]) > 0 && 2*len(nr[stDips]) >= judged {
				set(CaseISP)
				f.Providers = np[stDips]
				f.ProvidersJudged = len(np[stDips]) + len(np[stUnclear])
				f.RegionsJudged = judged
				for _, k := range nr[stDips] {
					if isRegion(in.Catalog, k) {
						f.Regions = append(f.Regions, k)
					}
				}
				f.FarRegions, f.group = rows.far, rows.tests
				return true
			}
		}
		prov, reg := states(byProvider, in.Loc, up, p), states(byRegion, in.Loc, up, p)
		// One provider, dipping in at least 2 regions where another provider
		// is measured and flat.
		if len(prov[stDips]) == 1 {
			pv := prov[stDips][0]
			var regs []string
			compared := false
			for _, r := range regionsOf(byProvider[pv]) {
				if inRegion(pv, r, p) != stDips {
					continue
				}
				regs = append(regs, r)
				for q := range byProvider {
					if q != pv && inRegion(q, r, p) == stFlat {
						compared = true
					}
				}
			}
			if len(regs) >= 2 && compared {
				set(CaseProvider)
				f.Providers, f.Regions = []string{pv}, regs
				f.Network, f.Via = sharedHandoff(byProvider[pv]), sharedVia(byProvider[pv])
				f.FarEvening = closeZones(in.Catalog, regs)
				f.group = byProvider[pv]
				return true
			}
		}
		// One network past the ISP (the handoff), of at least 2 providers,
		// the other networks flat.
		if net := states(byNet, in.Loc, up, p); len(net[stDips]) == 1 && len(net[stFlat]) > 0 {
			k := net[stDips][0]
			var provs []string
			for _, t := range byNet[k] {
				if pv := provider(t); !slices.Contains(provs, pv) {
					provs = append(provs, pv)
				}
			}
			if regs := regionsOf(byNet[k]); len(provs) >= 2 && len(regs) >= 2 {
				slices.Sort(provs)
				set(CaseProvider)
				f.Providers, f.Regions, f.Network, f.Via = provs, regs, k, sharedVia(byNet[k])
				f.FarEvening = closeZones(in.Catalog, regs)
				f.group = byNet[k]
				return true
			}
		}
		// One region, the other regions flat.
		if len(reg[stDips]) == 1 && len(reg[stFlat]) > 0 {
			r := reg[stDips][0]
			set(CaseRoute)
			f.Regions = []string{r}
			for _, t := range byRegion[r] {
				if pv := provider(t); !slices.Contains(f.Providers, pv) {
					f.Providers = append(f.Providers, pv)
				}
			}
			slices.Sort(f.Providers)
			if rr, ok := regionByID(in.Catalog, r); ok {
				f.FarEvening = eveningThere(p.Hours, in.Loc, rr.Zone(in.Catalog))
			}
			f.group = byRegion[r]
			return true
		}
		// One server, the rest of its metro (or, without one, the others)
		// flat.
		srv := states(byServer, in.Loc, up, p)
		if len(srv[stDips]) == 1 {
			id := srv[stDips][0]
			metro := ""
			if sv, ok := in.Catalog.Server(id); ok {
				metro = sv.Metro
			}
			flat := 0
			for _, k := range srv[stFlat] {
				if sv, ok := in.Catalog.Server(k); ok && sv.Metro == metro || metro == "" {
					flat++
				}
			}
			if flat > 0 && len(prov[stDips]) <= 1 && len(reg[stDips]) <= 1 {
				set(CaseServer)
				f.Servers, f.Providers = []string{id}, []string{providerOf(in.Catalog, id)}
				f.group = byServer[id]
				return true
			}
		}
	}
	return false
}

// nearSpeed: a row whose p90 reaches this share of the best row's shows
// the line's own speed (access-sensitive).
const nearSpeed = 0.6

// accessRows sorts the rows (GroupKey: regions, or servers outside
// regions) with enough tests into near (access-sensitive) and the far
// regions' IDs.
func accessRows(tests []model.SpeedTest, up bool) (near map[string]bool, far []string) {
	vals := map[string][]float64{}
	for _, t := range tests {
		v := t.DownBps
		if up {
			v = t.UpBps
		}
		if v != nil && t.Outcome == model.SpeedOK {
			vals[GroupKey(t)] = append(vals[GroupKey(t)], *v)
		}
	}
	p90 := map[string]float64{}
	best := 0.0
	for k, v := range vals {
		if len(v) >= 2*MinPerHour {
			p90[k] = callq.Quantile(v, 0.9)
			best = max(best, p90[k])
		}
	}
	near = map[string]bool{}
	for k, v := range p90 {
		if v >= nearSpeed*best {
			near[k] = true
		} else {
			far = append(far, k)
		}
	}
	slices.Sort(far)
	return near, far
}

func isRegion(c *Catalog, id string) bool { _, ok := regionByID(c, id); return ok }

func regionByID(c *Catalog, id string) (Region, bool) {
	for _, r := range c.Regions {
		if r.ID == id {
			return r, true
		}
	}
	return Region{}, false
}

// eveningThere reports whether the viewer's dip hours fall in the far
// end's evening (18:00 to midnight there): its own busy time, or that
// region's evening peak, explains the dip as well as the route does.
func eveningThere(hours []int, loc, there *time.Location) bool {
	if there == nil {
		return false
	}
	now := time.Now().In(zone(loc))
	for _, h := range hours {
		at := time.Date(now.Year(), now.Month(), now.Day(), h, 30, 0, 0, zone(loc))
		if lh := at.In(there).Hour(); lh >= 18 {
			return true
		}
	}
	return false
}

// closeZones reports regions whose known time zones are all within 3
// hours of each other: one provider dipping in them at the same hours may
// be its own evening rather than its link (false when unknown).
func closeZones(c *Catalog, regions []string) bool {
	now := time.Now()
	var offs []int
	for _, id := range regions {
		if r, ok := regionByID(c, id); ok {
			if z := r.Zone(c); z != nil {
				_, off := now.In(z).Zone()
				offs = append(offs, off)
			}
		}
	}
	if len(offs) < 2 || len(offs) < len(regions) {
		return false
	}
	return slices.Max(offs)-slices.Min(offs) < 3*3600
}

// handoffKey names a test's handoff network ("LEVEL3 (AS3356)"); ""
// unknown.
func handoffKey(t model.SpeedTest) string {
	h := t.Handoff
	if h == nil || h.ASN == 0 {
		return ""
	}
	if h.Name == "" {
		return fmt.Sprintf("AS%d", h.ASN)
	}
	return fmt.Sprintf("%s (AS%d)", h.Name, h.ASN)
}

// sharedVia is the internet exchange most of ts (≥ 80% of those with a
// handoff) crossed, else "".
func sharedVia(ts []model.SpeedTest) string {
	n := map[string]int{}
	known := 0
	for _, t := range ts {
		if t.Handoff != nil {
			known++
			if t.Handoff.Via != "" {
				n[t.Handoff.Via]++
			}
		}
	}
	for k, c := range n {
		if c*5 >= known*4 {
			return k
		}
	}
	return ""
}

// meetAt is the wording for where the ISP meets a network at an internet
// exchange ("" without one).
func meetAt(via string) string {
	if via == "" {
		return ""
	}
	return ", where they meet at " + via + " (an internet exchange)"
}

// sharedHandoff is the handoff network of most of ts (≥ 80%), else "".
func sharedHandoff(ts []model.SpeedTest) string {
	n := map[string]int{}
	known := 0
	for _, t := range ts {
		if k := handoffKey(t); k != "" {
			n[k]++
			known++
		}
	}
	for k, c := range n {
		if known > 0 && c*5 >= known*4 {
			return k
		}
	}
	return ""
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
//
// Tiers: "points to" needs ≥ 7 days, full coverage, a wired connection
// (not behind another router), the dip on at least half the days, and, for
// the ISP case, a drop of at least 30% with the ISP edge rising too
// (short, capped tests vary by about ±10%, and a far server's ramp-up
// alone can move them); "most likely" ≥ 5 days and full coverage; "early
// sign" otherwise.
func (f *Finding) wording(in FindingInput, tests []model.SpeedTest) {
	when := hoursText(f.Hours)
	pct := int(f.Drop*100 + 0.5)
	region := func(id string) string {
		if r, ok := regionByID(in.Catalog, id); ok {
			return r.Title
		}
		return id
	}
	// Coverage: each dip hour × providers × tests.
	minTests, minProv := math.MaxInt, math.MaxInt
	for _, h := range f.Hours {
		n := 0
		provs := map[string]bool{}
		for _, t := range tests {
			if localHour(t.Start, in.Loc) == h && !t.Fallback {
				n++
				provs[providerOf(in.Catalog, t.Server)] = true
			}
		}
		minTests, minProv = min(minTests, n), min(minProv, len(provs))
	}
	connWifi, connWired, bridged := 0, 0, 0
	var gwRise, edgeRise []float64
	for _, t := range tests {
		switch t.Conn {
		case model.ConnWiFi:
			connWifi++
		case model.ConnWired:
			connWired++
		}
		if ParseExtra(t.Extra).Bridged {
			bridged++
		}
		if l := t.Latency; l != nil && slices.Contains(f.Hours, localHour(t.Start, in.Loc)) {
			g, e := l.Gateway, l.Edge
			if g.Idle != nil && g.Down != nil && e.Idle != nil && e.Down != nil {
				gwRise = append(gwRise, g.Down.P50Ms-g.Idle.P50Ms)
				edgeRise = append(edgeRise, e.Down.P50Ms-e.Idle.P50Ms)
			}
		}
	}
	f.Bridged = 2*bridged > len(tests)
	group := f.group
	if group == nil {
		group = tests
	}
	f.SlowDays, f.DayCount = slowDays(group, in.Loc, f.Hours, f.Drop, f.Up)
	full := minProv >= 2 && minTests >= MinPerHour
	wired := connWired > 0 && connWifi == 0 && !f.Bridged
	steady := f.DayCount > 0 && 2*f.SlowDays >= f.DayCount
	ctl := ""
	if f.Case == CaseISP && !f.Bridged {
		ctl = edgeControl(in.EdgeIdle, f.Hours)
	}
	edgeRises := ctl != "" || (len(edgeRise) >= MinPerHour && callq.Median(edgeRise) >= 5)
	switch {
	case f.span >= 7 && full && wired && steady && (f.Case != CaseISP || (f.Drop >= 0.3 && edgeRises)):
		f.Tier = TierPointsTo
	case f.span >= 5 && full:
		f.Tier = TierMostLikely
	default:
		f.Tier = TierEarly
	}
	// Caveats: Wi-Fi, the router's own delay rising, other people at home
	// (always in the ISP case: their traffic shares the line and raises
	// the edge's delay, and nothing here measures it), and a container.
	var caveats []string
	household := connWifi > 0 || f.Case == CaseISP
	if len(gwRise) >= MinPerHour {
		gr, er := callq.Median(gwRise), callq.Median(edgeRise)
		household = household || (er > 5 && gr >= 0.7*er)
	}
	if household {
		caveats = append(caveats, "Wi-Fi, or other people at home using the internet heavily at those hours, can look the same.")
	}
	if f.Bridged {
		caveats = append(caveats, "fyisp measured from inside a container or VM (or behind a second router), so Wi-Fi and your own router can't be ruled out.")
	}
	f.Caveat = strings.Join(caveats, " ")
	f.CaveatPublic = strings.NewReplacer("other people at home", "people in the home", "your own router", "the home router").Replace(f.Caveat)
	var claim, who, whoPublic string
	switch f.Case {
	case CaseISP:
		what := "Every region"
		switch {
		case len(f.Regions) == f.RegionsJudged && len(f.FarRegions) == 0:
		case len(f.Regions) == f.RegionsJudged && f.RegionsJudged == 1:
			what = "The only region close enough to show your line's speed (" + region(f.Regions[0]) + ")"
		case len(f.Regions) == f.RegionsJudged:
			what = fmt.Sprintf("All %d regions close enough to show your line's speed", f.RegionsJudged)
		case len(f.FarRegions) == 0:
			what = fmt.Sprintf("Most regions (%d of %d)", len(f.Regions), f.RegionsJudged)
		default:
			what = fmt.Sprintf("Most regions close enough to show your line's speed (%d of %d)", len(f.Regions), f.RegionsJudged)
		}
		slow := "slow"
		if strings.HasPrefix(what, "Every") || strings.HasPrefix(what, "The only") {
			slow = "slows"
		}
		if f.Up {
			slow += " on uploads"
		}
		with := "with every provider we could test"
		if len(f.Providers) < f.ProvidersJudged {
			with = "with " + joinAnd(f.Providers)
		}
		claim = fmt.Sprintf("%s %s from about %s, %s: tests read about %d%% below the same servers' usual results.", what, slow, when, with, pct)
		if n := len(f.FarRegions); n > 0 {
			var far []string
			for _, r := range f.FarRegions {
				far = append(far, region(r))
			}
			if n <= 3 {
				claim += fmt.Sprintf(" %s %s too far away to show it.", joinAnd(far), map[bool]string{true: "is", false: "are"}[n == 1])
			} else {
				claim += fmt.Sprintf(" %d farther regions are too far away to show it.", n)
			}
		}
		if ctl != "" {
			claim += " " + ctl
		}
		who, whoPublic = "your internet provider's own network", "the internet provider's own network"
	case CaseProvider:
		var regs []string
		for _, r := range f.Regions {
			regs = append(regs, region(r))
		}
		if len(f.Providers) > 1 { // grouped by the network past the ISP
			claim = fmt.Sprintf("Only servers reached through one network (%s; %s) %s from about %s; the others we could test stay normal.",
				joinAnd(f.Providers), joinAnd(regs), slowVerb(f.Up), when)
			who, whoPublic = "the link between your internet provider and "+f.Network+meetAt(f.Via), "the link between the internet provider and that network"
		} else {
			claim = fmt.Sprintf("Only %s servers %s from about %s (%s); other providers we could test there stay normal.", f.Providers[0], slowVerb(f.Up), when, joinAnd(regs))
			who, whoPublic = "the link between your internet provider and "+f.Providers[0], "the link between the internet provider and "+f.Providers[0]
			switch {
			case f.Network != "" && f.Via != "":
				who = "the link between your internet provider and " + f.Network + meetAt(f.Via)
			case f.Network != "":
				who += " (reached through " + f.Network + ")"
			case f.Via != "": // the traceroutes end at the exchange: the peer isn't known
				who = "the route through " + f.Via + " (an internet exchange) to these servers"
			}
		}
		if f.FarEvening {
			who += ", or " + f.Providers[0] + "'s own busy evening hours (the regions are in nearby time zones)"
			whoPublic += ", or " + f.Providers[0] + "'s own busy evening hours (the regions are in nearby time zones)"
		}
	case CaseRoute:
		claim = fmt.Sprintf("Only tests to %s %s, from about %s, whichever provider; the other regions stay normal.", region(f.Regions[0]), slowVerb(f.Up), when)
		who = "the long-distance route to " + region(f.Regions[0])
		if f.FarEvening {
			who += ", or busy time at that end (it's evening there too)"
		}
		whoPublic = who
	case CaseServer:
		label := f.Servers[0]
		if s, ok := in.Catalog.Server(label); ok {
			label = s.Label
		}
		claim = fmt.Sprintf("Only %s %s from about %s.", label, slowVerb1(f.Up), when)
		if f.Tier == TierEarly {
			f.Text = claim + " It may be that server's own busy time rather than your connection. A few more days will tell."
		} else {
			f.Text = claim + " That's most likely that server's own busy time rather than your connection."
		}
		f.TextPublic = strings.Replace(f.Text, "your connection", "the connection", 1)
		f.evidence(minTests, minProv)
		return
	}
	f.Text = claim + " " + tierTail(f.Tier, who)
	f.TextPublic = strings.NewReplacer("your line's", "the line's", "Your provider's", "The provider's", "every provider we could test", "every provider tested",
		"the others we could test stay normal", "the others tested stay normal", "other providers we could test there", "other providers tested there").Replace(claim) +
		" " + strings.Replace(tierTail(f.Tier, whoPublic), "We'll know more in a few days.", "A few more days of tests will tell.", 1)
	f.evidence(minTests, minProv)
}

// slowDays counts the days (local dates) with tests both inside and outside
// the dip's hours, and those whose dip-hour median score was at least half
// the drop below the rest of that day's.
func slowDays(tests []model.SpeedTest, loc *time.Location, hours []int, drop float64, up bool) (slow, days int) {
	type acc struct{ in, out []float64 }
	by := map[string]*acc{}
	for _, t := range tests {
		sc := t.ScoreDown
		if up {
			sc = t.ScoreUp
		}
		if sc == nil || t.Fallback {
			continue
		}
		d := t.Start.In(zone(loc)).Format("2006-01-02")
		a := by[d]
		if a == nil {
			a = &acc{}
			by[d] = a
		}
		if slices.Contains(hours, localHour(t.Start, loc)) {
			a.in = append(a.in, *sc)
		} else {
			a.out = append(a.out, *sc)
		}
	}
	for _, a := range by {
		if len(a.in) == 0 || len(a.out) < MinPerHour {
			continue
		}
		days++
		if callq.Median(a.in) <= (1-drop/2)*callq.Median(a.out) {
			slow++
		}
	}
	return slow, days
}

// tierTail is the closing sentence: who, as sure as the tier allows.
func tierTail(tier, who string) string {
	switch tier {
	case TierPointsTo:
		return "That points to " + who + "."
	case TierMostLikely:
		return "So it's most likely " + who + "."
	}
	return "It may be " + who + ". We'll know more in a few days."
}

func (f *Finding) evidence(minTests, minProv int) {
	part, _ := partOfDay(&Pattern{DipFrom: f.Hours[0], Hours: f.Hours})
	p := strings.ToLower(strings.TrimSuffix(part, "s"))
	each := fmt.Sprintf("each %s hour was tested at least %s across %s.", p,
		plural(minTests, "time", "times"), plural(minProv, "provider", "providers"))
	days := plural(f.Days, "day", "days")
	f.Evidence = fmt.Sprintf("Based on %s on %s between %s and %s; %s", plural(f.Tests, "test", "tests"), days,
		dayName(f.First, f.loc), dayName(f.Last, f.loc), each)
	f.EvidencePublic = fmt.Sprintf("Based on %s on %s; %s", plural(f.Tests, "test", "tests"), days, each)
	var more string
	if f.Tier == TierEarly {
		more += fmt.Sprintf(" Early: %s of tests. A week gives a clearer picture.", days)
	}
	if f.DayCount > 0 {
		more += fmt.Sprintf(" Slower on %d of %s.", f.SlowDays, plural(f.DayCount, p, p+"s"))
	}
	f.Evidence += more
	f.EvidencePublic += more
}

// dayName is t's date in loc (nil: UTC), e.g. "Mon 6 Oct".
func dayName(t time.Time, loc *time.Location) string { return t.In(zone(loc)).Format("Mon 2 Jan") }

// coverage says why the tests cover fewer days than the window's: tests
// started within it (only the owner's voice names the date), or days
// without tests.
func (f *Finding) coverage(in FindingInput, tests []model.SpeedTest) {
	if in.From.IsZero() {
		return
	}
	to := in.To
	if to.IsZero() {
		to = f.Last
	}
	w := int(math.Round(to.Sub(in.From).Hours() / 24))
	days := map[string]bool{}
	for _, t := range tests {
		if !t.Start.Before(in.From) && !t.Start.After(to) {
			days[t.Start.In(zone(in.Loc)).Format("2006-01-02")] = true
		}
	}
	d := len(days)
	if d >= w {
		return
	}
	if f.First.Sub(in.From) > 24*time.Hour {
		f.Coverage = fmt.Sprintf("Speed tests started on %s, so this covers %s, not %d.", dayName(f.First, in.Loc), plural(d, "day", "days"), w)
		f.CoveragePublic = fmt.Sprintf("This covers %s, not %d.", plural(d, "day", "days"), w)
		return
	}
	f.Coverage = fmt.Sprintf("Tests ran on %d of the last %d days.", d, w)
	f.CoveragePublic = f.Coverage
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
// other hours: rising (by 5 ms and 30%), it fits congestion in the
// provider's local network (the household's own traffic can raise it too,
// hence the ISP case's caveat).
// It only supports the cross-destination comparison: a flat edge says
// nothing on lines with active queue management (fq_codel, CAKE).
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
	if a-b >= max(5, 0.3*b) { // smaller rises of ICMP to a BNG are noise
		return fmt.Sprintf("Your provider's first router also answers slower then (%.0f ms instead of %.0f ms), which fits congestion in its local network.", a, b)
	}
	// Flat proves nothing: a line with active queue management keeps its
	// delay flat while congested.
	return ""
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
func HomeLine(f Finding, tests []model.SpeedTest, loc *time.Location, now time.Time) *HomeSpeed {
	var n, ok int
	for _, t := range tests {
		if !t.OnDemand() && t.Outcome != model.SpeedCancelled && !t.Start.Before(now.Add(-24*time.Hour)) {
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
		return &HomeSpeed{Text: fmt.Sprintf("Speed: checking at different times of day. About %d days of tests are needed to see a pattern.", MinDays), Kind: "warmup", Impact: "ok"}
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
		if !t.OnDemand() && t.Outcome == model.SpeedOK && !t.Fallback && slices.Contains(f.Hours, localHour(t.Start, loc)) {
			if t.DownBps != nil {
				down = append(down, *t.DownBps)
			}
			if t.UpBps != nil {
				up = append(up, *t.UpBps)
			}
		}
	}
	// At most two sentences: the dip and what it means for calls; then
	// where it is, as sure as the finding's tier, with its caveat folded in.
	part, ref := partOfDay(&Pattern{DipFrom: f.Hours[0], Hours: f.Hours})
	than := map[string]string{"daytime": "than daytime", "usual": "than usual"}[ref]
	what := lowerFirst(part) + " are"
	if f.Up {
		what = "uploads in the " + lowerFirst(part) + " are"
	}
	impact := "ok"
	calls := "but still fast enough for video calls"
	if (len(down) > 0 && callq.Quantile(down, 0.1) < videoDownBps) || (len(up) > 0 && callq.Quantile(up, 0.1) < videoUpBps) {
		impact, calls = "video_risk", "and may be too slow for video calls then"
	}
	text := fmt.Sprintf("Speed: %s about %d%% slower %s (around %s), %s.", what, int(f.Drop*100+0.5), than, hoursText(f.Hours), calls)
	shows := "Every test server shows it"
	if len(f.Regions) < f.RegionsJudged || len(f.FarRegions) > 0 {
		shows = "Most test servers show it"
	}
	unless := ""
	switch {
	case f.Bridged:
		unless = ", though fyisp runs behind another router here"
	case f.Caveat != "":
		unless = ", unless it's Wi-Fi or heavy use by others at home"
	}
	switch f.Tier {
	case TierPointsTo:
		text += " " + shows + ", which points to your internet provider's network" + unless + "."
	case TierMostLikely:
		text += " " + shows + ", so it's most likely your internet provider's network" + unless + "."
	default:
		text += " " + shows + " so far, so it may be your internet provider's network" + unless + "."
	}
	return &HomeSpeed{Text: text, Kind: "pattern", Impact: impact}
}
