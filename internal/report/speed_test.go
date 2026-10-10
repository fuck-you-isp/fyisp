package report

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

func TestSpeedSection(t *testing.T) {
	f := newFixture(t, 7, 5*time.Minute)
	d := f.deps()
	var tests []model.SpeedTest
	start := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	for i := range 7 * 96 {
		at := start.Add(time.Duration(i) * 15 * time.Minute)
		for _, reg := range []string{"uk", "us-east"} {
			x := model.SpeedTest{Start: at, End: at.Add(23 * time.Second), Trigger: "schedule", Outcome: model.SpeedOK, FallbackLevel: "none", Region: reg, Server: "Clouvider-London"}
			base := 400e6
			if reg == "us-east" {
				x.Server, base = "Leaseweb-NewYork", 150e6
				if i%10 == 0 {
					x.Server, x.Fallback, x.FallbackLevel = "Clouvider-NewYork", true, "same_metro"
				}
			}
			if i%40 == 3 {
				x.Outcome = model.SpeedBusy
			} else {
				down, up := base, base/10
				if h := at.Hour(); h >= 19 && h <= 22 {
					down /= 2
				}
				sc := down / base
				x.DownBps, x.UpBps, x.ScoreDown = &down, &up, &sc
			}
			tests = append(tests, x)
		}
	}
	own := 900e6
	tests = append(tests, model.SpeedTest{Start: start.Add(time.Hour), End: start.Add(time.Hour + 20*time.Second), Outcome: model.SpeedOK,
		Server: "192.168.1.20:5201", DownBps: &own, UpBps: &own, Trigger: "manual"},
		model.SpeedTest{Start: start.Add(2 * time.Hour), End: start.Add(2*time.Hour + 20*time.Second), Outcome: model.SpeedBusy,
			Server: "Leaseweb-London", Trigger: "public_sweep"})
	d.SpeedTests = func(_ context.Context, from, to time.Time) ([]model.SpeedTest, error) {
		var out []model.SpeedTest
		for _, x := range tests {
			if !x.Start.Before(from) && !x.Start.After(to) {
				out = append(out, x)
			}
		}
		return out, nil
	}
	html := f.build(t, d, Options{Redact: true})
	checkStatic(t, html)
	for _, want := range []string{
		`id="speed"`, "<h2>Speed tests</h2>",
		"1346 speed tests ran in this range: 1311 measured, 35 found the test servers busy (not counted as slow).",
		"Of these, 1 started by hand and 1 started from the share link; the rest were scheduled.",
		"Evenings are slower: about 50% below your daytime speed (19:00–23:00 UTC, every region alike).",
		"The largest dip: UK, 50% slower",
		"<td>UK</td><td>Clouvider London</td><td class=\"n\">672</td><td class=\"n\">400 Mbit/s</td>",
		"<td>US East</td><td>Leaseweb New York (1 fallback server)</td>",
		"<td>Your server</td>",
		"19–23 UTC, −50%",
		"17 busy",
		"left out everywhere in this report",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if strings.Contains(html, "192.168.1.20") || strings.Contains(html, "speedtest.") {
		t.Error("the report shows a speed test host")
	}
	if html := f.build(t, f.deps(), Options{}); strings.Contains(html, `id="speed"`) {
		t.Error("speed section without speed tests")
	}
}
