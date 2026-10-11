package speedtest

import (
	"math"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// findingCatalog: providers A, B and C in two metros, A and B in a far one.
func findingCatalog(t *testing.T) *Catalog {
	t.Helper()
	srv := func(p string) string {
		var b strings.Builder
		b.WriteString("provider: " + strings.ToLower(p) + "\ndisplay: " + p + "\nsources: [x]\nservers:\n")
		for i, m := range []string{"one", "two", "far"} {
			if p == "C" && m == "far" {
				continue
			}
			geo := "eu"
			if m == "far" {
				geo = "as"
			}
			b.WriteString("  - {id: " + p + string(rune('1'+i)) + ", host: " + strings.ToLower(p) + m + ".example, ports: 5201-5202, city: " + m +
				", metro: " + m + ", country: GB, geo: " + geo + ", verified: {date: 2026-10-10}}\n")
		}
		return b.String()
	}
	c, err := LoadCatalog(fstest.MapFS{
		"a.yml": {Data: []byte(srv("A"))}, "b.yml": {Data: []byte(srv("B"))}, "c.yml": {Data: []byte(srv("C"))},
		"regions.yml": {Data: []byte(`regions:
  - {id: r1, title: Region One, servers: [A1, B1, C1]}
  - {id: r2, title: Region Two, servers: [A2, B2, C2]}
  - {id: r3, title: Far Asia, servers: [A3, B3]}
`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// synth makes days of hourly scheduled tests per region, rotating the
// providers by day and hour; score(server, hour) is the download score.
func synth(c *Catalog, days int, score func(server string, h int) float64, conn string) []model.SpeedTest {
	var out []model.SpeedTest
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for d := range days {
		for h := range 24 {
			for i, r := range c.Regions {
				at := start.Add(time.Duration(d*24+h)*time.Hour + time.Duration(i)*20*time.Minute)
				sv := r.Chain(c, at, 0)[0].Server.ID
				sc := score(sv, h)
				down, up := sc*400e6, 40e6
				out = append(out, model.SpeedTest{Start: at, Trigger: "schedule", Outcome: model.SpeedOK, Region: r.ID, Server: sv,
					FallbackLevel: FallbackNone, DownBps: &down, UpBps: &up, ScoreDown: &sc, Conn: conn})
			}
		}
	}
	return out
}

func evening(h int) bool { return h >= 19 && h <= 22 }

func TestFinding(t *testing.T) {
	c := findingCatalog(t)
	nan := [24]float64{}
	for i := range nan {
		nan[i] = math.NaN()
	}
	edgeRise := nan
	for h := range 24 {
		edgeRise[h] = 8
		if evening(h) {
			edgeRise[h] = 25
		}
	}
	cases := []struct {
		name  string
		days  int
		score func(sv string, h int) float64
		conn  string
		edge  [24]float64
		want  string // case
		tier  string
		text  []string
	}{
		{"isp", 9, func(_ string, h int) float64 {
			if evening(h) {
				return 0.6
			}
			return 1
		}, "wired", edgeRise, CaseISP, TierPointsTo, []string{
			"Every region slows from about 7 to 11 pm, with every provider we could test: tests read about 40% below the same servers' usual results.",
			"also answers slower then (25 ms instead of 8 ms), which fits congestion in its local network.",
			"That points to your internet provider's own network."}},
		{"isp on wifi, early", 4, func(_ string, h int) float64 {
			if evening(h) {
				return 0.6
			}
			return 1
		}, "wifi", nan, CaseISP, TierEarly, []string{"It may be your internet provider's own network. We'll know more in a few days."}},
		{"one provider", 9, func(sv string, h int) float64 {
			if evening(h) && (sv == "B1" || sv == "B2") {
				return 0.5
			}
			return 1
		}, "wired", nan, CaseProvider, TierPointsTo, []string{"Only B servers slow from about 7 to 11 pm (Region One and Region Two); other providers we could test there stay normal.",
			"That points to the link between your internet provider and B."}},
		{"long-distance route", 9, func(sv string, h int) float64 {
			if evening(h) && strings.HasSuffix(sv, "3") {
				return 0.5
			}
			return 1
		}, "wired", nan, CaseRoute, TierPointsTo, []string{"Only tests to Far Asia slow, from about 7 to 11 pm, whichever provider; the other regions stay normal.", "the long-distance route to Far Asia"}},
		{"one server", 9, func(sv string, h int) float64 {
			if evening(h) && sv == "C1" {
				return 0.4
			}
			return 1
		}, "wired", nan, CaseServer, "", []string{"Only C one slows from about 7 to 11 pm. That's most likely that server's own busy time rather than your connection."}},
		{"flat", 9, func(string, int) float64 { return 1 }, "wired", nan, CaseNothing, "", []string{"No slowdown of 25% or more stands out: every hour stays within about 10% of normal."}},
		{"two days", 2, func(string, int) float64 { return 1 }, "wired", nan, CaseNotEnough, "", []string{"2 of 3 days so far."}},
	}
	for _, tc := range cases {
		ts := synth(c, tc.days, tc.score, tc.conn)
		f := FindFinding(FindingInput{Tests: ts, Catalog: c, EdgeIdle: tc.edge})
		publicVoice(t, tc.name, f)
		if f.Case != tc.want || (tc.tier != "" && f.Tier != tc.tier) {
			t.Errorf("%s: case %s tier %s, want %s %s: %s", tc.name, f.Case, f.Tier, tc.want, tc.tier, f.Text)
			continue
		}
		for _, w := range tc.text {
			if !strings.Contains(f.Text, w) {
				t.Errorf("%s: %q lacks %q", tc.name, f.Text, w)
			}
		}
		if strings.Contains(f.TextPublic, "your internet provider") || strings.Contains(f.TextPublic, "Your provider") || f.Evidence == "" {
			t.Errorf("%s: public %q, evidence %q", tc.name, f.TextPublic, f.Evidence)
		}
		if tc.conn == "wifi" && f.Caveat == "" {
			t.Errorf("%s: no Wi-Fi caveat", tc.name)
		}
	}
	// Providers A and C reached through one network past the ISP, B
	// through another: A and C dip, B stays flat.
	ts := synth(c, 9, func(sv string, h int) float64 {
		if evening(h) && sv[0] != 'B' {
			return 0.5
		}
		return 1
	}, "wired")
	for i := range ts {
		h := &model.Handoff{ASN: 3356, Name: "LEVEL3", ISPASN: 3320}
		if ts[i].Server[0] == 'B' {
			h = &model.Handoff{ASN: 174, Name: "COGENT-174", ISPASN: 3320}
		}
		ts[i].Handoff = h
	}
	if f := FindFinding(FindingInput{Tests: ts, Catalog: c}); f.Case != CaseProvider || f.Network != "LEVEL3 (AS3356)" || len(f.Providers) != 2 ||
		!strings.Contains(f.Text, "the link between your internet provider and LEVEL3 (AS3356).") || strings.Contains(f.TextPublic, "LEVEL3") {
		t.Errorf("handoff: %s %q %v: %s / %s", f.Case, f.Network, f.Providers, f.Text, f.TextPublic)
	}
	// Meeting at an internet exchange (local wording only).
	for i := range ts {
		if ts[i].Server[0] != 'B' {
			h := *ts[i].Handoff
			h.Via = "DE-CIX"
			ts[i].Handoff = &h
		}
	}
	if f := FindFinding(FindingInput{Tests: ts, Catalog: c}); f.Via != "DE-CIX" ||
		!strings.Contains(f.Text, "the link between your internet provider and LEVEL3 (AS3356), where they meet at DE-CIX (an internet exchange)") ||
		regexp.MustCompile(`DE-CIX|AS[0-9]|via`).MatchString(f.TextPublic) {
		t.Errorf("via: %q: %s / %s", f.Via, f.Text, f.TextPublic)
	}
	// One provider dips; its traceroutes end at the exchange: the peer
	// isn't known.
	ts = synth(c, 9, func(sv string, h int) float64 {
		if evening(h) && sv[0] == 'A' && sv != "A3" {
			return 0.5
		}
		return 1
	}, "wired")
	for i := range ts {
		ts[i].Handoff = &model.Handoff{ISPASN: 3320}
		if ts[i].Server[0] == 'A' {
			ts[i].Handoff.Via = "DE-CIX"
		}
	}
	if f := FindFinding(FindingInput{Tests: ts, Catalog: c}); f.Case != CaseProvider ||
		!strings.Contains(f.Text, "the route through DE-CIX (an internet exchange) to these servers") ||
		regexp.MustCompile(`DE-CIX|AS[0-9]|via`).MatchString(f.TextPublic) {
		t.Errorf("peer unknown: %s %q: %s / %s", f.Case, f.Via, f.Text, f.TextPublic)
	}
	// The ISP case's evidence line.
	f := FindFinding(FindingInput{Tests: synth(c, 9, func(_ string, h int) float64 {
		if evening(h) {
			return 0.6
		}
		return 1
	}, "wired"), Catalog: c})
	if !strings.HasPrefix(f.Evidence, "Based on 648 tests over 9 days; each evening hour was tested at least") || len(f.Hours) != 4 || f.Hours[0] != 19 {
		t.Errorf("evidence %q hours %v", f.Evidence, f.Hours)
	}
	// Manual tests and sweeps never count.
	ts = synth(c, 9, func(string, int) float64 { return 1 }, "wired")
	for i := range ts {
		if evening(ts[i].Start.Hour()) {
			ts[i].Trigger = "manual"
			sc := 0.3
			ts[i].ScoreDown = &sc
		}
	}
	if f := FindFinding(FindingInput{Tests: ts, Catalog: c}); f.Case == CaseISP {
		t.Error("manual tests made a pattern")
	}
}

func TestHomeLine(t *testing.T) {
	c := findingCatalog(t)
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	dip := func(_ string, h int) float64 {
		if evening(h) {
			return 0.6
		}
		return 1
	}
	ts := synth(c, 9, dip, "wired")
	f := FindFinding(FindingInput{Tests: ts, Catalog: c})
	l := HomeLine(f, ts, nil, now)
	if l == nil || l.Kind != "pattern" || l.Impact != "ok" || l.Text != "Speed: evenings are about 40% slower than daytime (around 7 to 11 pm), but still fast enough for video calls."+
		" Every test server shows it, so it's most likely your internet provider's network, unless it's Wi-Fi or heavy use by others at home." {
		t.Fatalf("%+v", l)
	}
	// A slow evening: may be too slow for video calls.
	for i := range ts {
		if evening(ts[i].Start.Hour()) {
			v := 6e6
			ts[i].DownBps = &v
		}
	}
	if l := HomeLine(f, ts, nil, now); l == nil || l.Impact != "video_risk" || !strings.Contains(l.Text, "may be too slow for video calls") {
		t.Errorf("%+v", l)
	}
	// Warm-up, steady, narrow and failing.
	short := synth(c, 2, dip, "wired")
	if l := HomeLine(FindFinding(FindingInput{Tests: short, Catalog: c}), short, nil, now.Add(-7*24*time.Hour)); l == nil || l.Kind != "warmup" {
		t.Errorf("warm-up %+v", l)
	}
	flat := synth(c, 9, func(string, int) float64 { return 1 }, "wired")
	if l := HomeLine(FindFinding(FindingInput{Tests: flat, Catalog: c}), flat, nil, now); l == nil || l.Kind != "steady" {
		t.Errorf("steady %+v", l)
	}
	narrow := synth(c, 9, func(sv string, h int) float64 {
		if evening(h) && sv == "C1" {
			return 0.4
		}
		return 1
	}, "wired")
	if l := HomeLine(FindFinding(FindingInput{Tests: narrow, Catalog: c}), narrow, nil, now); l != nil {
		t.Errorf("narrow %+v", l)
	}
	for i := range flat {
		if flat[i].Start.After(now.Add(-24 * time.Hour)) {
			flat[i].Outcome = model.SpeedBusy
		}
	}
	if l := HomeLine(FindFinding(FindingInput{Tests: flat, Catalog: c}), flat, nil, now); l != nil {
		t.Errorf("failing %+v", l)
	}
}

func TestSummarize(t *testing.T) {
	c := findingCatalog(t)
	ts := synth(c, 9, func(_ string, h int) float64 {
		if evening(h) {
			return 0.6
		}
		return 1
	}, "wired")
	var r1 []model.SpeedTest
	for _, x := range ts {
		if x.Region == "r1" {
			r1 = append(r1, x)
		}
	}
	r1[0].Fallback, r1[1].Outcome = true, model.SpeedBusy
	r1 = append(r1, model.SpeedTest{Start: r1[2].Start, Trigger: "manual", Outcome: model.SpeedOK, Server: "A1"})
	st := Summarize(r1, nil)
	if st.N != 215 || st.Busy != 1 || st.Fallback != 1 || st.Dip == nil || st.Hours[20].N != 9 || *st.Hours[20].Score != 0.6 || len(st.Hours[20].Servers) != 3 {
		t.Errorf("%+v %+v", st, st.Hours[20])
	}
	if st.ServerNow == "" || st.BestDown == nil || st.Score == nil {
		t.Errorf("%+v", st)
	}
}

// TestFindingThin: webdev-like data (47 hours of hourly tests from
// mid-afternoon: 3 dates, at most 2 tests per region-hour) never reads "no
// time of day stands out": it is still learning, and names the dip it sees.
func TestFindingThin(t *testing.T) {
	c := findingCatalog(t)
	start := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	var ts []model.SpeedTest
	for k := range 47 { // webdev's 2-day backfill: 3 dates, at most 2 tests per region-hour
		for i, r := range c.Regions {
			at := start.Add(time.Duration(k)*time.Hour + time.Duration(i)*20*time.Minute)
			sv := r.Chain(c, at, 0)[0].Server.ID
			sc := 1.0
			if h := at.Hour(); h >= 21 || h < 1 {
				sc = 0.55
			}
			down := sc * 400e6
			ts = append(ts, model.SpeedTest{Start: at, Trigger: "schedule", Outcome: model.SpeedOK, Region: r.ID, Server: sv,
				FallbackLevel: FallbackNone, DownBps: &down, ScoreDown: &sc})
		}
	}
	f := FindFinding(FindingInput{Tests: ts, Catalog: c})
	if f.Case != CaseNotEnough || f.Tier != TierEarly || len(f.Hours) == 0 || f.Hours[0] != 21 ||
		!strings.Contains(f.Text, "Evenings are slower: about 50% below your daytime speed so far (around 9 pm to 1 am)") || !strings.Contains(f.Text, "more day") ||
		strings.Contains(f.TextPublic, "your") {
		t.Errorf("%s %s %v: %s / %s", f.Case, f.Tier, f.Hours, f.Text, f.TextPublic)
	}
	flat := ts[:0:0]
	for _, x := range ts {
		one := 1.0
		x.ScoreDown = &one
		flat = append(flat, x)
	}
	if f := FindFinding(FindingInput{Tests: flat, Catalog: c}); f.Case != CaseNotEnough || !strings.HasPrefix(f.Text, "Still learning:") {
		t.Errorf("flat: %s %s", f.Case, f.Text)
	}
}

// TestDSTHours: with an IANA zone, a test at 7 pm local time is in the 19
// cell on both sides of a DST change (a fixed offset would move it).
func TestDSTHours(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no zoneinfo")
	}
	var ts []model.SpeedTest
	for d := range 6 { // DST ends on 1 Nov 2026
		at := time.Date(2026, 10, 29+d, 19, 0, 0, 0, ny)
		sc := 0.5
		ts = append(ts, model.SpeedTest{Start: at, Trigger: "schedule", Outcome: model.SpeedOK, Server: "A1", ScoreDown: &sc})
	}
	st := Summarize(ts, ny)
	if st.Hours[19].N != 6 {
		t.Errorf("19 h cell: %d tests", st.Hours[19].N)
	}
	if fixed := Summarize(ts, time.FixedZone("", -4*3600)); fixed.Hours[19].N == 6 {
		t.Error("a fixed offset should split them")
	}
}

// synthDown is synth with each test's download set by base(server) times
// its score (far servers read lower), and seeded noise of ±noise.
func synthDown(c *Catalog, days int, score func(sv string, h int) float64, base func(sv string) float64, noise float64) []model.SpeedTest {
	r := rand.New(rand.NewPCG(7, 8))
	ts := synth(c, days, score, "wired")
	for i := range ts {
		sc := *ts[i].ScoreDown * (1 + noise*(2*r.Float64()-1))
		down := sc * base(ts[i].Server)
		ts[i].ScoreDown, ts[i].DownBps = &sc, &down
	}
	return ts
}

// TestFindingNetIsp: the re-review's cases (net-isp-review.md).
// publicVoice: the share link's and report's texts never address the
// viewer ("your …") or speak as fyisp ("we"): they're about someone
// else's line.
func publicVoice(t *testing.T, name string, f Finding) {
	t.Helper()
	for what, s := range map[string]string{"text_public": f.TextPublic, "caveat_public": f.CaveatPublic, "evidence": f.Evidence} {
		l := " " + strings.ToLower(s)
		if strings.Contains(l, "your") || strings.Contains(l, " you ") || strings.Contains(l, " we ") || strings.Contains(l, " we'") || strings.Contains(l, " we’") {
			t.Errorf("%s: %s %q", name, what, s)
		}
	}
}

func TestFindingNetIsp(t *testing.T) {
	c := findingCatalog(t)
	evening := func(drop float64, dips func(sv string) bool) func(string, int) float64 {
		return func(sv string, h int) float64 {
			if h >= 19 && h <= 22 && dips(sv) {
				return 1 - drop
			}
			return 1
		}
	}
	all := func(string) bool { return true }
	near := func(sv string) bool { return !strings.HasSuffix(sv, "3") }
	flatBase := func(string) float64 { return 400e6 }
	farSlow := func(sv string) float64 {
		if strings.HasSuffix(sv, "3") {
			return 60e6 // 150-300 ms away: far below the line's speed
		}
		return 400e6
	}
	// M3: a fast line's access dip shows in the near regions only.
	f := FindFinding(FindingInput{Tests: synthDown(c, 9, evening(0.4, near), farSlow, 0), Catalog: c})
	publicVoice(t, "net-isp", f)
	if f.Case != CaseISP || !strings.Contains(f.Text, "All 2 regions close enough to show your line's speed slow") ||
		!strings.Contains(f.Text, "Far Asia is too far away to show it.") || len(f.FarRegions) != 1 {
		t.Errorf("near only: %s %s: %s", f.Case, f.Tier, f.Text)
	}
	// M1: 2 of 3 regions is "most", not "every".
	f = FindFinding(FindingInput{Tests: synthDown(c, 9, evening(0.4, near), flatBase, 0), Catalog: c})
	publicVoice(t, "net-isp", f)
	if f.Case != CaseISP || !strings.Contains(f.Text, "Most regions (2 of 3) slow") || strings.Contains(f.Text, "Every region") {
		t.Errorf("2 of 3: %s: %s", f.Case, f.Text)
	}
	// S7: ±15% noise still finds it, and counts the evenings.
	f = FindFinding(FindingInput{Tests: synthDown(c, 9, evening(0.4, all), flatBase, 0.15), Catalog: c})
	publicVoice(t, "net-isp", f)
	if f.Case != CaseISP || f.DayCount < 8 || 2*f.SlowDays < f.DayCount || !strings.Contains(f.Evidence, " evenings.") {
		t.Errorf("noisy: %s %s %d/%d: %s / %s", f.Case, f.Tier, f.SlowDays, f.DayCount, f.Text, f.Evidence)
	}
	// M2: a 15% dip is "a little slower", never "within 10%".
	f = FindFinding(FindingInput{Tests: synthDown(c, 9, evening(0.15, all), flatBase, 0), Catalog: c})
	publicVoice(t, "net-isp", f)
	if f.Case != CaseMild || !strings.Contains(f.Text, "a little slower: about 15% below normal") {
		t.Errorf("mild: %s: %s", f.Case, f.Text)
	}
	f = FindFinding(FindingInput{Tests: synthDown(c, 9, func(string, int) float64 { return 1 }, flatBase, 0.05), Catalog: c})
	publicVoice(t, "net-isp", f)
	if f.Case != CaseNothing || !strings.HasPrefix(f.Text, "No slowdown of 25% or more stands out") {
		t.Errorf("flat: %s: %s", f.Case, f.Text)
	}
	// S1: B dips where no other provider was measured: never "the others
	// stay normal".
	var onlyB []model.SpeedTest
	for _, x := range synthDown(c, 9, evening(0.5, func(sv string) bool { return sv == "B1" || sv == "B2" }), flatBase, 0) {
		if x.Server[0] == 'B' || x.Region == "r3" {
			onlyB = append(onlyB, x)
		}
	}
	if f := FindFinding(FindingInput{Tests: onlyB, Catalog: c}); f.Case == CaseProvider {
		t.Errorf("provider without comparison: %s", f.Text)
	}
	// M5: behind another router: no "points to", and the caveat says so.
	ts := synthDown(c, 9, evening(0.4, all), flatBase, 0)
	for i := range ts {
		ts[i].Extra = `{"bridged":true}`
	}
	if f := FindFinding(FindingInput{Tests: ts, Catalog: c}); f.Tier == TierPointsTo || !f.Bridged || !strings.Contains(f.Caveat, "container") ||
		!strings.Contains(f.CaveatPublic, "people in the home") || !strings.Contains(f.CaveatPublic, "the home router") || strings.Contains(f.CaveatPublic, "your") {
		t.Errorf("bridged: %s %v %q / %q", f.Tier, f.Bridged, f.Caveat, f.CaveatPublic)
	}
}

// TestFarEnd: the far end's evening and regions in nearby time zones.
func TestFarEnd(t *testing.T) {
	c, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	ldn, _ := time.LoadLocation("Europe/London")
	tyo, _ := time.LoadLocation("Asia/Tokyo")
	eve := []int{19, 20, 21, 22}
	if !eveningThere(eve, ldn, ldn) || eveningThere(eve, ldn, tyo) {
		t.Error("eveningThere")
	}
	if !closeZones(c, []string{"uk", "eu-west"}) || closeZones(c, []string{"uk", "us-east"}) {
		t.Error("closeZones")
	}
}
