package speedtest

import (
	"math"
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
			"Every region slows from about 7 to 11 pm, with every provider: about 40% below normal.",
			"also answers slower then (25 ms instead of 8 ms), so it's in its local network.",
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
		}, "wired", nan, CaseProvider, TierPointsTo, []string{"Only B servers slow from about 7 to 11 pm (Region One and Region Two); other providers stay normal.",
			"That points to the link between your internet provider and B."}},
		{"long-distance route", 9, func(sv string, h int) float64 {
			if evening(h) && strings.HasSuffix(sv, "3") {
				return 0.5
			}
			return 1
		}, "wired", nan, CaseRoute, TierPointsTo, []string{"Only tests to Far Asia slow, from about 7 to 11 pm, whichever provider.", "the long-distance route to Far Asia"}},
		{"one server", 9, func(sv string, h int) float64 {
			if evening(h) && sv == "C1" {
				return 0.4
			}
			return 1
		}, "wired", nan, CaseServer, "", []string{"Only C one slows from about 7 to 11 pm. That's most likely that server's own busy time, not your connection."}},
		{"flat", 9, func(string, int) float64 { return 1 }, "wired", nan, CaseNothing, "", []string{"No time of day stands out."}},
		{"two days", 2, func(string, int) float64 { return 1 }, "wired", nan, CaseNotEnough, "", []string{"2 of 3 days so far."}},
	}
	for _, tc := range cases {
		ts := synth(c, tc.days, tc.score, tc.conn)
		f := FindFinding(FindingInput{Tests: ts, Catalog: c, EdgeIdle: tc.edge})
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
	if f := FindFinding(FindingInput{Tests: ts, Catalog: c}); f.Case != CaseProvider || f.Network != "AS3356 LEVEL3" || len(f.Providers) != 2 ||
		!strings.Contains(f.Text, "the link between your internet provider and AS3356 LEVEL3") || strings.Contains(f.TextPublic, "LEVEL3") {
		t.Errorf("handoff: %s %q %v: %s / %s", f.Case, f.Network, f.Providers, f.Text, f.TextPublic)
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
	l := HomeLine(f, ts, 0, now)
	if l == nil || l.Kind != "pattern" || l.Impact != "ok" || l.Text != "Evenings are slower: about 40% below your daytime speed (around 7 to 11 pm). That's still plenty for video calls." {
		t.Fatalf("%+v", l)
	}
	// A slow evening: may be too slow for video calls.
	for i := range ts {
		if evening(ts[i].Start.Hour()) {
			v := 6e6
			ts[i].DownBps = &v
		}
	}
	if l := HomeLine(f, ts, 0, now); l == nil || l.Impact != "video_risk" || !strings.Contains(l.Text, "may be too slow for video calls") {
		t.Errorf("%+v", l)
	}
	// Warm-up, steady, narrow and failing.
	short := synth(c, 2, dip, "wired")
	if l := HomeLine(FindFinding(FindingInput{Tests: short, Catalog: c}), short, 0, now.Add(-7*24*time.Hour)); l == nil || l.Kind != "warmup" {
		t.Errorf("warm-up %+v", l)
	}
	flat := synth(c, 9, func(string, int) float64 { return 1 }, "wired")
	if l := HomeLine(FindFinding(FindingInput{Tests: flat, Catalog: c}), flat, 0, now); l == nil || l.Kind != "steady" {
		t.Errorf("steady %+v", l)
	}
	narrow := synth(c, 9, func(sv string, h int) float64 {
		if evening(h) && sv == "C1" {
			return 0.4
		}
		return 1
	}, "wired")
	if l := HomeLine(FindFinding(FindingInput{Tests: narrow, Catalog: c}), narrow, 0, now); l != nil {
		t.Errorf("narrow %+v", l)
	}
	for i := range flat {
		if flat[i].Start.After(now.Add(-24 * time.Hour)) {
			flat[i].Outcome = model.SpeedBusy
		}
	}
	if l := HomeLine(FindFinding(FindingInput{Tests: flat, Catalog: c}), flat, 0, now); l != nil {
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
	st := Summarize(r1, 0)
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
