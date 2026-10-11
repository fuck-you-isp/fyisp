package report

import (
	"context"
	"regexp"
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
				x.Conn, x.DownSecs, x.UpSecs, x.DownShort = model.ConnWired, 0.9, 3.1, base > 300e6
				if x.DownShort {
					x.Extra = `{"rate_method":["peak90",""]}` // a capped download, levelled off
				}
				x.Latency = &model.SpeedLatency{Edge: model.LoadPhases{Idle: &model.LoadStats{N: 40, P50Ms: 8},
					Down: &model.LoadStats{N: 30, P50Ms: 21, P95Ms: 34}, Up: &model.LoadStats{N: 25, Lost: 1, P50Ms: 48, P95Ms: 90}}}
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
		"Pooled over every server, tests read about 50% lower from 7–11 pm UTC than the same servers",
		"for at most about 11 seconds each way, ramp-up included (the ramp-up is left out of the result)",
		"Connection: of 1311 measured tests, 1310 wired, 1 on a link of unknown type.",
		"Delay to the provider",
		"8 ms idle, 21 ms while downloading (95th percentile 34 ms, 0.0% lost), 48 ms while uploading (95th percentile 90 ms, 4.0% lost).",
		"downloads were measured for a median of 0.9 s (0.9–0.9 s), uploads for a median of 3.1 s (3.1–3.1 s); 655 of 2620 directions stopped at the 100 MB cap",
		"What to ask your provider:",
		"The largest dip: UK, 50% slower",
		"<td>UK</td><td>Clouvider London</td><td class=\"n\">672</td><td class=\"n\">400 Mbit/s</td>",
		"<td>US East</td><td>Leaseweb New York (1 fallback server)</td>",
		"<td>Your server</td>",
		"7–11 pm UTC, −50%",
		"<h3>Slowest servers from 7–11 pm UTC</h3>",
		`<li aria-label="UK, Clouvider, London: 50% of its usual result, median 200 versus usual 400 megabits per second">UK · Clouvider, London: 50% of its usual result (median 200 vs usual 400 Mbit/s)</li>`,
		"17 busy",
		"left out everywhere in this report",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	for _, bad := range []string{"every region alike", "3 to 8 seconds"} {
		if strings.Contains(html, bad) {
			t.Errorf("report still says %q", bad)
		}
	}
	if strings.Contains(html, "192.168.1.20") || strings.Contains(html, "speedtest.") {
		t.Error("the report shows a speed test host")
	}
	speed := html[strings.Index(html, `<section id="speed"`):]
	speed = speed[:strings.Index(speed, "</section>")]
	if regexp.MustCompile(`AS[0-9]|Speed tests started on|DE-CIX`).MatchString(speed) {
		t.Error("the shared report's speed section names a network or the start date")
	}
	if own := f.build(t, d, Options{}); !regexp.MustCompile(`Based on [\d,]+ tests on \d+ days between \w{3} \d+ \w{3} and \w{3} \d+ \w{3}`).MatchString(own) {
		t.Errorf("the owner's report lacks the evidence's date range: %q", regexp.MustCompile(`Based on[^<]*`).FindAllString(own, 3))
	}
	if regexp.MustCompile(`days between`).MatchString(speed) {
		t.Error("the shared report has the evidence's date range")
	}
	// The owner's zone: every time of day in it, labelled.
	ny, _ := time.LoadLocation("America/New_York")
	d.Zone = func(context.Context) *time.Location { return ny }
	html = f.build(t, d, Options{Redact: true})
	if !strings.Contains(html, "3–7 pm EDT (UTC−4), −50%") || !strings.Contains(html, "(times in EDT (UTC−4)).") || strings.Contains(html, "pm UTC") {
		t.Error("the speed section's times are not in the owner's zone")
	}
	if html := f.build(t, f.deps(), Options{}); strings.Contains(html, `id="speed"`) {
		t.Error("speed section without speed tests")
	}
}
